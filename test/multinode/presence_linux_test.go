//go:build linux

package multinode

import (
	"context"
	"os/exec"

	"github.com/mecharion/mecharion/internal/protocol"
	"strings"
	"testing"
	"time"
)

// 本文件钉住 `node list` 里那一列**说的是现在**。
//
// 这是 M8 第 4 步发现的一个问题：Web UI 把节点状态做成一列带颜色的标签之后，
// 三台机器全显示 online，而其中两台的 mechlet 早就不在了。原因是 status
// 列只有写 online 的路径、没有写回去的路径（22-multi-node §6.13）。
//
// 已有的几条验收全都只断言「加入之后是 online」——那一半在缺陷版本里也
// 成立。**判据必须落在同一台机器状态改变的那一刻**，否则测的只是初值。

// TestStatusFollowsTheConnection 是第 4 步补的验收。
func TestStatusFollowsTheConnection(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	n1, n2 := c.node(1), c.node(2)
	installOnce(ctx, t, c, n1)
	resetNode(ctx, t, c, n2)

	// ── ① 登记但还没人去那台机器上加入：pending ──
	//
	// 这个值要与 offline 分开。运维看到 pending 会去那台机器上敲 join，
	// 看到 offline 会去查它为什么掉了——指错方向的代价是一次白跑的现场。
	seedNode(ctx, t, c, n1, n2)
	if got := nodeStatus(ctx, t, c, n1, n2); got != "pending" {
		t.Fatalf("登记之后、加入之前 %s 应为 pending，实际 %q", n2, got)
	}

	// ── ② 连上来：online ──
	//
	// 这里要先把刚登记的那一行抹掉再 join：**join 拒绝任何已在册的名字**，
	// 包括一台还是 pending、既没证书也没跑任何东西的机器。设计文档
	// （22-multi-node §6.3）说的是「登记与加入是两件事」，而代码里
	// 这两件事互斥——这是一个独立于本条测试的缺陷，见 §6.14。
	// 这条测试不去绕过它，也不掩盖它：照实按能走通的路走，把问题留在
	// 它自己的位置上。
	forgetNode(ctx, t, c, n1, n2)
	joinNode(ctx, t, c, n1, n2)
	logPath := startAgent(ctx, t, c, n2, n1+":"+grpcPort)
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return nodeStatus(ctx, t, c, n1, n2) == "online"
	}) {
		t.Fatalf("[%s] 连上之后应为 online，实际 %q\n日志:\n%s",
			n2, nodeStatus(ctx, t, c, n1, n2),
			tailOf(readLog(ctx, c, n2, logPath), 30))
	}

	// ── ③ agent 没了：offline ──
	//
	// **这一步才是缺陷所在**。库里那一行一个字节都不会变，变的只有
	// 那条 Subscribe 流还在不在。
	stopAgent(ctx, c, n2)
	if !waitUntil(ctx, 90*time.Second, func() bool {
		return nodeStatus(ctx, t, c, n1, n2) == "offline"
	}) {
		t.Fatalf("[%s] agent 停掉之后应为 offline，实际 %q\n"+
			"（status 列若仍在声称在线，它就是个只进不出的门）",
			n2, nodeStatus(ctx, t, c, n1, n2))
	}

	// ── ④ 回来：重新 online ──
	//
	// 不测这一步的话，一个「一旦掉线就永久标记为 offline」的实现同样
	// 能过前三步——那是把同一个缺陷换了个方向。
	startAgent(ctx, t, c, n2, n1+":"+grpcPort)
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return nodeStatus(ctx, t, c, n1, n2) == "online"
	}) {
		t.Fatalf("[%s] agent 回来之后应当重新变回 online，实际 %q",
			n2, nodeStatus(ctx, t, c, n1, n2))
	}

	// ── ⑤ mechd 重启之后不许凭库里的旧值报在线 ──
	//
	// 这是「在线」这件事被存起来时最隐蔽的失败形态：进程重启、
	// 一个连接都还没建立，而库里躺着三行 online。
	stopAgent(ctx, c, n2)
	restartMechd(ctx, t, c, n1)
	// `is-active` 只说进程起来了，说不了 HTTP 已经能答话
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return statusOrEmpty(ctx, t, c, n1, n1) != ""
	}) {
		t.Fatal("mechd 重启之后没能重新答话")
	}
	if got := nodeStatus(ctx, t, c, n1, n2); got == "online" {
		t.Errorf("mechd 刚重启、%s 的 agent 还停着，却报 online——"+
			"在线状态被存进库里了", n2)
	}
}

