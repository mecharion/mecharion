package mechd

import (
	"strings"
	"testing"
	"time"

	"github.com/mecharion/mecharion/internal/protocol"
	"github.com/mecharion/mecharion/internal/store"
)

// 本文件钉住 **M7 第 8 步**：健康门禁。
//
// 这一步要挡住的那件事，用一句话说是：**别拿一台正在崩溃循环里的机器去
// 批准下一批**。少了它，一次坏升级会被逐批放大到全集群——每一批都「收敛
// 了」，每一批都在几秒后崩掉，而 Rollout 一路绿灯走到底。

// batchAt2 造一个已放行的批次，供纯函数测试用。
func releasedBatch(targets ...store.BatchTarget) store.RolloutBatch {
	return store.RolloutBatch{
		ID: 1, Seq: 1, Role: "server", Targets: targets,
		State: store.BatchReleased,
	}
}

func target(id int64, node string) store.BatchTarget {
	return store.BatchTarget{InstanceID: id, Role: "server", Node: node}
}

func healthy(restarts int) instanceGateState {
	return instanceGateState{Converged: true, Restarts: restarts}
}

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// TestGateNeverPassesOnFirstSight 是这一步最重要的一条。
//
// 一个刚起来还没崩的进程，在起来后的那一瞬间收敛与健康都成立。门禁
// **第一次看到就放行**的话，稳定窗口等于不存在——而那正是 M6
// 「崩溃重启循环曾经被报成 ok」的多节点版本。
func TestGateNeverPassesOnFirstSight(t *testing.T) {
	b := releasedBatch(target(1, "n1"))
	v := checkGate(b, map[int64]instanceGateState{1: healthy(0)}, t0)

	if v.Passed {
		t.Fatal("第一次看到就通过了——稳定窗口形同虚设")
	}
	if !v.Since.Equal(t0) {
		t.Errorf("这一轮应当开窗，实际窗口起点 %v", v.Since)
	}
}

// TestGatePassesAfterStableFor 钉住稳住之后确实会过。
//
// 少了这条，一个「永远不通过」的门禁也能让上面那条测试变绿——而它会让
// 每一次滚动升级都卡死在第一批。
func TestGatePassesAfterStableFor(t *testing.T) {
	b := releasedBatch(target(1, "n1"))
	b.HealthySince = t0
	b.RestartBaseline = map[int64]int{1: 0}

	if v := checkGate(b, map[int64]instanceGateState{1: healthy(0)},
		t0.Add(StableFor-time.Second)); v.Passed {
		t.Error("差一秒不该算过")
	}
	v := checkGate(b, map[int64]instanceGateState{1: healthy(0)}, t0.Add(StableFor))
	if !v.Passed {
		t.Fatalf("稳住 %s 之后应当通过，卡在: %v", StableFor, v.Blockers)
	}
}

// TestGateResetsWindowWhenAnInstanceDropsOut 钉住窗口会被清零。
//
// **这是门禁挡住崩溃循环的全部机制**：机器每崩一次就把窗口清掉，于是
// 永远攒不满 StableFor，最后撞上 BatchTimeout 被指名。
func TestGateResetsWindowWhenAnInstanceDropsOut(t *testing.T) {
	b := releasedBatch(target(1, "n1"), target(2, "n2"))
	b.HealthySince = t0
	b.RestartBaseline = map[int64]int{1: 0, 2: 0}

	// n2 变得不健康：即使已经稳了很久，窗口也要清零
	v := checkGate(b, map[int64]instanceGateState{
		1: healthy(0),
		2: {Converged: false, Why: "健康检查失败"},
	}, t0.Add(10*time.Minute))

	if v.Passed {
		t.Fatal("有实例掉出判据时不该通过")
	}
	if !v.Since.IsZero() {
		t.Errorf("窗口应当被清零，实际起点 %v", v.Since)
	}
	if len(v.Blockers) != 1 || !strings.Contains(v.Blockers[0], "n2") {
		t.Errorf("应当指名 n2，实际 %v", v.Blockers)
	}
}

