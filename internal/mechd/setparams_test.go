package mechd

import (
	"fmt"
	"strings"
	"testing"
)

// 第 6 步的判据集中在三件事上：
//
//	① 改配置**不重算放置**——它不该能缩掉一个实例
//	② 「保存前会发生什么」要算准，而且不能有副作用
//	③ 非法取值在**落库之前**被拒（验收表第 13 条）

func setParams(t *testing.T, f *fixture, req SetParamsRequest) *SetParamsResult {
	t.Helper()
	req.Site, req.Component = DefaultSite, "paramkit"
	if req.Actor == "" {
		req.Actor = "test"
	}
	out, err := f.svc.SetParams(ctx(), req)
	if err != nil {
		t.Fatalf("改配置: %v", err)
	}
	return out
}

// TestSetParamsNeverTouchesPlacement 是这条新入口存在的**唯一理由**。
//
// CLI 现有的改配置路径是 `deploy --update --set`，而 Deploy 要求指定节点
// 并重算放置——一次「把日志级别改成 debug」被迫重述整个拓扑，少写一个
// 节点名就是一次缩容。
func TestSetParamsNeverTouchesPlacement(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil) // 两个节点：n1、n2

	setParams(t, f, SetParamsRequest{Set: map[string]any{"p_int": 32}})

	comp, err := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "paramkit")
	if err != nil {
		t.Fatal(err)
	}
	insts, err := f.svc.Repos.Instances().List(ctx(), comp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 2 {
		t.Fatalf("改一个参数之后实例数应当仍是 2，实际 %d —— "+
			"这条入口的存在理由就是它不碰放置", len(insts))
	}
}

// TestSetParamsMergesRatherThanReplaces 守的是 PATCH 的语义。
//
// 整体替换会把「改一个参数」变成「把没提到的全部恢复默认」，
// 而两种请求体长得几乎一样。
func TestSetParamsMergesRatherThanReplaces(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	setParams(t, f, SetParamsRequest{Set: map[string]any{"p_int": 32}})
	setParams(t, f, SetParamsRequest{Set: map[string]any{"p_string": "changed"}})

	// 按字符串比：参数经 SQLite 的 JSON 列往返之后是 float64 而不是 int，
	// 而这里要断言的是「值还在」，不是「Go 类型没变」
	fs := fieldsOf(formOf(t, f, "paramkit", "main", ""))
	if got := fmt.Sprint(fs["p_int"].Value); got != "32" {
		t.Errorf("第二次改配置把第一次的 p_int 冲掉了，现在是 %s", got)
	}
	if fs["p_string"].Value != "changed" {
		t.Errorf("p_string 没改上，现在是 %v", fs["p_string"].Value)
	}
}

// TestUnsetRestoresDefault 守的是 Unset 与「设成 null」的区别。
func TestUnsetRestoresDefault(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	setParams(t, f, SetParamsRequest{Set: map[string]any{"p_string": "override"}})
	if got := fieldsOf(formOf(t, f, "paramkit", "main", ""))["p_string"]; got.Source != "component" {
		t.Fatalf("覆盖之后来源应为 component，得到 %s", got.Source)
	}

	setParams(t, f, SetParamsRequest{Unset: []string{"p_string"}})
	got := fieldsOf(formOf(t, f, "paramkit", "main", ""))["p_string"]
	if got.Source != "default" || got.Value != "hello" {
		t.Errorf("unset 之后应当回到 Pack 默认值 hello/default，得到 %v/%s",
			got.Value, got.Source)
	}
}

// TestIllegalValueIsRefusedBeforePersisting 是验收表第 13 条。
//
// **两条都要断言**：拒绝了，而且**没有落库**。只断言「返回了错误」的话，
// 一个先写库再校验的实现照样通过——而它留下的是「库里是非法值、机器上
// 是旧值」的中间状态，那个状态下每一次调和都会失败，且失败原因指向一份
// 谁也没审过的数据。
func TestIllegalValueIsRefusedBeforePersisting(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	// p_int 声明了 min 1 / max 64
	_, err := f.svc.SetParams(ctx(), SetParamsRequest{
		Site: DefaultSite, Component: "paramkit",
		Set: map[string]any{"p_int": 9999}, Actor: "test",
	})
	if err == nil {
		t.Fatal("超出 max 的取值应当被拒绝")
	}
	if !strings.Contains(err.Error(), "p_int") {
		t.Errorf("错误里应当指名是哪个参数，得到: %v", err)
	}

	comp, err2 := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "paramkit")
	if err2 != nil {
		t.Fatal(err2)
	}
	if v, ok := comp.Params["p_int"]; ok {
		t.Errorf("被拒绝的取值不该落库，库里却有 p_int=%v", v)
	}
}

// TestDryRunChangesNothing 守的是预览没有副作用。
func TestDryRunChangesNothing(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	// 部署本身就唤醒过节点，基线要从这一刻起算
	f.notify.woken = nil

	out := setParams(t, f, SetParamsRequest{
		Set: map[string]any{"p_int": 32}, DryRun: true,
	})
	if len(out.Changed) == 0 {
		t.Fatal("干跑也要算出会变哪些实例，否则预览是空的")
	}

	comp, err := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "paramkit")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := comp.Params["p_int"]; ok {
		t.Error("干跑不该落库")
	}
	if len(f.notify.woken) != 0 {
		t.Errorf("干跑不该唤醒任何节点，却唤醒了 %v", f.notify.woken)
	}
}

