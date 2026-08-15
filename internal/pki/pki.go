// Package pki 是 Mecharion 自己的证书体系：自签 CA、服务端证书、节点客户端证书。
//
// **单独成包而不是留在 mechd 的命令里**，是因为它有三个使用者：mechd 启动时
// 备好服务端证书、`mechd ca` 手工签发、以及 M7 第 3 步的 Join RPC 自动签发。
// 三处必须用同一套签发逻辑——两套 x509 模板意味着两套边界条件，
// 而证书的边界条件出错时症状都长得一样（一句 TLS 错误）。
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/mecharion/mecharion/internal/ident"
)

// 证书有效期（08-security §3.2）。
const (
	// CAValidity 是自签 CA 的有效期。
	//
	// 10 年：它要被导进浏览器与远程 mechctl，重签一次意味着所有客户端
	// 都得重新导入——那件事的成本远高于一张长期 CA 的风险。
	CAValidity = 10 * 365 * 24 * time.Hour
	// ServerValidity 是服务端证书的有效期。
	ServerValidity = 365 * 24 * time.Hour
	// RenewBefore 是提前重签的阈值。
	RenewBefore = 30 * 24 * time.Hour
)

// CertPaths 是一组证书文件的位置。
type CertPaths struct {
	CAFile   string
	CertFile string
	KeyFile  string
}

// EnsureServer 准备好自签 CA 与服务端证书，必要时重新签发。
//
// **首次启动自动生成，无需任何人工步骤**：一旦要求运维先弄一张证书才能
// 启动，绝大多数人会转而去关掉 HTTPS。
// **参数是 pki 目录，不是 conf 目录**——本包的每个函数都是。混着来
// （一处 confDir、一处 pkiDir）会让调用方多传一层或少传一层，
// 而症状是「证书明明生成了却读不到」。
func EnsureServer(pkiDir, listenAddr string) (CertPaths, error) {
	if err := os.MkdirAll(pkiDir, 0o700); err != nil {
		return CertPaths{}, err
	}
	p := CertPaths{
		CAFile:   filepath.Join(pkiDir, "ca.crt"),
		CertFile: filepath.Join(pkiDir, "server.crt"),
		KeyFile:  filepath.Join(pkiDir, "server.key"),
	}

	caCert, caKey, err := EnsureCA(pkiDir)
	if err != nil {
		return CertPaths{}, err
	}

	sans := collectSANs(listenAddr)
	if ok, why := serverCertUsable(p.CertFile, sans); ok {
		return p, nil
	} else if why != "" {
		slog.Info("reissuing server certificate", "reason", why)
	}

	if err := issueServer(p, caCert, caKey, sans); err != nil {
		return CertPaths{}, err
	}
	return p, nil
}

// EnsureCA 读出（或首次生成）自签 CA。
func EnsureCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	if cert, key, err := loadPair(certPath, keyPath); err == nil {
		return cert, key, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Mecharion CA",
			Organization: []string{"Mecharion"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	if err := writeCert(certPath, der); err != nil {
		return nil, nil, err
	}
	if err := writeKey(keyPath, key); err != nil {
		return nil, nil, err
	}
	slog.Info("generated self-signed CA", "path", certPath, "validity", "10y")

	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

// serverCertUsable 判断现有服务端证书是否还能用。
//
// 两个条件都要满足：**还没快过期**，且**覆盖了当前要用的地址**。
// 后者是给 DHCP 环境准备的——主机 IP 变了，旧证书虽然没过期却已经不对。
func serverCertUsable(path string, sans sanSet) (bool, string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, ""
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return false, "证书文件无法解析"
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false, "证书内容无法解析"
	}
	if time.Until(cert.NotAfter) < RenewBefore {
		return false, fmt.Sprintf("剩余有效期不足 %d 天", int(RenewBefore.Hours()/24))
	}
	// 旧版本只写了叶子证书。没有 CA 那一段，join 的指纹校验就无从做起，
	// 因此把它当成「该重签了」——重签是无损的，而缺链会让加入直接失败。
	if rest := trailingPEM(b); rest == 0 {
		return false, "证书文件里没有带上 CA（join 的指纹校验需要它）"
	}
	for _, ip := range sans.ips {
		if !hasIP(cert.IPAddresses, ip) {
			return false, "本机地址 " + ip.String() + " 不在证书的 SAN 里"
		}
	}
	for _, name := range sans.dns {
		if !hasName(cert.DNSNames, name) {
			return false, "主机名 " + name + " 不在证书的 SAN 里"
		}
	}
	return true, ""
}

func issueServer(p CertPaths, ca *x509.Certificate, caKey *ecdsa.PrivateKey, sans sanSet) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}
	cn := "mechd"
	if len(sans.dns) > 0 {
		cn = sans.dns[0]
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"Mecharion"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(ServerValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     sans.dns,
		IPAddresses:  sans.ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	// **证书文件里带上 CA**，服务端因此会把整条链发给对端。
	//
	// 自签根一般不必发，这里发是为了 join：一台还没加入的机器手上只有
	// CA 的公钥指纹，它要在链里找到那张 CA 才校验得了（22-multi-node §3.2）。
	// 多发一张根证书没有任何代价——它本来就是公开的。
	bundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	bundle = append(bundle, pem.EncodeToMemory(
		&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})...)
	if err := os.WriteFile(p.CertFile, bundle, 0o644); err != nil {
		return err
	}
	return writeKey(p.KeyFile, key)
}

