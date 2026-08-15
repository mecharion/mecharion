package mechd

import (
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/protocol"
	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/store"
)

// startRemoving 把夹具里的 paramkit 置为 removing，返回它。
func startRemoving(t *testing.T, f *fixture, rm store.ComponentRemoval) store.Component {
	t.Helper()
	comp, err := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "paramkit")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Repos.Components().StartRemoval(
		ctx(), comp.ID, rm, f.svc.now()); err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "paramkit")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// reportRemoved 模拟一个节点报告「这个实例拆干净了」。
func reportRemoved(t *testing.T, f *fixture, node, role string, retained ...string) {
	t.Helper()
	b := &Backend{S: f.svc}
	err := b.Report(ctx(), protocol.Report{
		Node: node,
		Instances: []protocol.InstanceStatus{{
			Component: "paramkit", Role: role, Result: "changed",
			Removed: true, RetainedPaths: retained,
		}},
	})
	if err != nil {
		t.Fatalf("上报失败: %v", err)
	}
}

func componentExists(t *testing.T, f *fixture, name string) bool {
	t.Helper()
	_, err := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, name)
	return err == nil
}

// ── 状态与下发 ──────────────────────────────────────────────────────────

// TestRemovingDeliversRemovedRunState 是这一步与第 1 步的接缝。
//
// 第 1 步做出了「收到 removed 就卸载」的节点侧，但**中心发不出来**。
// 这条测的正是那一段：Component 一进 removing，下发里的 runState 就得变。
func TestRemovingDeliversRemovedRunState(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)
	startRemoving(t, f, store.ComponentRemoval{PurgeData: true})

	b := &Backend{S: f.svc}
	specs, err := b.Assignment(ctx(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) == 0 {
		t.Fatal("下发里一个实例都没有，这条测试无从证伪")
	}
	for _, in := range specs {
		if in.Spec.RunState != spec.RunStateRemoved {
			t.Errorf("%s 的 runState = %q，期望 removed",
				in.Spec.InstanceKey(), in.Spec.RunState)
		}
		// 开关必须跟着下去：mechlet 不做判断，它只照规格办
		if in.Spec.Removal == nil {
			t.Fatalf("%s 没带 removal 开关，节点无从知道数据该不该删",
				in.Spec.InstanceKey())
		}
		if !in.Spec.Removal.PurgeData {
			t.Errorf("%s 的 purgeData 丢了：命令行说了要删数据，下发里却没有",
				in.Spec.InstanceKey())
		}
	}
}

// TestRemovingOverridesPerInstanceStopped 守一个边角。
//
// remove 是**组件级**的决定：一个先前被 `component stop` 停掉的实例
// 同样要被拆掉。若逐实例的 runState 盖过组件状态，那个实例会永远停在
// stopped，而整个组件因此永远卡在 removing。
func TestRemovingOverridesPerInstanceStopped(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)
	if _, err := f.svc.SetRunState(ctx(), SetRunStateRequest{
		Component: "paramkit", State: spec.RunStateStopped, Actor: "test",
	}); err != nil {
		t.Fatal(err)
	}
	startRemoving(t, f, store.ComponentRemoval{})

	b := &Backend{S: f.svc}
	specs, err := b.Assignment(ctx(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range specs {
		if in.Spec.RunState != spec.RunStateRemoved {
			t.Errorf("被 stop 过的实例也必须收到 removed，实际 %q", in.Spec.RunState)
		}
	}
}

// ── 记录清理 ────────────────────────────────────────────────────────────

// TestRecordSurvivesUntilAllInstancesReport 是 §2.2 的核心判据。
//
// **记录不能在下发那一刻就删。** 删了之后那个实例就不在下发里了，节点
// 再也收不到「这个实例不该存在」——它会变成孤儿，而孤儿永不自动删。
// 一次「删除」于是变成「机器上永远留着一个没人管的服务」。
func TestRecordSurvivesUntilAllInstancesReport(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil) // paramkit 的 main 角色在 n1、n2 两台
	startRemoving(t, f, store.ComponentRemoval{})

	if !componentExists(t, f, "paramkit") {
		t.Fatal("刚进 removing 就把记录删了")
	}

	reportRemoved(t, f, "n1", "main")
	if !componentExists(t, f, "paramkit") {
		t.Fatal("只有 n1 报了拆完，n2 还没——记录不该消失")
	}

	reportRemoved(t, f, "n2", "main")
	if componentExists(t, f, "paramkit") {
		t.Error("两台都报完了，记录应当消失")
	}
}

