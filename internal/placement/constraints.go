package placement

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/pack"
)

// checkConstraints 校验 Pack 声明的放置约束（spec §12）。
//
// 约束表达的是**角色之间的位置关系**，因此校验对象是「本次放置后的全部
// 实例」——Keep 与 Add 合起来，而不只是新增的那些。只看新增会漏掉
// 「新加的 DataNode 与已有的 NameNode 撞在一起」这类情况。
func checkConstraints(in Input, plan *Plan, roles []pack.EffectiveRole) error {
	all := append(append([]Assignment{}, plan.Keep...), plan.Add...)
	if len(all) == 0 {
		return nil
	}

	enabled := map[string]bool{}
	for _, r := range roles {
		enabled[r.Name] = r.Enabled
	}

	for _, c := range in.Pack.PlacementForProfile(in.Profile) {
		if err := checkOne(in, c, all, enabled, &plan.Warnings); err != nil {
			return err
		}
	}
	return nil
}

func checkOne(
	in Input, c pack.Placement, all []Assignment,
	enabled map[string]bool, warnings *[]string,
) error {
	scope := c.EffectiveScope()
	required := c.EffectiveEnforcement() != pack.EnforcePreferred

	// 引用了本形态下已关闭的角色时，约束**静默失效**——这在 lint 阶段
	// 由 R18 拦（同 Pack 内可见）；这里再兜一次，因为形态是部署时才定的。
	roles := c.AntiAffinity
	kind := "antiAffinity"
	if len(c.Affinity) > 0 {
		roles, kind = c.Affinity, "affinity"
	}
	for _, r := range roles {
		if on, known := enabled[r]; known && !on {
			return nil
		}
	}

	// 按 scope 把实例分组：scope=node 用节点名，否则用节点上该 label 的值
	groups, err := groupByScope(in, all, roles, scope, required, warnings, c)
	if err != nil {
		return err
	}

	if kind == "antiAffinity" {
		return checkAnti(in, c, roles, scope, groups, required, warnings)
	}
	return checkAffinity(in, c, roles, scope, groups, required, warnings)
}

// bucket 是一个 scope 分组内的实例。
type bucket struct {
	key     string
	members []Assignment
}

func groupByScope(
	in Input, all []Assignment, roles []string, scope string,
	required bool, warnings *[]string, c pack.Placement,
) ([]bucket, error) {
	want := map[string]bool{}
	for _, r := range roles {
		want[r] = true
	}

	byKey := map[string][]Assignment{}
	for _, a := range all {
		if !want[a.Role] {
			continue
		}
		key := a.Node.Name
		if scope != "node" {
			v, ok := a.Node.Labels[scope]
			if !ok {
				// **无法验证不等于通过**：required 时报错，preferred 时告警
				msg := fmt.Sprintf("node %s is missing label %q, cannot validate constraint %s",
					a.Node.Name, scope, describe(c))
				if required {
					return nil, faults.Permanentf("", "placement validation failed: %s\n  %s\n%s",
						in.Component, msg, reasonLine(c))
				}
				*warnings = append(*warnings, msg)
				continue
			}
			key = v
		}
		byKey[key] = append(byKey[key], a)
	}

	out := make([]bucket, 0, len(byKey))
	for _, k := range sortedKeys(byKey) {
		out = append(out, bucket{key: k, members: byKey[k]})
	}
	return out, nil
}

// checkAnti 校验反亲和。
//
// 两种形式：多角色时两两不得同处一个 scope；单角色时该角色的多个实例
// 不得同处一个 scope。
func checkAnti(
	in Input, c pack.Placement, roles []string, scope string,
	groups []bucket, required bool, warnings *[]string,
) error {
	single := len(roles) == 1

	for _, g := range groups {
		var offenders []Assignment
		if single {
			if len(g.members) > 1 {
				offenders = g.members
			}
		} else {
			seen := map[string]bool{}
			for _, m := range g.members {
				seen[m.Role] = true
			}
			if len(seen) > 1 {
				offenders = g.members
			}
		}
		if len(offenders) == 0 {
			continue
		}

		msg := conflictReport(in.Component, c, scope, g, offenders)
		if required {
			return faults.Permanentf("", "%s", msg)
		}
		*warnings = append(*warnings, msg)
	}
	return nil
}

