//go:build linux

package e2e

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// saLocalSocket 是这条测试专用的 --local socket，与真实安装用的
// /run/mecharion/mechlet.sock 分开，避免连累同一次运行里的其它测试。
var saLocalSocket = filepath.Join(saDataDir, "run", "mechlet.sock")

// TestLocalStatusWorksWhenMechdIsDown 是 ADR-0026 的验收：
// `mechctl --local` 存在的唯一理由是「mechd 不可达时还能看」。
//
// 这条测试特意包含**失败模式**（06 缺陷台账的验收要求）：
// 不只验证 --local 平时能用，更要验证 mechd 真的停掉之后——常规路径
// 报连接失败——--local 依然能读到本机实例的真实状态。跳过这一步，
// 测的就只是「多了一个可以查看状态的接口」，而不是这个入口存在的
// 理由本身。
func TestLocalStatusWorksWhenMechdIsDown(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cleanupStandalone(ctx, t)
	t.Cleanup(func() { cleanupStandalone(context.Background(), t) })

	stagePack(t)
	sum := installBlobIn(t, saDataDir, buildTarball(t))
	rewritePackBlob(t, sum)

	mechd := startMechd(ctx, t)
	waitAPI(ctx, t)
	token := readToken(t)
	seedSite(ctx, t)
	agent := startAgent(ctx, t, "--local-socket", saLocalSocket)
	defer stopProc(agent)

	if out, err := runCtl(ctx, token, "component", "deploy", "go-webapp",
		"-c", "web", "--nodes", saNode); err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	if !waitUntil(ctx, 90*time.Second, func() bool { return isActive(ctx, saUnit) }) {
		dumpDiagnostics(ctx, t)
		t.Fatal("服务没起来")
	}

	// ── ① mechd 还在时，--local 也能看到本机实例（先确认它平时就能用）──
	var lastOut string
	var lastErr error
	if !waitUntil(ctx, 60*time.Second, func() bool {
		lastOut, lastErr = runLocal(ctx, saLocalSocket, "component", "status")
		return lastErr == nil && strings.Contains(lastOut, "web") && strings.Contains(lastOut, "Running")
	}) {
		dumpDiagnostics(ctx, t)
		t.Fatalf("--local component status 在 mechd 可达时也应当能看到本机实例\n最后一次: err=%v out=%q", lastErr, lastOut)
	}

	// ── ② 停掉 mechd：常规路径必须失败 ──
	stopProc(mechd)
	if out, err := runCtl(ctx, token, "component", "status", "web"); err == nil {
		t.Fatalf("mechd 已停，常规路径应当报错，实际成功了:\n%s", out)
	}

	// ── ③ --local 依然能读到本机实例——这才是这条入口存在的意义 ──
	out, err := runLocal(ctx, saLocalSocket, "component", "status")
	if err != nil {
		t.Fatalf("mechd 不可达时 --local 应当仍然可用: %v\n%s", err, out)
	}
	if !strings.Contains(out, "web") || !strings.Contains(out, "Running") {
		t.Fatalf("--local 的输出应当仍然显示 web 处于 Running，实际:\n%s", out)
	}
	if !strings.Contains(out, saNode) {
		t.Fatalf("--local 的输出应当带上本机节点名 %s，实际:\n%s", saNode, out)
	}

	// ── ④ 连 mechlet 也停掉：--local 必须给出清楚的错误，而不是裸的连接失败 ──
	stopProc(agent)
	out, err = runLocal(ctx, saLocalSocket, "component", "status")
	if err == nil {
		t.Fatalf("mechlet 也停了之后 --local 应当报错，实际成功了:\n%s", out)
	}
	if !strings.Contains(out+err.Error(), "mechlet") {
		t.Errorf("错误信息应当说清是连不上本机 mechlet，实际: %v\n%s", err, out)
	}
}

// runLocal 把 `--local`/`--local-socket` 放在名词**前面**——这是
// 10-cli §1.5、troubleshooting.md 等处文档写下的规范用法
// （`mechctl --local component status`）。此前这里放在名词后面，
// 从未真正测过文档承诺的这个顺序，也就没测到「mechctl --local
// component ...` 曾经被 cobra 错误解析」这条回归。
func runLocal(ctx context.Context, sock string, args ...string) (string, error) {
	full := append([]string{"--local", "--local-socket", sock}, args...)
	out, err := exec.CommandContext(ctx,
		filepath.Join(binDir, "mechctl"), full...).CombinedOutput()
	return string(out), err
}
