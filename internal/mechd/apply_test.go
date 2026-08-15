package mechd

import (
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/store"
)

func mustParse(t *testing.T, y string) *ApplyDoc {
	t.Helper()
	doc, err := ParseApplyDoc([]byte(y))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	return doc
}

const kitDoc = `
components:
  - name: paramkit
    pack: paramkit
    roles:
      main: [n1, n2]
`

// ── 格式 ────────────────────────────────────────────────────────────────

// TestApplyDocRejectsUnknownFields 是这个格式最要紧的一条。
//
// **一个被静默忽略的字段是这类文件最贵的失败方式**：文件看起来说了一件
// 事，系统做的是另一件，而两者都不报错。`reqiure:` 打错一个字母，依赖
// 就悄悄没绑上，然后在几周后表现成一个完全无关的故障。
func TestApplyDocRejectsUnknownFields(t *testing.T) {
	_, err := ParseApplyDoc([]byte(`
components:
  - name: web
    pack: go-webapp
    reqiure: {zk: zk-prod}
`))
	if err == nil {
		t.Fatal("拼错的字段必须报错，不能静默忽略")
	}
	if !strings.Contains(err.Error(), "reqiure") {
		t.Errorf("错误里要指出是哪个字段，得到: %v", err)
	}
}

// TestApplyDocRejectsMultiDocument：`---` 分隔的第二段会被静默丢掉。
func TestApplyDocRejectsMultiDocument(t *testing.T) {
	_, err := ParseApplyDoc([]byte(`
components:
  - {name: a, pack: go-webapp}
---
components:
  - {name: b, pack: go-webapp}
`))
	if err == nil {
		t.Fatal("多文档必须报错——第二段会被静默丢掉")
	}
	if !strings.Contains(err.Error(), "single-document") {
		t.Errorf("要说清这个格式是单文档的，得到: %v", err)
	}
}

func TestApplyDocRejectsDuplicates(t *testing.T) {
	_, err := ParseApplyDoc([]byte(`
components:
  - {name: web, pack: go-webapp}
  - {name: web, pack: go-webapp}
`))
	if err == nil {
		t.Fatal("同名组件出现两次必须报错——后一条会覆盖前一条")
	}
}

// TestApplyDocRejectsRolesAndNodesTogether：两者互斥。
func TestApplyDocRejectsRolesAndNodesTogether(t *testing.T) {
	_, err := ParseApplyDoc([]byte(`
components:
  - name: web
    pack: go-webapp
    roles: {main: [n1]}
    nodes: [n2]
`))
	if err == nil {
		t.Fatal("roles 与 nodes 互斥")
	}
}

// TestApplyDocNameDefaultsToPack 与 deploy 一致。
func TestApplyDocNameDefaultsToPack(t *testing.T) {
	doc := mustParse(t, `
components:
  - pack: go-webapp
    nodes: [n1]
`)
	if got := doc.ComponentNames(); len(got) != 1 || got[0] != "go-webapp" {
		t.Errorf("省略 name 时应当等于 pack 名，得到 %v", got)
	}
}

// ── 收敛 ────────────────────────────────────────────────────────────────

