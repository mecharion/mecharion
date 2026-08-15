package mechd

import (
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/store"
)

// targets 造 n 个实例，ordinal 0..n-1，节点名 n0..n{n-1}。
func targets(role string, n int) []store.BatchTarget {
	out := make([]store.BatchTarget, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, store.BatchTarget{
			InstanceID: int64(i + 1), Role: role,
			Node: "n" + string(rune('0'+i)), Ordinal: i,
		})
	}
	return out
}

// describe 把批次压成一行，方便断言。
func describe(bs []store.RolloutBatch) string {
	var parts []string
	for _, b := range bs {
		var nodes []string
		for _, t := range b.Targets {
			nodes = append(nodes, t.Node)
		}
		parts = append(parts, b.Role+":"+strings.Join(nodes, "+"))
	}
	return strings.Join(parts, " | ")
}

// TestCanaryGoesFirst 钉住首批只动一台。
//
// 绝大多数「新版本根本起不来」的问题在第一台上就暴露了，而那时受影响的
// 面最小——这是 canary 默认为 1 的全部理由。
func TestCanaryGoesFirst(t *testing.T) {
	bs, _, err := planBatches(batchInput{
		Roles: map[string][]store.BatchTarget{"web": targets("web", 5)},
		Order: []string{"web"}, MaxUnavailable: 2, Canary: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "web:n0 | web:n1+n2 | web:n3+n4"
	if got := describe(bs); got != want {
		t.Errorf("分批 = %q，期望 %q", got, want)
	}
}

// TestCanaryZeroDisablesIt 钉住 canary=0 就是不做金丝雀。
func TestCanaryZeroDisablesIt(t *testing.T) {
	bs, _, err := planBatches(batchInput{
		Roles: map[string][]store.BatchTarget{"web": targets("web", 4)},
		Order: []string{"web"}, MaxUnavailable: 2, Canary: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := describe(bs), "web:n0+n1 | web:n2+n3"; got != want {
		t.Errorf("分批 = %q，期望 %q", got, want)
	}
}

// TestQuorumCapsConcurrency 是这一步最重要的一条。
//
// **仲裁语义只有 Pack 作者知道**——用户设 maxUnavailable 时无从判断某个
// 组件能同时下线几个。没有这条约束，一次「并发度 2」的滚动重启就能让
// 3 节点的 ZooKeeper 失去多数派（14-placement §5）。
func TestQuorumCapsConcurrency(t *testing.T) {
	bs, _, err := planBatches(batchInput{
		Roles:  map[string][]store.BatchTarget{"zk": targets("zk", 3)},
		Order:  []string{"zk"},
		Quorum: map[string]bool{"zk": true},
		// 用户要 3 并发——对 3 节点仲裁来说那是一次全灭
		MaxUnavailable: 3, Canary: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	// (3-1)/2 = 1，因此每批只能动一台
	if got, want := describe(bs), "zk:n0 | zk:n1 | zk:n2"; got != want {
		t.Errorf("quorum 角色应当被限到每批 1 台，实际 %q", got)
	}
}

// TestQuorumCountsOnlyEligibleTargets 钉住上限按**参与分批的**实例数算。
//
// 被 cordon 的那几台本来就不会动，把它们计进分母只会让并发度虚高——
// 5 台里 cordon 掉 2 台，真正在动的是 3 台，上限就该按 3 台算。
func TestQuorumCountsOnlyEligibleTargets(t *testing.T) {
	bs, skipped, err := planBatches(batchInput{
		Roles:    map[string][]store.BatchTarget{"zk": targets("zk", 5)},
		Order:    []string{"zk"},
		Quorum:   map[string]bool{"zk": true},
		Cordoned: map[string]bool{"n3": true, "n4": true},
		// 5 台时 (5-1)/2 = 2；但只有 3 台参与，应当按 (3-1)/2 = 1
		MaxUnavailable: 5, Canary: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := describe(bs), "zk:n0 | zk:n1 | zk:n2"; got != want {
		t.Errorf("应当按参与分批的 3 台算上限，实际 %q", got)
	}
	if len(skipped) != 2 {
		t.Errorf("被 cordon 的 2 台应当单独列出，实际 %d 台", len(skipped))
	}
}

// TestCordonedNodesNeverEnterBatches 钉住 §2.7 的那条。
//
// cordon 是**人明确说过**「别动这台」。但它们必须被单独列出来——
// 不列的话，「为什么这台还是旧版」会变成一次排查。
func TestCordonedNodesNeverEnterBatches(t *testing.T) {
	bs, skipped, err := planBatches(batchInput{
		Roles:          map[string][]store.BatchTarget{"web": targets("web", 3)},
		Order:          []string{"web"},
		Cordoned:       map[string]bool{"n1": true},
		MaxUnavailable: 1, Canary: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := describe(bs), "web:n0 | web:n2"; got != want {
		t.Errorf("被 cordon 的节点不该进批次，实际 %q", got)
	}
	if len(skipped) != 1 || skipped[0].Node != "n1" {
		t.Errorf("跳过的应当是 n1，实际 %+v", skipped)
	}
}

// TestAllCordonedYieldsNoBatch 钉住「整个角色都被 cordon 了」不会造出空批。
//
// 一个空批次会永远等不到收敛——它没有任何目标可以上报。
func TestAllCordonedYieldsNoBatch(t *testing.T) {
	bs, skipped, err := planBatches(batchInput{
		Roles:          map[string][]store.BatchTarget{"web": targets("web", 2)},
		Order:          []string{"web"},
		Cordoned:       map[string]bool{"n0": true, "n1": true},
		MaxUnavailable: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 0 {
		t.Errorf("不该造出空批次，实际 %q", describe(bs))
	}
	if len(skipped) != 2 {
		t.Errorf("两台都该被列为跳过，实际 %d", len(skipped))
	}
}

// TestStagesFollowRequires 钉住阶段按 requires 拓扑序：被依赖者先升。
func TestStagesFollowRequires(t *testing.T) {
	bs, _, err := planBatches(batchInput{
		Roles: map[string][]store.BatchTarget{
			"broker": targets("broker", 1),
			"zk":     targets("zk", 1),
		},
		Order: []string{"zk", "broker"}, MaxUnavailable: 1, Canary: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := describe(bs), "zk:n0 | broker:n0"; got != want {
		t.Errorf("应当先升被依赖的 zk，实际 %q", got)
	}
	if bs[0].Stage != 1 || bs[1].Stage != 2 {
		t.Errorf("两个角色应当在两个阶段，实际 %d / %d", bs[0].Stage, bs[1].Stage)
	}
	// 全局序号跨阶段连续——`rollout status` 的「第 2/4 批」分母是全局的
	if bs[0].Seq != 1 || bs[1].Seq != 2 {
		t.Errorf("序号应当跨阶段连续，实际 %d / %d", bs[0].Seq, bs[1].Seq)
	}
}

// TestBatchOrderIsStable 钉住批内按 ordinal 升序。
//
// 稳定顺序让「上次停在第几台」这个问题有答案；随机顺序会让同一次故障
// 每次复现在不同机器上。
func TestBatchOrderIsStable(t *testing.T) {
	shuffled := []store.BatchTarget{
		{Role: "web", Node: "c", Ordinal: 2},
		{Role: "web", Node: "a", Ordinal: 0},
		{Role: "web", Node: "b", Ordinal: 1},
	}
	bs, _, err := planBatches(batchInput{
		Roles: map[string][]store.BatchTarget{"web": shuffled},
		Order: []string{"web"}, MaxUnavailable: 1, Canary: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := describe(bs), "web:a | web:b | web:c"; got != want {
		t.Errorf("应当按 ordinal 升序，实际 %q", got)
	}
}

// TestRoleOrderRespectsRequiresAndProfile 钉住拓扑排序本身。
func TestRoleOrderRespectsRequiresAndProfile(t *testing.T) {
	roles := []pack.EffectiveRole{
		{Name: "broker", Requires: []string{"zk"}, Enabled: true},
		{Name: "zk", Enabled: true},
		{Name: "ui", Requires: []string{"broker"}, Enabled: true},
		{Name: "disabled-in-profile", Enabled: false},
	}
	got := roleOrder(roles)
	want := []string{"zk", "broker", "ui"}
	if len(got) != len(want) {
		t.Fatalf("顺序 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("顺序 = %v，期望 %v", got, want)
		}
	}
}

// TestRoleOrderSurvivesCycle 钉住成环时不崩。
//
// 环由 lint 在打包时拦（spec §7）。多节点升级不该因为一个本该被拒的 Pack
// 而挂掉——那会让一个格式问题变成一次生产事故。
func TestRoleOrderSurvivesCycle(t *testing.T) {
	roles := []pack.EffectiveRole{
		{Name: "a", Requires: []string{"b"}, Enabled: true},
		{Name: "b", Requires: []string{"a"}, Enabled: true},
	}
	if got := roleOrder(roles); len(got) != 2 {
		t.Errorf("成环时也该把两个角色都排进去，实际 %v", got)
	}
}
