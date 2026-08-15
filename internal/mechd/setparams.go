package mechd

import (
	"context"
	"fmt"
	"sort"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/render"
	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/store"
	"github.com/mecharion/mecharion/internal/vault"
)

// SetParamsRequest 是一次「只改参数」的请求（23-web-ui §4.3 ①）。
//
// **它刻意不带节点列表。** `Deploy` 要求指定节点并重算放置，于是一次
// 「把日志级别改成 debug」被迫重述整个拓扑——而重述拓扑时少写一个节点名
// 就是一次缩容。`--allow-remove` 拦得住，但那道闸门是给「确实要缩容」
// 准备的，让日常改配置反复去撞它，迟早有人习惯性地把它加上。
type SetParamsRequest struct {
	Site      string
	Component string
	// Set 是要改的参数。**只合并，不替换**：没提到的参数保持原值。
	//
	// 整体替换会让「改一个参数」变成「把没提到的全部恢复默认」，
	// 而两者在请求体上长得几乎一样。
	Set map[string]any
	// Unset 是要恢复默认的参数名。
	//
	// 与「Set 成 null」分开：一个参数的合法取值可以**就是** null，
	// 而「删掉这个覆盖」是另一件事。合并成一件迟早会撞上。
	Unset []string

	// Role / Group 指定改哪一层。两个都空 = Component 级。
	//
	// **Group 非空时 Role 必填**：组名只在 (Component, Role) 内唯一，
	// 少了角色这个坐标，两个角色下的同名组无从区分。
	Role  string
	Group string
	// Node 是「只改这一台」的简写：自动建一个只含它的组 node-<名字>
	// （ADR-0021 用它抵消强制建组的摩擦）。与 Group 互斥。
	Node string
	// DryRun 只算不落库，用于保存前的预览。
	DryRun bool
	Actor  string
}