// TestNoOpSaveIsNotAChange 守的是「一次没改动的保存不留痕迹」。
//
// 它不该被算作 previewSecrets 的判据——那个位置上它是**假的**：变异测试
// 证明把预览换成一次性密钥库之后这条测试照样通过，因为密钥的值根本不进
// digest（只有 SecretRefs 的 id 与版本号进）。真正守住 previewSecrets 的
// 是 TestFromDigestIsTheRealOne。
//
// 留着它是因为它仍然守着一件真事：设成同一个值不该唤醒节点、不该在审计里
// 留下一条没发生过的变更。
func TestNoOpSaveIsNotAChange(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	// 先改成 32 并落库
	setParams(t, f, SetParamsRequest{Set: map[string]any{"p_int": 32}})

	// 再设成同一个值：什么都不该变
	out := setParams(t, f, SetParamsRequest{Set: map[string]any{"p_int": 32}})
	if len(out.Changed) != 0 {
		t.Errorf("设成同一个值不该产生变化，却报了 %d 个实例变了 —— "+
			"多半是密钥每次渲染都换了一个随机值", len(out.Changed))
	}
	if out.Effect != EffectNone {
		t.Errorf("没有变化时 Effect 应为 %s，得到 %s", EffectNone, out.Effect)
	}
}

// TestEffectIsTheUnionOfWhatActuallyMoved 是验收表第 11 条的判据。
//
// 「会发生什么」有两种错法，方向相反：
//
//	只看声明   —— 把参数改成它原本的值也报「会重启」，白紧张一次
//	只看变化   —— 没有标志的参数变了被算成不用动，服务在用户以为安全时被重启
//
// 因此这条测试同时钉两端。
func TestEffectIsTheUnionOfWhatActuallyMoved(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	// p_restart 声明 restartRequired，p_reload 声明 reloadRequired。
	// 只动 reload 的那个：结论应当是 reload，不是 restart。
	out := setParams(t, f, SetParamsRequest{
		Set: map[string]any{"p_reload": 999}, DryRun: true,
	})
	if out.Effect != EffectReload {
		t.Errorf("只改了 reloadRequired 的参数，Effect 应为 %s，得到 %s（%+v）",
			EffectReload, out.Effect, out)
	}

	// 两个一起动：restart 压过 reload——影响更大的那个才是结论
	out = setParams(t, f, SetParamsRequest{
		Set: map[string]any{"p_reload": 998, "p_restart": 101}, DryRun: true,
	})
	if out.Effect != EffectRestart {
		t.Errorf("同时改了 restartRequired 的参数，Effect 应为 %s，得到 %s",
			EffectRestart, out.Effect)
	}

	// 没有任何标志的参数：不该报会重启
	out = setParams(t, f, SetParamsRequest{
		Set: map[string]any{"p_string": "quiet"}, DryRun: true,
	})
	if out.Effect != EffectNone {
		t.Errorf("改一个没有标志的参数不该报 %s，得到 %s（%v）",
			out.Effect, out.Effect, out.Restarted)
	}
}

// TestSameValueDoesNotTriggerRestart 守的是 effectOf 的「真的变了」那一半。
func TestSameValueDoesNotTriggerRestart(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	// p_restart 的默认值是 100，把它设成 100
	out := setParams(t, f, SetParamsRequest{
		Set: map[string]any{"p_restart": 100}, DryRun: true,
	})
	if out.Effect == EffectRestart {
		t.Error("把参数设成它原本的值不该报「会重启」——" +
			"这会让人对重启提示脱敏，而下一次是真的")
	}
}

// TestFromDigestIsTheRealOne 是 previewSecrets 存在的真正理由。
//
// 前一版的判据是「设成同一个值时 Changed 为空」，而变异测试证明它抓不住
// 任何东西：密钥的**值**根本不进 digest（只有 SecretRefs 的 id 与版本号
// 进，见 spec/digest.go），因此换成一次性随机密钥后 digest 照样稳定。
//
// 真正被破坏的是**别的东西**：一次性密钥库的 id 是 `dryrun.<param>`，
// 与 Vault 里那个真 id 不同。于是 `Changed[].From` 报的是一个集群上
// 从来不存在的 digest——界面上显示 `abc123 → def456`，而机器上跑的
// 既不是 abc123 也不是 def456。
//
// **「digest 变了没有」不是判据，「它是不是那台机器上真正的那个」才是。**
// 这与 M7 那条「失败了不是判据，因为什么失败才是」是同一条纪律。
func TestFromDigestIsTheRealOne(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	// 真实渲染一遍，拿到此刻集群上那份期望 digest
	st, err := f.svc.Status(ctx(), DefaultSite, "paramkit")
	if err != nil {
		t.Fatal(err)
	}
	real := map[string]string{}
	for _, i := range st.Instances {
		real[i.Role+"@"+i.Node] = i.Want
	}
	if len(real) == 0 {
		t.Fatal("状态里没有实例，测不了")
	}

	out := setParams(t, f, SetParamsRequest{
		Set: map[string]any{"p_int": 32}, DryRun: true,
	})
	if len(out.Changed) == 0 {
		t.Fatal("改了 p_int 应当有实例变化")
	}
	for _, c := range out.Changed {
		key := c.Role + "@" + c.Node
		if want := real[key]; c.From != want {
			t.Errorf("%s 的 From 是 %s，而集群上真正的期望 digest 是 %s\n"+
				"  界面会显示一个从来不存在的「改之前」——多半是预览用了"+
				"一次性密钥库，它的 SecretRefs id 与 Vault 里的不是同一个",
				key, short12(c.From), short12(want))
		}
	}
}

func short12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