// sanSet 是证书要覆盖的名字与地址。
type sanSet struct {
	dns []string
	ips []net.IP
}

// collectSANs 收集本机的全部可达地址。
//
// 把探测到的本机 IP 全放进 SAN，是因为「拿笔记本连门店那台机」时用的是
// 哪个地址，装机的人事先并不知道。
func collectSANs(listenAddr string) sanSet {
	out := sanSet{dns: []string{"localhost"}}
	seen := map[string]bool{"localhost": true}

	if host, err := os.Hostname(); err == nil && host != "" && !seen[host] {
		out.dns = append(out.dns, host)
		seen[host] = true
	}
	out.ips = append(out.ips, net.IPv4(127, 0, 0, 1), net.IPv6loopback)

	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && !ip.IsLoopback() && !hasIP(out.ips, ip) {
				out.ips = append(out.ips, ip)
			}
		}
	}
	// 监听地址里显式写了 IP 时也纳入
	if host, _, err := net.SplitHostPort(listenAddr); err == nil {
		if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() && !hasIP(out.ips, ip) {
			out.ips = append(out.ips, ip)
		}
	}
	return out
}

func hasIP(list []net.IP, ip net.IP) bool {
	for _, x := range list {
		if x.Equal(ip) {
			return true
		}
	}
	return false
}

func hasName(list []string, name string) bool {
	for _, x := range list {
		if x == name {
			return true
		}
	}
	return false
}

// ── 文件读写 ────────────────────────────────────────────────────────────

