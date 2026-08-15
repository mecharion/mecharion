package mechd

import (
	"context"
	"sort"
	"strings"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/render"
	"github.com/mecharion/mecharion/internal/store"
	"github.com/mecharion/mecharion/internal/vault"
)

// ConfigGroup 的完整链路（23-web-ui §4.4）。
//
// 表、解析链、渲染路径、多盘绑定的求值代码从更早的里程碑起就在，
// 缺的一直是**创建入口**。这个文件补的就是那一件事。

// GroupRequest 是建组 / 改组的入参。
type GroupRequest struct {
	Site      string
	Component string
	Role      string
	Name      string
	// Members 是节点名单。**显式，不是标签选择器**（§4.4.1）：
	// 成员变更会触发重渲染与可能的重启，而标签选择器意味着
	// 「给节点打个标签」这个动作会悄悄重启一批服务。
	Members []string
	// Params 是这一组的参数覆盖。
	Params map[string]any
	// Paths 是按卷名的多盘绑定：路径名 → 卷名列表（spec §8.6）。
	Paths map[string][]string

	DryRun bool
	Actor  string
}

// GroupDetail 是一个配置组的完整样子。
type GroupDetail struct {
	Name    string              `json:"name"`
	Role    string              `json:"role"`
	Members []string            `json:"members"`
	Params  map[string]any      `json:"params,omitempty"`
	Paths   map[string][]string `json:"paths,omitempty"`
}

// ListGroups 列出一个组件的全部配置组。
func (s *Service) ListGroups(
	ctx context.Context, siteName, compName, role string,
) ([]GroupDetail, error) {
	_, comp, err := s.componentOf(ctx, siteName, compName)
	if err != nil {
		return nil, err
	}
	groups, err := s.Repos.ConfigGroups().List(ctx, comp.ID)
	if err != nil {
		return nil, err
	}
	out := make([]GroupDetail, 0, len(groups))
	for _, g := range groups {
		if role != "" && g.Role != role {
			continue
		}
		out = append(out, GroupDetail{
			Name: g.Name, Role: g.Role, Members: g.Members,
			Params: g.Params, Paths: g.Paths,
		})
	}
	return out, nil
}

// SaveGroup 建组或改组。
//
// 三条校验，**全部在写入之前**：
//
//	① 角色存在，成员节点都在册且真的跑着这个角色的实例
//	② 成员不与本角色下的**其它**组重叠
//	③ paths 绑定的卷在每个成员节点上都声明过
//
// 第 ③ 条留到渲染时才报的话，症状是「组建好了，下一次调和整个组失败」
// ——而那时用户已经离开了建组的上下文（§4.4.6）。
func (s *Service) SaveGroup(ctx context.Context, req GroupRequest) (*SetParamsResult, error) {
	site, comp, err := s.componentOfForWrite(ctx, req.Site, req.Component)
	if err != nil {
		return nil, err
	}
	if req.Name == "" {
		return nil, faults.Permanentf("config group", "group name must not be empty")
	}
	if req.Role == "" {
		return nil, faults.Permanentf("config group", "role must be specified")
	}

	insts, byID, err := s.existingInstances(ctx, comp.ID)
	if err != nil {
		return nil, err
	}

	// ① 成员必须是**本角色下真的有实例的**节点。
	//
	// 不是「节点在册」就行：把一台没有跑这个角色的机器放进组里，
	// 那条成员记录永远不会生效，而用户会以为自己配好了。
	inRole := map[string]bool{}
	for _, ri := range insts {
		if ri.Role == req.Role {
			inRole[byID[ri.NodeID].Name] = true
		}
	}
	for _, m := range req.Members {
		if !inRole[m] {
			return nil, faults.Permanentf("config group",
				"node %s has no %s %s instance\n  this role's instances are on: %s",
				m, comp.Name, req.Role, strings.Join(sortedSet(inRole), ", "))
		}
	}

	groups, err := s.Repos.ConfigGroups().List(ctx, comp.ID)
	if err != nil {
		return nil, err
	}

	// ② 重叠**在写入时拒绝**，不在读取时按优先级挑一个（§4.4.2）。
	//
	// 读取时挑选意味着「这台机器到底用哪个组的配置」取决于一条没人记得的
	// 规则，而症状是「我明明改了 A 组，那台机器没变」。
	for _, g := range groups {
		if g.Role != req.Role || g.Name == req.Name {
			continue
		}
		for _, m := range g.Members {
			for _, want := range req.Members {
				if m == want {
					return nil, faults.Permanentf("config group",
						"node %s already belongs to config group %q\n"+
							"  an instance can only belong to one group. to move it, use:\n"+
							"    mechctl config group move %s --to %s -c %s -r %s",
						m, g.Name, m, req.Name, comp.Name, req.Role)
				}
			}
		}
	}

	// ③ paths 绑定的卷必须在每个成员上都存在
	if err := s.checkVolumes(req, insts, byID); err != nil {
		return nil, err
	}

	next := store.ConfigGroup{
		ComponentID: comp.ID, Role: req.Role, Name: req.Name,
		Members: req.Members, Params: req.Params, Paths: req.Paths,
	}

	// 影响面与 restart/reload 结论走与改配置**同一条**路径：
	// 建组、改成员、删组都是真实的配置变更，不是「整理组织结构」。
	out, apply, err := s.previewGroupChange(ctx, site, comp, groups, next, false)
	if err != nil {
		return nil, err
	}
	if req.DryRun {
		return out, nil
	}
	if err := apply(); err != nil {
		return nil, err
	}
	s.audit(ctx, req.Actor, "configgroup.save", comp.Name+"/"+req.Name, nil, "ok")
	s.event(ctx, site.ID, "configgroup.saved", comp.Name, map[string]any{
		"role": req.Role, "group": req.Name,
		"members": req.Members, "effect": out.Effect,
	})
	return out, nil
}