// TestProgressNamesThePendingInstances：只给数字是不够的。
//
// 一个停在 1/2 的移除，运维要问的是「哪一台」——而那一台通常正是
// 失联的那台，也正是决定要不要 --force 的依据。
func TestProgressNamesThePendingInstances(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)
	comp := startRemoving(t, f, store.ComponentRemoval{})
	reportRemoved(t, f, "n1", "main")

	p, err := f.svc.removalProgress(ctx(), comp)
	if err != nil {
		t.Fatal(err)
	}
	if p.Done != 1 || p.Total != 2 {
		t.Errorf("进度 = %d/%d，期望 1/2", p.Done, p.Total)
	}
	if len(p.Pending) != 1 || !strings.Contains(p.Pending[0], "n2") {
		t.Errorf("未完成的应当是 n2，实际 %v", p.Pending)
	}
}

// TestRetainedPathsAreCollected：删完之前要说得清会留下什么。
func TestRetainedPathsAreCollected(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)
	comp := startRemoving(t, f, store.ComponentRemoval{})
	reportRemoved(t, f, "n1", "main", "/var/lib/mecharion/apps/paramkit")

	p, err := f.svc.removalProgress(ctx(), comp)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Retained) != 1 || p.Retained[0] != "/var/lib/mecharion/apps/paramkit" {
		t.Errorf("保留目录没收上来: %v", p.Retained)
	}
}

// TestNeverReportedIsNotDone 守一个会让删除「看起来完成了」的错误。
//
// 一台从没连上来的机器与一台报告拆完的机器，在库里的区别就只有那条
// 状态记录。把「没有记录」当成「拆完了」，一个失联节点上的服务会被
// 静默遗忘——记录删了，机器上还跑着。
func TestNeverReportedIsNotDone(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)
	comp := startRemoving(t, f, store.ComponentRemoval{})

	p, err := f.svc.removalProgress(ctx(), comp)
	if err != nil {
		t.Fatal(err)
	}
	if p.Done != 0 {
		t.Errorf("一台都没上报过，Done 应当是 0，实际 %d", p.Done)
	}
	if p.Complete() {
		t.Error("更不该判定为完成")
	}
}

// ── 写操作闸门 ──────────────────────────────────────────────────────────

