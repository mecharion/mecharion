//go:build linux

package multinode

import (
	"context"
	"strings"
	"testing"
	"time"
)

// M9 第 7 步的验收：`component restart` 在真机上把进程踢一下，
// 并**逐节点**把结果带回来（ADR-0038）。
//
// 单元测试验的是通道语义（离线判定、超时、结果对齐），用的是假的流。
// 这里验的是整条链路：命令真的到了节点、进程真的换了、失联的那台真的
// 被报成「不可达、未执行」。

func TestRestartKicksTheProcessAndReportsPerNode(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	n1, n2, n3 := c.node(1), c.node(2), c.node(3)
	site := setupThreeNodeSite(ctx, t, c, n1, n2, n3)
	srv := []string{"--server", "https://" + n1 + ":" + mechdPort,
		"--token", site.token, "--ca-file", caPath}

	c.mustRun(ctx, t, n1, append([]string{"mechctl", "component", "deploy",
		"go-webapp", "-c", "web", "--nodes", n1 + "," + n2}, srv...)...)
	if !waitUntil(ctx, 4*time.Minute, func() bool {
		return componentConverged(ctx, t, c, n1, site.token)
	}) {
		t.Fatalf("没有收敛:\n%s", statusDump(ctx, t, c, n1, site.token))
	}

	before := mainPIDOn(ctx, t, c, n1)
	if before == "" || before == "0" {
		t.Fatalf("重启前应当有一个跑着的进程，得到 %q", before)
	}
	// n2 的 PID 也记下来：后面要证明「联系不上的那台什么也没发生」
	beforeN2 := mainPIDOn(ctx, t, c, n2)

	// ── ① 重启一台 ──
	//
	// 指定 --node 时不弹确认（那本来就是「只动这一台」的意思）。
	out := c.mustRun(ctx, t, n1, append([]string{"mechctl", "component",
		"restart", "web", "--node", n1}, srv...)...)
	if !strings.Contains(out, "restarted") {
		t.Fatalf("restart 输出不对:\n%s", out)
	}

	// **判据是 PID 真的换了。** 一个什么都不做的实现同样会打印「已重启」。
	after := mainPIDOn(ctx, t, c, n1)
	if after == before {
		t.Errorf("主进程 PID 没变（%s）——进程根本没被重启", before)
	}
	if after == "" || after == "0" {
		t.Errorf("重启之后应当有一个新进程，得到 %q", after)
	}

	// ── ② 期望状态一个字都不该变 ──
	//
	// restart 不改期望状态，因此它既不产生新 generation，也不该让
	// 那个实例变成「未收敛」。
	if !componentConverged(ctx, t, c, n1, site.token) {
		t.Errorf("restart 之后不该变成未收敛:\n%s",
			statusDump(ctx, t, c, n1, site.token))
	}

	// ── ③ 失联的节点如实报告，不排队 ──
	//
	// 把 n2 的 mechlet 停掉，再重启整个组件：n1 该成功，n2 该被报成
	// 「不可达、未执行」——而不是挂在那里等它回来。
	// **用 stopAgent，不是 systemctl stop。**
	//
	// setupThreeNodeSite 对 n2/n3 用的是 startAgent——**裸 nohup 进程**，
	// 不在 systemd 下（只有 n1 的 mechlet 由 install 起在 systemd 里）。
	// 停那个 unit 只会停掉一个多半没在跑的东西，裸进程照样连着，于是：
	//
	//	is-active   inactive     ← 看起来「停掉了」
	//	中心        仍然 online   ← 因为流还挂着
	//	命令        真的执行了     ← 裸进程收到并执行
	//	journal     0 次          ← 裸进程写的是 /var/tmp 的日志文件
	//
	// 四个现象拼起来就是「中心凭空报告成功」，而实际每一环都没错。
	// 这一坑花了很久，因为**每一个单独的观察都指向别处**。
	stopAgent(ctx, c, n2)
	t.Cleanup(func() {
		_, _ = c.run(context.Background(), n2, "systemctl", "start", "mecharion-mechlet")
	})

	// **前提不成立就必须停下来。**
	//
	// 这条测试的全部价值在于「n2 确实联系不上」。若它其实还活着，下面
	// 那几条断言测的就是另一回事了——而日志一句「继续」再往下走，是把
	// 前提的失败吞掉：测试要么红得莫名其妙，要么绿得毫无意义。
	// 再等中心确实不再认为它连着——命令通道的判据是它自己的流注册表，
	// 而那与 node list 的 online/offline 是同一个事实的两个出口。
	if !waitUntil(ctx, 2*time.Minute, func() bool {
		o, _ := c.run(ctx, n1, "mechctl", "node", "list",
			"--server", "https://"+n1+":"+mechdPort,
			"--token", site.token, "--ca-file", caPath)
		return strings.Contains(o, "offline")
	}) {
		o, lerr := c.run(ctx, n1, "mechctl", "node", "list",
			"--server", "https://"+n1+":"+mechdPort,
			"--token", site.token, "--ca-file", caPath)
		// **把错误也打出来。** 上一版只打输出，而输出是空的——那时
		// 「中心认为它在线」与「这条命令根本没跑成」看起来一模一样，
		// 而它们要查的方向完全相反。
		mechd, _ := c.run(ctx, n1, "systemctl", "is-active", mechdUnit)
		t.Fatalf("%s 的 mechlet 停了，中心却仍不认为它掉线——这条测试的前提不成立\n"+
			"  node list err: %v\n  node list out: %q\n  mechd: %s",
			n2, lerr, o, strings.TrimSpace(mechd))
	}

	start := time.Now()
	out, err := c.runStdin(ctx, n1, "y\n", append([]string{"mechctl", "component",
		"restart", "web"}, srv...)...)
	elapsed := time.Since(start)

	// 部分失败要非零退出
	if err == nil {
		t.Errorf("有节点不可达时应当非零退出:\n%s", out)
	}
	if !strings.Contains(out, "unreachable") {
		t.Errorf("要如实报告哪台不可达:\n%s", out)
	}
	if !strings.Contains(out, "restarted") {
		t.Errorf("在线的那台仍然应当被重启:\n%s", out)
	}
	// **不排队**：不该等到超时才回来
	if elapsed > 90*time.Second {
		t.Errorf("失联节点应当立刻判定，却等了 %v", elapsed)
	}

	// **判据也要来自机器本身。** 中心说「不可达」而机器上其实重启了，
	// 与中心说「已重启」而机器上什么也没发生，是同样严重的两种谎话。
	//
	// n2 的 agent 是裸进程，日志在文件里而不在 journal 里——**这一点
	// 本身就误导过一次**：查 journal 得到 0 次，看起来像「中心撒谎」，
	// 实际是查错了地方。
	if pid := mainPIDOn(ctx, t, c, n2); pid != beforeN2 {
		t.Errorf("%s 联系不上，它的工作负载却被换了进程（%s → %s）",
			n2, beforeN2, pid)
	}
}

// mainPIDOn 取那个 unit 的主进程 PID。
//
// **判据用 PID 而不是 is-active**：一个从没被重启过的服务同样是 active，
// 而「重启了」唯一诚实的证据是进程换了一个。
func mainPIDOn(ctx context.Context, t *testing.T, c *cluster, node string) string {
	t.Helper()
	out, _ := c.sh(ctx, node, "systemctl show -p MainPID --value "+webUnit)
	return strings.TrimSpace(out)
}
