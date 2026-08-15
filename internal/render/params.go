package render

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/mecharion/mecharion/internal/pack"
	"gopkg.in/yaml.v3"
)

// paramDecls 合并三处 params 声明（spec §7.5）。
//
//	顶层（Component 级）  →  roles[].params  →  profiles[].params
//
// profile 那一层通常只是改 default，因此在已有声明上**逐字段覆盖**而非整体
// 替换——否则一个只想调默认值的 profile 会把 type / min / max 一并抹掉，
// 而那要到某个非法取值溜过校验时才被发现。
func (r *run) paramDecls(role string) map[string]pack.Param {
	out := map[string]pack.Param{}
	for k, v := range r.req.Pack.Params {
		out[k] = v
	}
	if rl := r.req.Pack.RoleByName(role); rl != nil {
		for k, v := range rl.Params {
			out[k] = v
		}
	}
	for _, pf := range r.req.Pack.Profiles {
		if pf.Name != r.req.Profile {
			continue
		}
		for k, v := range pf.Params {
			base, exists := out[k]
			if !exists {
				out[k] = v
				continue
			}
			out[k] = overlay(base, v)
		}
	}
	return out
}

// overlay 把 profile 层的非零字段盖到基础声明上。
func overlay(base, over pack.Param) pack.Param {
	if over.Type != "" {
		base.Type = over.Type
	}
	if over.Default != nil {
		base.Default = over.Default
	}
	if over.Min != nil {
		base.Min = over.Min
	}
	if over.Max != nil {
		base.Max = over.Max
	}
	if over.Pattern != "" {
		base.Pattern = over.Pattern
	}
	if len(over.Values) > 0 {
		base.Values = over.Values
	}
	if over.Description != "" {
		base.Description = over.Description
	}
	if over.Required {
		base.Required = true
	}
	if over.From != "" {
		base.From = over.From
	}
	if over.DefaultFrom != "" {
		base.DefaultFrom = over.DefaultFrom
	}
	if over.Generate != nil {
		base.Generate = over.Generate
	}
	return base
}

// resolveStaticParams 走完参数链的静态部分：声明默认值 → Component →
// Role → ConfigGroup。
//
// **每一层的取值都按声明的类型与约束校验**，某层给了非法值就在那一层报错，
// 不留到渲染时才炸——那时错误信息里只剩一个渲染失败的模板名。
func (r *run) resolveStaticParams(decls map[string]pack.Param, inst *instance) (map[string]any, error) {
	out := map[string]any{}

	for name, d := range decls {
		if d.From != "" {
			continue // 完全推导，在拓扑就绪后求值
		}
		if d.Default != nil {
			out[name] = d.Default
		}
	}

	layers := []struct {
		what   string
		values map[string]any
	}{
		{"the component", r.req.Overrides.Component},
		{"role " + inst.Role, r.req.Overrides.Role[inst.Role]},
		{"config group " + inst.ConfigGroup, r.req.Overrides.Group[inst.ConfigGroup]},
	}
	for _, l := range layers {
		for _, name := range sortedAnyKeys(l.values) {
			v := l.values[name]
			d, ok := decls[name]
			if !ok {
				return nil, fmt.Errorf(
					"component %s: %s overrides parameter %q, but Pack %s does not declare it\n"+
						"  declared parameters: %s",
					r.req.Component, l.what, name, r.req.Pack.Name,
					strings.Join(sortedParamNames(decls), ", "))
			}
			if d.From != "" {
				return nil, fmt.Errorf(
					"component %s: %s tries to override parameter %q, but it declares from -- "+
						"a from value is an objective fact about the deployment and cannot be set by the user (spec §7.4)",
					r.req.Component, l.what, name)
			}
			if err := d.ValidateValue(v); err != nil {
				return nil, fmt.Errorf("component %s: %s's parameter %s: %w",
					r.req.Component, l.what, name, err)
			}
			out[name] = v
		}
	}
	return out, nil
}

// resolveDefaultFrom 求值 defaultFrom（spec §7.4）。
//
// **逐实例**：同一个 Component 的不同实例可以得到不同的值——它们在不同的
// 机器上。这也是它必须排在放置之后的原因。
//
// 求值失败（事实缺失、除零）回落到 default 并记录告警，**不中止部署**：
// 一个采集不到内存的节点不该阻断整个 Rollout。
func (r *run) resolveDefaultFrom(
	decls map[string]pack.Param, ctx *Ctx, inst *instance,
) error {
	for _, name := range sortedParamNames(decls) {
		d := decls[name]
		if d.DefaultFrom == "" {
			continue
		}
		if _, userSet := r.userSet(inst, name); userSet {
			continue // 用户给了值就不再求值——这正是 defaultFrom 与 from 的分界
		}

		s, err := r.eng.Expr("params."+name+".defaultFrom", d.DefaultFrom, ctx)
		if err != nil {
			r.warnf("%s@%s: parameter %s's defaultFrom evaluation failed, falling back to default(%v): %v",
				r.req.Component, inst.Node.Name, name, d.Default, err)
			continue
		}
		v, err := coerce(d, s)
		if err != nil {
			r.warnf("%s@%s: parameter %s's defaultFrom result %q is invalid, falling back to default(%v): %v",
				r.req.Component, inst.Node.Name, name, s, d.Default, err)
			continue
		}
		ctx.Params[name] = v
	}
	return nil
}

