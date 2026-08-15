package mechd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mecharion/mecharion/internal/store"
)

// 本文件钉住 **M7 第 7 步真正的那一半**：批次不只是被算出来记下来，
// 它还得管住下发。
//
// 一个只把批次落盘、下发照旧全量的实现能通过 batch_test.go 里的每一条，
// 也能让 `rollout status` 说出「第 1/3 批」——而三批的机器早已全部升完。
// 那比没有分批更糟：它给了一个假的安全感。

// stageZKVersion 在 Pack 集合里造一个新版本的 zookeeper。
//
// 同名不同版本各占一个目录——这正是 packindex 的布局，测试不该绕开它
// 自己造索引。
func stageZKVersion(t *testing.T, f *fixture, version string) {
	t.Helper()
	src := filepath.Join(f.packDir, "zookeeper")
	dst := filepath.Join(t.TempDir(), "zookeeper-"+version)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	copyPackTree(t, src, dst)

	p := filepath.Join(dst, "pack.yaml")
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	out, n := bumpVersion(string(body), version)
	if n == 0 {
		t.Fatalf("没能改掉版本号，pack.yaml 的形状变了？")
	}
	if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Packs.AddDir(filepath.Dir(dst)); err != nil {
		t.Fatalf("加载新版本 Pack: %v", err)
	}
}

// bumpVersion 换掉 pack.yaml 顶层的 version。
func bumpVersion(body, to string) (string, int) {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "version:") {
			continue
		}
		return strings.Replace(body, line, `version: "`+to+`"`, 1), 1
	}
	return body, 0
}

func copyPackTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, fi.Mode())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// upgradeZK 把 zk-main 升到 version，并返回该组件。
func upgradeZK(t *testing.T, f *fixture, version string) store.Component {
	t.Helper()
	if _, err := f.svc.Upgrade(ctx(), UpgradeRequest{
		Component: "zk-main", Version: version, Actor: "tester",
	}); err != nil {
		t.Fatalf("升级到 %s: %v", version, err)
	}
	comp, err := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	return comp
}

// assignedVersions 返回每个节点这一刻被下发的规格里的 Pack 版本。
//
// 从**真实的下发出口**取，不是从渲染结果取：这条测试要钉的正是
// 「下发有没有被批次管住」，绕过 Assignment 就什么都没验。
func assignedVersions(t *testing.T, f *fixture, nodes ...string) map[string]string {
	t.Helper()
	b := &Backend{S: f.svc}
	out := map[string]string{}
	for _, n := range nodes {
		specs, err := b.Assignment(ctx(), n)
		if err != nil {
			t.Fatalf("取 %s 的规格: %v", n, err)
		}
		for _, is := range specs {
			if is.Spec.Component == "zk-main" {
				out[n] = is.Spec.Pack.Version
			}
		}
	}
	return out
}

// TestBatchesGateDelivery 是这一步的核心验收。
//
// 三台 ZK、canary=1：放行第一批之后**只有一台**拿到新版规格，另外两台
// 拿到的仍然是旧版——而且是旧版**真正解析出来的**规格，不是一份改了
// 版本号的新版规格（那样的 digest 与两边都对不上，节点会当成第三个
// 版本反复物化）。
func TestBatchesGateDelivery(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")
	before := assignedVersions(t, f, "n1", "n2", "n3")

	stageZKVersion(t, f, "9.9.9")
	comp := upgradeZK(t, f, "9.9.9")

	// ── ① 升级刚发起：还没有任何一批被放行，三台都停在旧版 ──
	//
	// 「批次算好了但还没放行」这个中间态必须是**谁都不动**。一个在
	// startRollout 里顺手放行第一批的实现会跳过这条。
	got := assignedVersions(t, f, "n1", "n2", "n3")
	for _, n := range []string{"n1", "n2", "n3"} {
		if got[n] != before[n] {
			t.Errorf("放行之前 %s 不该拿到新版，实际 %s", n, got[n])
		}
	}

	// ── ② 放行第一批：只有一台变 ──
	releaseNext(t, f, comp)
	got = assignedVersions(t, f, "n1", "n2", "n3")
	if n := countAt(got, "9.9.9"); n != 1 {
		t.Fatalf("canary=1 时第一批应当只有 1 台拿到新版，实际 %d 台：%v", n, got)
	}
	// 另外两台拿到的必须是**旧版真正解析出来的**规格
	for n, v := range got {
		if v != "9.9.9" && v != before[n] {
			t.Errorf("%s 应当停在旧版 %s，实际 %s", n, before[n], v)
		}
	}

	// ── ③ 逐批放完，三台全在新版 ──
	for i := 0; i < 4; i++ {
		releaseNext(t, f, comp)
	}
	got = assignedVersions(t, f, "n1", "n2", "n3")
	if n := countAt(got, "9.9.9"); n != 3 {
		t.Errorf("全部放行之后三台都该在新版，实际 %d 台：%v", n, got)
	}
}

