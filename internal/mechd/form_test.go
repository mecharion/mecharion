package mechd

import (
	"embed"
	"os"
	"path/filepath"
	"testing"

	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/packindex"
	"github.com/mecharion/mecharion/internal/render"
)

// 验收表第 8–12 条走一遍真 Pack（23-web-ui §5）。
//
// render 的单元测试是拿手搭的 pack.Pack 结构体验的判断逻辑；这里验的是
// **从 YAML 到表单的整条链路**：解析、三层声明合并、profile 逐字段覆盖、
// 配置组、密钥保管库。中间任何一环把字段丢了，那边的测试都看不见。

// paramkitFS 把夹具 Pack **编进测试二进制**。
//
// 不走相对路径读 testdata：这个测试要在容器里跑（那才算验收），而容器里
// 没有源码树。已有的 packsRoot 在那种情况下 t.Skip——一条永远在跳过的
// 验收等于没有验收，而「一直在跳过」很容易被当成「一直在通过」。
//
//go:embed testdata/packs
var packsFS embed.FS

// formFixture 起一套控制面，Pack 集合指向 paramkit 夹具。
func formFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, "n1", "n2")

	root := filepath.Join(t.TempDir(), "packs")
	for _, name := range []string{"paramkit", "noname"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		b, err := packsFS.ReadFile("testdata/packs/" + name + "/pack.yaml")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pack.yaml"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	idx := packindex.New()
	if err := idx.AddDir(root); err != nil {
		t.Fatalf("加载夹具 Pack: %v", err)
	}
	f.svc.Packs = idx
	return f
}

func formOf(t *testing.T, f *fixture, comp, role, group string) *FormView {
	t.Helper()
	v, err := f.svc.Form(ctx(), DefaultSite, comp, role, group)
	if err != nil {
		t.Fatalf("取表单: %v", err)
	}
	return v
}

func fieldsOf(v *FormView) map[string]render.FormParam {
	m := map[string]render.FormParam{}
	for _, p := range v.Params {
		m[p.Name] = p
	}
	return m
}

func deployKit(t *testing.T, f *fixture, set map[string]any) {
	t.Helper()
	if _, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "paramkit", Roles: map[string][]string{"main": {"n1", "n2"}},
		Set: set, Actor: "test",
	}); err != nil {
		t.Fatalf("部署夹具: %v", err)
	}
}

// TestAllTwelveTypesRender 是验收表第 8 条。
//
// 判据是**类型集合完全相等**，不是「至少有 12 个字段」：后者在少了
// float、多了一个重复 string 时照样通过，而 float 恰恰是现有示例包
// 一次都没覆盖过的那个。
func TestAllTwelveTypesRender(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	got := map[string]bool{}
	for _, p := range formOf(t, f, "paramkit", "", "").Params {
		got[p.Type] = true
	}

	want := []string{
		string(pack.TypeString), string(pack.TypeInt), string(pack.TypeFloat),
		string(pack.TypeBool), string(pack.TypeEnum), string(pack.TypePath),
		string(pack.TypePort), string(pack.TypeDuration), string(pack.TypeSize),
		string(pack.TypeCIDR), string(pack.TypeSecret), "list<string>",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("类型 %s 没有出现在表单里", w)
		}
		delete(got, w)
	}
	for extra := range got {
		t.Errorf("表单里出现了预期之外的类型 %q", extra)
	}
}

// TestGroupingAndAdvancedSurvive 也是第 8 条的一半：「分组与折叠符合
// group/advanced」。
func TestGroupingAndAdvancedSurvive(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)
	fs := fieldsOf(formOf(t, f, "paramkit", "", ""))

	if fs["p_advanced"].Advanced != true {
		t.Error("p_advanced 应当标成 advanced，否则它不会被折叠")
	}
	if g := fs["p_int"].Group; g != "类型" {
		t.Errorf("p_int 的分组应为「类型」，得到 %q", g)
	}
	// 未分组的排最前：它们通常是最要紧的几个
	if first := formOf(t, f, "paramkit", "", "").Params[0].Name; first != "p_ungrouped" {
		t.Errorf("未分组的字段应排在最前，实际第一个是 %s", first)
	}
}

// TestRoleSpecificParamAppears 验的是声明合并的第二层。
//
// 规范里 `roles[].params` 是**该角色专有**的声明，不是覆盖顶层 default
// 的手段（spec §7.5，那是 profiles[].params 的活）。因此判据是「它出现
// 在这个角色的表单里」，而不是「它盖掉了什么」。
//
// 第一版夹具把它当成覆盖用，测试当场变红——那是对的，写错的是夹具。
func TestRoleSpecificParamAppears(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)
	fs := fieldsOf(formOf(t, f, "paramkit", "main", ""))

	p, ok := fs["p_role_only"]
	if !ok {
		t.Fatal("角色专有的参数没有出现在表单里")
	}
	if p.Value != "only-on-main" || p.Type != "string" {
		t.Errorf("角色专有参数的声明应当完整带过来，得到 %+v", p)
	}
}