// resolveSecretParams 处理全部敏感参数：生成的与用户给的。
//
// generate 的语义是**只在首次**（spec §7.6）：每轮调和都重新生成会让密码
// 每 60 秒换一次，服务永远连不上。固化的键是 (component, param)，与实例
// 无关——同一个 Component 的所有实例共用一份凭据。
//
// **用户给的 secret 也必须进 Vault**，而不是只在内存里传一圈。否则它虽然
// 在 Params 里被抹成空值，明文却仍旧留在渲染出的配置内容里，随规格进归档、
// 审计与 diff——那正是 16-secrets §1 那条不变式要挡住的事。这个洞是
// minio 的示例暴露出来的：它的 root_password 是 required 而非 generate。
func (r *run) resolveSecretParams(decls map[string]pack.Param, ctx *Ctx, inst *instance) error {
	for _, name := range sortedParamNames(decls) {
		d := decls[name]
		given, userSet := r.userSet(inst, name)

		switch {
		case userSet && d.IsSensitive():
			s, ok := given.(string)
			if !ok || s == "" {
				continue
			}
			if err := r.storeSecret(name, s, "persisting"); err != nil {
				return err
			}
		case d.Generate != nil:
			if r.req.Secrets == nil {
				return fmt.Errorf(
					"component %s: parameter %s declares generate, but this render did not have a SecretVault attached",
					r.req.Component, name)
			}
			s, err := r.req.Secrets.Ensure(r.req.Component, name, *d.Generate)
			if err != nil {
				return fmt.Errorf("component %s: generating parameter %s: %w", r.req.Component, name, err)
			}
			r.secrets[name] = s
			ctx.Params[name] = s.Value
		}
	}
	return nil
}

// storeSecret 把一个用户给的敏感值交给 Vault 固化。
func (r *run) storeSecret(name, value, what string) error {
	if r.req.Secrets == nil {
		// 没接 Vault 时不能假装成功：那会让明文原样留在规格里
		return fmt.Errorf(
			"component %s: parameter %s is a sensitive value, but this render did not have a SecretVault attached\n"+
				"  → sensitive values must be persisted before becoming references; otherwise the plaintext ends up archived and audited along with the spec",
			r.req.Component, name)
	}
	s, err := r.req.Secrets.Store(r.req.Component, name, value)
	if err != nil {
		return fmt.Errorf("component %s: %s parameter %s: %w", r.req.Component, what, name, err)
	}
	r.secrets[name] = s
	return nil
}

// resolveFrom 求值 from（spec §7.4）。
//
// 排在依赖绑定与拓扑之后，因为它引用的正是这两样。**用户不可覆盖**——
// primary 在哪台机器上不是能「选择」的事。
func (r *run) resolveFrom(decls map[string]pack.Param, ctx *Ctx, inst *instance) error {
	for _, name := range sortedParamNames(decls) {
		d := decls[name]
		if d.From == "" {
			continue
		}
		s, err := r.eng.Expr("params."+name+".from", d.From, ctx)
		if err != nil {
			return fmt.Errorf("component %s@%s: parameter %s's from evaluation failed: %w",
				r.req.Component, inst.Node.Name, name, err)
		}
		v, err := coerce(d, s)
		if err != nil {
			return fmt.Errorf("component %s@%s: parameter %s's from result %q is invalid: %w",
				r.req.Component, inst.Node.Name, name, s, err)
		}
		ctx.Params[name] = v

		// **敏感传播**：取值来自敏感导出字段时，直接标为敏感，
		// 不管消费方 Pack 声明的是什么（15-render-pipeline §4）
		if r.fromTouchesSensitive(d.From) {
			r.tainted[name] = true
			if !d.IsSensitive() {
				r.warnf("%s: parameter %s's value comes from a sensitive export field and has been treated as sensitive; "+
					"consider adding type: secret to it in Pack %s",
					r.req.Component, name, r.req.Pack.Name)
			}
		} else if d.IsSensitive() {
			r.warnf("%s: parameter %s is marked secret, but its value contains no sensitive fields -- "+
				"over-marking hides even \"which database is this\" during troubleshooting, turning the marker into noise",
				r.req.Component, name)
		}
	}
	return nil
}

// exportFieldRe 匹配 `requires.<dep>.exports.<export>.<field>` 两种写法。
var exportFieldRe = regexp.MustCompile(
	`(?i)requires\.([A-Za-z0-9_-]+)\.exports\.([A-Za-z0-9_]+)\.([A-Za-z0-9_]+)`)

