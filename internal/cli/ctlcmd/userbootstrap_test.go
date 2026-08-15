package ctlcmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/mechd"
)

// 本文件钉住「继续 ADR-0039 的开发」这一步：`mechctl user bootstrap` 面向
// 无人值守场景，替代「人在浏览器里粘贴初始化令牌」那条路径——它必须真的
// 把令牌带上，而不是只是又发了一次没有令牌的请求。

// stubBootstrapServer 记录收到的 bootstrap 请求体，供断言令牌是不是真的
// 被带上了。
type stubBootstrapServer struct {
	srv  *httptest.Server
	last mechd.BootstrapBody
}

func newStubBootstrapServer(t *testing.T) *stubBootstrapServer {
	t.Helper()
	s := &stubBootstrapServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&s.last)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(mechd.AdminView{
				Name: "admin", Initialized: true,
			})
		}))
	t.Cleanup(s.srv.Close)
	return s
}

func runUserBootstrap(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	root := newMechctlRoot(NewUserCmd)
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"user", "bootstrap"}, args...))
	err := root.Execute()
	return out.String() + errBuf.String(), err
}

// passwordFile 写一份口令文件，供 --password-file 用——非交互环境下
// readPassword 拒绝从 stdin 裸读，这与命令本身面向脚本调用的定位是
// 同一件事，测试用同样的入口才是代表性的用法。
func passwordFile(t *testing.T, pw string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(p, []byte(pw), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestUserBootstrapSendsTheResolvedToken 是这条命令存在的全部意义：
// 它必须真的把 token 塞进请求体，不能只是换了个入口发一份没有令牌的
// bootstrap 请求——那样虽然「命令跑通了」，服务端仍然会因为 ADR-0039
// 的门禁而拒绝。
func TestUserBootstrapSendsTheResolvedToken(t *testing.T) {
	s := newStubBootstrapServer(t)
	pwFile := passwordFile(t, "a-long-enough-password")

	out, err := runUserBootstrap(t, "",
		"--server", s.srv.URL, "--token", "m7n_explicit-token",
		"--password-file", pwFile)
	if err != nil {
		t.Fatalf("不该报错: %v\n输出: %s", err, out)
	}
	if s.last.Token != "m7n_explicit-token" {
		t.Fatalf("请求体里的 token = %q，期望 %q", s.last.Token, "m7n_explicit-token")
	}
	if s.last.Password != "a-long-enough-password" {
		t.Errorf("请求体里的 password = %q", s.last.Password)
	}
}

// TestUserBootstrapReadsTokenFromDefaultFile 钉住「本机零配置」这条路：
// 不给 --token 时，应当自动读到 DefaultTokenFile 里的值，与 CA 证书的
// 零配置读取是同一条纪律（08-security §3.2）。这正是这条命令存在的
// 理由——脚本不需要知道令牌在哪，`mechctl` 自己找得到。
func TestUserBootstrapReadsTokenFromDefaultFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MECHARION_ROOT", root)
	tokenPath := filepath.Join(root, "etc", "mecharion", "admin.token")
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("m7n_from-disk-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := newStubBootstrapServer(t)
	pwFile := passwordFile(t, "a-long-enough-password")
	out, err := runUserBootstrap(t, "",
		"--server", s.srv.URL, "--password-file", pwFile)
	if err != nil {
		t.Fatalf("不该报错: %v\n输出: %s", err, out)
	}
	if s.last.Token != "m7n_from-disk-token" {
		t.Fatalf("请求体里的 token = %q，期望从磁盘读到的 %q",
			s.last.Token, "m7n_from-disk-token")
	}
}

// TestUserBootstrapWithoutAnyTokenFailsBeforeSendingAnything 钉住：
// 一个 token 都解析不出来时，命令必须在本地就失败，而不是发一个空
// token 出去指望服务端拦。**口令要给对**（--password-file），否则
// readPassword 会先因为「没给口令」失败，测到的就不是本该测的这件事。
func TestUserBootstrapWithoutAnyTokenFailsBeforeSendingAnything(t *testing.T) {
	t.Setenv("MECHARION_ROOT", t.TempDir()) // 让默认路径读不到任何 token
	s := newStubBootstrapServer(t)
	pwFile := passwordFile(t, "a-long-enough-password")

	_, err := runUserBootstrap(t, "", "--server", s.srv.URL, "--password-file", pwFile)
	if err == nil {
		t.Fatal("没有 token 时应当报错")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("错误信息应当说清是 token 的问题，实际: %v", err)
	}
	if s.last.Token != "" || s.last.Password != "" {
		t.Errorf("不该发出任何请求，实际收到: %+v", s.last)
	}
}
