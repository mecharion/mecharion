package render

import (
	"testing"

	"github.com/mecharion/mecharion/internal/pack"
)

// 表单的判据集中在一件事上：**这个值是从哪一层来的**。
//
// 「值对不对」由渲染管线的测试守着；这里守的是展示层能不能让人看出
// 「我改的是谁」——改错层会波及整个角色，而那是最常见的一类误操作
// （23-web-ui §4.1）。

func kit() *pack.Pack {
	return &pack.Pack{
		Name: "kit",
		Params: map[string]pack.Param{
			"a": {Type: pack.TypeString, Default: "pack"},
			"b": {Type: pack.TypeString, Default: "pack"},
			"c": {Type: pack.TypeString, Default: "pack"},
			"d": {Type: pack.TypeString, Default: "pack"},
		},
		Roles: []pack.Role{{Name: "main"}},
	}
}

func byName(fs []FormParam) map[string]FormParam {
	m := map[string]FormParam{}
	for _, f := range fs {
		m[f.Name] = f
	}
	return m
}

// TestSourceIsTheDeepestLayerThatSetIt 是这一整块的核心判据。
//
// 四个参数分别只在一层被设过，因此每一个的 Source 都不同——一个把
// 优先级写反的实现会在这里同时错三个，而只测「值对不对」的断言在
// 组与角色设了同一个值时完全看不出区别。
func TestSourceIsTheDeepestLayerThatSetIt(t *testing.T) {
	got := byName(Form(FormRequest{
		Pack: kit(), Role: "main", Group: "g1",
		Overrides: Overrides{
			Component: map[string]any{"a": "comp", "b": "comp", "c": "comp"},
			Role:      map[string]map[string]any{"main": {"b": "role", "c": "role"}},
			Group:     map[string]map[string]any{"g1": {"c": "group"}},
		},
	}))

	for _, tc := range []struct{ name, value, source string }{
		{"c", "group", SourceGroup},
		{"b", "role", SourceRole},
		{"a", "comp", SourceComponent},
		{"d", "pack", SourceDefault},
	} {
		f := got[tc.name]
		if f.Value != tc.value || f.Source != tc.source {
			t.Errorf("%s: 期望 %q(来自 %s)，得到 %v(来自 %s)",
				tc.name, tc.value, tc.source, f.Value, f.Source)
		}
	}
}

// TestGroupOverrideIgnoredWhenNoGroupSelected 守的是「我改的是谁」的另一半。
//
// 没选组时看的是**角色级**取值。若实现漏了 `req.Group != ""` 这个条件，
// 表单会把某个组的覆盖显示成角色级取值——用户据此以为整个角色都是这个
// 值，而实际上只有那个组是。
func TestGroupOverrideIgnoredWhenNoGroupSelected(t *testing.T) {
	got := byName(Form(FormRequest{
		Pack: kit(), Role: "main", Group: "",
		Overrides: Overrides{
			Component: map[string]any{"c": "comp"},
			Group:     map[string]map[string]any{"g1": {"c": "group"}},
		},
	}))
	if f := got["c"]; f.Value != "comp" || f.Source != SourceComponent {
		t.Errorf("没选组时应当显示组件级取值 comp/%s，得到 %v/%s",
			SourceComponent, f.Value, f.Source)
	}
}

// TestSecretNeverCarriesValueNorLength 是验收表第 12 条。
//
// 断言不能只写「Value 为空」：一个把值换成 "********" 的实现同样满足
// 那条，而它泄漏了长度——长度把爆破空间从「所有口令」缩到「12 位的
// 口令」。因此这里连 Default 一起查。
func TestSecretNeverCarriesValueNorLength(t *testing.T) {
	p := &pack.Pack{
		Name:  "kit",
		Roles: []pack.Role{{Name: "main"}},
		Params: map[string]pack.Param{
			"pw": {Type: pack.TypeSecret, Default: "hunter2-the-default"},
		},
	}
	got := byName(Form(FormRequest{
		Pack: p, Role: "main",
		Overrides: Overrides{Component: map[string]any{"pw": "s3cret-in-the-clear"}},
	}))

	f := got["pw"]
	if f.Value != nil {
		t.Errorf("secret 的 Value 必须为空，得到 %v", f.Value)
	}
	if f.Default != nil {
		t.Errorf("secret 的 Default 也必须为空（它同样是口令），得到 %v", f.Default)
	}
	if !f.Set {
		t.Error("设过值的 secret 应当报 Set=true——否则界面会说「未设置」")
	}
}

