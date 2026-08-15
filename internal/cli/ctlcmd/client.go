package ctlcmd

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 默认连接参数。
const (
	// DefaultServer 是未配置时连的地址。
	DefaultServer = "https://127.0.0.1:8443"
	// DefaultCAFile 是本机自签 CA 的位置。
	//
	// **本机零配置**：mechctl 直接读它，不需要任何 context 设置
	// （08-security §3.2）。
	DefaultCAFile = "/etc/mecharion/pki/ca.crt"
	// DefaultTokenFile 是本机 admin token 的位置。
	DefaultTokenFile = "/etc/mecharion/admin.token"
	// DefaultLocalSocket 是 `--local` 只读诊断入口的地址——直连本机
	// mechlet，不经过 mechd（ADR-0026、10-cli §1.5）。
	DefaultLocalSocket = "/run/mecharion/mechlet.sock"
)

// ClientConfig 是连接 mechd 所需的一切。
type ClientConfig struct {
	Server string
	Token  string
	CAFile string
	// Insecure 跳过证书校验。**仅供排障**——它让中间人攻击无法被发现。
	Insecure bool
	Site     string
	// Local 为 true 时不连 mechd，直连本机 mechlet 的只读诊断 socket——
	// Server/Token/CAFile/Insecure 全部被忽略（ADR-0026）。
	Local bool
	// LocalSocket 覆盖 --local 的 socket 路径，缺省 DefaultLocalSocket；
	// 只在 Local 为 true 时生效。
	LocalSocket string
}

// Client 是 mechd HTTP API 的客户端，或 `--local` 模式下本机 mechlet
// 只读诊断入口的客户端——两者复用同一个类型，因为二者都只是
// 「往一个 JSON HTTP 端点发请求」，差别仅在传输层怎么建连接、要不要
// 认证。分开建两个类型只会让调用方（每条命令）多判一次该拿哪一个。
type Client struct {
	cfg  ClientConfig
	http *http.Client
}

// NewClient 构造客户端。
//
// 证书信任是**显式**的：读本机 CA，而不是 TOFU（首次连接即信任）——
// 后者会让中间人攻击在首次连接时无法被发现（08-security §3.2）。
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Local {
		return newLocalClient(cfg)
	}
	if cfg.Server == "" {
		cfg.Server = DefaultServer
	}
	cfg.Server = strings.TrimSuffix(cfg.Server, "/")

	if cfg.Token == "" {
		if b, err := os.ReadFile(defaultPath(DefaultTokenFile)); err == nil {
			cfg.Token = strings.TrimSpace(string(b))
		}
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf(
			"no token. Specify one with --token, or let mechctl read it from %s",
			DefaultTokenFile)
	}

	tr := &http.Transport{}
	if strings.HasPrefix(cfg.Server, "https://") {
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
		switch {
		case cfg.Insecure:
			tlsCfg.InsecureSkipVerify = true
		default:
			caFile := cfg.CAFile
			if caFile == "" {
				caFile = defaultPath(DefaultCAFile)
			}
			pem, err := os.ReadFile(caFile)
			if err != nil {
				return nil, fmt.Errorf(
					"couldn't read CA certificate %s: %w\n"+
						"  → this should be readable directly on the local machine; for remote connections, "+
						"specify --ca-file or export one with mechd ca export",
					caFile, err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("no usable certificate in %s", caFile)
			}
			tlsCfg.RootCAs = pool
		}
		tr.TLSClientConfig = tlsCfg
	}

	return &Client{
		cfg:  cfg,
		http: &http.Client{Transport: tr, Timeout: 60 * time.Second},
	}, nil
}

// localServerPlaceholder 是 --local 请求的 URL 主机部分。
//
// 从不真正被拿去解析域名——`DialContext` 无视它，永远直连
// LocalSocket——只是 `net/http` 要求 URL 长得像一个 URL。
const localServerPlaceholder = "http://mechlet.local"

// newLocalClient 构造 `--local` 模式的客户端：不认证、不用 TLS，
// 直连本机 mechlet 的 unix socket（ADR-0026：文件系统权限就是这条
// 通道的认证）。
func newLocalClient(cfg ClientConfig) (*Client, error) {
	sock := cfg.LocalSocket
	if sock == "" {
		sock = defaultPath(DefaultLocalSocket)
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}
	return &Client{
		cfg:  ClientConfig{Server: localServerPlaceholder, Site: cfg.Site, Local: true},
		http: &http.Client{Transport: tr, Timeout: 10 * time.Second},
	}, nil
}