func TestApplyCreatesThenIsIdempotent(t *testing.T) {
	f := formFixture(t)

	first, err := f.svc.Apply(ctx(), ApplyRequest{
		Doc: mustParse(t, kitDoc), Actor: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Components) != 1 || first.Components[0].Action != "created" {
		t.Fatalf("第一次应当是 created，得到 %+v", first.Components)
	}
	if first.Components[0].Instances != 2 {
		t.Errorf("应当放置 2 个实例，得到 %d", first.Components[0].Instances)
	}

	// **第二次不能报错。**
	//
	// 不带 --update 的 deploy 会撞上「组件已存在，重复 deploy 默认拒绝」
	// ——那条拒绝对 deploy 是对的，对 apply 是致命的：幂等正是这条命令
	// 的全部意义，一份声明文件本来就该反复应用。
	second, err := f.svc.Apply(ctx(), ApplyRequest{
		Doc: mustParse(t, kitDoc), Actor: "test",
	})
	if err != nil {
		t.Fatalf("同一份文件第二次 apply 必须成功: %v", err)
	}
	if second.Components[0].Error != "" {
		t.Fatalf("第二次不该有错: %s", second.Components[0].Error)
	}
	if second.Components[0].Instances != 2 {
		t.Errorf("第二次实例数应当不变，得到 %d", second.Components[0].Instances)
	}
}

// TestApplyNeverDeletes 是验收表第 14 条，也是这条命令的安全底线。
//
// **声明式不等于「文件里没有就删」。** 一份写漏了的文件不该删掉线上
// 组件——那是这类工具最经典的事故。
func TestApplyNeverDeletes(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil) // 集群里先有 paramkit

	// 一份**完全不提** paramkit 的文件
	res, err := f.svc.Apply(ctx(), ApplyRequest{
		Doc: mustParse(t, `
components:
  - name: other
    pack: paramkit
    roles: {main: [n1]}
`),
		Actor: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !componentExists(t, f, "paramkit") {
		t.Fatal("文件里没提的组件被删掉了——声明式不等于「文件里没有就删」")
	}
	// 但差异必须被说出来
	if len(res.Extra) != 1 || res.Extra[0] != "paramkit" {
		t.Errorf("要把「集群里有、文件里没有」的组件列出来，得到 %v", res.Extra)
	}
}

// TestApplyDryRunChangesNothing。
func TestApplyDryRunChangesNothing(t *testing.T) {
	f := formFixture(t)

	res, err := f.svc.Apply(ctx(), ApplyRequest{
		Doc: mustParse(t, kitDoc), DryRun: true, Actor: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun {
		t.Error("结果里要标出这是干跑")
	}
	if componentExists(t, f, "paramkit") {
		t.Fatal("干跑把组件真的建出来了")
	}
}

// TestApplySkipsRemovingComponents 守一个正常会发生的时序。
//
// 删除是异步的，一次 apply 撞上一个正在被删的组件完全正常。让整份文件
// 因此失败，会把「等它删完」变成「先把这一段从文件里删掉再跑」。
func TestApplySkipsRemovingComponents(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)
	startRemoving(t, f, store.ComponentRemoval{})

	res, err := f.svc.Apply(ctx(), ApplyRequest{
		Doc: mustParse(t, kitDoc), Actor: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Components) != 1 {
		t.Fatalf("应当有一条结果，得到 %+v", res.Components)
	}
	if !res.Components[0].Removing {
		t.Errorf("正在移除的组件应当被跳过并标出来，得到 %+v", res.Components[0])
	}
	if res.Components[0].Error != "" {
		t.Errorf("跳过不是失败，得到错误: %s", res.Components[0].Error)
	}
}

// TestApplyRefusesAMismatchedSite 守「文件用错了地方」。
//
// 站点名不匹配多半意味着有人把 staging 的文件敲到了生产上。**不自动
// 切换**——那种「贴心」会让一次手滑变成一次跨环境事故。
func TestApplyRefusesAMismatchedSite(t *testing.T) {
	f := formFixture(t)
	_, err := f.svc.Apply(ctx(), ApplyRequest{
		Doc: mustParse(t, `
site:
  name: some-other-site
components:
  - {name: web, pack: paramkit, roles: {main: [n1]}}
`),
		Actor: "test",
	})
	if err == nil {
		t.Fatal("站点名不匹配必须拒绝")
	}
	if !strings.Contains(err.Error(), "site name mismatch") {
		t.Errorf("要说清原因，得到: %v", err)
	}
}

// TestApplyOneFailureDoesNotStopTheRest：组件彼此独立。
func TestApplyOneFailureDoesNotStopTheRest(t *testing.T) {
	f := formFixture(t)

	res, err := f.svc.Apply(ctx(), ApplyRequest{
		Doc: mustParse(t, `
components:
  - {name: bad, pack: no-such-pack, roles: {main: [n1]}}
  - {name: good, pack: paramkit, roles: {main: [n1]}}
`),
		Actor: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Components) != 2 {
		t.Fatalf("两个组件都该有结果，得到 %+v", res.Components)
	}
	if res.Components[0].Error == "" {
		t.Error("第一个用了不存在的 Pack，应当失败")
	}
	// **第二个照常执行**：「因为 A 装不上所以 B 也没装」是最难解释的现场
	if res.Components[1].Error != "" {
		t.Errorf("一个失败不该拖垮其余的: %s", res.Components[1].Error)
	}
	if !componentExists(t, f, "good") {
		t.Error("good 应当被真的部署出来")
	}
}

// ── secret（setFile 走过来的明文）────────────────────────────────────────
//
// 也是 **M9 里程碑审查补的**：`splitSecretKey` 收尾时是 **0.0%**。
//
// 这条路是这样的：CLI 读 `setFile:` 指的文件，把明文按 `<组件>/<参数>`
// 塞进 `ApplyRequest.Secrets`，服务端拆开、只发给同名的那个组件。
// 「apply 里的 secret 只能走 setFile」写在 `apply --help` 里，是这份文件
// 敢进版本库的前提——而它一次都没被执行过。

// TestApplyRoutesSecretsToTheirComponent：明文要真的落到那个组件上。
func TestApplyRoutesSecretsToTheirComponent(t *testing.T) {
	f := formFixture(t)

	res, err := f.svc.Apply(ctx(), ApplyRequest{
		Doc:     mustParse(t, kitDoc),
		Secrets: map[string]any{"paramkit/p_secret": "from-set-file"},
		Actor:   "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Components[0].Error != "" {
		t.Fatalf("部署失败: %s", res.Components[0].Error)
	}

	// **判据是表单说它设过了**，不是「apply 没报错」——一个把 Secrets
	// 整个丢掉的实现同样不会报错。
	if pw := fieldsOf(formOf(t, f, "paramkit", "main", ""))["p_secret"]; !pw.Set {
		t.Error("setFile 送来的 secret 没有落到组件上（表单里 Set=false）")
	}
}

// TestApplySecretsDoNotLeakAcrossComponents 是这条路真正的风险所在。
//
// 键里带组件名正是为了这个。拆键拆错、或者干脆不比对组件名，一份文件里
// A 的口令就会被一并写进 B——**而两边都不会报错**，因为 B 的那个参数
// 恰好也叫这个名字。等到发现时，那个口令已经在另一个组件的配置文件里了。
func TestApplySecretsDoNotLeakAcrossComponents(t *testing.T) {
	f := formFixture(t)

	res, err := f.svc.Apply(ctx(), ApplyRequest{
		Doc: mustParse(t, `
components:
  - {name: kit-a, pack: paramkit, roles: {main: [n1]}}
  - {name: kit-b, pack: paramkit, roles: {main: [n2]}}
`),
		// 只给 kit-a
		Secrets: map[string]any{"kit-a/p_secret": "only-for-a"},
		Actor:   "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Components {
		if c.Error != "" {
			t.Fatalf("%s 部署失败: %s", c.Name, c.Error)
		}
	}

	if !fieldsOf(formOf(t, f, "kit-a", "main", ""))["p_secret"].Set {
		t.Error("kit-a 的 secret 没设上")
	}
	if fieldsOf(formOf(t, f, "kit-b", "main", ""))["p_secret"].Set {
		t.Error("kit-a 的口令漏到 kit-b 去了——键里带组件名就是为了防这个")
	}
}

// TestApplyIgnoresAMalformedSecretKey：没有 `/` 的键。
//
// 它只可能来自一个对不上的客户端。**丢掉而不是当成参数名**——后者会把
// 一个明文口令写进一个谁也没声明过的参数里。
func TestApplyIgnoresAMalformedSecretKey(t *testing.T) {
	f := formFixture(t)

	res, err := f.svc.Apply(ctx(), ApplyRequest{
		Doc:     mustParse(t, kitDoc),
		Secrets: map[string]any{"p_secret": "no-component-prefix"},
		Actor:   "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Components[0].Error != "" {
		t.Fatalf("畸形的键不该让部署失败: %s", res.Components[0].Error)
	}
	if fieldsOf(formOf(t, f, "paramkit", "main", ""))["p_secret"].Set {
		t.Error("没有组件前缀的键被当成参数名用了")
	}
}

// ── 配置组 ──────────────────────────────────────────────────────────────
//
// 这一段是 **M9 里程碑审查补的**。`configGroups:` 是 §2.5「单文件按名词
// 分段」定下的一半，`mechctl apply --help` 里也写着它，但收尾时 applyGroup
// 的语句覆盖率是 **0.0%**——单元测试、多节点验收都没有一处走过它。
//
// 一个「发布了、文档里写着、一次都没执行过」的字段，比一个没做的字段更贵：
// 没做的至少会当场报错。

func componentByName(t *testing.T, f *fixture, name string) store.Component {
	t.Helper()
	c, err := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, name)
	if err != nil {
		t.Fatalf("取组件 %s: %v", name, err)
	}
	return c
}

const groupDoc = kitDoc + `
configGroups:
  - component: paramkit
    role: main
    name: just-n1
    members: [n1]
    params:
      p_int: 42
`

// TestApplyCreatesConfigGroups 是这一段的主判据。
func TestApplyCreatesConfigGroups(t *testing.T) {
	f := formFixture(t)

	res, err := f.svc.Apply(ctx(), ApplyRequest{
		Doc: mustParse(t, groupDoc), Actor: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Groups) != 1 {
		t.Fatalf("应当有一条配置组结果，得到 %+v", res.Groups)
	}
	if g := res.Groups[0]; g.Error != "" || g.Action != "updated" {
		t.Fatalf("配置组应当被建出来，得到 %+v", g)
	}

	// **落到库里了，不只是结果里有一行。** 只看返回值的话，一个什么都
	// 没做、只把入参回显出来的实现同样能通过。
	comp := componentByName(t, f, "paramkit")
	groups, err := f.svc.Repos.ConfigGroups().List(ctx(), comp.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found *store.ConfigGroup
	for i := range groups {
		if groups[i].Name == "just-n1" {
			found = &groups[i]
		}
	}
	if found == nil {
		t.Fatalf("库里没有 just-n1，实际 %+v", groups)
	}
	if len(found.Members) != 1 || found.Members[0] != "n1" {
		t.Errorf("成员不对: %v", found.Members)
	}
	if found.Params["p_int"] != float64(42) && found.Params["p_int"] != 42 {
		t.Errorf("参数没写进去: %v", found.Params)
	}
}

// TestApplyGroupsRunAfterComponents 钉住 Apply 里那个顺序。
//
// 配置组挂在组件上。**同一份文件里新建组件 + 新建它的配置组**是最自然的
// 写法，而如果配置组先跑，它无处可挂——错误会是「组件不存在」，
// 而人看着文件里明明写了那个组件。
func TestApplyGroupsRunAfterComponents(t *testing.T) {
	f := formFixture(t)

	// 组件此刻还不存在；两段在同一次 apply 里
	if componentExists(t, f, "paramkit") {
		t.Fatal("前提不成立：组件不该已经存在")
	}
	res, err := f.svc.Apply(ctx(), ApplyRequest{
		Doc: mustParse(t, groupDoc), Actor: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Groups[0].Error != "" {
		t.Fatalf("同一份文件里的配置组应当能挂上刚建出来的组件: %s",
			res.Groups[0].Error)
	}
}

// TestApplyGroupsAreIdempotent：同一份文件跑两次，第二次不该报错。
func TestApplyGroupsAreIdempotent(t *testing.T) {
	f := formFixture(t)

	for i := 0; i < 2; i++ {
		res, err := f.svc.Apply(ctx(), ApplyRequest{
			Doc: mustParse(t, groupDoc), Actor: "test",
		})
		if err != nil {
			t.Fatalf("第 %d 次: %v", i+1, err)
		}
		if res.Groups[0].Error != "" {
			t.Fatalf("第 %d 次配置组报错: %s", i+1, res.Groups[0].Error)
		}
	}
}

// TestApplyDryRunCreatesNoGroup 守干跑。
//
// 干跑把配置组真写进去，是这条命令最不该有的副作用：人跑 --dry-run
// 正是为了**先看看**。
func TestApplyDryRunCreatesNoGroup(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil) // 组件先存在，免得干跑因为组件缺失而「碰巧」没建组

	res, err := f.svc.Apply(ctx(), ApplyRequest{
		Doc: mustParse(t, groupDoc), DryRun: true, Actor: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Groups[0].Action != "unchanged" {
		t.Errorf("干跑时配置组应当标成 unchanged，得到 %q", res.Groups[0].Action)
	}

	comp := componentByName(t, f, "paramkit")
	groups, err := f.svc.Repos.ConfigGroups().List(ctx(), comp.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g.Name == "just-n1" {
			t.Fatal("干跑把配置组真的建出来了")
		}
	}
}

// TestApplyGroupFailureIsReportedNotFatal：一个组失败不该让整次 apply 报错。
//
// 与组件那条同一个理由——但这里还多一层：组件已经**改过了**。让 Apply
// 整体返回 error，人看到的是一次「失败」，而集群其实已经变了。
func TestApplyGroupFailureIsReportedNotFatal(t *testing.T) {
	f := formFixture(t)

	res, err := f.svc.Apply(ctx(), ApplyRequest{
		Doc: mustParse(t, kitDoc+`
configGroups:
  - component: no-such-component
    role: main
    name: g
    members: [n1]
`),
		Actor: "test",
	})
	if err != nil {
		t.Fatalf("配置组失败不该让整次 apply 返回 error: %v", err)
	}
	if res.Groups[0].Error == "" || res.Groups[0].Action != "failed" {
		t.Errorf("要如实报出这一条失败了，得到 %+v", res.Groups[0])
	}
	// 组件那一段照常生效
	if res.Components[0].Error != "" {
		t.Errorf("组件不该被拖累: %s", res.Components[0].Error)
	}
}
