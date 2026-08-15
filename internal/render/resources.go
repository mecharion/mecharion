package render

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/spec"
	"gopkg.in/yaml.v3"
)

// commonArgs 是与类型参数混在同一层的通用字段（spec §14.1）。
// 它们在 spec.Resource 上有独立位置，不该在 Args 里重复一份。
var commonArgs = map[string]bool{
	"id": true, "when": true, "driftPolicy": true, "notify": true,
}

// renderResources 渲染 shared 与角色的资源清单。
//
// 顺序是 shared 在前、role 在后，且 `when` 已求值——**求值为 false 的资源
// 根本不在列表里**，不是带着标记下发让 mechlet 再判一次。mechlet 不做
// 条件判断，是 ADR-0006 的直接结论。
func (r *run) renderResources(ctx *Ctx, it *instance) ([]spec.Resource, error) {
	var out []spec.Resource

	var groups []struct {
		origin string
		list   []pack.Resource
	}
	if sh := r.req.Pack.Shared; sh != nil {
		groups = append(groups, struct {
			origin string
			list   []pack.Resource
		}{"shared", sh.Resources})
	}
	if rl := r.req.Pack.RoleByName(it.Role); rl != nil {
		groups = append(groups, struct {
			origin string
			list   []pack.Resource
		}{"role", rl.Resources})
	}

	seen := map[string]bool{}
	for _, g := range groups {
		for i, res := range g.list {
			keep, err := r.evalWhen(ctx, res, g.origin, i)
			if err != nil {
				return nil, err
			}
			if !keep {
				continue
			}
			rendered, err := r.renderOneResource(ctx, it, res, g.origin, i)
			if err != nil {
				return nil, err
			}
			if seen[rendered.ID] {
				return nil, fmt.Errorf(
					"component %s@%s: duplicate resource id %q\n"+
						"  ids must be unique after merging shared and role %s's resource lists",
					r.req.Component, it.Node.Name, rendered.ID, it.Role)
			}
			seen[rendered.ID] = true
			out = append(out, *rendered)
		}
	}
	return out, nil
}

// evalWhen 求值资源的 when 条件。
func (r *run) evalWhen(ctx *Ctx, res pack.Resource, origin string, idx int) (bool, error) {
	if strings.TrimSpace(res.When) == "" {
		return true, nil
	}
	s, err := r.eng.Expr(fmt.Sprintf("%s.resources[%d].when", origin, idx), res.When, ctx)
	if err != nil {
		return false, err
	}
	return isTruthy(s), nil
}

// isTruthy 判定 when 的求值结果。
//
// 模板求值出来的一律是字符串，因此这里明确列出哪些算假，其余算真。
// `{{ eq .Profile "ha" }}` 在 Go 模板里渲染成 "true" / "false"。
func isTruthy(s string) bool {
	switch strings.TrimSpace(s) {
	case "", "false", "0", "no", "off", "<no value>":
		return false
	}
	return true
}

// renderOneResource 渲染一条资源。
func (r *run) renderOneResource(
	ctx *Ctx, it *instance, res pack.Resource, origin string, idx int,
) (*spec.Resource, error) {
	where := fmt.Sprintf("%s.resources[%d](%s)", origin, idx, res.Type)

	args, err := nodeToAny(&res.Args)
	if err != nil {
		return nil, fmt.Errorf("component %s: %s: %w", r.req.Component, where, err)
	}
	m, _ := args.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	for k := range commonArgs {
		delete(m, k)
	}

	rendered, err := r.renderAny(ctx, where, m)
	if err != nil {
		return nil, err
	}
	m = rendered.(map[string]any)

	// template 资源：mechd 渲染出 content，src **不进已解析规格**。
	// 它出现在那里就是 mechd 的 bug，mechlet 会报错（15-render-pipeline §6）。
	if res.Type == pack.ResTemplate {
		src, _ := m["src"].(string)
		if src == "" {
			return nil, fmt.Errorf("component %s: %s: template resource is missing src",
				r.req.Component, where)
		}
		if !r.eng.Has(src) {
			return nil, fmt.Errorf("component %s: %s: template %s/%s does not exist",
				r.req.Component, where, pack.DirTemplates, src)
		}
		body, err := r.eng.Render(src, ctx)
		if err != nil {
			return nil, fmt.Errorf("component %s@%s: %w", r.req.Component, it.Node.Name, err)
		}
		delete(m, "src")
		m["content"] = body
	}

	id := res.ID
	if id == "" {
		id = autoID(res, m, origin, idx)
	}

	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("component %s: %s: serializing args: %w", r.req.Component, where, err)
	}

	return &spec.Resource{
		ID:   id,
		Type: res.Type,
		Args: raw,
		// 站点覆盖在这里合进去——下发出去的是最终值
		DriftPolicy: spec.EffectiveDriftPolicy(res.DriftPolicy, r.req.DriftPolicy),
		Notify:      res.Notify,
		Origin:      origin,
	}, nil
}

