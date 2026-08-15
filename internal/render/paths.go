package render

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/spec"
)

// 预定义路径名与默认值（spec §8.2）。
//
// Pack 可覆盖其中任意一条，也可另定义自己的名字。
var predefined = []struct{ name, dflt string }{
	{"home", "{{ .Node.Roots.opt }}/apps/{{ .Component }}"},
	{"config", "{{ .Node.Roots.etc }}/apps/{{ .Component }}"},
	{"data", "{{ .Node.Roots.data }}/apps/{{ .Component }}"},
	{"logs", "{{ .Node.Roots.logs }}/apps/{{ .Component }}"},
	{"runtime", "{{ .Node.Roots.run }}/apps/{{ .Component }}"},
}

// DefaultRoots 是 mechlet.yaml 未声明时的路径根。
var DefaultRoots = map[string]string{
	"opt":  "/opt/mecharion",
	"etc":  "/etc/mecharion",
	"data": "/var/lib/mecharion",
	"logs": "/var/log/mecharion",
	"run":  "/run/mecharion",
}

// derivedPaths 是只读派生值，Pack 不能声明同名路径。
//
// Generation 恒为占位符：generation 目录名含一个 mechlet 本地分配的序号，
// mechd 无从得知（spec §8.2、12-spec-and-state §1.4）。
var derivedPaths = []string{"generation", "current"}

// resolvePaths 求值一个实例的全部路径。
//
// 路径之间可以互相引用（`config.linkInto: "{{ .Paths.Generation }}/config"`、
// `data: "{{ .Paths.Home }}/data"`），且引用关系由 Pack 作者决定，因此不能
// 按固定顺序求值——用**逐轮推进到不动点**：每轮求值那些依赖已就绪的路径，
// 一轮没有任何进展就说明剩下的构成环，报错并指名到具体的路径。
func (r *run) resolvePaths(ctx *Ctx, inst *instance) (map[string]spec.PathValue, error) {
	decls := r.pathDecls(inst.Role)

	// 派生值先就位：它们不依赖任何东西，却几乎人人依赖
	ctx.Paths = map[string]any{
		"Generation": spec.GenerationPlaceholder,
		"generation": spec.GenerationPlaceholder,
	}

	out := map[string]spec.PathValue{}
	pending := map[string]pack.Path{}
	for name, d := range decls {
		pending[name] = d
	}

	for len(pending) > 0 {
		progress := false
		for _, name := range sortedPathNames(pending) {
			d := pending[name]
			values, err := r.resolveOnePath(ctx, inst, name, d)
			if err != nil {
				if isUnresolved(err) {
					continue // 依赖还没就绪，下一轮再试
				}
				return nil, err
			}

			pv := spec.PathValue{
				Name:   name,
				Values: values,
				Kind:   string(d.EffectiveKind()),
				Layout: string(d.EffectiveLayout()),
				Mode:   orDefault(d.Mode, "0755"),
				Owner:  d.Owner,
				Group:  d.Group,
			}
			out[name] = pv
			publishPath(ctx.Paths, name, d, values)
			delete(pending, name)
			progress = true

			// current 依赖 home，而 home 本身是被求值出来的
			if name == "home" {
				cur := path.Join(values[0], "current")
				ctx.Paths["Current"], ctx.Paths["current"] = cur, cur
			}
		}
		if !progress {
			return nil, unresolvableCycle(pending)
		}
	}

	// linkInto / distDir 引用 .Paths.Generation，必须等全部路径就绪后再求值
	for name, d := range decls {
		pv := out[name]
		if d.LinkInto != "" {
			s, err := r.eng.Expr("paths."+name+".linkInto", d.LinkInto, ctx)
			if err != nil {
				return nil, err
			}
			pv.LinkInto = s
		}
		pv.DistDir = d.DistDir
		out[name] = pv
	}

	if _, ok := out["home"]; !ok {
		return nil, fmt.Errorf("component %s: failed to resolve paths.home", r.req.Component)
	}
	return out, nil
}