// RemoveGroup 删掉一个配置组，成员回落到角色级取值。
//
// **这是一次真实的配置变更**，不是清理（§4.4.4）：成员机器上的配置文件
// 会变、digest 会变、声明了 restartRequired 的参数会重启服务。因此它与
// SaveGroup 走同一套预览与确认。
func (s *Service) RemoveGroup(ctx context.Context, req GroupRequest) (*SetParamsResult, error) {
	site, comp, err := s.componentOfForWrite(ctx, req.Site, req.Component)
	if err != nil {
		return nil, err
	}
	groups, err := s.Repos.ConfigGroups().List(ctx, comp.ID)
	if err != nil {
		return nil, err
	}
	if _, err := s.Repos.ConfigGroups().Get(ctx, comp.ID, req.Role, req.Name); err != nil {
		return nil, err
	}

	gone := store.ConfigGroup{ComponentID: comp.ID, Role: req.Role, Name: req.Name}
	out, apply, err := s.previewGroupChange(ctx, site, comp, groups, gone, true)
	if err != nil {
		return nil, err
	}
	if req.DryRun {
		return out, nil
	}
	if err := apply(); err != nil {
		return nil, err
	}
	s.audit(ctx, req.Actor, "configgroup.remove", comp.Name+"/"+req.Name, nil, "ok")
	s.event(ctx, site.ID, "configgroup.removed", comp.Name, map[string]any{
		"role": req.Role, "group": req.Name, "effect": out.Effect,
	})
	return out, nil
}