// TestGateCatchesRestartLoopThatOutlastsTheWindow 是加第 3 条判据的理由。
//
// 稳定窗口是**有限的**：一个每 40 秒崩一次的进程能从 30 秒窗口里干干净净
// 地溜过去——每次采样都是「收敛、健康、running」。重启计数不会漏，
// 涨了就是崩过，与观察时机无关。
func TestGateCatchesRestartLoopThatOutlastsTheWindow(t *testing.T) {
	b := releasedBatch(target(1, "n1"))
	b.HealthySince = t0
	b.RestartBaseline = map[int64]int{1: 7}

	// 窗口内每一次采样都「健康」，但重启次数从 7 涨到了 8
	v := checkGate(b, map[int64]instanceGateState{1: healthy(8)}, t0.Add(StableFor))

	if v.Passed {
		t.Fatal("窗口内崩过一次，不该通过——这正是稳定窗口漏掉的那一类")
	}
	if len(v.Blockers) != 1 || !strings.Contains(v.Blockers[0], "restarted") {
		t.Errorf("应当说清是重启了，实际 %v", v.Blockers)
	}
	if !v.Since.IsZero() {
		t.Error("崩过之后窗口应当清零重来")
	}
}

// TestOneCrashCostsTheWholeWindow 钉住崩溃**不会被吸收**。
//
// 走整条链路（真的库、真的 saveGate），因为「基线会不会被悄悄吸收」这件事
// 取决于窗口起点什么时候落盘——那不在 checkGate 这个纯函数里。
//
// 一次崩溃必须赔掉一整个 StableFor：崩在窗口末尾的那一下，不能因为
// 「刚才那 29 秒都是好的」就放行。
func TestOneCrashCostsTheWholeWindow(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")
	stageZKVersion(t, f, "9.9.9")
	comp := upgradeZK(t, f, "9.9.9")

	f.svc.AdvanceRollout(ctx(), comp) // 放行第 1 批
	first := batchAt(t, f, comp, 1)

	// 窗口开起来，稳稳地走过 29 秒
	reportCrashing(t, f, comp, first, 0)
	f.svc.AdvanceRollout(ctx(), comp)
	f.clock.advance(StableFor - time.Second)
	reportCrashing(t, f, comp, first, 0)
	f.svc.AdvanceRollout(ctx(), comp)
	if seq := releasedSeq(t, f, comp); seq != 1 {
		t.Fatalf("还差一秒就放行了，实际第 %d 批", seq)
	}

	// 就在这一刻它崩了一次
	f.clock.advance(time.Second)
	reportCrashing(t, f, comp, first, 1)
	f.svc.AdvanceRollout(ctx(), comp)
	if seq := releasedSeq(t, f, comp); seq != 1 {
		t.Fatalf("崩过一次就放行了下一批，实际第 %d 批", seq)
	}

	// **再等一个完整的 StableFor 才行**，不是「补上刚才那一秒」。
	//
	// 少了这一条，一个每 30 秒崩一次的进程会一路绿灯——每次都刚好在
	// 窗口末尾崩，然后靠之前攒的时间过关。
	f.clock.advance(StableFor - time.Second)
	reportCrashing(t, f, comp, first, 1)
	f.svc.AdvanceRollout(ctx(), comp)
	if seq := releasedSeq(t, f, comp); seq != 1 {
		t.Fatalf("崩溃之后窗口该从头算，实际第 %d 批", seq)
	}

	// 窗口是在上一轮（崩溃之后的第一次上报）才重新开的，因此还要再走
	// 一整个 StableFor
	f.clock.advance(StableFor)
	reportCrashing(t, f, comp, first, 1)
	f.svc.AdvanceRollout(ctx(), comp)
	if seq := releasedSeq(t, f, comp); seq != 2 {
		t.Errorf("稳住一整个窗口之后应当放行，实际第 %d 批", seq)
	}
}