// TestDerivedParamsAreReadOnly 是验收表第 9 条。
func TestDerivedParamsAreReadOnly(t *testing.T) {
	p := &pack.Pack{
		Name:  "kit",
		Roles: []pack.Role{{Name: "main"}},
		Params: map[string]pack.Param{
			"host": {Type: pack.TypeString, From: "site.name"},
			"gen":  {Type: pack.TypeSecret, Generate: &pack.Generate{Length: 16}},
		},
	}
	got := byName(Form(FormRequest{
		Pack: p, Role: "main",
		Derived: map[string]any{"host": "hq"},
	}))

	if f := got["host"]; !f.ReadOnly || f.Source != SourceDerived || f.Value != "hq" {
		t.Errorf("from 参数应当只读、来源 derived、带上算出来的值，得到 %+v", f)
	}
	if f := got["gen"]; !f.ReadOnly || f.Source != SourceGenerated {
		t.Errorf("generate 参数应当只读、来源 generated，得到 %+v", f)
	}
}

// TestUncomputableDerivedIsPendingNotEmpty 守的是「空值」与「还算不出来」
// 的区别。
//
// 未部署的 Pack 上没有放置结果，from 算不出来。若那时回一个空值，界面
// 会显示成「没配」——而用户对「没配」的反应是去填它，可它根本没有输入框。
func TestUncomputableDerivedIsPendingNotEmpty(t *testing.T) {
	p := &pack.Pack{
		Name:   "kit",
		Roles:  []pack.Role{{Name: "main"}},
		Params: map[string]pack.Param{"host": {Type: pack.TypeString, From: "site.name"}},
	}
	f := byName(Form(FormRequest{Pack: p, Role: "main"}))["host"]
	if !f.Pending {
		t.Error("算不出来的 from 应当标 Pending——空值看起来像「没配」")
	}
	if f.Value != nil {
		t.Errorf("算不出来时不该编一个值出来，得到 %v", f.Value)
	}
}

// TestOrderIsStableAcrossCalls 守的是表单不会自己重排。
//
// `pack.Pack.Params` 是 map，Go 的遍历顺序每次都不同。不排序的话表单
// **每次轮询都重新洗牌**——用户正在填的字段会跳走。
func TestOrderIsStableAcrossCalls(t *testing.T) {
	p := &pack.Pack{
		Name:  "kit",
		Roles: []pack.Role{{Name: "main"}},
		Params: map[string]pack.Param{
			"z_plain": {Type: pack.TypeString},
			"a_adv":   {Type: pack.TypeString, Group: "网络", Advanced: true},
			"b_net":   {Type: pack.TypeString, Group: "网络"},
			"c_mem":   {Type: pack.TypeString, Group: "内存"},
		},
	}
	want := []string{"z_plain", "c_mem", "b_net", "a_adv"}

	// 跑 20 次：map 顺序随机，一次跑对可能只是运气
	for i := 0; i < 20; i++ {
		var got []string
		for _, f := range Form(FormRequest{Pack: p, Role: "main"}) {
			got = append(got, f.Name)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("第 %d 次顺序不对：期望 %v，得到 %v", i, want, got)
			}
		}
	}
}

// TestProfileOverlaysDeclarationFieldwise 守的是声明合并那一层。
//
// profile 通常只想改默认值。若实现做成整体替换，type / min / max 会被
// 一并抹掉——而症状要到某个非法取值溜过校验时才出现。
func TestProfileOverlaysDeclarationFieldwise(t *testing.T) {
	p := &pack.Pack{
		Name:  "kit",
		Roles: []pack.Role{{Name: "main"}},
		Params: map[string]pack.Param{
			"heap": {Type: pack.TypeSize, Default: "1Gi", Min: "512Mi", Group: "内存"},
		},
		Profiles: []pack.Profile{{
			Name:   "small",
			Params: map[string]pack.Param{"heap": {Default: "256Mi"}},
		}},
	}
	f := byName(Form(FormRequest{Pack: p, Profile: "small", Role: "main"}))["heap"]
	if f.Value != "256Mi" {
		t.Errorf("profile 应当改掉默认值，得到 %v", f.Value)
	}
	if f.Type != string(pack.TypeSize) || f.Min != "512Mi" || f.Group != "内存" {
		t.Errorf("profile 只该盖 default，其余声明要留着，得到 %+v", f)
	}
}
