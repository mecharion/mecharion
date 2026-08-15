//go:build linux

package multinode

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 本文件是 **M7 第 6 步的验收**：cordon 的节点不被调和。
//
// cordon 与 revoke / remove 是**三件不同的事**，而它们很容易被做成一件：
//
//	cordon  暂停调和；连接、上报、进程**全都不动**
//	revoke  切断证书；节点仍在册
//	remove  从册子上抹掉
//
// 这条验收要钉住的正是 cordon 那一列的每个「不动」——一个「顺手把连接也
// 断掉」的实现会让 `node list` 看不到那台机器，而运维恰恰是在**调试它**。

// TestCordonStopsReconcileOnly 是第 6 步的验收。
func TestCordonStopsReconcileOnly(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	n1, n2 := c.node(1), c.node(2)
	installOnce(ctx, t, c, n1)
	resetNode(ctx, t, c, n2)
	joinNode(ctx, t, c, n1, n2)

	logPath := startAgent(ctx, t, c, n2, n1+":"+grpcPort)
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return strings.Contains(readLog(ctx, c, n2, logPath), "registered with mechd")
	}) {
		t.Fatalf("[%s] cordon 之前应当连得上", n2)
	}

	// ── ① cordon ──
	out := c.mustRun(ctx, t, n1, "mechctl", "node", "cordon", n2,
		"--server", "https://"+n1+":"+mechdPort,
		"--token", adminToken(ctx, t, c, n1), "--ca-file", caPath)
	for _, want := range []string{"still connected", "uncordon"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出应当说清 cordon 的语义与恢复方式，缺 %q:\n%s", want, out)
		}
	}

	// ── ② 那个**还开着的** agent 收到并停下来 ──
	//
	// cordon 是**状态**，随下一次全量下发送达。mechd 改完立刻唤醒该节点，
	// 因此不必等下一次期望状态变化。
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return strings.Contains(readLog(ctx, c, n2, logPath), "is paused (cordoned)")
	}) {
		t.Fatalf("[%s] cordon 应当送达并停止调和，日志:\n%s",
			n2, tailOf(readLog(ctx, c, n2, logPath), 30))
	}

	// ── ③ 但连接与上报**没有断** ──
	//
	// 这是 cordon 与 revoke 的分界。少了这条，一个「顺手把连接也断掉」的
	// 实现照样通过上面那条——而那会让运维在调试期间失去这台机器的视图。
	if strings.Contains(readLog(ctx, c, n2, logPath), "control plane refused this node") {
		t.Errorf("[%s] cordon 不该切断连接——那是 revoke 干的事", n2)
	}
	if got := nodeStatus(ctx, t, c, n1, n2); got != "online" {
		t.Errorf("cordon 之后 %s 仍应在线，实际 %q", n2, got)
	}
	if !strings.Contains(nodeLine(ctx, t, c, n1, n2), "cordoned") {
		t.Errorf("node list 应当显示暂停状态:\n%s", nodeLine(ctx, t, c, n1, n2))
	}

	// ── ④ uncordon 之后恢复调和，且**说得出来** ──
	//
	// 两个方向都要有声音：运维 cordon 一台机器去调试，事后最想确认的
	// 就是「它恢复了没有」。
	c.mustRun(ctx, t, n1, "mechctl", "node", "uncordon", n2,
		"--server", "https://"+n1+":"+mechdPort,
		"--token", adminToken(ctx, t, c, n1), "--ca-file", caPath)

	if !waitUntil(ctx, 60*time.Second, func() bool {
		return strings.Contains(readLog(ctx, c, n2, logPath), "reconcile pause was lifted")
	}) {
		t.Fatalf("[%s] uncordon 应当送达并说出来，日志:\n%s",
			n2, tailOf(readLog(ctx, c, n2, logPath), 30))
	}
	if strings.Contains(nodeLine(ctx, t, c, n1, n2), "cordoned") {
		t.Errorf("uncordon 之后不该还显示暂停")
	}
}

// TestCordonSurvivesReconnect 钉住 cordon 是**状态不是动词**。
//
// agent 重启之后不该「忘了自己被暂停」——那是指令式接口的典型症状。
// 状态随全量重推，因此重连之后自然还是对的（ADR-0029）。
func TestCordonSurvivesReconnect(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	n1, n3 := c.node(1), c.node(3)
	installOnce(ctx, t, c, n1)
	resetNode(ctx, t, c, n3)
	joinNode(ctx, t, c, n1, n3)

	c.mustRun(ctx, t, n1, "mechctl", "node", "cordon", n3,
		"--server", "https://"+n1+":"+mechdPort,
		"--token", adminToken(ctx, t, c, n1), "--ca-file", caPath)
	t.Cleanup(func() {
		_, _ = c.run(context.Background(), n1, "mechctl", "node", "uncordon", n3,
			"--server", "https://"+n1+":"+mechdPort,
			"--token", adminToken(context.Background(), t, c, n1), "--ca-file", caPath)
	})

	// **agent 是在 cordon 之后才起来的**：它从没收到过「去暂停」这个动作，
	// 只是拿到了一份说「你现在是暂停的」的全量。
	logPath := startAgent(ctx, t, c, n3, n1+":"+grpcPort)
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return strings.Contains(readLog(ctx, c, n3, logPath), "is paused (cordoned)")
	}) {
		t.Fatalf("[%s] 重连之后仍应处于暂停状态，日志:\n%s",
			n3, tailOf(readLog(ctx, c, n3, logPath), 30))
	}
}

// nodeLine 取 `node list` 里某个节点那一行。
func nodeLine(ctx context.Context, t *testing.T, c *cluster, mechdNode, node string) string {
	t.Helper()
	out, err := c.run(ctx, mechdNode, "mechctl", "node", "list",
		"--server", "https://"+mechdNode+":"+mechdPort,
		"--token", adminToken(ctx, t, c, mechdNode), "--ca-file", caPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), node) {
			return line
		}
	}
	return ""
}
