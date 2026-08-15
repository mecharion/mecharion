//go:build linux

package multinode

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// 本文件是 **M7 第 7 步的验收**：`rollout status` 说得出「第 2/3 批」。
//
// 但那句话只是**表面**。真正要钉住的是它下面那件事：批次得管住下发。
// 一个只把批次落盘、下发照旧全量的实现能让 status 说出同样漂亮的
// 「第 1/3 批」——而三批的机器早已全部升完。那比没有分批更糟，因为它
// 给了一个假的安全感：运维看着「第 1/3 批」以为还来得及叫停。
//
// 因此这里的判据是**机器上真正跑着的版本**，不是 status 的措辞。

// TestRolloutBatchesGateRealMachines 是第 7 步的验收。
func TestRolloutBatchesGateRealMachines(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	n1, n2, n3 := c.node(1), c.node(2), c.node(3)
	site := setupThreeNodeSite(ctx, t, c, n1, n2, n3)
	tok := site.token

	// ── ① 三台一起部署 ──
	out := c.mustRun(ctx, t, n1, "mechctl", "component", "deploy", "go-webapp",
		"-c", "web", "--nodes", n1+","+n2+","+n3,
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	if !strings.Contains(out, "Deployed web") {
		t.Fatalf("deploy 输出不对:\n%s", out)
	}
	if !waitUntil(ctx, 4*time.Minute, func() bool {
		return componentConverged(ctx, t, c, n1, tok)
	}) {
		t.Fatalf("三台没有收敛到 1.2.0:\n%s", statusDump(ctx, t, c, n1, tok))
	}

	// ── ② 上一个新版本，发起升级 ──
	stagePackOnMechd(ctx, t, c, n1, "1.4.0")
	c.mustRun(ctx, t, n1, "mechctl", "component", "upgrade", "web",
		"--version", "1.4.0",
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)

	// ── ③ 批次说得出来 ──
	//
	// 三台、canary=1、maxUnavailable=1 ⇒ 3 批。
	ro := rolloutStatus(ctx, t, c, n1, tok)
	if ro.Batches != 3 {
		t.Fatalf("三台应当分成 3 批，实际 %d：%+v", ro.Batches, ro)
	}

	// ── ④ 并且**真的**一批一批走 ──
	//
	// 这是全文最重要的一条断言：进行中的任何一刻，跑着新版的机器数不能
	// 超过已放行的批次所覆盖的台数。一个不设门禁的实现会在这里被抓住。
	//
	// 采样要密：整条滚动在这套夹具上只花十几秒，3 秒一次的采样很可能
	// 一次中间态都撞不上，于是这条断言变成一句空话。
	sawPartial := false
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		ro = rolloutStatus(ctx, t, c, n1, tok)
		atNew := countAtVersion(ctx, t, c, n1, tok, "1.4.0")

		// 只在**进行中**比：结束之后 Batch 会停在总批数上，那时这条
		// 不等式已经不表达任何东西。
		if ro.State == "running" && atNew > ro.Batch {
			t.Fatalf("第 %d/%d 批时已有 %d 台在新版——分批没有管住下发\n%s",
				ro.Batch, ro.Batches, atNew, statusDump(ctx, t, c, n1, tok))
		}
		if atNew > 0 && atNew < 3 {
			sawPartial = true // 抓到了混版的那一刻
		}
		if ro.State == "succeeded" {
			break
		}
		if ro.State == "failed" || ro.State == "aborted" {
			t.Fatalf("升级没走完就 %s：%s\n%s", ro.State, ro.Reason,
				statusDump(ctx, t, c, n1, tok))
		}
		time.Sleep(500 * time.Millisecond)
	}

	if ro.State != "succeeded" {
		t.Fatalf("升级没有在期限内完成，停在 %s（第 %d/%d 批）\n%s",
			ro.State, ro.Batch, ro.Batches, statusDump(ctx, t, c, n1, tok))
	}
	if ro.Batch != ro.Batches || ro.Batches != 3 {
		t.Errorf("结束之后应当是「第 3/3 批」，实际第 %d/%d 批", ro.Batch, ro.Batches)
	}
	if !sawPartial {
		// 没抓到混版说明要么采样太慢，要么根本没分批。两者都该看一眼——
		// **不当成通过**：这条测试的全部价值就在那个中间态。
		t.Errorf("始终没观察到「一部分新版、一部分旧版」的中间态，" +
			"分批可能没有生效（或采样间隔太长）")
	}
	if n := countAtVersion(ctx, t, c, n1, tok, "1.4.0"); n != 3 {
		t.Errorf("升级完成后三台都该在 1.4.0，实际 %d 台", n)
	}
}

