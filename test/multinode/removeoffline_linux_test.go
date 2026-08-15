//go:build linux

package multinode

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 验收表第 7、8 条：**失联节点参与的移除**。
//
//	7  一台节点失联时 remove → 默认卡在 removing；--force 跳过并登记孤儿
//	8  失联节点重新上线 → 仍然收到 removed（若记录还在），或作为孤儿被发现
//
// 这两条是「卸载做成状态而不是指令」那个决定（24-lifecycle §2.1）唯一
// 真正的判据。单元测试证明得了「记录不删」，证明不了**一台真的断了三天
// 的机器回来之后会照做**——而那正是当初选状态而非指令的全部理由。

// removeStuck 停掉 n2、发起 remove，返回 srv 参数。调用方负责把 n2 拉回来。
func removeStuck(
	ctx context.Context, t *testing.T, c *cluster, n1, n2 string, site threeNodeSite,
) []string {
	t.Helper()
	srv := []string{"--server", "https://" + n1 + ":" + mechdPort,
		"--token", site.token, "--ca-file", caPath}

	c.mustRun(ctx, t, n1, append([]string{"mechctl", "component", "deploy",
		"go-webapp", "-c", "web", "--nodes", n1 + "," + n2}, srv...)...)
	if !waitUntil(ctx, 4*time.Minute, func() bool {
		return componentConverged(ctx, t, c, n1, site.token)
	}) {
		t.Fatalf("没有收敛:\n%s", statusDump(ctx, t, c, n1, site.token))
	}

	// **用夹具自己的手法断连**：n2/n3 的 agent 是 startAgent 起的裸进程，
	// 不在 systemd 下。用 systemctl 停一个没在跑的 unit 会让「断连」根本
	// 没发生，而后面每一条断言都会因此测成另一回事。
	stopAgent(ctx, c, n2)
	if !waitUntil(ctx, 2*time.Minute, func() bool {
		return nodeStatus(ctx, t, c, n1, n2) == "offline"
	}) {
		t.Fatalf("%s 没有被判成 offline，这条测试的前提不成立", n2)
	}

	// remove：n1 能拆，n2 拆不了
	out := c.mustRunStdin(ctx, t, n1, "web\n",
		append([]string{"mechctl", "component", "remove", "web"}, srv...)...)
	if !strings.Contains(out, "being removed") {
		t.Fatalf("remove 输出不对:\n%s", out)
	}
	return srv
}

// TestRemoveStaysStuckThenTheNodeComesBack 是第 7 条前半 + 第 8 条前半。
//
// **记录不能在下发那一刻就删。** 它要一直等到那台失联的机器回来、拆干净、
// 报上来——这正是「卸载是状态不是指令」换来的东西：n2 断连期间 mechd
// 什么也不用记，n2 回来之后自然收到「这个实例不该存在」。
func TestRemoveStaysStuckThenTheNodeComesBack(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	n1, n2, n3 := c.node(1), c.node(2), c.node(3)
	site := setupThreeNodeSite(ctx, t, c, n1, n2, n3)
	srv := removeStuck(ctx, t, c, n1, n2, site)

	// ── ① 卡在 removing，记录不消失 ──
	//
	// 等一段时间：n1 那边会很快拆完并上报，若判据写错（比如「有一个报了
	// 就算完」），记录会在这段时间里消失。
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if componentGone(ctx, t, c, n1, site.token) {
			t.Fatal("有节点还没拆完，记录就消失了——那台机器上的东西会变成没人管的孤儿")
		}
		time.Sleep(3 * time.Second)
	}
	list := componentList(ctx, t, c, n1, site.token)
	if !strings.Contains(list, "Removing") {
		t.Errorf("列表里应当标出「正在移除」:\n%s", list)
	}

	// ── ② removing 期间不接受其它写操作（第 6 条的真机版）──
	out, err := c.run(ctx, n1, append([]string{"mechctl", "config", "set",
		"-c", "web", "log_level=debug"}, srv...)...)
	if err == nil {
		t.Errorf("正在被删的组件不该还能改配置:\n%s", out)
	}

	// ── ③ 那台机器回来了：它仍然收到 removed ──
	//
	// **这是整个设计的落脚点。** mechd 没有为 n2 记住任何「待办」，
	// n2 回来之后收到的仍是「这个实例不该存在」，照做即可。
	startAgent(ctx, t, c, n2, n1+":"+grpcPort)

	if !waitUntil(ctx, 5*time.Minute, func() bool {
		return componentGone(ctx, t, c, n1, site.token)
	}) {
		t.Fatalf("%s 回来之后应当拆掉并让记录消失:\n%s",
			n2, componentList(ctx, t, c, n1, site.token))
	}
	// 机器上真的拆干净了
	if unitLoadedOn(ctx, t, c, n2) {
		t.Errorf("%s 上的 unit 还在——它回来之后没有真的执行卸载", n2)
	}
}