// resolveOnePath 求值单条路径声明的全部取值。
func (r *run) resolveOnePath(ctx *Ctx, inst *instance, name string, d pack.Path) ([]string, error) {
	// ConfigGroup 可以按卷名绑定多盘路径（spec §8.6）
	if bound := inst.PathBindings[name]; len(bound) > 0 {
		out := make([]string, 0, len(bound))
		for _, volName := range bound {
			v, ok := inst.Node.Volumes[volName]
			if !ok {
				return nil, fmt.Errorf(
					"component %s: paths.%s is bound to volume %q, but node %s does not declare that volume\n"+
						"  volumes declared on this node: %s",
					r.req.Component, name, volName, inst.Node.Name,
					strings.Join(volumeNames(inst.Node.Volumes), ", "))
			}
			out = append(out, path.Join(v.Path, d.Subpath))
		}
		return out, nil
	}

	raw, err := d.DefaultStrings()
	if err != nil {
		return nil, fmt.Errorf("component %s: paths.%s: %w", r.req.Component, name, err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("component %s: paths.%s is missing default", r.req.Component, name)
	}

	out := make([]string, 0, len(raw))
	for i, expr := range raw {
		s, err := r.eng.Expr(fmt.Sprintf("paths.%s[%d]", name, i), expr, ctx)
		if err != nil {
			return nil, err
		}
		if !path.IsAbs(s) && !strings.Contains(s, spec.GenerationPlaceholder) {
			return nil, fmt.Errorf("component %s: paths.%s resolved to %q, which is not an absolute path",
				r.req.Component, name, s)
		}
		out = append(out, s)
	}
	return out, nil
}

// publishPath 把已解析的路径放进渲染上下文。
//
// 单值路径是 string，多值是 []string——`{{ .Paths.Config }}/x` 与
// `{{ range .Paths.DataDirs }}` 两种写法都要自然成立。
func publishPath(dst map[string]any, name string, d pack.Path, values []string) {
	var v any = values
	if d.EffectiveKind() == pack.KindSingle {
		v = values[0]
	}
	dst[name] = v
	if c := capitalize(name); c != name {
		dst[c] = v
	}
}

// pathDecls 合并 Pack 顶层与角色级的 paths 声明，并补上未覆盖的预定义路径。
func (r *run) pathDecls(role string) map[string]pack.Path {
	out := map[string]pack.Path{}
	for _, pd := range predefined {
		var d pack.Path
		d.Default = scalarNode(pd.dflt)
		out[pd.name] = d
	}
	for k, v := range r.req.Pack.Paths {
		out[k] = v
	}
	if rl := r.req.Pack.RoleByName(role); rl != nil {
		for k, v := range rl.Paths {
			out[k] = v
		}
	}
	for _, d := range derivedPaths {
		delete(out, d) // 派生值不可声明；lint 的 R13 已经拦过一次
	}
	return out
}

// isUnresolved 判断错误是否为「引用了尚未求值的路径」。
//
// missingkey=error 对 map 缺键报的是固定措辞，靠它区分「还没轮到」与
// 「真的写错了」。判断错了的代价是对称的：误判为「还没轮到」会在不动点
// 处报环错误（仍然拦得住），误判为「真错了」才会漏掉重试——因此这里
// 宁可宽松。
func isUnresolved(err error) bool {
	s := err.Error()
	return strings.Contains(s, "map has no entry for key") ||
		strings.Contains(s, "nil pointer evaluating")
}

func unresolvableCycle(pending map[string]pack.Path) error {
	names := sortedPathNames(pending)
	return fmt.Errorf(
		"paths could not be resolved -- the following paths reference each other or a path that doesn't exist: %s\n"+
			"  hint: the only derived values are .Paths.Generation and .Paths.Current, "+
			"everything else must be a name declared in paths",
		strings.Join(names, ", "))
}

func sortedPathNames(m map[string]pack.Path) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func volumeNames(m map[string]Volume) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