// TestRolloutSkipsCordonedNodeAndSaysSo 钉住 cordon 与滚动升级的交界。
//
// 被 cordon 的机器不参与本次变更，而且 **status 要列出来**——不列的话，
// 「为什么这台还是旧版」会变成一次排查，而答案早就有人明确说过了。
func TestRolloutSkipsCordonedNodeAndSaysSo(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	n1, n2, n3 := c.node(1), c.node(2), c.node(3)
	site := setupThreeNodeSite(ctx, t, c, n1, n2, n3)
	tok := site.token

	c.mustRun(ctx, t, n1, "mechctl", "component", "deploy", "go-webapp",
		"-c", "web", "--nodes", n1+","+n2+","+n3,
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	if !waitUntil(ctx, 4*time.Minute, func() bool {
		return componentConverged(ctx, t, c, n1, tok)
	}) {
		t.Fatalf("三台没有收敛:\n%s", statusDump(ctx, t, c, n1, tok))
	}

	// cordon 掉 n3，再升级
	c.mustRun(ctx, t, n1, "mechctl", "node", "cordon", n3,
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	t.Cleanup(func() {
		_, _ = c.run(context.Background(), n1, "mechctl", "node", "uncordon", n3,
			"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	})

	stagePackOnMechd(ctx, t, c, n1, "1.5.0")
	c.mustRun(ctx, t, n1, "mechctl", "component", "upgrade", "web",
		"--version", "1.5.0",
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)

	ro := rolloutStatus(ctx, t, c, n1, tok)
	if ro.Batches != 2 {
		t.Errorf("cordon 掉一台之后应当剩 2 批，实际 %d", ro.Batches)
	}
	if len(ro.Skipped) != 1 || ro.Skipped[0] != n3 {
		t.Fatalf("被跳过的应当是 %s，实际 %v", n3, ro.Skipped)
	}

	// 人读的那一行也要说得出来
	out := c.mustRun(ctx, t, n1, "mechctl", "rollout", "status", "web",
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	for _, want := range []string{"Batch", n3, "uncordon"} {
		if !strings.Contains(out, want) {
			t.Errorf("rollout status 应当说清批次与被跳过的机器，缺 %q:\n%s", want, out)
		}
	}

	// 等两批走完；n3 **仍在旧版**
	if !waitUntil(ctx, 6*time.Minute, func() bool {
		return rolloutStatus(ctx, t, c, n1, tok).State == "succeeded"
	}) {
		t.Fatalf("升级没走完:\n%s", statusDump(ctx, t, c, n1, tok))
	}
	if v := versionOn(ctx, t, c, n1, tok, n3); v == "1.5.0" {
		t.Errorf("被 cordon 的 %s 不该被升级，实际在 %s", n3, v)
	}
	if n := countAtVersion(ctx, t, c, n1, tok, "1.5.0"); n != 2 {
		t.Errorf("另外两台该升上去，实际 %d 台在 1.5.0", n)
	}
}

// TestRolloutAbortMidwayBringsMachinesBack 钉住中途 abort **真的把机器退回来**。
//
// 单机验收里证不了这一条：一批、且 pause 发生在放行之前，机器根本没动过。
// 三节点才有那个中间态——两台已经在新版、一台还没轮到，此时 abort 必须
// 把前两台退回去。一条只把记录标成「已中止」的 abort，会让运维以为世界
// 回到了升级前，而两台机器上跑的还是新版。
func TestRolloutAbortMidwayBringsMachinesBack(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	n1, n2, n3 := c.node(1), c.node(2), c.node(3)
	site := setupThreeNodeSite(ctx, t, c, n1, n2, n3)
	tok := site.token

	c.mustRun(ctx, t, n1, "mechctl", "component", "deploy", "go-webapp",
		"-c", "web", "--nodes", n1+","+n2+","+n3,
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	if !waitUntil(ctx, 4*time.Minute, func() bool {
		return componentConverged(ctx, t, c, n1, tok)
	}) {
		t.Fatalf("三台没有收敛:\n%s", statusDump(ctx, t, c, n1, tok))
	}

	stagePackOnMechd(ctx, t, c, n1, "1.6.0")
	c.mustRun(ctx, t, n1, "mechctl", "component", "upgrade", "web",
		"--version", "1.6.0",
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)

	// 等到**至少一台**已经换上新版——那才是有东西可退的时刻。
	//
	// 立刻 abort 的话机器还没动，这条测试就退化成单机那条。
	if !waitUntil(ctx, 4*time.Minute, func() bool {
		return countAtVersion(ctx, t, c, n1, tok, "1.6.0") >= 1
	}) {
		t.Fatalf("第一批应当先升上去:\n%s", statusDump(ctx, t, c, n1, tok))
	}
	// 冻住队列，这样后面几批不会在 abort 之前把三台都升完
	c.mustRun(ctx, t, n1, "mechctl", "rollout", "pause", "web",
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	upgraded := countAtVersion(ctx, t, c, n1, tok, "1.6.0")
	if upgraded == 3 {
		t.Skip("三台在能 pause 之前就全升完了——这套夹具太快，换不出中间态")
	}

	c.mustRun(ctx, t, n1, "mechctl", "rollout", "abort", "web", "-y",
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)

	if !waitUntil(ctx, 6*time.Minute, func() bool {
		return countAtVersion(ctx, t, c, n1, tok, "1.6.0") == 0 &&
			componentConverged(ctx, t, c, n1, tok)
	}) {
		t.Fatalf("abort 应当把已经升上去的 %d 台退回 1.2.0\n%s",
			upgraded, statusDump(ctx, t, c, n1, tok))
	}
	if n := countAtVersion(ctx, t, c, n1, tok, "1.2.0"); n != 3 {
		t.Errorf("退回之后三台都该在 1.2.0，实际 %d 台", n)
	}
}

// TestCrashLoopNeverApprovesTheNextBatch 是 **M7 第 8 步的验收**。
//
// 一台起得来、健康检查也过、但每隔几秒就崩一次的机器，**不能批准下一批**。
// 少了这道门禁，一次坏升级会被逐批放大到全集群：每一批都「收敛了」，
// 每一批都在几秒后崩掉，而 Rollout 一路绿灯走到底。
//
// 这里造的崩溃是真的：新版本的 unit 起来就退出，systemd 按 restart=always
// 反复拉起。判据取自机器上真正跑着的版本——第一批那台之外，一台都不许动。
func TestCrashLoopNeverApprovesTheNextBatch(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	n1, n2, n3 := c.node(1), c.node(2), c.node(3)
	site := setupThreeNodeSite(ctx, t, c, n1, n2, n3)
	tok := site.token

	c.mustRun(ctx, t, n1, "mechctl", "component", "deploy", "go-webapp",
		"-c", "web", "--nodes", n1+","+n2+","+n3,
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	if !waitUntil(ctx, 4*time.Minute, func() bool {
		return componentConverged(ctx, t, c, n1, tok)
	}) {
		t.Fatalf("三台没有收敛:\n%s", statusDump(ctx, t, c, n1, tok))
	}

	// 造一个「起来就崩」的版本，然后升上去
	stageCrashingPack(ctx, t, c, n1, "1.7.0")
	c.mustRun(ctx, t, n1, "mechctl", "component", "upgrade", "web",
		"--version", "1.7.0",
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	t.Cleanup(func() {
		// 不收拾的话，后面每条测试都会撞上一个卡住的 Rollout
		bg := context.Background()
		_, _ = c.run(bg, n1, "mechctl", "rollout", "abort", "web", "-y",
			"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	})

	// **盯足够久**：稳定窗口 30 秒，一个不设门禁的实现会在 30~60 秒里
	// 把三批全放完。这里盯 3 分钟，足够走完好几轮。
	//
	// 一路记下**谁碰过坏版本**。只看终态是不够的：节点对起不来的
	// generation 会自动回滚（M6），坏版本可能只在机器上待几秒——那时
	// 「三批全升了又全退回来」与「一批都没放」长得一模一样。
	touched := map[string]bool{}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		ro := rolloutStatus(ctx, t, c, n1, tok)
		for _, node := range c.nodes {
			if versionOn(ctx, t, c, n1, tok, node) == "1.7.0" {
				touched[node] = true
			}
		}
		if len(touched) > 1 {
			t.Fatalf("崩溃循环批准了下一批：%v 都碰过坏版本（第 %d/%d 批）\n%s",
				keysOf(touched), ro.Batch, ro.Batches, statusDump(ctx, t, c, n1, tok))
		}
		if ro.Batch > 1 {
			t.Fatalf("崩溃循环让批次推进到了第 %d 批\n%s",
				ro.Batch, statusDump(ctx, t, c, n1, tok))
		}
		if ro.State == "succeeded" {
			t.Fatalf("一次装不起来的升级被判成功了\n%s", statusDump(ctx, t, c, n1, tok))
		}
		time.Sleep(3 * time.Second)
	}

	// 没被放行的那两台**一点没动**：从头到尾没碰过坏版本，还在旧版上活着
	for _, node := range c.nodes {
		if touched[node] {
			continue
		}
		if v := versionOn(ctx, t, c, n1, tok, node); v != "1.2.0" {
			t.Errorf("%s 没被放行过，应当原封不动停在 1.2.0，实际 %q", node, orNone(v))
		}
	}
	if ro := rolloutStatus(ctx, t, c, n1, tok); ro.State == "succeeded" {
		t.Errorf("一次装不起来的升级不该是成功，实际 %s", ro.State)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// stageCrashingPack 造一个「起来就崩」的版本。
//
// 崩法是真的：把 unit 的 exec 换成一条立刻退出的命令，systemd 按
// restart=always 反复拉起。**不是**把二进制删掉——那样 systemd 报的是
// 「找不到可执行文件」，与「起得来但活不久」不是同一类现象，而后者才是
// 稳定窗口存在的理由。
func stageCrashingPack(ctx context.Context, t *testing.T, c *cluster, node, version string) {
	t.Helper()
	stagePackOnMechd(ctx, t, c, node, version)
	dst := m7DataDir + "/mechd/packs/go-webapp-" + version

	// **YAML 里用单引号包 shell**，不要往双引号里塞转义的双引号：
	// 那串东西要穿过 Go 源码、docker exec、sh -c、sed 四层，每一层都会
	// 吃掉一次反斜杠——第一版就是这么坏掉的，而它坏的样子是
	// 「Pack 版本找不到」（packindex 静默跳过解析不了的 Pack）。
	script := `set -e
sed -i "s|^        exec: .*|        exec: \"/bin/sh -c 'sleep 5; exit 1'\"|" ` +
		dst + `/pack.yaml
grep -q "sleep 5" ` + dst + `/pack.yaml
`
	if out, err := c.sh(ctx, node, script); err != nil {
		t.Fatalf("[%s] 造崩溃版 Pack: %v\n%s", node, err, out)
	}
	// 改坏了的 Pack 会被 packindex **静默跳过**，症状是后面那句
	// 「没有满足 =x.y.z 的版本」——在这里就地确认，省掉一次误诊。
	body := mustRead(ctx, t, c, node, dst+"/pack.yaml")
	if !strings.Contains(body, `exec: "/bin/sh -c 'sleep 5; exit 1'"`) {
		t.Fatalf("[%s] exec 没被换成崩溃命令:\n%s", node, tailOf(body, 20))
	}
}

// TestResumePicksUpWhereItStopped 是 **M7 第 9 步的验收**。
//
// 一批没过门禁 → 整体停下 → 人修好 → `rollout resume` **从断点续做**，
// 已完成的批次一个都不重做。
//
// **故障挑的是「节点离线不上报」**（§2.7 的第一行），不是「新版起不来」。
// 后者走的是另一条路：节点侧发现软链切过去之后起不来，会**自动回滚**，
// mechd 观测到回滚就判 failed（终态）——那个 digest 在节点侧已被 blocked，
// resume 推一遍也不会动，因此它本来就不该可恢复。第一版验收挑错了故障，
// 白等了 13 分钟才看出来。
//
// 「不重做」靠 **generation 序号**抓：重做过的机器会多出一代。一个
// 「resume 就重新分批、从头再来」的实现能通过「最后三台都在新版」这种
// 终态断言，而它把已经稳住的机器又动了一遍。
func TestResumePicksUpWhereItStopped(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	n1, n2, n3 := c.node(1), c.node(2), c.node(3)
	site := setupThreeNodeSite(ctx, t, c, n1, n2, n3)
	tok := site.token

	c.mustRun(ctx, t, n1, "mechctl", "component", "deploy", "go-webapp",
		"-c", "web", "--nodes", n1+","+n2+","+n3,
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	if !waitUntil(ctx, 4*time.Minute, func() bool {
		return componentConverged(ctx, t, c, n1, tok)
	}) {
		t.Fatalf("三台没有收敛:\n%s", statusDump(ctx, t, c, n1, tok))
	}

	// ── ① 先让第 2 批那台离线，**再**发起升级 ──
	//
	// 顺序要紧。第一版是等批次放行之后才杀 agent，结果它可能**正在物化**
	// ——重启之后节点发现那个 digest 上次失败过（M6 的 Blocked），于是
	// 回落并上报回滚，mechd 判 failed。resume 救不了那种情况：解锁要靠
	// 一个新的 digest（M6 §6 记在案的那条限制）。
	//
	// 那一版单独跑是绿的、进套件就红——因为杀 agent 的时机全凭运气。
	// **一条会闪的测试比没有更糟**，何况它闪的方式恰好掩盖了这条边界。
	//
	// 升级之前就让它离线，它就永远没机会半途而废：批次放行时它压根不在，
	// 判据是干干净净的「还没上报过」，正是 §2.7 第一行写的那种。
	stalled := nodeWithOrdinal(ctx, t, c, n1, tok, 1)
	if stalled == "" || stalled == n1 {
		t.Skipf("第 2 批落在 %q 上，这条验收要它落在一台跑独立 agent 的机器上", stalled)
	}
	stopAgent(ctx, c, stalled)

	stagePackOnMechd(ctx, t, c, n1, "1.8.0")
	c.mustRun(ctx, t, n1, "mechctl", "component", "upgrade", "web",
		"--version", "1.8.0",
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = c.run(bg, n1, "mechctl", "rollout", "abort", "web", "-y",
			"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	})

	// ── ② 等第 1 批做完 ──
	//
	// 要有一批**已完成**的，「不重做」才有东西可验。
	if !waitUntil(ctx, 6*time.Minute, func() bool {
		return countAtVersion(ctx, t, c, n1, tok, "1.8.0") >= 1
	}) {
		t.Fatalf("第 1 批应当先升上去:\n%s", statusDump(ctx, t, c, n1, tok))
	}
	done1 := ""
	for _, node := range c.nodes {
		if versionOn(ctx, t, c, n1, tok, node) == "1.8.0" {
			done1 = node
		}
	}
	genDone1 := generationOn(ctx, t, c, done1)

	// ── ③ 等第 2 批超时，把整体停下 ──
	//
	// **慢是这条验收的一部分**：「停下来」就是靠那个 10 分钟的超时判出来的。
	if !waitUntil(ctx, 14*time.Minute, func() bool {
		return rolloutStatus(ctx, t, c, n1, tok).State == "halted"
	}) {
		t.Fatalf("离线的节点应当让批次超时并把变更停下:\n%s",
			statusDump(ctx, t, c, n1, tok))
	}

	out := c.mustRun(ctx, t, n1, "mechctl", "rollout", "status", "web",
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	for _, want := range []string{stalled, "resume", "abort", "Current distribution"} {
		if !strings.Contains(out, want) {
			t.Errorf("停下时的 status 该指名是哪台、并说清出路，缺 %q:\n%s", want, out)
		}
	}

	// 后面那批一台都没动
	if n := countAtVersion(ctx, t, c, n1, tok, "1.8.0"); n != 1 {
		t.Errorf("停下之后只该有第 1 批那台在新版，实际 %d 台", n)
	}

	// ── ④ 「修好」：把 agent 拉回来 ──
	startAgent(ctx, t, c, stalled, n1+":"+grpcPort)

	// ── ⑤ resume：从断点续做 ──
	c.mustRun(ctx, t, n1, "mechctl", "rollout", "resume", "web",
		"--server", "https://"+n1+":"+mechdPort, "--token", tok, "--ca-file", caPath)

	if !waitUntil(ctx, 8*time.Minute, func() bool {
		return rolloutStatus(ctx, t, c, n1, tok).State == "succeeded"
	}) {
		t.Fatalf("续做应当走完:\n%s", statusDump(ctx, t, c, n1, tok))
	}
	if n := countAtVersion(ctx, t, c, n1, tok, "1.8.0"); n != 3 {
		t.Errorf("续做完三台都该在 1.8.0，实际 %d 台", n)
	}

	// ── ⑥ 已完成的那一批没有被重做 ──
	//
	// 第 1 批那台在停下之前就已经物化到新版了。续做**不该再动它**——
	// 重新分批从头再来的话，它会多出一代。
	if got := generationOn(ctx, t, c, done1); got != genDone1 {
		t.Errorf("%s 属于已完成的批次，续做不该再动它（第 %d 代 → 第 %d 代）",
			done1, genDone1, got)
	}
}

// nodeWithOrdinal 返回某个序号上的机器。
//
// **批内顺序按 ordinal 升序**（§2.4），canary=1 因此第 N 批就是序号 N-1
// 那一台。升级发起之前就能算出来——这正是「稳定顺序」的用处：
// 它让「下一批会动哪台」成为一个可以提前回答的问题。
func nodeWithOrdinal(
	ctx context.Context, t *testing.T, c *cluster, mechdNode, tok string, ordinal int,
) string {
	t.Helper()
	for _, in := range componentStatus(ctx, t, c, mechdNode, tok).Instances {
		if in.Ordinal == ordinal {
			return in.Node
		}
	}
	return ""
}

// generationOn 返回某台机器上这个组件当前的 generation 序号。
//
// 它是「这台机器被动过几次」的直接读数：每物化一代就 +1。终态版本相同
// 而代数不同，说明有人被多动了一轮。
func generationOn(ctx context.Context, t *testing.T, c *cluster, node string) int {
	t.Helper()
	out, err := c.sh(ctx, node,
		"cat "+m7DataDir+"/mechlet/instances/*.json 2>/dev/null || true")
	if err != nil || strings.TrimSpace(out) == "" {
		return 0
	}
	dec := json.NewDecoder(strings.NewReader(out))
	for {
		var l nodeLedger
		if err := dec.Decode(&l); err != nil {
			return 0
		}
		if l.Component == "web" {
			return l.CurrentGeneration
		}
	}
}

// ── 夹具 ────────────────────────────────────────────────────────────────

type threeNodeSite struct{ token string }

// setupThreeNodeSite 起一个「n1 当控制面、三台都跑 agent」的站点。
//
// **n1 自己也是一个被管节点**：单机安装本来就会把本机注册进去，
// 而「控制面所在的机器不能被管」是一条没人会接受的限制。
func setupThreeNodeSite(
	ctx context.Context, t *testing.T, c *cluster, n1, n2, n3 string,
) threeNodeSite {
	t.Helper()
	// **从干净的控制面开始。**
	//
	// 滚动升级要部署一个组件并把它升上去。不擦库的话，第二次跑会撞上
	// 「组件已存在，重复 deploy 默认拒绝」——那条拒绝是对的，只是测试
	// 没有把前提
	//
	// （M9 之后 `component remove` 已经有了，但擦库仍然更合适：这条测试
	// 要的是一个确定的起点，而不是「上一次跑完之后剩下什么」。）
	// 建立完整。
	//
	// 与 resetNode 是同一条纪律：**擦机器也要擦册子**，否则两边的认知
	// 会在下一次跑时对不上。
	wipeControlPlane(ctx, t, c, n1)
	installOnce(ctx, t, c, n1)
	tok := adminToken(ctx, t, c, n1)

	stagePackOnMechd(ctx, t, c, n1, "") // 基线 1.2.0

	for _, n := range []string{n2, n3} {
		resetNode(ctx, t, c, n)
		joinNode(ctx, t, c, n1, n)
		startAgent(ctx, t, c, n, n1+":"+grpcPort)
	}
	// n1 的 mechlet 由 install 起在 systemd 下，不用另外拉

	// 三台都在线才算站点起好了：少一台的话，后面的批次会一直等
	if !waitUntil(ctx, 90*time.Second, func() bool {
		for _, n := range []string{n1, n2, n3} {
			if nodeStatus(ctx, t, c, n1, n) != "online" {
				return false
			}
		}
		return true
	}) {
		t.Fatalf("三台没有全部上线")
	}
	return threeNodeSite{token: tok}
}

// stagePackOnMechd 把 go-webapp 放进 mechd 的 Pack 集合。
//
// version 为空表示放基线（1.2.0）；否则复制一份并改掉版本号。
// 同名不同版本各占一个目录——那正是 packindex 的布局，测试不该绕开它。
func stagePackOnMechd(ctx context.Context, t *testing.T, c *cluster, node, version string) {
	t.Helper()
	// mechd 的 Pack 集合与载荷库都在 <data-dir>/mechd 之下——以启动日志
	// 里那一行为准，不按配置文件里的 packDir 猜
	packs := m7DataDir + "/mechd/packs"
	if version == "" {
		script := "set -e\n" +
			"mkdir -p " + packs + "\n" +
			"[ -d " + packs + "/go-webapp ] || cp -r /examples/packs/go-webapp " + packs + "/\n"
		if out, err := c.sh(ctx, node, script); err != nil {
			t.Fatalf("[%s] 放基线 Pack: %v\n%s", node, err, out)
		}
		// 示例 Pack 的 blobs 是占位 sha256（真实产物由 `mechpack assemble`
		// 填）。这条验收要的是**滚动升级的链路**，因此把摘要换成夹具
		// 二进制的即可。
		sum := stageWebappBlob(ctx, t, c, node)
		if out, err := c.sh(ctx, node,
			"sed -i 's/"+strings.Repeat("0", 64)+"/"+sum+"/' "+
				packs+"/go-webapp/pack.yaml"); err != nil {
			t.Fatalf("[%s] 改写占位摘要: %v\n%s", node, err, out)
		}
		// **放完要重启 mechd**：Pack 集合在启动时扫一遍，之后只有 upgrade
		// 会重扫（升级是唯一需要新版本 Pack 的流程）。deploy 不重扫，
		// 因此新放进去的基线 Pack 在这之前是看不见的。
		restartMechd(ctx, t, c, node)
		return
	}
	dst := packs + "/go-webapp-" + version
	script := "set -e\n" +
		"rm -rf " + dst + "\n" +
		"cp -r " + packs + "/go-webapp " + dst + "\n" +
		// 只改顶层那一行 version:，不碰 params 里可能出现的同名字段。
		// **要连行尾一起换掉**（`.*`）：只换 `^version:` 的话旧值会留在
		// 后面，得到 `version: "1.4.0" "1.2.0"` 这种解析不了的 YAML——
		// 而那时 packindex 只是静默跳过这个 Pack。
		"sed -i '0,/^version:.*/s//version: \"" + version + "\"/' " + dst + "/pack.yaml\n" +
		"grep -qx 'version: \"" + version + "\"' " + dst + "/pack.yaml\n"
	if out, err := c.sh(ctx, node, script); err != nil {
		t.Fatalf("[%s] 造 %s 版 Pack: %v\n%s", node, version, err, out)
	}
}

// wipeControlPlane 把控制面那台机器擦回未安装状态。
//
// 连 PKI 一起擦：留着旧 CA 而擦掉库，会得到一台「证书是我签的、但我不
// 认识你」的 mechd——那正是 revoke 想表达的状态，出现在这里只会让下一条
// 失败看起来像吊销出了问题。
func wipeControlPlane(ctx context.Context, t *testing.T, c *cluster, node string) {
	t.Helper()
	_, _ = c.run(ctx, node, "systemctl", "stop",
		"mecharion-mechd", "mecharion-mechlet")
	if out, err := c.sh(ctx, node,
		"rm -rf "+m7ConfDir+" "+m7DataDir+" "+m7Prefix); err != nil {
		t.Fatalf("[%s] 清理控制面: %v\n%s", node, err, out)
	}
	if out, err := c.sh(ctx, node,
		"for b in mechctl mechpack mechd mechlet; do "+
			"ln -sfn /usr/local/lib/mecharion/current/bin/$b /usr/bin/$b; done"); err != nil {
		t.Fatalf("[%s] 复原软链: %v\n%s", node, err, out)
	}
}

// restartMechd 重启 mechd 并等它起来。
func restartMechd(ctx context.Context, t *testing.T, c *cluster, node string) {
	t.Helper()
	if out, err := c.run(ctx, node, "systemctl", "restart", mechdUnit); err != nil {
		t.Fatalf("[%s] 重启 mechd: %v\n%s", node, err, out)
	}
	if !waitUntil(ctx, 90*time.Second, func() bool {
		o, err := c.run(ctx, node, "systemctl", "is-active", mechdUnit)
		return err == nil && strings.HasPrefix(strings.TrimSpace(o), "active")
	}) {
		logs, _ := c.run(ctx, node, "journalctl", "-u", mechdUnit, "--no-pager", "-n", "40")
		t.Fatalf("[%s] mechd 重启后没起来:\n%s", node, tailOf(logs, 40))
	}
}

// stageWebappBlob 把夹具二进制打成 tar.gz 放进 mechd 的载荷库，返回其 sha256。
//
// 打包在**宿主侧**做：节点镜像里没有 Go，而这个 tarball 的形状（顶层套
// 一个版本目录，好让 strip: 1 有东西可剥）正是真实上游产物的形状。
func stageWebappBlob(ctx context.Context, t *testing.T, c *cluster, node string) string {
	t.Helper()
	body, err := os.ReadFile("bin/webapp")
	if err != nil {
		t.Skipf("找不到夹具二进制 bin/webapp: %v（先跑 ./hack/e2ebin.sh）", err)
	}

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	tw := tar.NewWriter(zw)
	for _, e := range []struct {
		name string
		mode int64
		body []byte
	}{
		{"webapp-1.2.0/bin/webapp", 0o755, body},
		{"webapp-1.2.0/README.md", 0o644, []byte("Mecharion 多节点验收夹具\n")},
	} {
		if err := tw.WriteHeader(&tar.Header{
			Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	sum := hex.EncodeToString(sha256Of(gz.Bytes()))
	dir := m7DataDir + "/mechd/blobs/sha256/" + sum[:2]
	if out, err := c.sh(ctx, node, "mkdir -p "+dir); err != nil {
		t.Fatalf("[%s] 建载荷目录: %v\n%s", node, err, out)
	}
	// 走 stdin 而不是把内容拼进 shell 命令：这是一个二进制 tarball
	cmd := dockerStdin(ctx, node, "sh", "-c", "cat > "+dir+"/"+sum)
	cmd.Stdin = bytes.NewReader(gz.Bytes())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("[%s] 写载荷: %v\n%s", node, err, out)
	}
	return sum
}

func sha256Of(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// ── 观测 ────────────────────────────────────────────────────────────────

type rolloutView struct {
	State   string   `json:"state"`
	Reason  string   `json:"reason"`
	To      string   `json:"to"`
	Batch   int      `json:"batch"`
	Batches int      `json:"batches"`
	Current string   `json:"current"`
	Skipped []string `json:"skipped"`
}

func rolloutStatus(ctx context.Context, t *testing.T, c *cluster, mechdNode, tok string) rolloutView {
	t.Helper()
	out, err := c.run(ctx, mechdNode, "mechctl", "rollout", "status", "web", "-o", "json",
		"--server", "https://"+mechdNode+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	if err != nil {
		t.Fatalf("取 rollout status: %v\n%s", err, out)
	}
	var v rolloutView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("解析 rollout status: %v\n%s", err, out)
	}
	return v
}

type statusView struct {
	Version   string `json:"version"`
	Converged bool   `json:"converged"`
	Instances []struct {
		Node           string `json:"node"`
		Ordinal        int    `json:"ordinal"`
		Converged      bool   `json:"converged"`
		Health         string `json:"health"`
		PendingVersion string `json:"pendingVersion"`
	} `json:"instances"`
}

func componentStatus(ctx context.Context, t *testing.T, c *cluster, mechdNode, tok string) statusView {
	t.Helper()
	out, err := c.run(ctx, mechdNode, "mechctl", "component", "status", "web", "-o", "json",
		"--server", "https://"+mechdNode+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	if err != nil {
		t.Fatalf("取 component status: %v\n%s", err, out)
	}
	var v statusView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("解析 component status: %v\n%s", err, out)
	}
	return v
}

func componentConverged(ctx context.Context, t *testing.T, c *cluster, mechdNode, tok string) bool {
	t.Helper()
	return componentStatus(ctx, t, c, mechdNode, tok).Converged
}

// countAtVersion 数有几台**机器上真正跑着**目标版本。
//
// 判据取自节点自己的当前代（`mechlet status`），不是 mechd 的期望：
// 这条测试问的正是「下发有没有被管住」，用期望回答等于自己给自己打分。
func countAtVersion(
	ctx context.Context, t *testing.T, c *cluster, mechdNode, tok, version string,
) int {
	t.Helper()
	n := 0
	for _, node := range c.nodes {
		if versionOn(ctx, t, c, mechdNode, tok, node) == version {
			n++
		}
	}
	return n
}

// nodeLedger 是 mechlet 本地台账里这条测试关心的那几个字段。
type nodeLedger struct {
	Component         string `json:"component"`
	CurrentGeneration int    `json:"currentGeneration"`
	Generations       []struct {
		Seq     int    `json:"seq"`
		Version string `json:"version"`
	} `json:"generations"`
}

// versionOn 返回某台机器上此刻**真正跑着**的 go-webapp 版本。
//
// 从节点自己的台账读，不从 mechd 的库读：这条测试问的正是「下发有没有
// 被管住」，用 mechd 的期望来回答等于自己给自己打分。
//
// 台账在宿主侧解析，不在容器里 `jq`——节点镜像里没有 jq，而为了一条
// 断言往被测机器上装工具，装的那一刻它就不再是被测机器了。
func versionOn(
	ctx context.Context, t *testing.T, c *cluster, mechdNode, tok, node string,
) string {
	t.Helper()
	out, err := c.sh(ctx, node,
		"cat "+m7DataDir+"/mechlet/instances/*.json 2>/dev/null || true")
	if err != nil || strings.TrimSpace(out) == "" {
		return ""
	}
	dec := json.NewDecoder(strings.NewReader(out))
	for {
		var l nodeLedger
		if err := dec.Decode(&l); err != nil {
			return ""
		}
		if l.Component != "web" {
			continue
		}
		for _, g := range l.Generations {
			if g.Seq == l.CurrentGeneration {
				return g.Version
			}
		}
	}
}

func statusDump(ctx context.Context, t *testing.T, c *cluster, mechdNode, tok string) string {
	t.Helper()
	var b strings.Builder
	out, _ := c.run(ctx, mechdNode, "mechctl", "component", "status", "web",
		"--server", "https://"+mechdNode+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	b.WriteString(out)
	out, _ = c.run(ctx, mechdNode, "mechctl", "rollout", "status", "web",
		"--server", "https://"+mechdNode+":"+mechdPort, "--token", tok, "--ca-file", caPath)
	b.WriteString("\n")
	b.WriteString(out)
	for _, node := range c.nodes {
		v := versionOn(ctx, t, c, mechdNode, tok, node)
		b.WriteString("\n" + node + " 机器上跑着: " + orNone(v))
	}
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "（没有）"
	}
	return s
}