// SetParamsResult 是一次参数变更的结果。
type SetParamsResult struct {
	Component string `json:"component"`
	// Changed 是 digest 真的变了的实例。
	Changed []ParamChange `json:"changed,omitempty"`
	// Effect 是合起来会发生什么：restart | reload | none。
	//
	// 它必须是**这一次改动的合并结论**，不是逐参数的标志——用户问的是
	// 「我点下去会发生什么」，而那是所有改动的并集。
	Effect string `json:"effect"`
	// Restarted / Reloaded 是触发它的参数名，用来解释 Effect 的由来。
	Restarted []string `json:"restarted,omitempty"`
	Reloaded  []string `json:"reloaded,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
	DryRun    bool     `json:"dryRun"`
	// CreatedGroup 非空表示这次顺手建了一个只含一台机器的配置组。
	//
	// **必须告诉用户**：他敲的是「改这一台的参数」，而模型里不存在无名的
	// per-node 覆盖（ADR-0021），于是多出来一个对象。静默创建会让
	// `config group list` 里凭空冒出一堆 node-* 组，而没人记得它们是怎么来的。
	CreatedGroup string `json:"createdGroup,omitempty"`
}

// ParamChange 是一个实例上的变化。
type ParamChange struct {
	Role string `json:"role"`
	Node string `json:"node"`
	From string `json:"from"`
	To   string `json:"to"`
}

// 变更的合并效果。
const (
	EffectNone    = "none"
	EffectReload  = "reload"
	EffectRestart = "restart"
)

// SetParams 改一个已部署组件的参数。
//
// 顺序上有一条硬要求：**先算后写**。
//
//	① 合并出新的参数集
//	② 用它跑一遍解析——非法取值在这里被拒
//	③ 解析过了才落库
//	④ 落库之后才下发
//
// 反过来（先落库再校验）会留下一个「库里是非法值、机器上是旧值」的中间
// 状态，而那个状态下任何一次调和都会失败，且失败原因指向的是库里那份
// 谁也没审过的数据。
func (s *Service) SetParams(
	ctx context.Context, req SetParamsRequest,
) (*SetParamsResult, error) {
	site, comp, err := s.componentOfForWrite(ctx, req.Site, req.Component)
	if err != nil {
		return nil, err
	}
	entry, err := s.Packs.Resolve(comp.Pack.Name, "="+comp.Pack.Version)
	if err != nil {
		return nil, faults.Permanentf("", "parsing Pack %s@%s: %w",
			comp.Pack.Name, comp.Pack.Version, err)
	}
	p := entry.Pack

	if len(req.Set) == 0 && len(req.Unset) == 0 {
		return nil, faults.Permanentf("changing config", "no parameters to change")
	}
	if req.Group != "" && req.Role == "" {
		return nil, faults.Permanentf("changing config",
			"config group %q given but no role -- a group name is only unique within (component, role)", req.Group)
	}
	if req.Group != "" && req.Node != "" {
		return nil, faults.Permanentf("changing config", "--group and --node cannot both be given")
	}
	if req.Node != "" && req.Role == "" {
		return nil, faults.Permanentf("changing config",
			"--node requires --role: a machine can run multiple roles of the same component")
	}

	insts, byID, err := s.existingInstances(ctx, comp.ID)
	if err != nil {
		return nil, err
	}
	if len(insts) == 0 {
		return nil, faults.Permanentf("changing config", "component %s has no instances", comp.Name)
	}
	inputs, err := s.freezeFacts(ctx, comp.ID, assignmentsOf(insts, byID), insts, false)
	if err != nil {
		return nil, err
	}
	current, err := s.Repos.ConfigGroups().List(ctx, comp.ID)
	if err != nil {
		return nil, err
	}

	// ── ① 算出改完之后是什么样 ──
	//
	// 三种落点：Component 级、某个具名组、或「只改这一台」（自动建组）。
	// 三条都只是**产出一份新的期望状态**，不落库——落库在第 ③ 步。
	nextComp, nextGroups, created, err := s.applyEdit(comp, current, req, insts, byID)
	if err != nil {
		return nil, err
	}

	// ── ② 用旧的与新的各解析一遍 ──
	//
	// 不能拿库里存着的 digest 当基线：那是**上次下发时**算的，中间可能有
	// Pack 重新加载、事实快照变化。两遍都在此刻算，比较的才是同一时刻的
	// 两个假设。
	//
	// 两遍都用 previewSecrets：读真密钥、不写任何东西。一次性随机密钥会让
	// From 报出一个集群上从来不存在的 digest（见该类型的注释）。
	preview := previewSecrets{real: vault.NewRenderStore(ctx, s.Vault, comp.ID)}

	before, err := s.renderWithGroups(ctx, site, comp, p, inputs, true, preview, current)
	if err != nil {
		return nil, faults.Permanentf("", "parsing current parameters: %w", err)
	}
	after, err := s.renderWithGroups(ctx, site, nextComp, p, inputs, true, preview, nextGroups)
	if err != nil {
		// 非法取值走到这里。错误里已经带了「哪个参数、为什么不合法」
		// （render 的 resolveStaticParams 负责措辞）
		return nil, faults.Permanentf("changing config", "%v", err)
	}

	out := &SetParamsResult{
		Component: comp.Name, DryRun: req.DryRun,
		Warnings: after.Warnings, CreatedGroup: created,
	}
	for _, key := range after.Order {
		a, b := after.Specs[key], before.Specs[key]
		if a == nil || b == nil || a.Digest == b.Digest {
			continue
		}
		out.Changed = append(out.Changed, ParamChange{
			Role: a.Role, Node: a.Node.Name, From: b.Digest, To: a.Digest,
		})
	}

	out.Restarted, out.Reloaded = s.effectOf(p, comp.Profile, touched(req), before, after)
	switch {
	case len(out.Restarted) > 0:
		out.Effect = EffectRestart
	case len(out.Reloaded) > 0:
		out.Effect = EffectReload
	default:
		out.Effect = EffectNone
	}

	if req.DryRun {
		return out, nil
	}
	// 一个字节都没变就别落库：一次 no-op 保存不该在审计里留下
	// 「改过配置」，也不该唤醒任何节点
	if len(out.Changed) == 0 {
		return out, nil
	}

	// ── ③ 落库 ──
	if req.Group != "" || req.Node != "" {
		g := findGroup(nextGroups, req.Role, out.targetGroup(req))
		if g == nil {
			return nil, fmt.Errorf("internal error: target config group not found")
		}
		if _, err := s.Repos.ConfigGroups().Upsert(ctx, *g); err != nil {
			return nil, err
		}
	} else if _, err := s.persistComponent(ctx, nextComp, true); err != nil {
		return nil, err
	}

	s.audit(ctx, req.Actor, "config.set", comp.Name, p, "ok")
	s.event(ctx, site.ID, "component.params.changed", comp.Name, map[string]any{
		"params": sortedNames(req.Set), "unset": req.Unset,
		"role": req.Role, "group": out.targetGroup(req),
		"effect": out.Effect, "instances": len(out.Changed),
	})

	// ── ④ 下发 ──
	s.notifyNodes(assignmentsOf(insts, byID))
	return out, nil
}

// targetGroup 返回这次改动落在哪个组上（Component 级时为空）。
func (r *SetParamsResult) targetGroup(req SetParamsRequest) string {
	if req.Group != "" {
		return req.Group
	}
	if req.Node != "" {
		return autoGroupName(req.Node)
	}
	return ""
}

// autoGroupName 是 `--node n7` 自动建的组名（ADR-0021）。
func autoGroupName(node string) string { return "node-" + node }

func findGroup(groups []store.ConfigGroup, role, name string) *store.ConfigGroup {
	for i := range groups {
		if groups[i].Role == role && groups[i].Name == name {
			return &groups[i]
		}
	}
	return nil
}

// applyEdit 把一次编辑套到当前状态上，返回改完之后的样子。
//
// **只算不写。** 返回的 created 非空表示这次顺手建了一个只含一台机器的
// 组——那要告诉用户，不能静默（ADR-0021 的自动建组）。
func (s *Service) applyEdit(
	comp store.Component, current []store.ConfigGroup, req SetParamsRequest,
	insts []store.RoleInstance, byID map[int64]store.Node,
) (store.Component, []store.ConfigGroup, string, error) {
	merge := func(base map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range req.Set {
			out[k] = v
		}
		for _, k := range req.Unset {
			delete(out, k)
		}
		return out
	}

	// Component 级：最常见的情形
	if req.Group == "" && req.Node == "" {
		next := comp
		next.Params = merge(comp.Params)
		return next, current, "", nil
	}

	name := req.Group
	created := ""
	if req.Node != "" {
		name = autoGroupName(req.Node)
		// 那台机器上必须真的有这个角色的实例，否则这条覆盖永远不生效
		ok := false
		for _, ri := range insts {
			if ri.Role == req.Role && byID[ri.NodeID].Name == req.Node {
				ok = true
			}
		}
		if !ok {
			return comp, nil, "", faults.Permanentf("changing config",
				"node %s has no %s %s instance", req.Node, comp.Name, req.Role)
		}
	}

	target := findGroup(current, req.Role, name)
	next := make([]store.ConfigGroup, 0, len(current)+1)
	for _, g := range current {
		if g.Role == req.Role && g.Name == name {
			continue
		}
		next = append(next, g)
	}

	if target == nil {
		if req.Group != "" {
			return comp, nil, "", faults.Permanentf("changing config",
				"role %s has no config group %q\n"+
					"  → create it first: mechctl config group create %s -c %s -r %s --nodes ...",
				req.Role, req.Group, req.Group, comp.Name, req.Role)
		}
		// --node 且组不存在：建一个只含这台机器的组
		created = name
		next = append(next, store.ConfigGroup{
			ComponentID: comp.ID, Role: req.Role, Name: name,
			Members: []string{req.Node}, Params: merge(nil),
		})
		return comp, next, created, nil
	}

	g := *target
	g.Params = merge(g.Params)
	next = append(next, g)
	return comp, next, "", nil
}

// touched 是这次动过的参数名集合。
func touched(req SetParamsRequest) map[string]bool {
	out := map[string]bool{}
	for k := range req.Set {
		out[k] = true
	}
	for _, k := range req.Unset {
		out[k] = true
	}
	return out
}

// effectOf 算出这次改动合起来会发生什么。
//
// **判据是「参数声明了 restartRequired」且「它真的变了」**，两条都要。
// 只看声明的话，一次把参数改成它原本的值也会报「会重启」；只看变化的话，
// 一个没有任何标志的参数变了会被算成不需要动——而那两种错的方向相反：
// 前者让人白紧张一次，后者让服务在用户以为安全时被重启。
//
// 声明按**角色**查：同一个参数名可以只在某个角色下声明，标志也可能不同。
func (s *Service) effectOf(
	p *pack.Pack, profile string, changed map[string]bool,
	before, after *render.Result,
) (restart, reload []string) {
	decls := map[string]map[string]render.FormParam{}
	declsOf := func(role string) map[string]render.FormParam {
		if m, ok := decls[role]; ok {
			return m
		}
		m := map[string]render.FormParam{}
		for _, f := range render.Form(render.FormRequest{
			Pack: p, Profile: profile, Role: role,
		}) {
			m[f.Name] = f
		}
		decls[role] = m
		return m
	}

	// 逐实例比对：同一个参数在不同实例上可能只有一部分变了
	hitRestart, hitReload := map[string]bool{}, map[string]bool{}
	for _, key := range after.Order {
		a, b := after.Specs[key], before.Specs[key]
		if a == nil || b == nil {
			continue
		}
		d := declsOf(a.Role)
		for name := range changed {
			if !paramMoved(a, b, name) {
				continue
			}
			switch {
			case d[name].RestartRequired:
				hitRestart[name] = true
			case d[name].ReloadRequired:
				hitReload[name] = true
			}
		}
	}
	return sortedKeys(hitRestart), sortedKeys(hitReload)
}

// paramMoved 报告某个参数在两份规格之间是否真的变了。
func paramMoved(a, b *spec.ResolvedSpec, name string) bool {
	av, aok := a.Params[name]
	bv, bok := b.Params[name]
	if aok != bok {
		return true
	}
	if !aok {
		return false
	}
	// sensitive 的值本来就没带出来，两边都是空——只能靠 digest 判。
	// 这里保守地认为它变了：一次多余的「会重启」提示，好过漏报。
	if av.Sensitive || bv.Sensitive {
		return a.Digest != b.Digest
	}
	return fmt.Sprint(av.Value) != fmt.Sprint(bv.Value)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedNames(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