// TestBatchAdvanceWaitsForConvergence 钉住推进的判据是**观测到的收敛**。
//
// 按「已经发出去了」推进等于一次性下发，只是把时间摊开了：批次会一批
// 接一批地立刻放行，谁都没等。
func TestBatchAdvanceWaitsForConvergence(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")
	stageZKVersion(t, f, "9.9.9")
	comp := upgradeZK(t, f, "9.9.9")

	// 第一次推进：放行第一批
	f.svc.AdvanceRollout(ctx(), comp)
	if seq := releasedSeq(t, f, comp); seq != 1 {
		t.Fatalf("第一次推进应当放行第 1 批，实际放行中的是第 %d 批", seq)
	}

	// 再推进几次，但**没有任何实例上报收敛**——不该往前走
	for i := 0; i < 3; i++ {
		f.svc.AdvanceRollout(ctx(), comp)
	}
	if seq := releasedSeq(t, f, comp); seq != 1 {
		t.Errorf("没有收敛上报时不该放行下一批，实际已经到第 %d 批", seq)
	}

	// 第一批的那台收敛了——但**光收敛还不够**，还得稳住 StableFor。
	//
	// 这一条是第 8 步的核心：一个刚起来还没崩的进程，在起来那一瞬间收敛与
	// 健康都成立。没有稳定窗口，Rollout 会用一台正在崩溃循环里的机器去
	// 批准下一批。
	reportConverged(t, f, comp, batchAt(t, f, comp, 1))
	f.svc.AdvanceRollout(ctx(), comp)
	if seq := releasedSeq(t, f, comp); seq != 1 {
		t.Errorf("刚收敛还没稳住，不该放行下一批，实际第 %d 批", seq)
	}

	// 稳住了才放行
	f.clock.advance(StableFor)
	f.svc.AdvanceRollout(ctx(), comp)
	if seq := releasedSeq(t, f, comp); seq != 2 {
		t.Errorf("稳定窗口过后应当放行第 2 批，实际第 %d 批", seq)
	}
}