// fromTouchesSensitive 报告表达式是否读到了某个敏感的导出字段。
//
// 这条判断**不可能在 lint 里做**——lint 只看得见一个 Pack，而提供方可能
// 来自别处、单独发布。mechd 是唯一有全局视角的地方，放在这里也让它
// 不可能被遗忘（spec §5.4）。
func (r *run) fromTouchesSensitive(expr string) bool {
	for _, m := range exportFieldRe.FindAllStringSubmatch(expr, -1) {
		dep, export, field := strings.ToLower(m[1]), m[2], m[3]
		b, ok := r.req.Requires[dep]
		if !ok {
			continue
		}
		if b.Exports[export].SensitiveFields[field] {
			return true
		}
	}
	return false
}

// userSet 报告某个参数是否被用户在任一层显式设过值。
func (r *run) userSet(inst *instance, name string) (any, bool) {
	for _, m := range []map[string]any{
		r.req.Overrides.Group[inst.ConfigGroup],
		r.req.Overrides.Role[inst.Role],
		r.req.Overrides.Component,
	} {
		if v, ok := m[name]; ok {
			return v, true
		}
	}
	return nil, false
}

// checkRequired 在全部求值完成后检查必填项。
//
// 放在最后而不是每层各查一次：一个参数可能由 defaultFrom 或 generate 补上，
// 提前报「缺必填」是假警报。
func (r *run) checkRequired(decls map[string]pack.Param, ctx *Ctx, inst *instance) error {
	var missing []string
	for _, name := range sortedParamNames(decls) {
		d := decls[name]
		if !d.Required {
			continue
		}
		if v, ok := ctx.Params[name]; ok && !isEmpty(v) {
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"component %s@%s: the following required parameters have no value: %s\n"+
				"  → use mechctl component set %s <param>=<value> to supply them",
			r.req.Component, inst.Node.Name, strings.Join(missing, ", "), r.req.Component)
	}
	return nil
}

// ── 取值归一 ────────────────────────────────────────────────────────────

// coerce 把表达式求值出的字符串变回参数声明的类型。
//
// 模板永远返回字符串，而 `{{ min (div .Node.Facts.Memory.Total 2) "31GB" }}`
// 算出来的是字节数。若原样留作 `"8000000000"`，同一个参数在走 default 时
// 是 `"2GB"`、走 defaultFrom 时是一串数字——**同一个参数在模板里有两种形状**，
// 引用它的每个 Pack 都得自己处理这个差异。因此 size 类型在这里归一回
// 人读的形式。
func coerce(d pack.Param, s string) (any, error) {
	t := d.Type
	if t.IsList() {
		return nil, fmt.Errorf("parameters of type list cannot be evaluated from an expression")
	}
	switch t {
	case pack.TypeSize:
		n, err := pack.ParseSize(s)
		if err != nil {
			return nil, err
		}
		v := FormatSize(n)
		return v, d.ValidateValue(v)

	case pack.TypeInt, pack.TypePort:
		var n int64
		if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil {
			return nil, fmt.Errorf("%q is not an integer", s)
		}
		return n, d.ValidateValue(n)

	case pack.TypeBool:
		switch strings.TrimSpace(s) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		return nil, fmt.Errorf("%q is not a boolean", s)

	case pack.TypeFloat:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(s), "%g", &f); err != nil {
			return nil, fmt.Errorf("%q is not a number", s)
		}
		return f, d.ValidateValue(f)

	default:
		return s, d.ValidateValue(s)
	}
}

// sizeUnits 是 FormatSize 的候选单位，从大到小。
//
// 二进制单位排在前面：算出来的字节数多半来自「内存的一半」这类除法，
// 而内存本身是 2 的幂，用 Gi 表达往往正好整除，用 GB 则会得到
// 8.589934592 这种没人想看的数。
var formatUnits = []struct {
	suffix string
	mult   int64
}{
	{"Pi", 1 << 50}, {"Ti", 1 << 40}, {"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10},
	{"PB", 1e15}, {"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3},
}

// FormatSize 把字节数变回最紧凑的**整除**单位；除不尽则原样输出字节数。
//
// 只用整除是刻意的：`8.5GB` 这种带小数的写法在各家应用的解析器里行为不一，
// 而一个纯数字（字节）在任何地方都是明确的。
func FormatSize(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := ""
	if n < 0 {
		neg, n = "-", -n
	}
	for _, u := range formatUnits {
		if n%u.mult == 0 {
			return fmt.Sprintf("%s%d%s", neg, n/u.mult, u.suffix)
		}
	}
	return fmt.Sprintf("%s%d", neg, n)
}

// ── 助手 ────────────────────────────────────────────────────────────────

func sortedParamNames(m map[string]pack.Param) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedAnyKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func scalarNode(s string) yaml.Node {
	var n yaml.Node
	n.Kind = yaml.ScalarNode
	n.Tag = "!!str"
	n.Value = s
	return n
}
