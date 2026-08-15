package mechletcmd

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mecharion/mecharion/internal/mechd"
	"github.com/mecharion/mecharion/internal/pki"
)

// joinOptions 是一次加入所需的一切。
type joinOptions struct {
	Server  string
	Token   string
	CAHash  string
	Node    string
	Address string
	ConfDir string
}

// runJoin 用一张 token 换回本机证书，落到 <conf-dir>/pki。
//
// **私钥在这里生成，不出这台机器**：发出去的只有 CSR（一份公钥 + 自签名）。
func runJoin(out io.Writer, o joinOptions) error {
	if o.Token == "" {
		return fmt.Errorf("--token cannot be empty")
	}
	if o.CAHash == "" {
		return fmt.Errorf(
			"--ca-hash cannot be empty.\n" +
				"  It's mechd's CA public-key fingerprint, used to verify the peer while\n" +
				"  there's **no CA yet** — without it, this step degrades to trust-on-first-use,\n" +
				"  and a man-in-the-middle can't be detected.\n" +
				"  The join command printed by `mechctl node token create` includes it")
	}

	fmt.Fprintf(out, "[1/3] generating a local key and certificate request\n")
	csrPEM, keyPEM, err := pki.NewNodeCSR(o.Node)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "[2/3] exchanging the token with %s for a certificate\n", o.Server)
	res, err := postJoin(o, csrPEM)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "[3/3] writing certificates to disk\n")
	dir := pki.Dir(o.ConfDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		body []byte
		mode os.FileMode
	}{
		{"node.crt", []byte(res.Cert), 0o644},
		{"node.key", keyPEM, 0o600},
		{"ca.crt", []byte(res.CA), 0o644},
	} {
		p := filepath.Join(dir, f.name)
		if err := os.WriteFile(p, f.body, f.mode); err != nil {
			return err
		}
		fmt.Fprintf(out, "      %s\n", p)
	}
	fmt.Fprintf(out, "\nJoined as node %s.\n", res.Node)
	return nil
}

// postJoin 发一次 join 请求。
//
// **校验对端用的是 CA 公钥指纹，不是系统信任库**：这台机器还没有 CA，
// 而指纹是跟 token 一起带外交付的（22-multi-node §3.2）。
func postJoin(o joinOptions, csrPEM []byte) (*mechd.JoinResponse, error) {
	body, err := json.Marshal(mechd.JoinBody{
		Token: o.Token, Node: o.Node, CSR: string(csrPEM), Address: o.Address,
	})
	if err != nil {
		return nil, err
	}

	cli := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				// 关掉标准校验之后**必须**自己校验，否则就是裸奔。
				// VerifyPeerCertificate 在这里不是可选项。
				InsecureSkipVerify:    true, //nolint:gosec // 由下面的指纹校验替代
				VerifyPeerCertificate: verifyCAHash(o.CAHash),
			},
		},
	}
	url := strings.TrimSuffix(o.Server, "/") + mechd.APIPrefix + "/join"
	resp, err := cli.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", url, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("join rejected: %s", e.Error)
		}
		return nil, fmt.Errorf("join failed (HTTP %d): %s", resp.StatusCode, raw)
	}
	var out mechd.JoinResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	if out.Cert == "" || out.CA == "" {
		return nil, fmt.Errorf("no certificate in the response")
	}
	return &out, nil
}

// verifyCAHash 校验证书链的根，其公钥指纹必须与带外拿到的一致。
//
// 取**链上最后一张**（根）而不是叶子：叶子是服务端证书，一年一换；
// 根是 CA，指纹才稳定。
func verifyCAHash(want string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("peer presented no certificate")
		}
		var got []string
		for _, raw := range rawCerts {
			cert, err := x509.ParseCertificate(raw)
			if err != nil {
				return err
			}
			h := pki.PublicKeyHash(cert)
			got = append(got, h)
			if h == want {
				return nil
			}
		}
		// 自签场景下服务端可能只发叶子证书而不带 CA。此时用叶子的
		// **签发者**校验不了，只能明确报错——比默默放过去强得多。
		return fmt.Errorf(
			"the peer certificate's CA fingerprint doesn't match, this may not be the machine you meant to connect to\n"+
				"  expected: %s\n  got:      %s", want, strings.Join(got, "\n            "))
	}
}
