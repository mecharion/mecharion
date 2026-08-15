//go:build linux

package multinode

import (
	"context"
	"strings"
	"testing"
	"time"
)

// M9 第 1–3 步的**整条链路**验收：从 `mechctl component remove` 敲下去，
// 到三台真机上的 unit 消失、数据目录还在、中心的记录自己删掉。
//
// 前面几步各自的单元测试都绿了，但它们各自只覆盖一段：
//
//	第 1 步  节点收到 removed 之后会卸载        （单机容器验过）
//	第 2 步  中心会下发 removed、会等上报        （只有单元测试）
//	第 3 步  CLI 的确认与影响面                 （只有单元测试）
//
// **中间的接缝没有任何测试覆盖**——下发到了不等于节点收到了，节点报了
// 不等于中心认了。这条测试要的就是那几段接缝。

// TestRemoveTearsDownAcrossRealNodes 是这条链路的主验收。
func TestRemoveTearsDownAcrossRealNodes(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	n1, n2, n3 := c.node(1), c.node(2), c.node(3)
	site := setupThreeNodeSite(ctx, t, c, n1, n2, n3)
	tok := site.token

	// ── ① 三台部署起来 ──
	c.mustRun(ctx, t, n1, "mechctl", "component", "deploy", "go-webapp",
		"-c", "web", "--nodes", n1+","+n2+","+n3,
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	if !waitUntil(ctx, 4*time.Minute, func() bool {
		return componentConverged(ctx, t, c, n1, tok)
	}) {
		t.Fatalf("没有收敛:\n%s", statusDump(ctx, t, c, n1, tok))
	}
	for _, n := range c.nodes {
		if !unitLoadedOn(ctx, t, c, n) {
			t.Fatalf("%s 上的 unit 没起来，后面的断言无从证伪", n)
		}
	}

	// 每台机器的数据目录里放一个记号：只有「没被删」才留得下来
	for _, n := range c.nodes {
		if out, err := c.sh(ctx, n,
			"mkdir -p "+webDataDir+" && echo payload > "+webDataDir+"/keepme"); err != nil {
			t.Fatalf("[%s] 放记号: %v\n%s", n, err, out)
		}
	}

	// ── ② 先看影响面（--dry-run 什么都不该动）──
	dry := c.mustRun(ctx, t, n1, "mechctl", "component", "remove", "web", "--dry-run",
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	if !strings.Contains(dry, "3 instance") {
		t.Errorf("影响面要说清几个实例:\n%s", dry)
	}
	if !strings.Contains(dry, "will keep") {
		t.Errorf("影响面要列出会保留的目录——否则人无从知道盘上还剩什么:\n%s", dry)
	}
	if componentGone(ctx, t, c, n1, tok) {
		t.Fatal("--dry-run 把组件删掉了")
	}

	// ── ③ 真的移除。组件名从标准输入喂进去（二档确认）──
	out := c.mustRunStdin(ctx, t, n1, "web\n",
		"mechctl", "component", "remove", "web",
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	if !strings.Contains(out, "being removed") {
		t.Fatalf("remove 输出不对:\n%s", out)
	}

	// ── ④ 三台机器上真的拆干净了 ──
	//
	// 判据是 **systemd 的 LoadState**，不是 `is-active`：一个 stop 掉但
	// unit 文件还在的服务同样是 inactive，而它会在下次 daemon-reload
	// 或重启时重新出现。
	if !waitUntil(ctx, 5*time.Minute, func() bool {
		for _, n := range c.nodes {
			if unitLoadedOn(ctx, t, c, n) {
				return false
			}
		}
		return true
	}) {
		var b strings.Builder
		for _, n := range c.nodes {
			o, _ := c.sh(ctx, n, "systemctl show -p LoadState --value "+webUnit)
			b.WriteString(n + ": " + strings.TrimSpace(o) + "\n")
		}
		t.Fatalf("有机器上的 unit 没被卸掉:\n%s", b.String())
	}

	// ── ⑤ 中心的记录**自己**消失了 ──
	//
	// 这是第 2 步那条「全部实例报告拆干净才删记录」在真机上的样子。
	// 它不能靠 CLI 再敲一次，必须是上报驱动的。
	if !waitUntil(ctx, 3*time.Minute, func() bool {
		return componentGone(ctx, t, c, n1, tok)
	}) {
		t.Fatalf("三台都拆完了，中心的记录却没消失:\n%s",
			componentList(ctx, t, c, n1, tok))
	}

	// ── ⑥ 数据目录还在，内容没被动过 ──
	//
	// **这一半和上一半同样要紧。** 只验「拆掉了」的话，一个 rm -rf 全删光
	// 的实现也能通过，而那正是我们最不想要的那种。
	for _, n := range c.nodes {
		got, err := c.sh(ctx, n, "cat "+webDataDir+"/keepme 2>&1")
		if err != nil || strings.TrimSpace(got) != "payload" {
			t.Errorf("[%s] 数据目录默认保留且不该被改动，读到 %q (%v)", n, got, err)
		}
	}
}

// TestRemoveRefusesWithoutTheName 是二档确认在真机上的样子。
//
// 单元测试证明了 confirmName 的判读，证明不了这条命令**真的**把它接上了
// ——一个忘了调用确认的 RunE 会让那些单元测试全绿。
func TestRemoveRefusesWithoutTheName(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	n1, n2, n3 := c.node(1), c.node(2), c.node(3)
	site := setupThreeNodeSite(ctx, t, c, n1, n2, n3)
	tok := site.token

	c.mustRun(ctx, t, n1, "mechctl", "component", "deploy", "go-webapp",
		"-c", "web", "--nodes", n1,
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)

	// 什么都不输入 —— 必须被拒
	out, err := c.runStdin(ctx, n1, "", "mechctl", "component", "remove", "web",
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	if err == nil {
		t.Fatalf("没有输入组件名时必须拒绝:\n%s", out)
	}
	if componentGone(ctx, t, c, n1, tok) {
		t.Fatal("被拒绝了却还是把组件删了")
	}

	// 输错名字 —— 同样被拒
	out, err = c.runStdin(ctx, n1, "wev\n", "mechctl", "component", "remove", "web",
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	if err == nil {
		t.Fatalf("输错名字时必须拒绝:\n%s", out)
	}

	// **-y 也跳不过这一档**（10-cli §7）
	out, err = c.runStdin(ctx, n1, "", "mechctl", "component", "remove", "web", "-y",
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	if err == nil {
		t.Fatalf("-y 不该能跳过输名字这一档:\n%s", out)
	}
	if componentGone(ctx, t, c, n1, tok) {
		t.Fatal("-y 把组件删掉了——那正是这一档要防的")
	}
}

// ── 辅助 ────────────────────────────────────────────────────────────────

const (
	webUnit    = "mecharion-web-default.service"
	webDataDir = "/var/lib/mecharion/apps/web"
)

// unitLoadedOn 报告 systemd 还认不认识那个 unit。
//
// **不是 is-active。** 一个 stop 掉但 unit 文件还在的服务同样是 inactive，
// 而卸载要交付的是「systemd 忘了它」。
func unitLoadedOn(ctx context.Context, t *testing.T, c *cluster, node string) bool {
	t.Helper()
	out, _ := c.sh(ctx, node, "systemctl show -p LoadState --value "+webUnit)
	return strings.TrimSpace(out) != "not-found"
}

func componentList(ctx context.Context, t *testing.T, c *cluster, mechdNode, tok string) string {
	t.Helper()
	out, _ := c.run(ctx, mechdNode, "mechctl", "component", "list",
		"--server", "https://"+mechdNode+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	return out
}

// componentGone 报告 web 这个组件在中心是不是已经没有了。
func componentGone(ctx context.Context, t *testing.T, c *cluster, mechdNode, tok string) bool {
	t.Helper()
	return !strings.Contains(componentList(ctx, t, c, mechdNode, tok), "web")
}