// TestSecretShowsSetNotValue 是验收表第 12 条，走真保管库。
func TestSecretShowsSetNotValue(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, map[string]any{"p_secret": "s3cret-in-the-clear"})
	fs := fieldsOf(formOf(t, f, "paramkit", "main", ""))

	pw := fs["p_secret"]
	if pw.Value != nil || pw.Default != nil {
		t.Errorf("secret 不该带出任何值，得到 Value=%v Default=%v", pw.Value, pw.Default)
	}
	if !pw.Set {
		t.Error("设过的 secret 应报 Set=true")
	}

	// generate 的那个也不能漏：它是引擎生成的，同样是口令
	if gen := fs["p_generated"]; gen.Value != nil || !gen.ReadOnly {
		t.Errorf("generate 的 secret 应当只读且无值，得到 %+v", gen)
	}
}

// TestImmutableAndRestartAreFlagged 是验收表第 10、11 条的数据面。
//
// 界面上「禁用并说明需重建」「保存前告知会重启」都要靠这两个标志，
// 而它们在 Pack 声明与表单之间要经过三层合并——丢在半路上的话，
// 用户会在毫不知情的情况下重启一个生产服务。
func TestImmutableAndRestartAreFlagged(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)
	fs := fieldsOf(formOf(t, f, "paramkit", "main", ""))

	if !fs["p_immutable"].Immutable {
		t.Error("p_immutable 的 immutable 标志丢了——界面不会提示需重建")
	}
	if !fs["p_restart"].RestartRequired {
		t.Error("p_restart 的 restartRequired 丢了——用户会在不知情时重启服务")
	}
	if !fs["p_reload"].ReloadRequired {
		t.Error("p_reload 的 reloadRequired 丢了")
	}
	// immutable 与 readOnly 是两件事：前者能填但要重建，后者永远填不了
	if fs["p_immutable"].ReadOnly {
		t.Error("immutable 不该是 readOnly——它是「改了要重建」，不是「填不了」")
	}
}

// TestDerivedParamGetsRealValue 是验收表第 9 条走真解析。
//
// p_from 声明 `from: site.name`。表单必须显示**算出来的值**，
// 而算它需要跑一次真正的解析——这正是 Form 不走捷径的理由。
func TestDerivedParamGetsRealValue(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)
	fs := fieldsOf(formOf(t, f, "paramkit", "main", ""))

	from := fs["p_from"]
	if !from.ReadOnly {
		t.Error("from 参数必须只读")
	}
	if from.Pending {
		t.Error("已部署的组件上 from 应当算得出来，不该是 Pending")
	}
	if from.Value != DefaultSite {
		t.Errorf("p_from 应当等于站点名 %q，得到 %v —— "+
			"这说明表单没有真的跑解析", DefaultSite, from.Value)
	}
}

// TestProfileNarrowsDefault 验的是声明合并的第三层。
func TestProfileNarrowsDefault(t *testing.T) {
	f := formFixture(t)
	if _, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "paramkit", Profile: "small",
		Roles: map[string][]string{"main": {"n1"}}, Actor: "test",
	}); err != nil {
		t.Fatalf("部署夹具: %v", err)
	}
	fs := fieldsOf(formOf(t, f, "paramkit", "main", ""))

	// profile 比角色层更靠后，因此 2 胜出
	if v := fs["p_int"].Value; v != 2 {
		t.Errorf("small 形态下 p_int 应为 2，得到 %v", v)
	}
	// 只该盖 default：min/max 还在
	if fs["p_int"].Max != 64 {
		t.Errorf("profile 只该改默认值，max 应仍为 64，得到 %v", fs["p_int"].Max)
	}
}

// TestUnknownRoleIsRefused 守的是坐标本身。
//
// 一个打错的 role 若被当成「用默认角色」，用户会以为自己在看 B 角色的
// 表单，实际看的是 A 的——而两个角色的参数集可以完全不同。
func TestUnknownRoleIsRefused(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)
	if _, err := f.svc.Form(ctx(), DefaultSite, "paramkit", "nosuch", ""); err == nil {
		t.Fatal("不存在的角色应当被拒绝，而不是悄悄回退到默认角色")
	}
}

// TestUnnamedRoleIsCalledDefault 守的是单角色 Pack 的省略写法。
//
// 规范允许单角色 Pack 省略 `name`，此时角色叫 `default`
// （pack.Role.EffectiveName）。第一版实现直接读了 `r.Name`，于是角色
// 下拉框里是一个空字符串——而 RoleByName 认的是 EffectiveName，
// 角色层的参数声明因此一条都合不进来。
//
// **paramkit 抓不到这个缺陷**：它老老实实写了 name。抓住它的是拿真集群
// 跑的那次检查——线上的 go-webapp 正是省略写法。于是把这个形状补进夹具
// 集合（testdata/packs/noname），让它在容器里也跑得到。
//
// 教训写在 23-web-ui §6.6：照着规范的「推荐写法」造夹具，就会系统性地
// 避开规范允许的其它写法。夹具越干净，它掩盖的形状越多。
func TestUnnamedRoleIsCalledDefault(t *testing.T) {
	f := formFixture(t)
	if _, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "noname", Nodes: []string{"n1"}, Actor: "test",
	}); err != nil {
		t.Fatalf("部署 noname: %v", err)
	}

	v := formOf(t, f, "noname", "", "")
	if v.Role != "default" {
		t.Errorf("省略了 name 的单角色应当叫 default，得到 %q", v.Role)
	}
	for _, r := range v.Roles {
		if r == "" {
			t.Error("角色列表里出现了空名字——下拉框会是空的")
		}
	}
	// 角色查得到，声明才合得进来
	if fs := fieldsOf(v); fs["greeting"].Value != "hi" {
		t.Errorf("参数声明没合进来，greeting=%v", fs["greeting"].Value)
	}
}