// TestRolloutNotSucceededWhileBatchesPending 钉住一条**很容易漏掉**的。
//
// 分批期间，还没轮到的实例拿的是旧版规格，它们**如实上报也是「已收敛」**
// ——它们确实在跑该跑的那一版。于是「整体已收敛且版本 == 目标版本」这个
// 判据会在第一批刚做完时就成立，把一次刚开头的升级宣布成功。
//
// 那之后 Rollout 记录被归档，剩下两批再也不会被放行：三分之二的机器
// 永远停在旧版，而 `rollout history` 说这次升级成功了。
func TestRolloutNotSucceededWhileBatchesPending(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")
	stageZKVersion(t, f, "9.9.9")
	comp := upgradeZK(t, f, "9.9.9")

	f.svc.AdvanceRollout(ctx(), comp) // 放行第 1 批
	// **每个实例都如实上报**——这正是真实集群里的样子，而不是只有
	// 被放行的那台在说话。
	passCurrentBatch(t, f, comp)

	v, err := f.svc.RolloutStatus(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	if v.State == store.RolloutSucceeded {
		// 只做完第 1/3 批就宣布成功——剩下两批再也不会被放行
		t.Fatalf("只做完第一批就宣布成功了，状态 %s", v.State)
	}
	if v.Batch != 2 {
		t.Errorf("应当推进到第 2 批，实际第 %d 批", v.Batch)
	}

	// 全部做完之后**才**成功
	for i := 0; i < 3; i++ {
		passCurrentBatch(t, f, comp)
	}
	v, err = f.svc.RolloutStatus(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	if v.State != store.RolloutSucceeded {
		t.Errorf("全部批次做完后应当成功，实际 %s（第 %d/%d 批）",
			v.State, v.Batch, v.Batches)
	}
}

// TestRolloutStatusReportsBatchProgress 是这一步在验收表上的那一行。
func TestRolloutStatusReportsBatchProgress(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")
	stageZKVersion(t, f, "9.9.9")
	comp := upgradeZK(t, f, "9.9.9")

	f.svc.AdvanceRollout(ctx(), comp) // 放行第 1 批
	v, err := f.svc.RolloutStatus(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	if v.Batches != 3 {
		t.Errorf("三台 ZK、canary=1 应当分成 3 批，实际 %d", v.Batches)
	}
	if v.Batch != 1 {
		t.Errorf("应当显示在第 1 批，实际第 %d 批", v.Batch)
	}
	if !strings.Contains(v.Current, "server") {
		t.Errorf("应当说得出正在做哪个角色的哪台，实际 %q", v.Current)
	}

	passCurrentBatch(t, f, comp)
	v, err = f.svc.RolloutStatus(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	if v.Batch != 2 {
		t.Errorf("第一批完成后应当显示第 2 批，实际第 %d 批", v.Batch)
	}
}

// TestCordonedNodesListedInStatus 钉住被跳过的节点**说得出来**。
//
// 只写日志是不够的：运维事后问的是「为什么这台还是旧版」，而那时日志
// 已经翻过去了。
func TestCordonedNodesListedInStatus(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")
	stageZKVersion(t, f, "9.9.9")

	n2, err := f.svc.Repos.Nodes().GetByName(ctx(), f.site.ID, "n2")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Repos.Nodes().SetCordoned(ctx(), n2.ID, ptrNow(f)); err != nil {
		t.Fatal(err)
	}
	comp := upgradeZK(t, f, "9.9.9")

	v, err := f.svc.RolloutStatus(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	if v.Batches != 2 {
		t.Errorf("cordon 掉一台之后应当剩 2 批，实际 %d", v.Batches)
	}
	if len(v.Skipped) != 1 || v.Skipped[0] != "n2" {
		t.Errorf("被跳过的应当是 n2，实际 %v", v.Skipped)
	}

	// 而且它**真的**不会拿到新版：把所有批次放完，n2 仍在旧版
	for i := 0; i < 6; i++ {
		releaseNext(t, f, comp)
	}
	if got := assignedVersions(t, f, "n2"); got["n2"] == "9.9.9" {
		t.Errorf("被 cordon 的 n2 不该拿到新版")
	}
}

// TestCordonedNodeDoesNotBlockSuccess 钉住被 cordon 的机器不拖住整次升级。
//
// 它们按定义就停在旧版，因此「整体已收敛」对这次变更永远为假。把那个
// 当成成功判据的话，**每一次「先 cordon 掉一台再升级」都会以超时失败
// 收场**——而那正是 cordon 最常见的用法：先隔离一台有问题的机器，再升级
// 其余的。
//
// 判据只能是「每一批都做完了」。
func TestCordonedNodeDoesNotBlockSuccess(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")
	stageZKVersion(t, f, "9.9.9")

	n2, err := f.svc.Repos.Nodes().GetByName(ctx(), f.site.ID, "n2")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Repos.Nodes().SetCordoned(ctx(), n2.ID, ptrNow(f)); err != nil {
		t.Fatal(err)
	}
	comp := upgradeZK(t, f, "9.9.9")

	// 两批（n1、n3），n2 被跳过。逐批推完。
	for i := 0; i < 6; i++ {
		passCurrentBatch(t, f, comp)
	}

	v, err := f.svc.RolloutStatus(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	if v.State != store.RolloutSucceeded {
		t.Fatalf("cordon 掉一台之后，其余两台升完就该算成功，实际 %s（%s，第 %d/%d 批）",
			v.State, v.Reason, v.Batch, v.Batches)
	}
}

// TestPauseStopsBatchRelease 钉住 pause 真的停在当前这批。
//
// 一个只冻结判定却继续推批次的实现等于没有 pause——而运维敲 pause
// 正是因为他觉得不对劲，最不想看到的就是它继续往前走。
func TestPauseStopsBatchRelease(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")
	stageZKVersion(t, f, "9.9.9")
	comp := upgradeZK(t, f, "9.9.9")

	f.svc.AdvanceRollout(ctx(), comp)
	reportConverged(t, f, comp, batchAt(t, f, comp, 1))
	f.svc.AdvanceRollout(ctx(), comp) // 开窗
	f.clock.advance(StableFor)        // 门禁这时已经满足，只差有人来判

	if _, err := f.svc.SetRolloutPaused(ctx(), "", "zk-main", true, "tester"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		f.svc.AdvanceRollout(ctx(), comp)
	}
	if seq := releasedSeq(t, f, comp); seq != 1 {
		t.Errorf("pause 期间不该放行下一批，实际已经到第 %d 批", seq)
	}

	if _, err := f.svc.SetRolloutPaused(ctx(), "", "zk-main", false, "tester"); err != nil {
		t.Fatal(err)
	}
	f.svc.AdvanceRollout(ctx(), comp)
	if seq := releasedSeq(t, f, comp); seq != 2 {
		t.Errorf("resume 之后应当继续放行，实际第 %d 批", seq)
	}
}

// TestSetRolloutPolicyAppliesToNextChange 钉住旋钮只影响下一次变更。
func TestSetRolloutPolicyAppliesToNextChange(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")

	zero := 0
	v, err := f.svc.SetRolloutPolicy(ctx(), "", "zk-main", nil, &zero, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if v.Canary != 0 {
		t.Fatalf("canary 应当能被设成 0（关掉金丝雀），实际 %d", v.Canary)
	}

	// ZK 声明了 quorum，因此 3 台时每批仍被压到 1 台——但少了金丝雀那批
	stageZKVersion(t, f, "9.9.9")
	upgradeZK(t, f, "9.9.9")
	sv, err := f.svc.RolloutStatus(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	if sv.Batches != 3 {
		t.Errorf("quorum 角色 3 台仍应是 3 批，实际 %d", sv.Batches)
	}

	// 改动被记下来了，而且下一次读得出来
	got, err := f.svc.RolloutPolicy(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	if got.Canary != 0 {
		t.Errorf("canary 应当持久化为 0，实际 %d", got.Canary)
	}
	if got.Note == "" {
		t.Error("有变更进行中时应当说清「本次不生效」")
	}
}

// TestQuorumCapIsVisible 是 §5 第 13 行的验收。
//
// 验收表原来写的是「设 2 被拒」。改成「压到 1 **并且说出来**」，理由是：
// 那个旋钮在 **Component** 上，而 quorum 在**角色**上——一个组件可以同时
// 有仲裁角色和无状态角色，为了前者拒掉整条设置，会让后者也用不上本来
// 合法的并发度。
//
// 于是机制是按角色压，而这条钉的是**它不能是静默的**：用户设了 2 却看到
// 每批只动 1 台，不说的话他会以为设置没生效。
func TestQuorumCapIsVisible(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3") // zookeeper 的 server 角色声明了 quorum

	two := 2
	v, err := f.svc.SetRolloutPolicy(ctx(), "", "zk-main", &two, nil, "tester")
	if err != nil {
		t.Fatalf("设 2 不该被拒——那会让同组件里的无状态角色也用不上: %v", err)
	}
	if v.MaxUnavailable != 2 {
		t.Errorf("用户设的值应当照收，实际 %d", v.MaxUnavailable)
	}
	// **但要说出哪些角色不受它控制**
	if len(v.QuorumRoles) != 1 || v.QuorumRoles[0] != "server" {
		t.Fatalf("应当列出声明了仲裁的角色 server，实际 %v", v.QuorumRoles)
	}

	// 而且真的被压住了：3 台仲裁角色仍是每批 1 台
	stageZKVersion(t, f, "9.9.9")
	upgradeZK(t, f, "9.9.9")
	sv, err := f.svc.RolloutStatus(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	if sv.Batches != 3 {
		t.Errorf("quorum 角色 3 台应当被压成 3 批（每批 1 台），实际 %d 批", sv.Batches)
	}
}

// TestSetRolloutPolicyRejectsZeroConcurrency 钉住 maxUnavailable=0 被拒。
//
// 设成 0 等于谁都不许动，那不是一个更安全的默认，是一次永远不会结束的
// 升级。
func TestSetRolloutPolicyRejectsZeroConcurrency(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")

	zero := 0
	_, err := f.svc.SetRolloutPolicy(ctx(), "", "zk-main", &zero, nil, "tester")
	if err == nil {
		t.Fatal("--max-unavailable 0 应当被拒绝")
	}
	if !strings.Contains(err.Error(), "must be at least 1") {
		t.Errorf("错误信息该说清底线，实际: %v", err)
	}
}

// ── 测试辅助 ────────────────────────────────────────────────────────────

func countAt(got map[string]string, version string) int {
	n := 0
	for _, v := range got {
		if v == version {
			n++
		}
	}
	return n
}

func ptrNow(f *fixture) *time.Time { t := f.svc.now(); return &t }

// releaseNext 放行下一批（不管有没有收敛），供只关心下发门禁的测试用。
func releaseNext(t *testing.T, f *fixture, comp store.Component) {
	t.Helper()
	rel, err := f.svc.releaseStateOf(ctx(), comp)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rel.Batches {
		b := rel.Batches[i]
		if b.State == store.BatchReleased {
			if err := f.svc.Repos.RolloutBatches().SetState(ctx(),
				b.ID, store.BatchDone); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if b.State == store.BatchPending {
			if err := f.svc.Repos.RolloutBatches().SetState(ctx(),
				b.ID, store.BatchReleased); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
}

// releasedSeq 返回正在等待收敛的那一批的序号；没有则返回 0。
func releasedSeq(t *testing.T, f *fixture, comp store.Component) int {
	t.Helper()
	rel, err := f.svc.releaseStateOf(ctx(), comp)
	if err != nil {
		t.Fatal(err)
	}
	if cur := rel.current(); cur != nil {
		return cur.Seq
	}
	return 0
}

// batchAt 返回第 seq 批。
func batchAt(t *testing.T, f *fixture, comp store.Component, seq int) store.RolloutBatch {
	t.Helper()
	rel, err := f.svc.releaseStateOf(ctx(), comp)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range rel.Batches {
		if b.Seq == seq {
			return b
		}
	}
	t.Fatalf("没有第 %d 批", seq)
	return store.RolloutBatch{}
}

// passCurrentBatch 让当前这批过门禁。
//
// 三步，缺一不可：上报收敛 → 判一次（**开窗**，第一次看到不算过）→
// 推过稳定窗口 → 再判一次（这次才放行下一批）。
func passCurrentBatch(t *testing.T, f *fixture, comp store.Component) {
	t.Helper()
	reportAllConverged(t, f, comp)
	f.svc.AdvanceRollout(ctx(), comp)
	f.clock.advance(StableFor)
	reportAllConverged(t, f, comp)
	f.svc.AdvanceRollout(ctx(), comp)
}

// reportAllConverged 替**每个**实例上报「已收敛」，各自按此刻发给它的版本。
//
// 这是真实集群里的样子：还没轮到的那些机器也在 15 秒一次地上报，而且
// 它们确实在跑该跑的那一版。
func reportAllConverged(t *testing.T, f *fixture, comp store.Component) {
	t.Helper()
	view, err := f.svc.Status(ctx(), "", comp.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, iv := range view.Instances {
		if err := f.svc.Repos.Status().Put(ctx(), store.InstanceStatus{
			InstanceID: iv.ID, Digest: iv.Want, Result: "ok",
			WorkloadState: "running", Health: "healthy",
			Generation: 1, ReportedAt: f.svc.now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// reportConverged 替一批的目标上报「已收敛」。
//
// 上报的 digest 取自**此刻真正会下发给它的规格**——伪造一个 digest 只会
// 让测试自己定义收敛，而收敛判据正是被测的东西。
func reportConverged(t *testing.T, f *fixture, comp store.Component, b store.RolloutBatch) {
	t.Helper()
	view, err := f.svc.Status(ctx(), "", comp.Name)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]string{}
	for _, iv := range view.Instances {
		want[iv.ID] = iv.Want
	}
	for _, tg := range b.Targets {
		if err := f.svc.Repos.Status().Put(ctx(), store.InstanceStatus{
			InstanceID: tg.InstanceID,
			Digest:     want[tg.InstanceID], Result: "ok",
			WorkloadState: "running", Health: "healthy",
			Generation: 1, ReportedAt: f.svc.now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
}
