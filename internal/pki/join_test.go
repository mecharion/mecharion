package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"strings"
	"testing"
)

// TestSignNodeCSRUsesCallerCN 钉住 ADR-0034 的一条：
// **CN 由授权决定，不信 CSR 里写的**。
//
// CSR 是节点自己造的，里面的 CN 与 token 的授权范围无关。信它的话，
// 一张绑定了 store-042 的 token 能签出任何名字的证书——绑名就白绑了。
func TestSignNodeCSRUsesCallerCN(t *testing.T) {
	dir := t.TempDir()
	csrPEM, _, err := NewNodeCSR("我说我是谁都行")
	if err != nil {
		t.Fatal(err)
	}

	certPEM, _, err := SignNodeCSR(dir, "store-042", csrPEM)
	if err != nil {
		t.Fatalf("签发: %v", err)
	}
	cert := parseCert(t, certPEM)
	if cert.Subject.CommonName != "store-042" {
		t.Errorf("CN 应当是调用方给的 store-042，实际 %q", cert.Subject.CommonName)
	}
}

// TestSignNodeCSRRejectsForgedSignature 钉住「必须验 CSR 的自签名」。
//
// 不验的话，任何人都能拿**别人的公钥**来换一张证书——而那张证书的
// 真正持有者会是别人，冒名就成立了。
func TestSignNodeCSRRejectsForgedSignature(t *testing.T) {
	dir := t.TempDir()
	csrPEM, _, err := NewNodeCSR("n1")
	if err != nil {
		t.Fatal(err)
	}
	// 把 CSR 的最后一个字节改掉，签名就对不上了
	block, _ := pem.Decode(csrPEM)
	block.Bytes[len(block.Bytes)-1] ^= 0xff
	bad := pem.EncodeToMemory(block)

	if _, _, err := SignNodeCSR(dir, "n1", bad); err == nil {
		t.Fatal("签名被改过的 CSR 应当被拒")
	}
}

// TestSignedNodeCertIsClientOnly 钉住签出来的是**客户端**证书。
//
// 带上 ServerAuth 的话，一张节点证书就能用来冒充 mechd 本身——
// 而节点从不需要被别人连（它只主动拨出）。
func TestSignedNodeCertIsClientOnly(t *testing.T) {
	dir := t.TempDir()
	csrPEM, _, err := NewNodeCSR("n1")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, _, err := SignNodeCSR(dir, "n1", csrPEM)
	if err != nil {
		t.Fatal(err)
	}
	cert := parseCert(t, certPEM)
	for _, u := range cert.ExtKeyUsage {
		if u == x509.ExtKeyUsageServerAuth {
			t.Error("节点证书不该带 ServerAuth——节点从不被别人连")
		}
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("应当只有 ClientAuth，实际 %v", cert.ExtKeyUsage)
	}
}

// TestPublicKeyHashIsStableAcrossReissue 钉住指纹取的是**公钥**。
//
// 按整张证书算的话，CA 重签那一刻所有还没用掉的 join token 会集体失效
// ——而那件事没有任何人会预期（22-multi-node §3.2）。
func TestPublicKeyHashIsStableAcrossReissue(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(cn string) *x509.Certificate {
		tmpl := &x509.Certificate{
			SerialNumber: mustSerial(t),
			Subject:      pkix.Name{CommonName: cn},
			IsCA:         true, BasicConstraintsValid: true,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		c, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	// 同一把密钥、不同的证书内容（重签的样子）
	a, b := mk("Mecharion CA"), mk("Mecharion CA v2")
	if PublicKeyHash(a) != PublicKeyHash(b) {
		t.Error("同一把公钥重签之后指纹应当不变")
	}
	if !strings.HasPrefix(PublicKeyHash(a), HashPrefix) {
		t.Errorf("指纹应当带 %s 前缀，实际 %s", HashPrefix, PublicKeyHash(a))
	}
}

// TestServerCertCarriesCA 钉住服务端证书文件里带着 CA。
//
// 少了它，一台**还没有 CA** 的机器无法在链里找到那张 CA，
// join 的指纹校验就无从做起（22-multi-node §3.2）。
func TestServerCertCarriesCA(t *testing.T) {
	dir := t.TempDir()
	p, err := EnsureServer(dir, "0.0.0.0:8443")
	if err != nil {
		t.Fatal(err)
	}
	body := readFile(t, p.CertFile)
	if n := countPEM(body); n < 2 {
		t.Fatalf("服务端证书文件里应当是「叶子 + CA」两块 PEM，实际 %d 块", n)
	}
	// 第二块必须真的是那张 CA
	_, rest := pem.Decode(body)
	blk, _ := pem.Decode(rest)
	ca, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA {
		t.Error("第二块应当是 CA 证书")
	}
	want, err := CAHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := PublicKeyHash(ca); got != want {
		t.Errorf("链里那张 CA 的指纹 = %s，期望 %s", got, want)
	}
}

// ── 夹具 ────────────────────────────────────────────────────────────────

func parseCert(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("不是 PEM")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustSerial(t *testing.T) *big.Int {
	t.Helper()
	s, err := randomSerial()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func countPEM(b []byte) int {
	n := 0
	rest := b
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			return n
		}
		n++
	}
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