func loadPair(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	kb, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	cblock, _ := pem.Decode(cb)
	kblock, _ := pem.Decode(kb)
	if cblock == nil || kblock == nil {
		return nil, nil, fmt.Errorf("PEM parsing failed")
	}
	cert, err := x509.ParseCertificate(cblock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(kblock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func writeCert(path string, der []byte) error {
	return os.WriteFile(path,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// writeKey 落私钥，0600。
func writeKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return os.WriteFile(path,
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600)
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

// ── 节点客户端证书（M7）────────────────────────────────────────────────

// NodeValidity 是节点客户端证书的有效期。
//
// 与服务端证书同为 1 年、同用 RenewBefore 提前 30 天重签：两套期限只会
// 让「哪张证书什么时候到期」变成一个需要查表的问题。
const NodeValidity = ServerValidity

// IssueNode 用 CA 给一个节点签一张客户端证书。
//
// **CN 就是节点名**——那是多节点下唯一被信任的身份来源（ADR-0034）。
// 不往 SAN 里放任何东西：节点从不被别人连，它只主动拨出，没有可供校验的
// 地址；放进去只会让人以为那是身份的一部分。
// validity <= 0 时用 NodeValidity。短有效期是**真实需求**（高安全环境
// 用几天一换的证书），也让「轮换」这件事可以在几分钟内验一遍——
// 否则要等一年才知道它成不成立。
func IssueNode(pkiDir, node string, validity time.Duration) (CertPaths, error) {
	if validity <= 0 {
		validity = NodeValidity
	}
	// **不信任调用方已经校验过。** pki 有三个使用者（mechd 启动、`mechd ca`
	// 手工签发、Join RPC），node 会被直接拼进证书/私钥文件名；与其要求
	// 三处各自记得先校验，不如让这个共享原语自己校验一遍。
	if err := ident.Validate(ident.Node, node); err != nil {
		return CertPaths{}, err
	}
	if err := os.MkdirAll(pkiDir, 0o700); err != nil {
		return CertPaths{}, err
	}
	ca, caKey, err := EnsureCA(pkiDir)
	if err != nil {
		return CertPaths{}, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return CertPaths{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return CertPaths{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: node, Organization: []string{"Mecharion"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return CertPaths{}, err
	}

	p := CertPaths{
		CAFile:   filepath.Join(pkiDir, "ca.crt"),
		CertFile: filepath.Join(pkiDir, "nodes", node+".crt"),
		KeyFile:  filepath.Join(pkiDir, "nodes", node+".key"),
	}
	if err := os.MkdirAll(filepath.Join(pkiDir, "nodes"), 0o700); err != nil {
		return CertPaths{}, err
	}
	if err := writeCert(p.CertFile, der); err != nil {
		return CertPaths{}, err
	}
	if err := writeKey(p.KeyFile, key); err != nil {
		return CertPaths{}, err
	}
	return p, nil
}

// CAPool 读出 CA，供校验对端证书用。
func CAPool(caFile string) (*x509.CertPool, error) {
	b, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading CA %s: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("no usable CA certificate in %s", caFile)
	}
	return pool, nil
}

// Dir 返回 confDir 下的 pki 目录。
func Dir(confDir string) string { return filepath.Join(confDir, "pki") }

// ── CSR 签发（join 路径）────────────────────────────────────────────────

// SignNodeCSR 签一份节点提交的 CSR，返回证书与 CA 的 PEM。
//
// **私钥不过网**：节点自己生成密钥对，只把公钥装进 CSR 发出来。
// `IssueNode` 那条离线路径做不到这件事（那台机器不可达，只能由 mechd 代
// 生成密钥再搬过去），因此它是次选而不是等价的另一条路。
//
// CN **以调用方给的 node 为准**，不信 CSR 里写的：CSR 是节点自己造的，
// 里面的 CN 与 token 的授权范围无关。授权在 token，身份由授权决定。
func SignNodeCSR(pkiDir, node string, csrPEM []byte) (certPEM, caPEM []byte, err error) {
	if node == "" {
		return nil, nil, fmt.Errorf("node name cannot be empty — it is the certificate's identity")
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, nil, fmt.Errorf("not a PEM-encoded certificate request")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing certificate request: %w", err)
	}
	// **必须验签**：它证明提交者确实持有那把私钥。不验的话，任何人都能
	// 拿别人的公钥来换一张证书，而那张证书的持有者会是别人。
	if err := csr.CheckSignature(); err != nil {
		return nil, nil, fmt.Errorf("certificate request's self-signature is invalid: %w", err)
	}

	ca, caKey, err := EnsureCA(pkiDir)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: node, Organization: []string{"Mecharion"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(NodeValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, csr.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	caBytes, err := os.ReadFile(filepath.Join(pkiDir, "ca.crt"))
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), caBytes, nil
}

// NewNodeCSR 在节点本机生成密钥对与 CSR。
func NewNodeCSR(node string) (csrPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: node, Organization: []string{"Mecharion"}},
	}, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

// ── CA 指纹（join 的信任锚）────────────────────────────────────────────

// HashPrefix 是 CA 指纹的前缀，让它一眼可辨。
const HashPrefix = "sha256:"

// PublicKeyHash 返回一张证书的 SubjectPublicKeyInfo 摘要。
//
// **取公钥而不是整张证书**：CA 到期重签时公钥可以不变，那时旧指纹仍然
// 有效。按整张证书算的话，所有还没用掉的 join token 会在重签那一刻集体
// 失效——而那件事没有任何人会预期。
//
// 与 kubeadm 的 --discovery-token-ca-cert-hash 是同一个做法。
func PublicKeyHash(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return HashPrefix + hex.EncodeToString(sum[:])
}

// CAHash 读出 CA 并返回它的公钥指纹。
func CAHash(pkiDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(pkiDir, "ca.crt"))
	if err != nil {
		return "", fmt.Errorf("reading CA: %w", err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return "", fmt.Errorf("CA file is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	return PublicKeyHash(cert), nil
}

// trailingPEM 数一数第一块之后还有几块 PEM。
func trailingPEM(b []byte) int {
	_, rest := pem.Decode(b)
	n := 0
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			return n
		}
		n++
	}
}

// CAHashFromFile 由一个 CA 文件算出公钥指纹。
//
// 与 CAHash 的区别只是入参：那个收 pki 目录，这个收具体文件——
// mechctl 手上有的是 `--ca-file` 指向的那一份，不一定在 pki 目录里。
func CAHashFromFile(caFile string) (string, error) {
	b, err := os.ReadFile(caFile)
	if err != nil {
		return "", fmt.Errorf("reading CA %s: %w", caFile, err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return "", fmt.Errorf("%s is not PEM", caFile)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	return PublicKeyHash(cert), nil
}
