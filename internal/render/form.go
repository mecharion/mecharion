package render

import (
	"sort"

	"github.com/mecharion/mecharion/internal/pack"
)

// 表单视图：把「这个角色有哪些参数、每个此刻是多少、值来自哪一层」
// 一次答完（23-web-ui §4.2）。
//
// **为什么在 render 里而不是在 mechd 里**：声明的三层合并（Pack 顶层 →
// roles[].params → profiles[].params，profile 层逐字段覆盖）与取值的三层
// 优先级（组 → 角色 → 组件）已经在本包里各有一份实现。在 mechd 里照着
// 再写一遍，两处只会在某个取值不对时才被发现不一致——而那时没人会想到
// 去比对两份实现。
//
// 导出面因此多了一个函数。一个包的导出面越小越好，但「同一条规则只有
// 一份实现」比它更重要。

// 取值来源的四层。它们在界面上要能区分：改错层会波及整个角色。
const (
	// SourceDefault：Pack 声明的默认值，没有人覆盖过。
	SourceDefault = "default"
	// SourceComponent：Component 级覆盖，对该组件全部实例生效。
	SourceComponent = "component"
	// SourceRole：角色级覆盖，对该角色全部实例生效。
	SourceRole = "role"
	// SourceGroup：配置组覆盖，只对组内成员生效。
	SourceGroup = "group"
	// SourceDerived：`from`。它是部署的客观事实，任何人都不能设值。
	SourceDerived = "derived"
	// SourceGenerated：`generate`。引擎生成，没有输入框。
	SourceGenerated = "generated"
	// SourceDefaultFrom：`defaultFrom` 算出来的建议值，可被覆盖。
	SourceDefaultFrom = "defaultFrom"
)