// TestFrozenNodeIsDetected 验的是 keepalive。
//
// 前一条测试里 agent 是被 kill 掉的——内核替它关了连接，服务端立刻知道。
// 真实的边缘现场不长这样：断电、机器卡死、网线被拔，服务端那一侧只是
// **再也收不到东西**，而**没有任何人替它关那个连接**。
//
// 这里用 `docker pause` 模拟：容器里的进程被冻住，TCP 栈仍在内核里正常
// 回 ACK，但没有人再处理一个 HTTP/2 帧。这恰恰是 TCP keepalive 也救不了
// 的一类——只有应用层的探测能发现。
//
// **第一版用的是 `docker network disconnect`，那是一条假测试**：拆掉
// veth 会让服务端那一侧的 socket 直接出错，34 秒就变了 offline，而把
// `ServerKeepalive` 里的探测整个删掉之后它照样通过。判据不能是「最后
// 变成 offline 了」，得是「**因为探测超时**才变的」——所以这条测试的
// 下限与上限都要卡：早于 protocol.KeepaliveTimeout 变的，一定是别的原因。
func TestFrozenNodeIsDetected(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	n1, n2 := c.node(1), c.node(2)
	installOnce(ctx, t, c, n1)
	resetNode(ctx, t, c, n2)
	joinNode(ctx, t, c, n1, n2)
	startAgent(ctx, t, c, n2, n1+":"+grpcPort)
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return nodeStatus(ctx, t, c, n1, n2) == "online"
	}) {
		t.Fatalf("[%s] 冻住之前应当是 online", n2)
	}

	// 冻住。t.Cleanup 而不是 defer：这台机器解冻之前，
	// 后面每一条测试都会莫名其妙地超时。
	if out, err := dockerRun(ctx, "pause", n2); err != nil {
		t.Fatalf("冻结 %s: %v\n%s", n2, err, out)
	}
	t.Cleanup(func() { _, _ = dockerRun(context.Background(), "unpause", n2) })

	froze := time.Now()
	// 上限 KeepaliveTime(60s) + protocol.KeepaliveTimeout(20s)，留一倍余量
	if !waitUntil(ctx, 170*time.Second, func() bool {
		return nodeStatus(ctx, t, c, n1, n2) == "offline"
	}) {
		t.Fatalf("[%s] 冻住之后应当在 80s 内变 offline，实际 %q\n"+
			"（没有应用层探测的话，这条连接会挂在服务端手上两个小时）",
			n2, nodeStatus(ctx, t, c, n1, n2))
	}

	// **下限**：探测周期一个都没走完就变了，说明发现它的是别的东西，
	// 而这条测试要验的正是 keepalive 本身。
	if el := time.Since(froze); el < protocol.KeepaliveTimeout {
		t.Errorf("冻住之后 %v 就变成 offline 了，比一次探测超时（%v）还快——"+
			"发现它的不是 keepalive，这条测试没验到想验的东西", el, protocol.KeepaliveTimeout)
	}
}

// dockerRun 在宿主侧执行 docker 命令。
func dockerRun(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return string(out), err
}

// statusOrEmpty 是 nodeStatus 的不致命版本，供等待循环用。
func statusOrEmpty(ctx context.Context, t *testing.T, c *cluster, mechdNode, node string) string {
	t.Helper()
	token := strings.TrimSpace(mustRead(ctx, t, c, mechdNode, tokenPath))
	out, err := c.run(ctx, mechdNode, "mechctl", "node", "list",
		"--server", "https://"+mechdNode+":"+mechdPort,
		"--token", token, "--ca-file", caPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), node) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