// previewGroupChange 算出一次组变更的影响面，并返回一个真正落库的闭包。
//
// **先算后写**，与 SetParams 同一条纪律：非法状态在落库之前被拒。
func (s *Service) previewGroupChange(
	ctx context.Context, site store.Site, comp store.Component,
	current []store.ConfigGroup, next store.ConfigGroup, remove bool,
) (*SetParamsResult, func() error, error) {
	entry, err := s.Packs.Resolve(comp.Pack.Name, "="+comp.Pack.Version)
	if err != nil {
		return nil, nil, faults.Permanentf("", "parsing Pack %s@%s: %w",
			comp.Pack.Name, comp.Pack.Version, err)
	}
	p := entry.Pack

	insts, byID, err := s.existingInstances(ctx, comp.ID)
	if err != nil {
		return nil, nil, err
	}
	inputs, err := s.freezeFacts(ctx, comp.ID, assignmentsOf(insts, byID), insts, false)
	if err != nil {
		return nil, nil, err
	}
	preview := previewSecrets{real: vault.NewRenderStore(ctx, s.Vault, comp.ID)}

	before, err := s.renderWithGroups(ctx, site, comp, p, inputs, true, preview, current)
	if err != nil {
		return nil, nil, faults.Permanentf("", "parsing current config: %w", err)
	}
	after, err := s.renderWithGroups(ctx, site, comp, p, inputs, true, preview,
		mergeGroups(current, next, remove))
	if err != nil {
		return nil, nil, faults.Permanentf("config group", "%v", err)
	}

	out := &SetParamsResult{Component: comp.Name, Warnings: after.Warnings}
	for _, key := range after.Order {
		a, b := after.Specs[key], before.Specs[key]
		if a == nil || b == nil || a.Digest == b.Digest {
			continue
		}
		out.Changed = append(out.Changed, ParamChange{
			Role: a.Role, Node: a.Node.Name, From: b.Digest, To: a.Digest,
		})
	}

	// 组变更可能动到任何参数，因此这里不限定「动过的那几个」——
	// 把该角色下声明的全部参数都拿去比对。
	all := map[string]bool{}
	for _, f := range render.Form(render.FormRequest{
		Pack: p, Profile: comp.Profile, Role: next.Role,
	}) {
		all[f.Name] = true
	}
	out.Restarted, out.Reloaded = s.effectOf(p, comp.Profile, all, before, after)
	switch {
	case len(out.Restarted) > 0:
		out.Effect = EffectRestart
	case len(out.Reloaded) > 0:
		out.Effect = EffectReload
	default:
		out.Effect = EffectNone
	}

	apply := func() error {
		if remove {
			if err := s.Repos.ConfigGroups().Delete(
				ctx, comp.ID, next.Role, next.Name); err != nil {
				return err
			}
		} else if _, err := s.Repos.ConfigGroups().Upsert(ctx, next); err != nil {
			return err
		}
		s.notifyNodes(assignmentsOf(insts, byID))
		return nil
	}
	return out, apply, nil
}

// mergeGroups 把一次变更套到当前的组集合上，得到变更后的样子。
func mergeGroups(
	current []store.ConfigGroup, next store.ConfigGroup, remove bool,
) []store.ConfigGroup {
	out := make([]store.ConfigGroup, 0, len(current)+1)
	for _, g := range current {
		if g.Role == next.Role && g.Name == next.Name {
			continue // 被替换或被删掉的那个
		}
		out = append(out, g)
	}
	if !remove {
		out = append(out, next)
	}
	return out
}

// checkVolumes 校验 paths 绑定的卷在每个成员节点上都声明过。
func (s *Service) checkVolumes(
	req GroupRequest, insts []store.RoleInstance, byID map[int64]store.Node,
) error {
	if len(req.Paths) == 0 {
		return nil
	}
	byName := map[string]store.Node{}
	for _, ri := range insts {
		n := byID[ri.NodeID]
		byName[n.Name] = n
	}
	for _, member := range req.Members {
		n, ok := byName[member]
		if !ok {
			continue // ① 已经查过了
		}
		have := map[string]bool{}
		var names []string
		for _, v := range n.Volumes {
			have[v.Name] = true
			names = append(names, v.Name)
		}
		sort.Strings(names)
		for pathName, vols := range req.Paths {
			for _, v := range vols {
				if !have[v] {
					return faults.Permanentf("config group",
						"paths.%s is bound to volume %q, but node %s does not declare it\n"+
							"  volumes declared on this node: %s\n"+
							"  → use mechctl node volumes to check or add it",
						pathName, v, member, strings.Join(names, ", "))
				}
			}
		}
	}
	return nil
}

// componentOf 解析 site 与 component。
func (s *Service) componentOf(
	ctx context.Context, siteName, compName string,
) (store.Site, store.Component, error) {
	site, err := s.resolveSite(ctx, siteName)
	if err != nil {
		return store.Site{}, store.Component{}, err
	}
	comp, err := s.Repos.Components().GetByName(ctx, site.ID, compName)
	if err != nil {
		return store.Site{}, store.Component{}, err
	}
	return site, comp, nil
}

// componentOfForWrite 与 componentOf 相同，但挡住正在移除的组件。
//
// 分成两个函数而不是给 componentOf 加参数：`ListGroups` 是读，读一个
// 正在被删的组件完全正当——运维正想看它还剩什么。布尔参数会让每个
// 调用点都要重新想一遍该传什么，而想错的那次不会有症状。
func (s *Service) componentOfForWrite(
	ctx context.Context, siteName, compName string,
) (store.Site, store.Component, error) {
	site, err := s.resolveSite(ctx, siteName)
	if err != nil {
		return store.Site{}, store.Component{}, err
	}
	comp, err := s.componentForWrite(ctx, site.ID, compName)
	if err != nil {
		return store.Site{}, store.Component{}, err
	}
	return site, comp, nil
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
