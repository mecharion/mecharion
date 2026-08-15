package mechd

import (
	"strings"
	"testing"
	"time"

	"github.com/mecharion/mecharion/internal/store"
)

// 本文件钉住 **M7 第 9 步**：失败即暂停 + `resume` 断点续做。
//
// 「断点续做」这四个字里，**「不重做」是难的那一半**。一个「resume 就重新
// 分批、从头再来」的实现能通过「最后三台都在新版」这种终态断言——而它把
// 已经稳住的机器又动了一遍，正好是运维在故障处理中最不想看到的事。

// haltAtFirstBatch 把 Rollout 推到「第 1 批没过门禁、已停下」的状态。
func haltAtFirstBatch(t *testing.T, f *fixture) (store.Component, store.RolloutBatch) {
	t.Helper()
	comp := upgradeZK(t, f, "9.9.9")

	f.svc.AdvanceRollout(ctx(), comp) // 放行第 1 批
	first := batchAt(t, f, comp, 1)

	// 一直崩到撞上 BatchTimeout
	for i := 0; i < 20; i++ {
		reportCrashing(t, f, comp, first, i+1)
		f.clock.advance(StableFor + time.Second)
		f.svc.AdvanceRollout(ctx(), comp)
	}
	v, err := f.svc.RolloutStatus(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	if v.State != store.RolloutHalted {
		t.Fatalf("前置条件没建立起来：期望 halted，实际 %s（%s）", v.State, v.Reason)
	}
	return comp, first
}

// TestHaltedNotFailed 钉住「失败即暂停」而不是「失败即完结」。
//
// 状态词不是措辞问题：`failed` 读起来是终态，脚本会照着它判「这次升级
// 完了」。而这里真正的情况是**停下来等人**，它有前路。
func TestHaltedNotFailed(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")
	stageZKVersion(t, f, "9.9.9")
	_, _ = haltAtFirstBatch(t, f)

	v, err := f.svc.RolloutStatus(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	// **没有结束时间**：这次变更还没收场，只是停着。写了的话 history 里
	// 它看起来就像已经完了。
	if v.EndedAt != "" {
		t.Errorf("停下来等人的变更不该有结束时间，实际 %q", v.EndedAt)
	}
	// 停在哪、卡在哪台，要摆在最显眼的位置
	if !strings.Contains(v.Reason, "n1") {
		t.Errorf("原因里该指名是哪台，实际 %q", v.Reason)
	}
	if v.Batch != 1 {
		t.Errorf("应当停在第 1 批，实际第 %d 批", v.Batch)
	}
}

// TestPauseRefusedWhenHalted 钉住 halted 时 pause 被拒。
//
// 它已经停了，pause 什么都不会改变——却会把「为什么停的」那句原因冲掉，
// 而那正是运维此刻唯一需要的信息。
func TestPauseRefusedWhenHalted(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")
	stageZKVersion(t, f, "9.9.9")
	haltAtFirstBatch(t, f)

	before, err := f.svc.RolloutStatus(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}

	_, err = f.svc.SetRolloutPaused(ctx(), "", "zk-main", true, "tester")
	if err == nil {
		t.Fatal("已经因故障停下时，pause 应当被拒绝")
	}
	for _, want := range []string{"resume", "abort"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("拒绝时该告诉他能做什么，缺 %q：%v", want, err)
		}
	}

	// 原因没被冲掉
	after, err := f.svc.RolloutStatus(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	if after.Reason != before.Reason {
		t.Errorf("失败原因被冲掉了：%q → %q", before.Reason, after.Reason)
	}
}

// TestResumePicksUpFromTheFailedBatch 是第 9 步的核心验收。
//
// **不重做已完成的批次**：resume 之后，已经 done 的批次仍然是 done，
// 而没过门禁的那一批重新等门禁。
func TestResumePicksUpFromTheFailedBatch(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")
	stageZKVersion(t, f, "9.9.9")
	comp := upgradeZK(t, f, "9.9.9")

	// 第 1 批正常做完
	f.svc.AdvanceRollout(ctx(), comp) // 放行第 1 批
	passCurrentBatch(t, f, comp)
	if seq := releasedSeq(t, f, comp); seq != 2 {
		t.Fatalf("第 1 批该做完并放行第 2 批，实际第 %d 批", seq)
	}

	// 第 2 批一直崩，直到停下
	second := batchAt(t, f, comp, 2)
	for i := 0; i < 20; i++ {
		reportCrashing(t, f, comp, second, i+1)
		f.clock.advance(StableFor + time.Second)
		f.svc.AdvanceRollout(ctx(), comp)
	}
	if v, _ := f.svc.RolloutStatus(ctx(), "", "zk-main"); v.State != store.RolloutHalted {
		t.Fatalf("第 2 批应当把变更停下来，实际 %s", v.State)
	}

	// ── resume ──
	if _, err := f.svc.SetRolloutPaused(ctx(), "", "zk-main", false, "tester"); err != nil {
		t.Fatalf("resume 应当成功: %v", err)
	}

	// **第 1 批仍然是 done**，一步都没退回去
	if got := batchAt(t, f, comp, 1).State; got != store.BatchDone {
		t.Errorf("已完成的第 1 批不该被重做，实际状态 %s", got)
	}
	// 第 2 批回到「正在等它」
	if seq := releasedSeq(t, f, comp); seq != 2 {
		t.Fatalf("resume 应当从第 2 批续做，实际第 %d 批", seq)
	}
	// 门禁窗口清零了：故障之前攒的时间不算数
	if got := batchAt(t, f, comp, 2); !got.HealthySince.IsZero() {
		t.Errorf("续做时门禁窗口应当清零，实际 %v", got.HealthySince)
	}

	// 修好之后走完
	passCurrentBatch(t, f, comp)
	passCurrentBatch(t, f, comp)
	v, err := f.svc.RolloutStatus(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	if v.State != store.RolloutSucceeded {
		t.Fatalf("续做完应当成功，实际 %s（第 %d/%d 批，%s）",
			v.State, v.Batch, v.Batches, v.Reason)
	}
}

// TestResumeRestartsTheBatchClock 钉住 resume 重置批次超时。
//
// 不重置的话，resume 之后第一次判定就会立刻再次超时——因为 `ReleasedAt`
// 还停在故障之前。运维会看到「刚 resume 就又停了」，而机器明明是好的。
func TestResumeRestartsTheBatchClock(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")
	stageZKVersion(t, f, "9.9.9")
	comp, first := haltAtFirstBatch(t, f)

	haltedAt := batchAt(t, f, comp, 1).ReleasedAt
	if _, err := f.svc.SetRolloutPaused(ctx(), "", "zk-main", false, "tester"); err != nil {
		t.Fatal(err)
	}
	if got := batchAt(t, f, comp, 1).ReleasedAt; !got.After(haltedAt) {
		t.Fatalf("续做时放行时刻应当重置，实际还是 %v", got)
	}

	// 机器好了：走一个完整窗口就该过，而**不是**立刻又超时
	reportConverged(t, f, comp, first)
	f.svc.AdvanceRollout(ctx(), comp)
	f.clock.advance(StableFor)
	reportConverged(t, f, comp, first)
	f.svc.AdvanceRollout(ctx(), comp)

	v, err := f.svc.RolloutStatus(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	if v.State == store.RolloutHalted {
		t.Fatalf("刚 resume 就又停了——批次时钟没有重置：%s", v.Reason)
	}
	if seq := releasedSeq(t, f, comp); seq != 2 {
		t.Errorf("修好之后应当放行第 2 批，实际第 %d 批", seq)
	}
}

// TestHaltedStopsDeliveryToLaterBatches 钉住停下之后**后面的批次不许动**。
//
// 这是「一个失败则整体暂停」里「整体」那两个字的全部含义。
func TestHaltedStopsDeliveryToLaterBatches(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")
	before := assignedVersions(t, f, "n1", "n2", "n3")
	stageZKVersion(t, f, "9.9.9")
	comp, first := haltAtFirstBatch(t, f)

	// 停下之后再怎么上报，也不该有第二台拿到新版
	for i := 0; i < 10; i++ {
		reportAllConverged(t, f, comp)
		f.clock.advance(StableFor + time.Second)
		f.svc.AdvanceRollout(ctx(), comp)
	}
	got := assignedVersions(t, f, "n1", "n2", "n3")
	if n := countAt(got, "9.9.9"); n != 1 {
		t.Fatalf("停下之后只该有第 1 批那台在新版，实际 %d 台：%v", n, got)
	}
	failed := first.Targets[0].Node
	for n, v := range got {
		if n != failed && v != before[n] {
			t.Errorf("%s 不在失败那一批里，不该被动过：%s → %s", n, before[n], v)
		}
	}
}

// TestHaltedSurvivesNodeAutoRollback 钉住停下之后 mechd **什么都不做**。
//
// 场景很常见：门禁把变更停下，节点侧随后对那个起不来的 generation 自动
// 回滚（M6 的节点自治）。若 mechd 还在照常判定，下一次上报就会把 halted
// 冲成 failed——**连同那句指名道姓的原因、以及续做的可能一起没了**。
//
// 「停下来等人」得是真的停下来。
func TestHaltedSurvivesNodeAutoRollback(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")
	stageZKVersion(t, f, "9.9.9")
	comp, first := haltAtFirstBatch(t, f)

	before, err := f.svc.RolloutStatus(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}

	// 节点把那一版回滚掉了，并如实上报
	reportRolledBack(t, f, comp, first)
	for i := 0; i < 3; i++ {
		f.svc.AdvanceRollout(ctx(), comp)
	}

	after, err := f.svc.RolloutStatus(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	if after.State != store.RolloutHalted {
		t.Fatalf("停下之后不该再自己改状态，实际 %s → %s", before.State, after.State)
	}
	if after.Reason != before.Reason {
		t.Errorf("指名道姓的原因被冲掉了：%q → %q", before.Reason, after.Reason)
	}
	// 还能续做
	if _, err := f.svc.SetRolloutPaused(ctx(), "", "zk-main", false, "tester"); err != nil {
		t.Errorf("自动回滚之后仍应能 resume: %v", err)
	}
}

// TestResumeClearsAPartiallyFilledWindow 钉住 resume 清掉**半满的**门禁窗口。
//
// 绝大多数情况下停下时窗口已经是零（机器抖了一下就被清掉）。但有一条窄
// 路径能让它非零：机器抖到最后一刻才稳下来，窗口刚开就撞上批次超时。
// 那时若不清零，续做只需要补齐剩下的几秒——**故障之前攒的时间被算数了**，
// 而稳定窗口的全部意义就是「修好之后重新稳住这么久」。
func TestResumeClearsAPartiallyFilledWindow(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")
	stageZKVersion(t, f, "9.9.9")
	comp := upgradeZK(t, f, "9.9.9")

	f.svc.AdvanceRollout(ctx(), comp) // 放行第 1 批
	first := batchAt(t, f, comp, 1)

	// **把时钟对准那条缝**：窗口开起来之后不到 StableFor 就撞上批次超时。
	// 缝只有 30 秒宽，靠模拟抖动去碰它既慢又不确定。
	f.clock.advance(BatchTimeout - 10*time.Second)
	reportConverged(t, f, comp, first)
	f.svc.AdvanceRollout(ctx(), comp) // 窗口在这一刻开
	if batchAt(t, f, comp, 1).HealthySince.IsZero() {
		t.Fatal("前置条件没建立起来：窗口应当已经开了")
	}

	// 再走 15 秒：超过了批次超时，但窗口才攒了 15 秒（< StableFor）
	f.clock.advance(15 * time.Second)
	reportConverged(t, f, comp, first)
	f.svc.AdvanceRollout(ctx(), comp)

	halted := batchAt(t, f, comp, 1)
	if halted.State != store.BatchFailed {
		t.Fatalf("应当因批次超时停下，实际批次状态 %s", halted.State)
	}
	if halted.HealthySince.IsZero() {
		t.Fatal("这条测试要的正是「窗口非零时停下」，前置条件没建立起来")
	}

	if _, err := f.svc.SetRolloutPaused(ctx(), "", "zk-main", false, "tester"); err != nil {
		t.Fatal(err)
	}
	if got := batchAt(t, f, comp, 1).HealthySince; !got.IsZero() {
		t.Errorf("续做时半满的窗口也必须清零，实际还留着 %v", got)
	}
}

// reportRolledBack 替一批的目标上报「节点把这一版回滚掉了」。
func reportRolledBack(
	t *testing.T, f *fixture, comp store.Component, b store.RolloutBatch,
) {
	t.Helper()
	for _, tg := range b.Targets {
		if err := f.svc.Repos.Status().Put(ctx(), store.InstanceStatus{
			InstanceID: tg.InstanceID, Digest: "旧版的-digest", Result: "ok",
			WorkloadState: "running", Health: "healthy",
			RolledBackFrom: "新版的-digest",
			Generation:     1, ReportedAt: f.svc.now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAbortWorksWhenHalted 钉住 halted 时还能整体退回。
//
// 那是 §2.6 给「不自动回滚已完成批次」留的出口：想整体退回的用户有现成的
// 路，只是需要人点一下。堵死这条路的话，混合版本就没法收场了。
func TestAbortWorksWhenHalted(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")
	stageZKVersion(t, f, "9.9.9")
	haltAtFirstBatch(t, f)

	v, err := f.svc.AbortRollout(ctx(), "", "zk-main", "tester")
	if err != nil {
		t.Fatalf("因故障停下时应当还能 abort: %v", err)
	}
	if v.State != store.RolloutAborted {
		t.Errorf("abort 之后应当是 aborted，实际 %s", v.State)
	}
	comp, err := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	if comp.Pack.Version == "9.9.9" {
		t.Error("abort 应当把组件版本退回起始版本")
	}
}
