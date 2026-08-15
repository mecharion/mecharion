// Package placement 把「用户说要装在哪些节点上」变成一份 RoleInstance 列表，
// 并校验它没有违反 Pack 声明的约束。
//
// 设计见 docs/design/14-placement.md。
//
// **Mecharion 没有调度器**：节点由用户显式指定，`placement` 是**校验规则**
// 而非调度输入。这条边界让整个环节从「约束求解」降为「约束检查」——前者是
// NP 问题且失败时无从解释，后者是遍历且失败时能精确指出是哪两个实例冲突。
package placement

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/store"
)

// Input 是一次放置请求。
type Input struct {
	// Component 是组件名，只用于错误信息。
	Component string
	Pack      *pack.Pack
	Profile   string

	// Nodes 是用户指定的 role → 节点。未出现的角色按 cardinality 下限判定：
	// 下限为 0 则不部署，≥1 则拒绝并列出缺什么。
	Nodes map[string][]store.Node

	// Existing 是该组件已有的实例。**必须传**——一个只看 Nodes 的实现
	// 不可能正确：已有实例的 ordinal 要原样保留（ADR-0028）。
	Existing []store.RoleInstance
	// NodeByID 把 Existing 里的 NodeID 映射回节点，用于差异计算与错误信息。
	NodeByID map[int64]store.Node
}

// Assignment 是一个角色实例的落点。
type Assignment struct {
	Role string
	Node store.Node
	// Ordinal 在 Keep 中是已固化的值；在 Add 中为 -1，提交时由
	// store.InstanceRepo.Ensure 分配。
	Ordinal int
}

// Plan 是放置的结果。
type Plan struct {
	// Keep 是保持不动的实例，ordinal 原样。
	Keep []Assignment
	// Add 是新增的实例，ordinal 待分配。
	Add []Assignment
	// Remove 是本次不再需要的实例。**移除是危险动作**，由调用方决定
	// 是否需要显式确认。
	Remove []store.RoleInstance
	// MaxUnavailable 是声明了 quorum 的角色的滚动并发上限。
	MaxUnavailable map[string]int
	// Warnings 是不阻断的问题（preferred 约束未满足、偶数仲裁等）。
	Warnings []string
}

// Total 返回本次放置后的实例总数。
func (p *Plan) Total() int { return len(p.Keep) + len(p.Add) }

// Compute 计算放置结果并做全部校验。
//
// **任何一条校验不过就整体拒绝，不做部分部署**——一个「一半装上了」的
// 组件比没装更难收拾。
func Compute(in Input) (*Plan, error) {
	if in.Pack == nil {
		return nil, faults.Permanentf("", "placement: missing Pack")
	}
	roles := in.Pack.RolesForProfile(in.Profile)

	if err := in.checkUnknownRoles(roles); err != nil {
		return nil, err
	}
	if err := in.checkDuplicateNodes(roles); err != nil {
		return nil, err
	}
	if err := in.checkCardinality(roles); err != nil {
		return nil, err
	}

	plan := in.diff(roles)

	if err := checkConstraints(in, plan, roles); err != nil {
		return nil, err
	}
	applyQuorum(plan, roles, &plan.Warnings)
	return plan, nil
}

// checkUnknownRoles 拦住指向不存在（或本形态下已关闭）角色的节点列表。
func (in Input) checkUnknownRoles(roles []pack.EffectiveRole) error {
	known := map[string]pack.EffectiveRole{}
	for _, r := range roles {
		known[r.Name] = r
	}
	for _, name := range sortedKeys(in.Nodes) {
		r, ok := known[name]
		if !ok {
			return faults.Permanentf("", "placement validation failed: %s\n  role %q does not exist\n  defined roles: %s",
				in.Component, name, strings.Join(roleNames(roles), ", "))
		}
		if !r.Enabled && len(in.Nodes[name]) > 0 {
			return faults.Permanentf("",
				"placement validation failed: %s\n  role %q is disabled under profile %q, cannot assign nodes to it",
				in.Component, name, in.Profile)
		}
	}
	return nil
}

// checkDuplicateNodes 拦住同一角色的节点列表里出现重复。
//
// 一个角色在一台机器上只能有一个实例（`role_instances` 的唯一约束）。
// 不拦的话后果是**静默的不一致**：cardinality 按 2 算过了关，提交时
// `Ensure` 却因为已存在而只建出 1 个——计划说要两个，实际一个，
// 而没有任何地方报错。
func (in Input) checkDuplicateNodes(roles []pack.EffectiveRole) error {
	for _, r := range roles {
		seen := map[int64]string{}
		for _, n := range in.Nodes[r.Name] {
			if prev, dup := seen[n.ID]; dup {
				return faults.Permanentf("",
					"placement validation failed: %s\n  %s appears more than once in role %s's node list\n"+
						"  a role can only have one instance per machine; "+
						"deploy a second Component if you need multiple instances on the same machine",
					in.Component, r.Name, prev)
			}
			seen[n.ID] = n.Name
		}
	}
	return nil
}