// autoID 为没写 id 的资源造一个稳定标识。
//
// 稳定是硬要求：id 进 digest，也是漂移报告里指认资源的依据。用序号
// 而非内容摘要，是因为前者在诊断时能直接对回 pack.yaml 的位置。
func autoID(res pack.Resource, args map[string]any, origin string, idx int) string {
	for _, k := range []string{"path", "dest", "name"} {
		if v, ok := args[k].(string); ok && v != "" {
			return res.Type + ":" + v
		}
	}
	return fmt.Sprintf("%s:%s[%d]", res.Type, origin, idx)
}

// renderAny 递归渲染任意结构中的字符串。
func (r *run) renderAny(ctx *Ctx, where string, v any) (any, error) {
	switch x := v.(type) {
	case string:
		return r.eng.Expr(where, x, ctx)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, sub := range x {
			s, err := r.renderAny(ctx, where+"."+k, sub)
			if err != nil {
				return nil, err
			}
			out[k] = s
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, sub := range x {
			s, err := r.renderAny(ctx, fmt.Sprintf("%s[%d]", where, i), sub)
			if err != nil {
				return nil, err
			}
			out[i] = s
		}
		return out, nil
	default:
		return v, nil
	}
}

// nodeToAny 把 YAML 节点变成 map / slice / 标量。
func nodeToAny(n *yaml.Node) (any, error) {
	if n == nil || n.Kind == 0 {
		return nil, nil
	}
	var out any
	if err := n.Decode(&out); err != nil {
		return nil, err
	}
	return normalizeKeys(out), nil
}

// normalizeKeys 把 yaml.v3 解出的 map[string]any 统一成可 JSON 序列化的形状。
func normalizeKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, sub := range x {
			out[k] = normalizeKeys(sub)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, sub := range x {
			out[fmt.Sprintf("%v", k)] = normalizeKeys(sub)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, sub := range x {
			out[i] = normalizeKeys(sub)
		}
		return out
	default:
		return v
	}
}

// ── workload / health ───────────────────────────────────────────────────

// renderWorkload 渲染角色的受监管进程声明。
//
// 逐字段渲染而非「整段过一遍模板」：pack 与 spec 两侧的类型并不一一对应
// （health 的 port 在 Pack 里是模板表达式、在规格里是 int），逐字段处理
// 才有地方做这些转换，也才能在出错时说清是哪个字段。
// 第二个返回值仅 compose 用到：那份 compose 文件要作为一条 template
// 资源落到盘上，`docker compose -f` 才读得到（19-container-runtime §6.6.1）。
func (r *run) renderWorkload(ctx *Ctx, it *instance) (*spec.Workload, *spec.Resource, error) {
	rl := r.req.Pack.RoleByName(it.Role)
	if rl == nil || rl.Workload == nil {
		return nil, nil, nil
	}
	w := rl.Workload
	rt := w.Runtime
	if rt == "" {
		rt = pack.RuntimeSystemd
	}
	out := &spec.Workload{Runtime: string(rt)}

	switch rt {
	case pack.RuntimeSystemd:
		if w.Systemd == nil {
			return nil, nil, fmt.Errorf("component %s: role %s has workload.runtime=systemd but is missing a systemd block",
				r.req.Component, it.Role)
		}
		sw, err := r.renderSystemd(ctx, w.Systemd)
		if err != nil {
			return nil, nil, err
		}
		out.Systemd = sw

	case pack.RuntimeDocker:
		b, err := r.renderThroughJSON(ctx, "workload.docker", w.Docker)
		if err != nil {
			return nil, nil, err
		}
		out.Docker = b

	case pack.RuntimeCompose:
		b, res, err := r.renderCompose(ctx, it, w.Compose)
		if err != nil {
			return nil, nil, err
		}
		out.Compose = b
		return out, res, nil

	default:
		return nil, nil, fmt.Errorf("component %s: unknown runtime %q", r.req.Component, rt)
	}
	return out, nil, nil
}

// composeFileName 是渲染产物在配置目录下的固定名字。
//
// 不让 Pack 挑：它是实现产物而非用户配置，而固定名字换来的是「排障时
// 一眼知道去哪看」。
const composeFileName = "compose.yaml"

// renderCompose 渲染 workload.compose，并把 compose 文件本身变成一条资源。
//
// Pack 里 `file` 写的是 templates/ 下的模板名，已解析规格里换成渲染产物的
// **绝对路径**。这个双重含义是有意的：让 Pack 作者再声明一条 template 资源
// 的话，两处必须一致而 lint 又查不了（两边都是模板表达式），写错的表现是
// `compose up` 读到一份过期文件。
func (r *run) renderCompose(
	ctx *Ctx, it *instance, cw *pack.ComposeWorkload,
) (json.RawMessage, *spec.Resource, error) {
	if cw == nil {
		return nil, nil, fmt.Errorf("component %s: role %s has workload.runtime=compose but is missing a compose block",
			r.req.Component, it.Role)
	}
	if strings.TrimSpace(cw.File) == "" {
		return nil, nil, fmt.Errorf("component %s: role %s's workload.compose.file is empty",
			r.req.Component, it.Role)
	}
	if !r.eng.Has(cw.File) {
		return nil, nil, fmt.Errorf("component %s: template %s/%s does not exist",
			r.req.Component, pack.DirTemplates, cw.File)
	}

	confDir := it.paths["config"].First()
	if confDir == "" {
		return nil, nil, fmt.Errorf(
			"component %s: runtime=compose requires paths.config -- the rendered %s needs somewhere to go",
			r.req.Component, composeFileName)
	}
	dest := path.Join(confDir, composeFileName)

	body, err := r.eng.Render(cw.File, ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("component %s@%s: %w", r.req.Component, it.Node.Name, err)
	}

	// 先把 file 换成绝对路径，再整段过模板——顺序反过来的话 dest 会被
	// 当成模板表达式再求值一次
	rendered := *cw
	rendered.File = dest
	b, err := r.renderThroughJSON(ctx, "workload.compose", &rendered)
	if err != nil {
		return nil, nil, err
	}

	args, err := json.Marshal(map[string]any{
		"dest": dest, "content": body, "mode": "0644",
	})
	if err != nil {
		return nil, nil, fmt.Errorf("component %s: serializing compose resource: %w", r.req.Component, err)
	}
	return b, &spec.Resource{
		ID:   pack.ResTemplate + ":" + dest,
		Type: pack.ResTemplate,
		Args: args,
		// compose 文件变了就得重建 project——但那由 spec-digest 驱动，
		// 不靠资源的 notify。这里只要求它与盘上一致。
		//
		// 站点覆盖同样作用于它：不设例外是刻意的，一条「别的都放松了、
		// 唯独这个文件还会被改回去」的规则没人猜得到。
		DriftPolicy: spec.EffectiveDriftPolicy(spec.DriftReconcile, r.req.DriftPolicy),
		Origin:      "role",
	}, nil
}

func (r *run) renderSystemd(ctx *Ctx, w *pack.SystemdWorkload) (*spec.SystemdWorkload, error) {
	f := func(name, expr string) (string, error) {
		return r.eng.Expr("workload.systemd."+name, expr, ctx)
	}
	out := &spec.SystemdWorkload{
		LimitNofile: w.LimitNofile,
	}
	var err error
	for _, fld := range []struct {
		name string
		src  string
		dst  *string
	}{
		{"exec", w.Exec, &out.Exec},
		{"execReload", w.ExecReload, &out.ExecReload},
		{"user", w.User, &out.User},
		{"group", w.Group, &out.Group},
		{"workingDir", w.WorkingDir, &out.WorkingDir},
		{"envFile", w.EnvFile, &out.EnvFile},
		{"restart", w.Restart, &out.Restart},
		{"restartSec", w.RestartSec, &out.RestartSec},
		{"killMode", w.KillMode, &out.KillMode},
		{"timeoutStop", w.TimeoutStop, &out.TimeoutStop},
		{"extraUnit", w.ExtraUnit, &out.ExtraUnit},
	} {
		if *fld.dst, err = f(fld.name, fld.src); err != nil {
			return nil, err
		}
	}
	if len(w.Env) > 0 {
		out.Env = map[string]string{}
		for k, v := range w.Env {
			if out.Env[k], err = f("env."+k, v); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// renderHealth 渲染健康检查声明。
func (r *run) renderHealth(ctx *Ctx, it *instance) (*spec.Health, error) {
	rl := r.req.Pack.RoleByName(it.Role)
	if rl == nil || rl.Health == nil {
		return nil, nil
	}
	h := rl.Health
	out := &spec.Health{
		StartupGrace:     h.StartupGrace,
		Interval:         h.Interval,
		Timeout:          h.Timeout,
		FailureThreshold: h.FailureThreshold,
		SuccessThreshold: h.SuccessThreshold,
	}

	switch {
	case h.HTTP != nil:
		port, err := r.renderPort(ctx, "health.http.port", h.HTTP.Port)
		if err != nil {
			return nil, err
		}
		p, err := r.eng.Expr("health.http.path", h.HTTP.Path, ctx)
		if err != nil {
			return nil, err
		}
		out.HTTP = &spec.HTTPProbe{
			Path: p, Port: port,
			Scheme:       h.HTTP.Scheme,
			ExpectStatus: h.HTTP.ExpectStatus,
		}
	case h.TCP != nil:
		port, err := r.renderPort(ctx, "health.tcp.port", h.TCP.Port)
		if err != nil {
			return nil, err
		}
		out.TCP = &spec.TCPProbe{Port: port}
	case h.Exec != nil:
		cmd, err := r.renderStrings(ctx, "health.exec.command", h.Exec.Command)
		if err != nil {
			return nil, err
		}
		out.Exec = &spec.ExecProbe{Command: cmd}
	default:
		return nil, fmt.Errorf("component %s: role %s's health declares no probes",
			r.req.Component, it.Role)
	}
	return out, nil
}

// renderPort 求值一个端口表达式并转成整数。
func (r *run) renderPort(ctx *Ctx, where, expr string) (int, error) {
	s, err := r.eng.Expr(where, expr, ctx)
	if err != nil {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil {
		return 0, fmt.Errorf("component %s: %s evaluated to %q, not a port number",
			r.req.Component, where, s)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("component %s: %s evaluated to %d, out of range 1-65535",
			r.req.Component, where, n)
	}
	return n, nil
}

// renderThroughJSON 把一个结构体过一遍模板渲染，产出 JSON。
// 用于 docker / compose 这两个 M4 才会真正消费的段。
func (r *run) renderThroughJSON(ctx *Ctx, where string, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	rendered, err := r.renderAny(ctx, where, m)
	if err != nil {
		return nil, err
	}
	return json.Marshal(rendered)
}

// ── hooks ───────────────────────────────────────────────────────────────

// renderHooks 挑出本实例要执行的 hook。
//
// scope 与 when 都在这里求值完毕：**scope:once 已执行过的、when 为 false 的
// 都不下发**，mechlet 收到就执行。仲裁必须在 mechd——只有它同时看得见
// 一个 Component 的全部实例（19-hooks）。
func (r *run) renderHooks(ctx *Ctx, it *instance) ([]spec.Hook, error) {
	var out []spec.Hook

	collect := func(hs pack.Hooks) error {
		var perr error
		hs.All(func(point string, hk pack.Hook) {
			if perr != nil {
				return
			}
			if strings.TrimSpace(hk.When) != "" {
				s, err := r.eng.Expr("hooks."+point+".when", hk.When, ctx)
				if err != nil {
					perr = err
					return
				}
				if !isTruthy(s) {
					return
				}
			}
			if hk.EffectiveScope() == pack.ScopeOnce {
				key := point + "/" + hk.Script
				if r.req.DoneOnce[key] {
					return
				}
				// 同一个 Component 内只跑一次：交给 ordinal 最小的实例。
				// 用 ordinal 而非「第一个遍历到的」，是因为前者稳定——
				// 重跑一次解析不该换一台机器执行。
				if !r.isOnceOwner(it) {
					return
				}
			}
			args, err := r.renderStrings(ctx, "hooks."+point+".args", hk.Args)
			if err != nil {
				perr = err
				return
			}
			out = append(out, spec.Hook{
				Point:   point,
				Script:  pack.HookScriptPath(hk.Script),
				Args:    args,
				Timeout: orDefault(hk.Timeout, hs.Timeout),
				User:    hk.User,
			})
		})
		return perr
	}

	if err := collect(r.req.Pack.Hooks); err != nil {
		return nil, err
	}
	if rl := r.req.Pack.RoleByName(it.Role); rl != nil {
		if err := collect(rl.Hooks); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// isOnceOwner 报告该实例是否是本角色里 ordinal 最小的那个。
func (r *run) isOnceOwner(it *instance) bool {
	lowest := it.Ordinal
	for _, other := range r.req.Instances {
		if other.Role == it.Role && other.Ordinal < lowest {
			lowest = other.Ordinal
		}
	}
	return lowest == it.Ordinal
}

func (r *run) renderStrings(ctx *Ctx, where string, in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		v, err := r.eng.Expr(fmt.Sprintf("%s[%d]", where, i), s, ctx)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}