// TestRemovingRefusesWrites 是验收表第 6 条。
//
// **一个正在被删的东西不该还能改。** 允许的话会出现「配置改了，但那台
// 已经拆完的机器上没有」这种事后完全解释不清的状态。
//
// 逐个动词地测，而不是只测一个：闸门是加在「取记录」上的，但每个动词
// 走的路不完全相同，漏掉的那一处不会有任何症状——直到有人真的用它。
func TestRemovingRefusesWrites(t *testing.T) {
	for _, tc := range []struct {
		verb string
		call func(f *fixture) error
	}{
		{"config set", func(f *fixture) error {
			_, err := f.svc.SetParams(ctx(), SetParamsRequest{
				Component: "paramkit", Set: map[string]any{"p_string": "x"},
				Actor: "test",
			})
			return err
		}},
		{"deploy --update", func(f *fixture) error {
			_, err := f.svc.Deploy(ctx(), DeployRequest{
				Pack: "paramkit", Roles: map[string][]string{"main": {"n1", "n2"}},
				Update: true, Actor: "test",
			})
			return err
		}},
		{"stop", func(f *fixture) error {
			_, err := f.svc.SetRunState(ctx(), SetRunStateRequest{
				Component: "paramkit", State: spec.RunStateStopped, Actor: "test",
			})
			return err
		}},
		{"set-drift-policy", func(f *fixture) error {
			return f.svc.SetDriftPolicy(ctx(), SetDriftPolicyRequest{
				Component: "paramkit", Policy: "ignore", Actor: "test",
			})
		}},
		{"set-rollout", func(f *fixture) error {
			three := 3
			_, err := f.svc.SetRolloutPolicy(ctx(), "", "paramkit", &three, nil, "test")
			return err
		}},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			f := formFixture(t)
			deployKit(t, f, nil)

			// 先证明这个动词在正常状态下是通的，否则下面的断言只是
			// 「它本来就会失败」
			if err := tc.call(f); err != nil {
				t.Fatalf("%s 在 active 状态下就失败了，这条测试无从证伪: %v", tc.verb, err)
			}

			startRemoving(t, f, store.ComponentRemoval{})

			err := tc.call(f)
			if err == nil {
				t.Fatalf("%s 在 removing 期间必须被拒绝", tc.verb)
			}
			if !strings.Contains(err.Error(), "is being removed") {
				t.Errorf("%s 的拒绝理由要说清楚，得到: %v", tc.verb, err)
			}
		})
	}
}

// TestRemovingStillAllowsReads：拦的是写，不是读。
//
// 运维正想看这个组件还剩什么——那恰恰是最需要能看的时候。
func TestRemovingStillAllowsReads(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)
	startRemoving(t, f, store.ComponentRemoval{})

	if _, err := f.svc.ListGroups(ctx(), "", "paramkit", "main"); err != nil {
		t.Errorf("配置组列表应当照常可读: %v", err)
	}
}

// TestDeletingTheRecordWakesTheNodes 守一条**沉默的**失败。
//
// 记录一删，节点就该收到一次不含这个实例的全量下发——那一次才会让它
// 清掉本地的期望状态，并把留下的收据报成孤儿。
//
// 不唤醒的话没有任何东西会报错：节点手里还攥着那份「runState: removed」
// 的期望，于是每 60 秒重新卸载一遍一个早就拆干净的实例，而
// `orphans list` 永远是空的——保留下来的数据因此永远无人认领。
//
// **这条是在三台真机的验收里发现的**，而不是被单元测试逮到的。当时
// 这一层的测试全绿：它们验的是「记录没了」，而没有一条问过「节点知不
// 知道」。补上它，同一类遗漏下次就不必再靠真机来发现。
func TestDeletingTheRecordWakesTheNodes(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil) // main 角色在 n1、n2
	startRemoving(t, f, store.ComponentRemoval{})

	f.notify.woken = nil // 只看删除那一刻唤醒了谁

	reportRemoved(t, f, "n1", "main")
	reportRemoved(t, f, "n2", "main") // 这一下会触发收尾

	if componentExists(t, f, "paramkit") {
		t.Fatal("两台都报完了，记录应当已经删掉")
	}
	for _, n := range []string{"n1", "n2"} {
		if !f.notify.wokeUp(n) {
			t.Errorf("%s 没被唤醒——它会一直攥着旧的期望状态，"+
				"每 60 秒重新卸载一遍，而孤儿永远报不上来。实际唤醒了 %v",
				n, f.notify.woken)
		}
	}
}

// TestForceAlsoWakesTheNodes：--force 那条路同样要唤醒。
//
// 它更要紧——被 --force 跳过的节点正是**还没拆完**的那些，而它们回来
// 之后要靠这次全量才知道「这个组件没了」。
func TestForceAlsoWakesTheNodes(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)
	if _, err := remove(f, RemoveRequest{}); err != nil {
		t.Fatal(err)
	}
	f.notify.woken = nil

	if _, err := remove(f, RemoveRequest{Force: true}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"n1", "n2"} {
		if !f.notify.wokeUp(n) {
			t.Errorf("--force 之后 %s 也要被唤醒，实际 %v", n, f.notify.woken)
		}
	}
}