// checkAffinity 校验亲和：列出的角色必须落在同一个 scope。
func checkAffinity(
	in Input, c pack.Placement, roles []string, scope string,
	groups []bucket, required bool, warnings *[]string,
) error {
	// 每个角色出现在哪些分组里
	where := map[string]map[string]bool{}
	for _, g := range groups {
		for _, m := range g.members {
			if where[m.Role] == nil {
				where[m.Role] = map[string]bool{}
			}
			where[m.Role][g.key] = true
		}
	}

	// 只在**全部**被约束的角色都有实例时才检查——某个角色没部署时
	// 谈不上「必须同处」
	for _, r := range roles {
		if len(where[r]) == 0 {
			return nil
		}
	}

	var lines []string
	base := ""
	violated := false
	for _, r := range roles {
		keys := sortedKeys(where[r])
		lines = append(lines, fmt.Sprintf("    %-18s → %s", r, strings.Join(keys, ", ")))
		if len(keys) != 1 {
			violated = true
			continue
		}
		if base == "" {
			base = keys[0]
		} else if keys[0] != base {
			violated = true
		}
	}
	if !violated {
		return nil
	}

	msg := fmt.Sprintf("placement validation failed: %s\n  constraint  %s  scope=%s  (%s)\n%s\n%s",
		in.Component, describe(c), scope, enforcementName(c),
		strings.Join(lines, "\n"), reasonLine(c))
	if required {
		return faults.Permanentf("", "%s", msg)
	}
	*warnings = append(*warnings, msg)
	return nil
}

// conflictReport 生成 spec §12 规定格式的错误信息。
//
// **必须指名到实例并带上 Pack 作者写的 reason**——那是现场唯一能解释
// 「为什么不让我这么放」的东西。
func conflictReport(
	component string, c pack.Placement, scope string,
	g bucket, offenders []Assignment,
) string {
	sorted := append([]Assignment(nil), offenders...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Role != sorted[j].Role {
			return sorted[i].Role < sorted[j].Role
		}
		return sorted[i].Node.Name < sorted[j].Node.Name
	})

	var b strings.Builder
	fmt.Fprintf(&b, "placement validation failed: %s\n", component)
	fmt.Fprintf(&b, "  constraint  %s  scope=%s  (%s)\n",
		describe(c), scope, enforcementName(c))
	for i, a := range sorted {
		mark := ""
		if i == len(sorted)-1 {
			mark = "    ← conflict"
		}
		fmt.Fprintf(&b, "    %-18s → %s%s\n", a.Role, a.Node.Name, mark)
	}
	if scope != "node" {
		fmt.Fprintf(&b, "    (co-located at %s=%s)\n", scope, g.key)
	}
	b.WriteString(reasonLine(c))
	return b.String()
}

func describe(c pack.Placement) string {
	if len(c.Affinity) > 0 {
		return "affinity[" + strings.Join(c.Affinity, ", ") + "]"
	}
	return "antiAffinity[" + strings.Join(c.AntiAffinity, ", ") + "]"
}

func enforcementName(c pack.Placement) string {
	if c.EffectiveEnforcement() == pack.EnforcePreferred {
		return "preferred"
	}
	return "required"
}

// reasonLine 把 Pack 作者写的理由带进错误信息。
func reasonLine(c pack.Placement) string {
	if strings.TrimSpace(c.Reason) == "" {
		return "  (this constraint has no reason declared — the Pack author should add one, " +
			"it's the only thing on site that explains \"why\")"
	}
	return "  reason: " + c.Reason
}

// ── 依赖的 scope 校验（spec §5.1） ──────────────────────────────────────

// DepPresence 描述一个依赖在各节点上的存在情况。
type DepPresence struct {
	// Name 是依赖的 Pack 名。
	Name string
	// Scope 是 node 或 site。
	Scope pack.DepScope
	// NodesWithIt 是已装有该依赖的节点 ID 集合，仅 scope:node 时需要。
	NodesWithIt map[int64]bool
	// PresentInSite 仅 scope:site 时使用。
	PresentInSite bool
}

// CheckDependencies 校验依赖的 scope 要求。
//
// 拆成独立函数而非并进 Compute，是因为它需要**站点范围**的数据（别的
// Component 装在哪些节点上），那是部署编排才持有的上下文。
func CheckDependencies(component string, plan *Plan, deps []DepPresence) error {
	if len(deps) == 0 {
		return nil
	}
	all := append(append([]Assignment{}, plan.Keep...), plan.Add...)

	for _, d := range deps {
		switch d.Scope {
		case pack.ScopeSite:
			if !d.PresentInSite {
				return faults.Permanentf("",
					"placement validation failed: %s\n  dependency %s (scope: site) does not exist in this Site\n"+
						"  → deploy it first, or specify an existing instance with --require %s=<component>",
					component, d.Name, d.Name)
			}
		default:
			// scope: node —— 本组件的**每个**实例所在节点上都必须有它
			var missing []string
			seen := map[string]bool{}
			for _, a := range all {
				if d.NodesWithIt[a.Node.ID] || seen[a.Node.Name] {
					continue
				}
				seen[a.Node.Name] = true
				missing = append(missing, a.Node.Name)
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				return faults.Permanentf("",
					"placement validation failed: %s\n  dependency %s (scope: node) is missing on these nodes: %s\n"+
						"  → deploy %s on these nodes first, or move this component's instances to nodes that already have it",
					component, d.Name, strings.Join(missing, ", "), d.Name)
			}
		}
	}
	return nil
}