// TestRestartsSurviveTheWholeReportPath 钉住重启次数**真的从上报走到了库里**。
//
// 这条补的是一个测试盲区：本文件其余的门禁测试都直接往 InstanceStatus 里
// 写 Restarts，因此「协议层 → 库」这一段接错了（比如永远取到 0）也照样
// 全绿——而门禁的第 3 条判据从此静默失效。
//
// 顺带钉住它在 `component status` 里看得见：一个每分钟崩一次又被拉起的
// 服务，在「running / healthy」两列里与健康的没有区别。
func TestRestartsSurviveTheWholeReportPath(t *testing.T) {
	f := newFixture(t, "n1")
	deployWebapp(t, f, "n1")

	st, _ := f.svc.Status(ctx(), "", "web")
	b := &Backend{S: f.svc}
	if err := b.Report(ctx(), protocol.Report{
		Node: "n1",
		Instances: []protocol.InstanceStatus{{
			Component: "web", Role: "default", Digest: st.Instances[0].Want,
			Generation: 1, Result: "ok",
			Health: &protocol.HealthStatus{State: "healthy"},
			Workload: &protocol.WorkloadStatus{
				Runtime: "systemd", State: "running", Restarts: 37,
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	st, err := f.svc.Status(ctx(), "", "web")
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Instances[0].Restarts; got != 37 {
		t.Fatalf("重启次数没走通上报链路：期望 37，实际 %d", got)
	}
	// 门禁看到的也得是同一个数
	if got := gateStateOf(st.Instances[0]).Restarts; got != 37 {
		t.Errorf("门禁看到的重启次数是 %d，期望 37", got)
	}
}

// TestGateNamesEveryBlocker 钉住超时的时候说得出该看哪台。
//
// 「10 分钟内未收敛」说不出该去看哪台机器——这是一条有用的报错与一句
// 废话之间的全部区别。
func TestGateNamesEveryBlocker(t *testing.T) {
	b := releasedBatch(target(1, "n1"), target(2, "n2"), target(3, "n3"))
	v := checkGate(b, map[int64]instanceGateState{
		1: healthy(0),
		2: {Why: "健康检查失败"},
		// n3 干脆没上报
	}, t0)

	text := blockerText(b, v.Blockers)
	for _, want := range []string{"n2", "健康检查失败", "n3", "has not reported in yet"} {
		if !strings.Contains(text, want) {
			t.Errorf("报错里缺 %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "n1") {
		t.Errorf("没问题的机器不该被列出来:\n%s", text)
	}
}

// TestGateStateJudgesIndependently 钉住门禁**自己判**，不搭 Converged 的便车。
//
// `InstanceView.Converged` 服务的是 `component status`，它的定义会为了显示
// 需要而调整（第 7 步就调过一次）。门禁跟着它走的话，下一次调整会悄悄改掉
// 滚动升级的判据。
func TestGateStateJudgesIndependently(t *testing.T) {
	cases := []struct {
		name string
		iv   InstanceView
		pass bool
		why  string
	}{
		{"没上报", InstanceView{Want: "a"}, false, "has not reported in yet"},
		{"在排队", InstanceView{Want: "a", Got: "a", PendingVersion: "1.2.0"},
			false, "not its turn yet"},
		{"digest 对不上", InstanceView{Want: "a", Got: "b"}, false, "digest"},
		{"不健康", InstanceView{Want: "a", Got: "a", Health: "unhealthy"},
			false, "health check failed"},
		{"调和失败", InstanceView{Want: "a", Got: "a", Result: "failed"},
			false, "reconciliation failed"},
		// 没声明健康探针的 Pack 拿不到任何健康信号——要求 healthy 会让
		// 它们永远卡在门口
		{"没探针也放行", InstanceView{Want: "a", Got: "a", Result: "ok"}, true, ""},
		{"健康", InstanceView{Want: "a", Got: "a", Health: "healthy", Result: "ok"},
			true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := gateStateOf(c.iv)
			if st.Converged != c.pass {
				t.Fatalf("通过 = %v，期望 %v（原因 %q）", st.Converged, c.pass, st.Why)
			}
			if c.why != "" && !strings.Contains(st.Why, c.why) {
				t.Errorf("原因 = %q，应当含 %q", st.Why, c.why)
			}
		})
	}
}

// TestBatchTimeoutCountsFromRelease 钉住超时从**放行**起算。
//
// 从 Rollout 开始起算的话，一次 4 批的升级合法地要花 4×（物化 + 稳定窗口），
// 后面几批必然被误判失败。
func TestBatchTimeoutCountsFromRelease(t *testing.T) {
	b := releasedBatch(target(1, "n1"))
	b.ReleasedAt = t0.Add(30 * time.Minute) // Rollout 早就开始了，这批刚放行

	if timedOut(b, t0.Add(30*time.Minute+BatchTimeout-time.Second)) {
		t.Error("从放行起算还没到期，不该判超时")
	}
	if !timedOut(b, t0.Add(30*time.Minute+BatchTimeout+time.Second)) {
		t.Error("从放行起算已经到期，应当判超时")
	}
}

// TestCrashLoopNeverApprovesTheNextBatch 是第 8 步在验收表上的那一行。
//
// 「崩溃循环的机器不会批准下一批」——这条走完整条链路：真的组件、真的
// 批次、真的上报，只有崩溃是造出来的。
func TestCrashLoopNeverApprovesTheNextBatch(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")
	stageZKVersion(t, f, "9.9.9")
	comp := upgradeZK(t, f, "9.9.9")

	f.svc.AdvanceRollout(ctx(), comp) // 放行第 1 批
	first := batchAt(t, f, comp, 1)

	// 第一批那台一直在崩：每一轮都「收敛且健康」，但重启次数在涨。
	//
	// **这正是稳定窗口单独挡不住的那一类**：每次采样看到的都是一个
	// 刚起来、还没崩的健康进程。
	restarts := 0
	for i := 0; i < 20; i++ {
		restarts++
		reportCrashing(t, f, comp, first, restarts)
		f.clock.advance(StableFor + time.Second) // 每轮都远超稳定窗口
		f.svc.AdvanceRollout(ctx(), comp)

		if seq := releasedSeq(t, f, comp); seq > 1 {
			t.Fatalf("第 %d 轮就放行了第 %d 批——崩溃循环批准了下一批", i, seq)
		}
	}

	// 攒够 BatchTimeout 之后判失败，并**指名道姓**
	v, err := f.svc.RolloutStatus(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	// **halted 而不是 failed**：它有前路——修好机器之后 resume 从这一批
	// 续做（22-multi-node §2.6）。
	if v.State != store.RolloutHalted {
		t.Fatalf("崩溃循环超时之后应当停下来等人（halted），实际 %s", v.State)
	}
	for _, want := range []string{first.Targets[0].Node, "restarted"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("失败原因该说清是哪台、卡在哪一条，缺 %q:\n%s", want, v.Reason)
		}
	}

	// 而且后面两批**一台都没动过**。
	//
	// 这一条抓住过一个真缺陷：失败之后 Rollout 不再是 active，门禁跟着
	// 消失，剩下两批在下一次心跳里**一起升上去**——正好是这道门禁存在的
	// 全部理由的反面。
	if n := countAt(assignedVersions(t, f, "n1", "n2", "n3"), "9.9.9"); n != 1 {
		t.Errorf("失败之后只该有第一批那 1 台在新版，实际 %d 台", n)
	}

	// 混合版本要**说出来**（§2.6）：已完成的批次不自动回滚，因此集群一定
	// 是混着的，而不说的话运维得一台台机器去看。
	if len(v.Mixed) != 2 {
		t.Fatalf("失败之后应当列出新旧两边的分布，实际 %v", v.Mixed)
	}
	if got := v.Mixed["9.9.9"]; len(got) != 1 || got[0] != first.Targets[0].Node {
		t.Errorf("新版那一边应当只有第一批那台，实际 %v", got)
	}
	if got := v.Mixed[v.From]; len(got) != 2 {
		t.Errorf("旧版（%s）那一边应当有两台，实际 %v", v.From, got)
	}
}

// reportCrashing 替一批的目标上报「收敛且健康，但重启次数在涨」。
func reportCrashing(
	t *testing.T, f *fixture, comp store.Component, b store.RolloutBatch, restarts int,
) {
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
			InstanceID: tg.InstanceID, Digest: want[tg.InstanceID], Result: "ok",
			WorkloadState: "running", Health: "healthy",
			Restarts: restarts, Generation: 1, ReportedAt: f.svc.now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestBatchWithoutReleaseTimeNeverTimesOut 钉住记账坏了不当成事故。
//
// 没记住放行时刻说明写盘那一步坏了。判它超时等于把一个记账问题变成一次
// 生产事故——它该停在那里等人看，而 `rollout status` 的「第 N/M」停着不动
// 本身就是看得见的。
func TestBatchWithoutReleaseTimeNeverTimesOut(t *testing.T) {
	b := releasedBatch(target(1, "n1")) // ReleasedAt 是零值
	if timedOut(b, t0.Add(100*BatchTimeout)) {
		t.Error("没有放行时刻的批次不该被判超时")
	}
}