// checkCardinality 校验每个角色的实例数。
func (in Input) checkCardinality(roles []pack.EffectiveRole) error {
	for _, r := range roles {
		if !r.Enabled {
			continue
		}
		nodes := in.Nodes[r.Name]
		lo, hi, ok := pack.CardinalityBounds(r.Cardinality)
		if !ok {
			// cardinality 写法非法由 lint 的 R11 拦；这里兜底放行，
			// 免得一个 Pack 的笔误让放置报出难懂的错误
			continue
		}

		n := len(nodes)
		if n == 0 && lo == 0 {
			continue // 可选角色，不部署
		}
		if n < lo {
			return faults.Permanentf("",
				"placement validation failed: %s\n  role %s requires cardinality %q, but only %d node(s) were given%s",
				in.Component, r.Name, r.Cardinality, n, listOf(nodes))
		}
		if hi >= 0 && n > hi {
			return faults.Permanentf("",
				"placement validation failed: %s\n  role %s requires cardinality %q, but %d node(s) were given%s",
				in.Component, r.Name, r.Cardinality, n, listOf(nodes))
		}
	}
	return nil
}

// diff 计算保留 / 新增 / 移除三个集合。
func (in Input) diff(roles []pack.EffectiveRole) *Plan {
	plan := &Plan{MaxUnavailable: map[string]int{}}

	// 已有实例按 (role, nodeID) 索引
	type key struct {
		role string
		node int64
	}
	existing := map[key]store.RoleInstance{}
	for _, ri := range in.Existing {
		existing[key{ri.Role, ri.NodeID}] = ri
	}

	wanted := map[key]bool{}
	for _, r := range roles {
		if !r.Enabled {
			continue
		}
		for _, n := range in.Nodes[r.Name] {
			k := key{r.Name, n.ID}
			wanted[k] = true
			if ri, ok := existing[k]; ok {
				// **已有实例的 ordinal 原样保留**，与节点名、成员集合都无关
				plan.Keep = append(plan.Keep, Assignment{
					Role: r.Name, Node: n, Ordinal: ri.Ordinal,
				})
				continue
			}
			plan.Add = append(plan.Add, Assignment{Role: r.Name, Node: n, Ordinal: -1})
		}
	}

	for _, ri := range in.Existing {
		if !wanted[key{ri.Role, ri.NodeID}] {
			plan.Remove = append(plan.Remove, ri)
		}
	}

	sortAssignments(plan.Keep)
	sortAssignments(plan.Add)
	sort.Slice(plan.Remove, func(i, j int) bool {
		if plan.Remove[i].Role != plan.Remove[j].Role {
			return plan.Remove[i].Role < plan.Remove[j].Role
		}
		return plan.Remove[i].Ordinal < plan.Remove[j].Ordinal
	})
	return plan
}

// applyQuorum 处理声明了 quorum 的角色。
//
// 这是放置阶段**唯一影响后续流程**的输出：只有此刻才同时知道「角色声明了
// 仲裁」与「实际有几个实例」。
func applyQuorum(plan *Plan, roles []pack.EffectiveRole, warnings *[]string) {
	counts := map[string]int{}
	for _, a := range plan.Keep {
		counts[a.Role]++
	}
	for _, a := range plan.Add {
		counts[a.Role]++
	}

	for _, r := range roles {
		if !r.Quorum || !r.Enabled {
			continue
		}
		n := counts[r.Name]
		if n == 0 {
			continue
		}
		// 3 节点 ZK 并发下线 2 个会直接失去多数派
		plan.MaxUnavailable[r.Name] = max((n-1)/2, 0)
		if n%2 == 0 {
			*warnings = append(*warnings, fmt.Sprintf(
				"role %s declares quorum, but has an even instance count (%d) — "+
					"an even count doesn't improve fault tolerance and raises split-brain risk", r.Name, n))
		}
	}
}

// ── 辅助 ────────────────────────────────────────────────────────────────

func sortAssignments(as []Assignment) {
	sort.Slice(as, func(i, j int) bool {
		if as[i].Role != as[j].Role {
			return as[i].Role < as[j].Role
		}
		return as[i].Node.Name < as[j].Node.Name
	})
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func roleNames(roles []pack.EffectiveRole) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, r.Name)
	}
	return out
}

func listOf(nodes []store.Node) string {
	if len(nodes) == 0 {
		return ""
	}
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	return ": " + strings.Join(names, ", ")
}