// TestForceLeavesAnOrphanThatIsFoundWhenTheNodeReturns 是第 7 条后半 +
// 第 8 条后半。
//
// `--force` 跳过失联节点之后，那台机器上的实例**会变成孤儿**——
// 20-continuous-reconcile §2.4 定死了孤儿永不自动删，因此它**不会**在
// 重新上线时自己消失，而是要靠 `orphans` 被发现。
func TestForceLeavesAnOrphanThatIsFoundWhenTheNodeReturns(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	n1, n2, n3 := c.node(1), c.node(2), c.node(3)
	site := setupThreeNodeSite(ctx, t, c, n1, n2, n3)
	srv := removeStuck(ctx, t, c, n1, n2, site)

	// ── ① --force 跳过它 ──
	out := c.mustRunStdin(ctx, t, n1, "web\n",
		append([]string{"mechctl", "component", "remove", "web", "--force"}, srv...)...)
	if !strings.Contains(out, "record has been deleted") {
		t.Fatalf("--force 应当当场删掉记录:\n%s", out)
	}
	// 要说清跳过了谁——它们会变成孤儿，人得知道去哪儿找
	if !strings.Contains(out, n2) {
		t.Errorf("要列出被跳过的实例:\n%s", out)
	}
	if !componentGone(ctx, t, c, n1, site.token) {
		t.Fatal("--force 之后记录应当已经没了")
	}

	// ── ② 那台机器回来：它**不会**自己消失，而是作为孤儿被发现 ──
	//
	// 这一条是刻意的：「mechd 少发了一条」与「用户真的删了这个组件」在
	// 节点侧分辨不了，而卸载不可逆。因此节点只报不删。
	startAgent(ctx, t, c, n2, n1+":"+grpcPort)

	// **只看 n2 那一行，不是任意一行。**
	//
	// n1 上也有一条孤儿（它成功卸载后留下的数据残留）。只等
	// `web__default` 出现的话，会被 n1 那条满足，然后下面针对 n2 的
	// 断言测的其实是 n1——与孤儿计数那次是同一类错误。
	var list string
	if !waitUntil(ctx, 5*time.Minute, func() bool {
		list = c.mustRun(ctx, t, n1,
			append([]string{"mechctl", "orphans", "list", "--node", n2}, srv...)...)
		return strings.Contains(list, "web__default")
	}) {
		t.Fatalf("%s 回来之后它上面的实例应当作为孤儿被发现:\n%s", n2, list)
	}

	// **它是「仍装着」那一类**，不是数据残留：机器上是一整个还在跑的实例。
	// 两类混为一谈会让人以为 purge 能停掉它。
	if !strings.Contains(list, "still installed") {
		t.Errorf("被 --force 跳过的实例是「仍装着」，不是数据残留:\n%s", list)
	}
	// 而且它**没有被自动删掉**——unit 还在
	if !unitLoadedOn(ctx, t, c, n2) {
		t.Errorf("孤儿永不自动删，%s 上的 unit 应当还在", n2)
	}
}