// FormParam 是表单里的一个字段。
//
// 它有意**不是** pack.Param 的别名：Param 是 Pack 作者写下的声明，
// 这里是「此刻这台集群上它长什么样」——两者在 secret、from、immutable
// 上的呈现完全不同，共用一个类型会让「哪些字段可信」变得含糊。
type FormParam struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Unit        string `json:"unit,omitempty"`
	Group       string `json:"group,omitempty"`
	Advanced    bool   `json:"advanced,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Min         any    `json:"min,omitempty"`
	Max         any    `json:"max,omitempty"`
	Pattern     string `json:"pattern,omitempty"`
	Values      []any  `json:"values,omitempty"`
	Default     any    `json:"default,omitempty"`

	// Value 是此刻的取值。**secret 永远为 nil**——见 Set。
	Value any `json:"value,omitempty"`
	// Source 是 Value 来自哪一层（Source* 常量之一）。
	Source string `json:"source"`

	// Set 只对 secret 有意义：口令设过没有。
	//
	// 不回值也**不回长度**：长度把爆破空间从「所有口令」缩到
	// 「12 位的口令」，它是信息（23-web-ui §4.2 第 ③ 条）。
	Set bool `json:"set,omitempty"`

	// ReadOnly 表示这个字段没有输入框。
	//
	// 与 Immutable 分开：ReadOnly 是「你永远填不了它」（from / generate），
	// Immutable 是「你本来能填，但这个组件已经部署了」——前者是 Pack 的
	// 性质，后者是此刻的处境，给用户的下一步动作完全不同。
	ReadOnly bool `json:"readOnly,omitempty"`
	// Immutable 表示改它需要重建组件。
	Immutable bool `json:"immutable,omitempty"`
	// Sensitive 表示不该记进日志、不该明文展示。
	Sensitive bool `json:"sensitive,omitempty"`
	// RestartRequired / ReloadRequired：**保存前**要告诉用户会发生什么。
	RestartRequired bool `json:"restartRequired,omitempty"`
	ReloadRequired  bool `json:"reloadRequired,omitempty"`

	// Pending 表示这个值现在算不出来（未部署组件上的 from）。
	//
	// 不用空值表示：空值看起来像「没配」，而它是「还没到能知道的时候」。
	Pending bool `json:"pending,omitempty"`
}

// FormRequest 描述要哪一份表单。
type FormRequest struct {
	Pack    *pack.Pack
	Profile string
	Role    string
	// Group 为空表示编辑角色级取值（影响该角色全部实例）。
	Group     string
	Overrides Overrides

	// Derived 是已经算出来的 from 取值（来自一次真正的解析）。
	//
	// 为空表示这份表单挂在一个**还没部署**的 Pack 上：那里没有放置结果，
	// from 算不出来，于是标成 Pending。
	Derived map[string]any
	// SecretSet 报告某个 secret 参数设过值没有。
	SecretSet func(name string) bool
}

// Form 生成一个角色的表单。
//
// 顺序按 (group, advanced, name)。**Pack 作者写的顺序保不住**——
// `pack.Pack.Params` 是 map，YAML 的书写顺序在解码时就丢了。不排序更糟：
// Go 的 map 遍历顺序是随机的，表单每次轮询都会重新洗牌。
func Form(req FormRequest) []FormParam {
	r := &run{req: Request{Pack: req.Pack, Profile: req.Profile}}
	decls := r.paramDecls(req.Role)

	out := make([]FormParam, 0, len(decls))
	for name, d := range decls {
		out = append(out, formParam(name, d, req))
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Group != b.Group {
			// 没写 group 的归「常规」，排在最前：它们通常是最要紧的几个
			return groupKey(a.Group) < groupKey(b.Group)
		}
		if a.Advanced != b.Advanced {
			return !a.Advanced
		}
		return a.Name < b.Name
	})
	return out
}

// groupKey 让未分组的排在最前。
func groupKey(g string) string {
	if g == "" {
		return "\x00"
	}
	return g
}

func formParam(name string, d pack.Param, req FormRequest) FormParam {
	f := FormParam{
		Name: name, Type: string(d.Type), Description: d.Description,
		Unit: d.Unit, Group: d.Group, Advanced: d.Advanced,
		Required: d.Required, Min: d.Min, Max: d.Max,
		Pattern: d.Pattern, Values: d.Values, Default: d.Default,
		Immutable: d.Immutable, Sensitive: d.Sensitive,
		RestartRequired: d.RestartRequired, ReloadRequired: d.ReloadRequired,
	}

	switch {
	case d.From != "":
		// 客观事实，任何人都不能设值（spec §7.4）
		f.ReadOnly = true
		f.Source = SourceDerived
		if v, ok := req.Derived[name]; ok {
			f.Value = v
		} else {
			f.Pending = true
		}
		return f

	case d.Generate != nil:
		// 引擎生成，没有输入框
		f.ReadOnly = true
		f.Source = SourceGenerated
		f.Set = req.secretSet(name)
		return f
	}

	// 取值的三层优先级：组 → 角色 → 组件。**与 run.userSet 同一个顺序**，
	// 那里是渲染时用的，这里是展示时用的，两者必须一致。
	if v, ok := req.Overrides.Group[req.Group][name]; ok && req.Group != "" {
		f.Value, f.Source = v, SourceGroup
	} else if v, ok := req.Overrides.Role[req.Role][name]; ok {
		f.Value, f.Source = v, SourceRole
	} else if v, ok := req.Overrides.Component[name]; ok {
		f.Value, f.Source = v, SourceComponent
	} else if d.DefaultFrom != "" {
		f.Source = SourceDefaultFrom
		if v, ok := req.Derived[name]; ok {
			f.Value = v
		} else {
			f.Pending = true
		}
	} else {
		f.Value, f.Source = d.Default, SourceDefault
	}

	// secret 的值一律不出去，无论它来自哪一层
	if d.Type == pack.TypeSecret {
		f.Set = f.Value != nil || req.secretSet(name)
		f.Value = nil
		f.Default = nil
	}
	return f
}

func (r FormRequest) secretSet(name string) bool {
	return r.SecretSet != nil && r.SecretSet(name)
}
