package mechd

import (
	"errors"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/store"
)

func remove(f *fixture, req RemoveRequest) (*RemoveResult, error) {
	req.Component = "paramkit"
	req.Actor = "test"
	return f.svc.RemoveComponent(ctx(), req)
}

// ── 前置校验 ────────────────────────────────────────────────────────────

// TestRemoveRefusesWhenDependentsExist 是验收表第 4 条。
//
// 一个还被别人依赖着的组件被删掉，依赖者会在下一次调和时拿到一份指向
// 不存在的东西的绑定——而那种失败发生在**依赖者**身上，排查的人会从
// 完全错误的一端开始。
func TestRemoveRefusesWhenDependentsExist(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	// 手工造一条依赖：另一个组件通过 require 槽位绑到 paramkit 上
	comp, err := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "paramkit")
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.svc.Repos.Components().Create(ctx(), store.Component{
		SiteID: f.site.ID, Name: "consumer",
		Pack: store.PackRef{Name: "paramkit", Version: "1.0.0", Revision: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Repos.Bindings().Create(ctx(), store.PackBinding{
		ComponentID: other.ID, RequireName: "kit", BoundComponentID: comp.ID,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = remove(f, RemoveRequest{})
	if err == nil {
		t.Fatal("有依赖者时必须拒绝")
	}
	// **列出是哪几个**：数字答不了「我还需不需要它们」这个唯一要紧的问题
	if !strings.Contains(err.Error(), "consumer") {
		t.Errorf("拒绝时要列出依赖者，得到: %v", err)
	}
	if !strings.Contains(err.Error(), "kit") {
		t.Errorf("还要说清是哪个 require 槽位，得到: %v", err)
	}
	// 拒绝了就不能留下任何痕迹
	if got, _ := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "paramkit"); got.Removing() {
		t.Error("被拒绝的 remove 不该把组件置为 removing")
	}
}

// TestRemoveIgnoreNotFound 是验收表第 11 条：脚本可无脑调用。
func TestRemoveIgnoreNotFound(t *testing.T) {
	f := formFixture(t)

	res, err := f.svc.RemoveComponent(ctx(), RemoveRequest{
		Component: "nope", IgnoreNotFound: true, Actor: "test",
	})
	if err != nil {
		t.Fatalf("--ignore-not-found 应当静默成功: %v", err)
	}
	if !res.NotFound {
		t.Error("结果里要说清它压根不存在")
	}
}

// 不给 --ignore-not-found 时要报错，否则上面那条测试什么也没证明。
func TestRemoveWithoutIgnoreNotFoundFails(t *testing.T) {
	f := formFixture(t)

	_, err := f.svc.RemoveComponent(ctx(), RemoveRequest{
		Component: "nope", Actor: "test",
	})
	if err == nil {
		t.Fatal("默认应当报错")
	}
	if !strings.Contains(err.Error(), "--ignore-not-found") {
		t.Errorf("错误里要给出脚本该怎么写，得到: %v", err)
	}
}

// ── 影响面 ──────────────────────────────────────────────────────────────

// TestDryRunComputesImpactWithoutChangingAnything 是二档确认的地基。
//
// 一个只说「确定要删 pg-main 吗」的提示没有信息量。人要知道的是几台机器、
// 哪些目录会没、哪些会留——而算这些的过程本身**绝不能动任何东西**。
func TestDryRunComputesImpactWithoutChangingAnything(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	res, err := remove(f, RemoveRequest{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	im := res.Impact
	if im.Instances != 2 || len(im.Nodes) != 2 {
		t.Errorf("影响面 = %d 实例 / %v 节点，期望 2 / [n1 n2]", im.Instances, im.Nodes)
	}
	// 两边都要列出来
	if len(im.Deleted) == 0 {
		t.Error("要列出会被删掉的目录")
	}
	if len(im.Retained) == 0 {
		t.Error("要列出会保留的目录——否则人无从知道盘上还剩什么")
	}
	// 干跑不能有任何副作用
	comp, _ := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "paramkit")
	if comp.Removing() {
		t.Fatal("干跑把组件置成 removing 了")
	}
}

// TestPurgeDataMovesDataFromRetainedToDeleted 钉住预览与开关的联动。
//
// 预览若不随开关变化，它就只是一段装饰——而人正是靠它决定要不要按下确认。
func TestPurgeDataMovesDataFromRetainedToDeleted(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	def, err := remove(f, RemoveRequest{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	purge, err := remove(f, RemoveRequest{DryRun: true, PurgeData: true})
	if err != nil {
		t.Fatal(err)
	}

	if len(purge.Impact.Retained) != 0 {
		t.Errorf("--purge-data 之后不该还有保留项: %v", purge.Impact.Retained)
	}
	if len(purge.Impact.Deleted) <= len(def.Impact.Deleted) {
		t.Errorf("--purge-data 应当让删除清单变长: %d → %d",
			len(def.Impact.Deleted), len(purge.Impact.Deleted))
	}
	// 默认那次保留的那些，现在应当出现在删除清单里
	for _, p := range def.Impact.Retained {
		if !containsStr(purge.Impact.Deleted, p) {
			t.Errorf("%s 默认保留，--purge-data 之后应当出现在删除清单里", p)
		}
	}
}

// TestKeepConfigMovesConfigToRetained 是另一头。
func TestKeepConfigMovesConfigToRetained(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	def, _ := remove(f, RemoveRequest{DryRun: true})
	keep, err := remove(f, RemoveRequest{DryRun: true, KeepConfig: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(keep.Impact.Retained) <= len(def.Impact.Retained) {
		t.Errorf("--keep-config 应当让保留清单变长: %d → %d",
			len(def.Impact.Retained), len(keep.Impact.Retained))
	}
}

// ── 移除的推进 ──────────────────────────────────────────────────────────

func TestRemovePutsComponentIntoRemoving(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	if _, err := remove(f, RemoveRequest{PurgeData: true}); err != nil {
		t.Fatal(err)
	}
	comp, err := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "paramkit")
	if err != nil {
		t.Fatalf("记录不该立刻消失: %v", err)
	}
	if !comp.Removing() {
		t.Fatal("应当进入 removing")
	}
	// 开关要落库：下发是持续行为，每一轮都要带上同样的开关
	if !comp.Removal.PurgeData {
		t.Error("--purge-data 没落库，下发时节点收不到")
	}
	if comp.RemovingAt.IsZero() {
		t.Error("要记下进入 removing 的时刻——「卡了多久」决定要不要 --force")
	}
}

// TestSecondRemoveRefusesSwitchChange 是用户定的那条规矩。
//
// 三个开关**逐节点生效**：改到一半会得到「一半节点删了数据、一半留着」
// 的集群，而那种不一致事后几乎无法排查。
func TestSecondRemoveRefusesSwitchChange(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)
	if _, err := remove(f, RemoveRequest{}); err != nil {
		t.Fatal(err)
	}

	_, err := remove(f, RemoveRequest{PurgeData: true})
	if err == nil {
		t.Fatal("移除中再敲一次 remove（想改开关）必须被拒绝")
	}
	if !strings.Contains(err.Error(), "--purge-data ignored") {
		t.Errorf("要说清哪个开关被忽略了，得到: %v", err)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("要给出唯一的出路，得到: %v", err)
	}
	// 库里的开关一个字都不能改
	comp, _ := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "paramkit")
	if comp.Removal.PurgeData {
		t.Fatal("第二次 remove 把开关改掉了——这正是要防的那件事")
	}
}

// TestForceDeletesRecordAndLeavesOrphans 是验收表第 7 条。
func TestForceDeletesRecordAndLeavesOrphans(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)
	if _, err := remove(f, RemoveRequest{}); err != nil {
		t.Fatal(err)
	}
	reportRemoved(t, f, "n1", "main") // 只有 n1 拆完了，n2 失联

	res, err := remove(f, RemoveRequest{Force: true})
	if err != nil {
		t.Fatalf("--force 应当放行: %v", err)
	}
	if !res.Deleted {
		t.Fatal("--force 之后记录应当已经删掉")
	}
	if _, err := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "paramkit"); err == nil {
		t.Error("记录还在")
	}
	// 未完成的那台要说出来——它会变成孤儿，人得知道去哪儿找
	if len(res.Impact.Progress.Pending) != 1 {
		t.Errorf("要列出被跳过的实例，实际 %v", res.Impact.Progress.Pending)
	}
}

// TestRemoveWithNoInstancesDeletesImmediately 守一个会永久卡住的边角。
//
// 一个没有任何实例的 Component 永远等不到上报。让它进 removing 就是
// 让它永久卡住——既不能改（写闸门），也不会消失。
func TestRemoveWithNoInstancesDeletesImmediately(t *testing.T) {
	f := formFixture(t)
	if _, err := f.svc.Repos.Components().Create(ctx(), store.Component{
		SiteID: f.site.ID, Name: "empty",
		Pack: store.PackRef{Name: "paramkit", Version: "1.0.0", Revision: 1},
	}); err != nil {
		t.Fatal(err)
	}

	res, err := f.svc.RemoveComponent(ctx(), RemoveRequest{
		Component: "empty", Actor: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Deleted {
		t.Fatal("没有实例的组件应当当场删掉，而不是卡在 removing")
	}
	_, err = f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "empty")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("记录应当已经没了，得到 %v", err)
	}
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
