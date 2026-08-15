package ctlcmd

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/protocol"
)

// 本文件钉住 ADR-0026：`mechctl --local component status` 直连
// 本机 mechlet 的只读诊断入口，不经过 mechd。走真实的 unix socket + HTTP
// + cobra 执行路径——这条命令存在的全部意义就是「mechd 不可达时还能用」，
// 跳过真实传输层测不出这一点。

// stubMechletSocket 起一个监听在 unix socket 上的假 mechlet，
// 只答 GET /local/v1/status。
func stubMechletSocket(t *testing.T, view localStatusResponse) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "mechlet.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /local/v1/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(view)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(lis)
	t.Cleanup(func() { srv.Close() })
	return sock
}

// runLocal 走真实 root 命令树，且 `--local` 特意放在名词**前面**
// （`mechctl --local component status`）——这是 10-cli §1.5/troubleshooting
// 等处文档写下的规范用法，而 `--local` 此前各名词命令各自 Bind 一份，
// 只在名词之后出现才认得，`--local` 在前会被 cobra 当成待值 flag 误吞掉
// 紧跟着的 "component"。测试走这个顺序才钉得住这条回归。
func runLocal(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newMechctlRoot(NewComponentCmd)
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(append([]string{"--local", "component", "status"}, args...))
	err := root.Execute()
	return out.String() + errBuf.String(), err
}

// TestLocalStatusShowsThisNodesInstances 是这条命令存在的核心场景：
// 不给 mechd 地址、不给 token，只连本机 socket，看到的是这台机器上
// 真实在跑的实例。
func TestLocalStatusShowsThisNodesInstances(t *testing.T) {
	sock := stubMechletSocket(t, localStatusResponse{
		Node: "node-7",
		Instances: []protocol.InstanceStatus{
			{
				Component: "pg-main", Role: "replica", Generation: 7,
				Workload: &protocol.WorkloadStatus{
					State: "running", Since: "2026-01-01T00:00:00Z",
				},
				Health: &protocol.HealthStatus{State: "healthy"},
			},
		},
	})

	out, err := runLocal(t, "--local-socket", sock)
	if err != nil {
		t.Fatalf("不该报错: %v\n%s", err, out)
	}
	for _, want := range []string{"node-7", "pg-main", "replica", "Running"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出里应当包含 %q，实际:\n%s", want, out)
		}
	}
}

// TestLocalStatusRejectsComponentName 钉住参数约束：--local 看的是整机，
// 不接受按 Component 名过滤（那需要跨节点核对期望状态，本机做不到）。
func TestLocalStatusRejectsComponentName(t *testing.T) {
	sock := stubMechletSocket(t, localStatusResponse{Node: "node-7"})
	_, err := runLocal(t, "--local-socket", sock, "pg-main")
	if err == nil {
		t.Fatal("--local 模式下带组件名应当被拒绝")
	}
}

// TestLocalStatusFailsClearlyWhenMechletUnreachable 钉住失败模式的验收
// 要求：mechlet 也连不上时，错误必须说清楚是连不上本机 mechlet，
// 而不是一句通用的连接失败，更不能被误认成「mechd 连不上」。
func TestLocalStatusFailsClearlyWhenMechletUnreachable(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "no-such-mechlet.sock") // 从未监听
	out, err := runLocal(t, "--local-socket", sock)
	if err == nil {
		t.Fatal("mechlet 不可达时应当报错")
	}
	if !strings.Contains(out+err.Error(), "mechlet") {
		t.Errorf("错误信息应当说清是连不上本机 mechlet，实际: %v\n%s", err, out)
	}
}

// TestLocalStatusEmptyNode 确认本机没有任何实例时给出清楚的空态提示，
// 不是裸表头或者一段 JSON 噪音。
func TestLocalStatusEmptyNode(t *testing.T) {
	sock := stubMechletSocket(t, localStatusResponse{Node: "node-7"})
	out, err := runLocal(t, "--local-socket", sock)
	if err != nil {
		t.Fatalf("不该报错: %v\n%s", err, out)
	}
	if !strings.Contains(out, "node-7") || !strings.Contains(out, "No instances") {
		t.Errorf("应当明确说本机没有实例，实际:\n%s", out)
	}
}