// Token 返回这个客户端解析出来的 token 明文。
//
// **只给 `user bootstrap` 这一个调用方用**：初始化门禁（ADR-0039）与
// Bearer 认证复用的是同一个值，本机零配置就能从 DefaultTokenFile 读到，
// 因此「用来认证自己」与「用来完成首次初始化」在这里是同一份凭据，
// 不是巧合。除此之外没有别的命令需要把 token 明文掏出来。
func (c *Client) Token() string { return c.cfg.Token }

// defaultPath 在非 Linux 上把绝对路径映射到相对目录，方便本机开发。
func defaultPath(p string) string {
	if root := os.Getenv("MECHARION_ROOT"); root != "" {
		return filepath.Join(root, strings.TrimPrefix(p, "/"))
	}
	return p
}

// Do 发一次请求并把响应解进 out。
func (c *Client) Do(method, path string, body, out any) error {
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}

	var req *http.Request
	var err error
	if rdr != nil {
		req, err = http.NewRequest(method, c.url(path), rdr)
	} else {
		req, err = http.NewRequest(method, c.url(path), nil)
	}
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, c.http, out)
}

// Upload 上传一个二进制文件（当前只用于 .mpack）。
//
// 与 Do 是两条独立的路：Do 的请求体一律 JSON 编码，复用它会把整个文件
// 当成一个 JSON 字符串塞进去。服务端也是分开处理的（`uploadPack` 直接
// 读裸字节，见 internal/mechd/upload.go）——两边的编码约定必须对得上。
//
// **用独立的、更长的超时**：Do 那份 60 秒的超时是给普通 API 调用配的，
// Pack 上传上限有 4 GiB（httpapi.go 的 maxUpload），慢链路上 60 秒很可能
// 连一半都传不完。
func (c *Client) Upload(path string, r io.Reader, out any) error {
	req, err := http.NewRequest(http.MethodPost, c.url(path), r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	up := &http.Client{Transport: c.http.Transport, Timeout: 10 * time.Minute}
	return c.do(req, up, out)
}

// url 拼出请求的完整地址，按需带上 site 查询参数。
func (c *Client) url(path string) string {
	full := path
	if c.cfg.Site != "" {
		full = appendQuery(path, url.Values{"site": {c.cfg.Site}})
	}
	return c.cfg.Server + full
}

// do 发送已经建好的请求，处理认证头、错误分类与响应解码——Do 与
// Upload 共用这一段，两者的差别只在请求体怎么编码、超时给多久。
func (c *Client) do(req *http.Request, h *http.Client, out any) error {
	// --local 不带认证头：socket 权限已经是认证（ADR-0026），
	// 带一个空 Bearer 头没有意义，徒增一行调试时的噪音。
	if !c.cfg.Local {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}

	resp, err := h.Do(req)
	if err != nil {
		if c.cfg.Local {
			return fmt.Errorf("connecting to the local mechlet's --local diagnostic endpoint: %w\n"+
				"  → check it's running: systemctl status mecharion-mechlet", err)
		}
		return fmt.Errorf("connecting to mechd (%s): %w\n"+
			"  → check it's running: systemctl status mecharion-mechd", c.cfg.Server, err)
	}
	defer resp.Body.Close()

	dec := json.NewDecoder(resp.Body)
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&e); err != nil || e.Error == "" {
			return fmt.Errorf("mechd returned %s", resp.Status)
		}
		// 服务端给的错误信息就是给用户看的文案，原样端出去。
		// 再包一层「请求失败:」只会把真正有用的那句挤到后面。
		return apiError{code: resp.StatusCode, msg: e.Error}
	}
	if out == nil {
		return nil
	}
	return dec.Decode(out)
}

// apiError 是服务端返回的错误。
type apiError struct {
	code int
	msg  string
}

func (e apiError) Error() string { return e.msg }

// Code 返回 HTTP 状态码。
func (e apiError) Code() int { return e.code }
