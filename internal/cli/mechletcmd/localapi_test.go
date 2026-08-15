package mechletcmd

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/mecharion/mecharion/internal/agent"
)

// TestLocalStatusHandlerServesReadOnlyJSON 钉住 ADR-0026 的
// `--local` 诊断入口：真的走 unix socket + HTTP，不是直接调 handler——
// 这条路径的价值恰恰在传输层（socket 权限即认证），跳过它测不出什么。
//
// `agent.Agent` 用什么数据回答 LocalStatus 已经在
// internal/agent/localstatus_test.go 里逐字段核对过（同包能直接摆
// 一份 last 状态）；这里只验证 HTTP 层的编解码与方法限制，用一个没有
// 任何实例的 Agent 已经够看出「JSON 信封对不对、只接 GET」。
func TestLocalStatusHandlerServesReadOnlyJSON(t *testing.T) {
	a := agent.New(agent.Options{Node: "node-7"})

	sock := filepath.Join(t.TempDir(), "mechlet.sock")
	lis, err := listenLocalUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: localStatusHandler(a, "node-7")}
	go srv.Serve(lis)
	defer srv.Close()

	cli := unixHTTPClient(sock)

	t.Run("GET 返回 JSON 信封", func(t *testing.T) {
		resp, err := cli.Get("http://local/local/v1/status")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
		}
		var v LocalStatusView
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			t.Fatal(err)
		}
		if v.Node != "node-7" {
			t.Errorf("Node = %q，期望 node-7", v.Node)
		}
		if v.Instances == nil || len(v.Instances) != 0 {
			t.Errorf("Instances = %+v，期望空切片（没有实例调和过）", v.Instances)
		}
	})

	t.Run("POST 不被接受——这条入口只读", func(t *testing.T) {
		resp, err := cli.Post("http://local/local/v1/status", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Error("POST 不该被 200——这条路径只声明了 GET")
		}
	})
}

// TestListenLocalUnixSocketPermissions 钉住 ADR-0026 的安全边界：
// 「文件系统权限就是这条通道的认证」，socket 必须是 0600。
func TestListenLocalUnixSocketPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的 unix socket 文件没有 POSIX 权限位语义")
	}
	sock := filepath.Join(t.TempDir(), "mechlet.sock")
	lis, err := listenLocalUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()

	info, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket 权限 = %o，期望 0600", perm)
	}
}

// TestListenLocalUnixRemovesStaleSocket 确认上一次进程留下的 socket 文件
// 不会挡住新的 Listen——这是运维重启 mechlet 后的常规路径，不该失败。
func TestListenLocalUnixRemovesStaleSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "mechlet.sock")
	if err := os.WriteFile(sock, []byte("残留文件，不是真的 socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	lis, err := listenLocalUnix(sock)
	if err != nil {
		t.Fatalf("清理残留 socket 文件后应当能正常监听: %v", err)
	}
	lis.Close()
}

func unixHTTPClient(sock string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
		Timeout: 5 * time.Second,
	}
}
