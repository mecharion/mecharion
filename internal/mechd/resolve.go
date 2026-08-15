package mechd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/placement"
	"github.com/mecharion/mecharion/internal/render"
	"github.com/mecharion/mecharion/internal/store"
)

// DefaultSite 是未指定站点时的名字。
const DefaultSite = "default"

func (s *Service) resolveSite(ctx context.Context, name string) (store.Site, error) {
	if name == "" {
		name = DefaultSite
	}
	site, err := s.Repos.Sites().GetByName(ctx, name)
	if err == nil {
		return site, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Site{}, err
	}
	return store.Site{}, faults.Permanentf("",
		"site %q does not exist -- run mechctl site create %s first, "+
			"or use mechlet install --standalone to initialize a standalone site", name, name)
}

// load 取出 Component 与它的 Pack。
func (s *Service) load(
	ctx context.Context, siteName, name string,
) (store.Site, store.Component, *pack.Pack, error) {
	site, err := s.resolveSite(ctx, siteName)
	if err != nil {
		return store.Site{}, store.Component{}, nil, err
	}
	comp, err := s.Repos.Components().GetByName(ctx, site.ID, name)
	if err != nil {
		return site, store.Component{}, nil, err
	}
	// 按**已记录的版本**取 Pack，不取最新：一个 Component 的期望状态
	// 由它当初部署的那个版本决定，升级是显式动作
	entry, err := s.Packs.Resolve(comp.Pack.Name, "="+comp.Pack.Version)
	if err != nil {
		return site, comp, nil, faults.Permanentf("",
			"component %s uses Pack %s %s, which is not in the local Pack collection: %w",
			name, comp.Pack.Name, comp.Pack.Version, err)
	}
	return site, comp, entry.Pack, nil
}

// upsertComponentDraft 构造（但不落库）本次部署后的 Component 记录。
func (s *Service) upsertComponentDraft(
	ctx context.Context, site store.Site, name string, p *pack.Pack, req DeployRequest,
) (store.Component, bool, error) {
	cur, err := s.componentForWrite(ctx, site.ID, name)
	existed := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.Component{}, false, err
	}

	if existed && !req.Update && !req.DryRun {
		return store.Component{}, true, faults.Permanentf("",
			"component %s already exists (Pack %s %s) -- "+
				"a repeat deploy is rejected by default, add --update to confirm the change",
			name, cur.Pack.Name, cur.Pack.Version)
	}

	draft := cur
	draft.SiteID = site.ID
	draft.Name = name
	draft.Pack = store.PackRef{
		Name: p.Name, Version: p.Version, Revision: p.Revision,
	}
	if req.Profile != "" {
		draft.Profile = req.Profile
	}
	if draft.Profile == "" {
		draft.Profile = defaultProfile(p)
	}
	// 参数是**合并**而非替换：--set 只给了一个键时，不该把其余的清空
	if draft.Params == nil {
		draft.Params = map[string]any{}
	}
	for k, v := range req.Set {
		draft.Params[k] = v
	}

	// 参数名拼错要在这里拦住：留到渲染时才报，错误信息里只剩一个模板名
	if err := checkParamNames(p, draft.Params); err != nil {
		return store.Component{}, existed, err
	}
	return draft, existed, nil
}

// defaultProfile 返回 Pack 声明的默认形态。
func defaultProfile(p *pack.Pack) string {
	for _, pf := range p.Profiles {
		if pf.Default {
			return pf.Name
		}
	}
	if len(p.Profiles) > 0 {
		return p.Profiles[0].Name
	}
	return ""
}

// checkParamNames 确认覆盖的参数都被 Pack 声明过。
func checkParamNames(p *pack.Pack, params map[string]any) error {
	if len(params) == 0 {
		return nil
	}
	known := map[string]bool{}
	for k := range p.Params {
		known[k] = true
	}
	for _, r := range p.Roles {
		for k := range r.Params {
			known[k] = true
		}
	}
	for _, pf := range p.Profiles {
		for k := range pf.Params {
			known[k] = true
		}
	}

	var bad []string
	for k := range params {
		if !known[k] {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	all := make([]string, 0, len(known))
	for k := range known {
		all = append(all, k)
	}
	sort.Strings(all)
	return faults.Permanentf("", "Pack %s does not declare these parameters: %s\n  declared parameters: %s",
		p.Name, strings.Join(bad, ", "), strings.Join(all, ", "))
}

func (s *Service) persistComponent(
	ctx context.Context, c store.Component, existed bool,
) (store.Component, error) {
	now := s.now()
	if existed {
		c.UpdatedAt = now
		return s.Repos.Components().Update(ctx, c)
	}
	c.CreatedAt, c.UpdatedAt = now, now
	return s.Repos.Components().Create(ctx, c)
}

// ── 节点与实例 ──────────────────────────────────────────────────────────

// resolveNodes 把请求里的节点名换成节点记录。
func (s *Service) resolveNodes(
	ctx context.Context, siteID int64, req DeployRequest, p *pack.Pack,
) (map[string][]store.Node, error) {
	all, err := s.Repos.Nodes().List(ctx, siteID)
	if err != nil {
		return nil, err
	}
	byName := map[string]store.Node{}
	for _, n := range all {
		byName[n.Name] = n
	}

	pick := func(names []string) ([]store.Node, error) {
		out := make([]store.Node, 0, len(names))
		for _, name := range names {
			n, ok := byName[name]
			if !ok {
				return nil, faults.Permanentf("",
					"node %q is not registered\n  registered nodes: %s\n"+
						"  → the node must run mechlet and register first, mechd does not create it out of thin air",
					name, strings.Join(sortedNodeNames(byName), ", "))
			}
			out = append(out, n)
		}
		return out, nil
	}

	out := map[string][]store.Node{}
	if len(req.Roles) > 0 {
		for role, names := range req.Roles {
			ns, err := pick(names)
			if err != nil {
				return nil, err
			}
			out[role] = ns
		}
		return out, nil
	}

	// --nodes 简写：所有 cardinality 允许的角色都放这些节点上
	if len(req.Nodes) == 0 {
		return nil, faults.Permanentf("", "nodes must be specified: use --nodes or --role <role>=<node-list>")
	}
	ns, err := pick(req.Nodes)
	if err != nil {
		return nil, err
	}
	for _, r := range p.RolesForProfile(profileOf(req, p)) {
		if !r.Enabled {
			continue
		}
		lo, _, ok := pack.CardinalityBounds(r.Cardinality)
		if ok && lo == 0 {
			continue // 可选角色不因为一个 --nodes 就被拉起来
		}
		out[r.Name] = ns
	}
	if len(out) == 0 {
		return nil, faults.Permanentf("",
			"--nodes did not match any required role; use --role <role>=<node-list> to specify explicitly")
	}
	return out, nil
}

func profileOf(req DeployRequest, p *pack.Pack) string {
	if req.Profile != "" {
		return req.Profile
	}
	return defaultProfile(p)
}

func (s *Service) existingInstances(
	ctx context.Context, componentID int64,
) ([]store.RoleInstance, map[int64]store.Node, error) {
	insts, err := s.Repos.Instances().List(ctx, componentID)
	if err != nil {
		return nil, nil, err
	}
	byID := map[int64]store.Node{}
	for _, ri := range insts {
		if _, ok := byID[ri.NodeID]; ok {
			continue
		}
		n, err := s.Repos.Nodes().Get(ctx, ri.NodeID)
		if err != nil {
			return nil, nil, err
		}
		byID[ri.NodeID] = n
	}
	return insts, byID, nil
}

// ensureInstances 固化 ordinal 并删掉不再需要的实例。
//
// **分配与插入在同一个事务里**（InstanceRepo.Ensure）：拆成「先查最大值、
// 再插入」会有竞态，而 ordinal 写错就是集群身份错乱（ADR-0028）。
func (s *Service) ensureInstances(
	ctx context.Context, componentID int64, plan *placement.Plan,
) ([]placement.Assignment, error) {
	out := make([]placement.Assignment, 0, plan.Total())
	out = append(out, plan.Keep...)

	for _, a := range plan.Add {
		ri, err := s.Repos.Instances().Ensure(ctx, componentID, a.Role, a.Node.ID, nil)
		if err != nil {
			return nil, err
		}
		a.Ordinal = ri.Ordinal
		out = append(out, a)
	}
	for _, ri := range plan.Remove {
		if err := s.Repos.Instances().Delete(ctx, ri.ID); err != nil {
			return nil, err
		}
	}
	sortAssignments(out)
	return out, nil
}

// previewOrdinals 给干跑的新增实例编一个不落库的序号。
//
// 真实分配在提交时才发生，因此这里只是让预览里的 myid 看起来合理，
// **不保证与将来实际分到的一致**——中间可能有别的部署插进来。
func previewOrdinals(plan *placement.Plan, existing []store.RoleInstance) []placement.Assignment {
	next := map[string]int{}
	for _, ri := range existing {
		if ri.Ordinal >= next[ri.Role] {
			next[ri.Role] = ri.Ordinal + 1
		}
	}
	out := make([]placement.Assignment, 0, len(plan.Add))
	for _, a := range plan.Add {
		a.Ordinal = next[a.Role]
		next[a.Role]++
		out = append(out, a)
	}
	return out
}

func assignmentsOf(insts []store.RoleInstance, byID map[int64]store.Node) []placement.Assignment {
	out := make([]placement.Assignment, 0, len(insts))
	for _, ri := range insts {
		out = append(out, placement.Assignment{
			Role: ri.Role, Node: byID[ri.NodeID], Ordinal: ri.Ordinal,
		})
	}
	sortAssignments(out)
	return out
}

func sortAssignments(as []placement.Assignment) {
	sort.Slice(as, func(i, j int) bool {
		if as[i].Role != as[j].Role {
			return as[i].Role < as[j].Role
		}
		return as[i].Ordinal < as[j].Ordinal
	})
}

// ── 依赖绑定 ────────────────────────────────────────────────────────────

// resolveBindings 解析 requires.packs（spec §5.2）。
//
// **绑定一旦确定就固化**，之后不再重新解析——否则新装一套 ZooKeeper
// 就可能让已有部署静默改指向。这与路径固化、ordinal 固化是同一条原则的
// 第三次应用（15-render-pipeline §4）。
func (s *Service) resolveBindings(
	ctx context.Context, site store.Site, comp store.Component, p *pack.Pack, dryRun bool,
) (map[string]render.Binding, error) {
	if p.Requires == nil || len(p.Requires.Packs) == 0 {
		return nil, nil
	}
	out := map[string]render.Binding{}

	for _, dep := range p.Requires.Packs {
		provider, err := s.bindOne(ctx, site, comp, dep, dryRun)
		if err != nil {
			return nil, err
		}
		b, err := s.bindingFor(ctx, site, dep, provider)
		if err != nil {
			return nil, err
		}
		out[dep.Name] = b
	}
	return out, nil
}

// bindOne 找出（或复用）某条依赖绑定的 Component。
func (s *Service) bindOne(
	ctx context.Context, site store.Site, comp store.Component,
	dep pack.PackRequire, dryRun bool,
) (store.Component, error) {
	// 已固化的绑定优先
	if comp.ID != 0 {
		if b, err := s.Repos.Bindings().Get(ctx, comp.ID, dep.Name); err == nil {
			return s.Repos.Components().Get(ctx, b.BoundComponentID)
		} else if !errors.Is(err, store.ErrNotFound) {
			return store.Component{}, err
		}
	}

	all, err := s.Repos.Components().List(ctx, site.ID)
	if err != nil {
		return store.Component{}, err
	}
	var candidates []store.Component
	for _, c := range all {
		if c.Pack.Name != dep.Name || c.ID == comp.ID {
			continue
		}
		if dep.Version != "" {
			ok, err := versionSatisfies(c.Pack.Version, dep.Version)
			if err != nil || !ok {
				continue
			}
		}
		candidates = append(candidates, c)
	}

	switch len(candidates) {
	case 1:
		if !dryRun && comp.ID != 0 {
			// 固化：之后不再重新解析
			if _, err := s.Repos.Bindings().Create(ctx, store.PackBinding{
				ComponentID: comp.ID, RequireName: dep.Name,
				BoundComponentID: candidates[0].ID, CreatedAt: s.now(),
			}); err != nil {
				return store.Component{}, err
			}
		}
		return candidates[0], nil
	case 0:
		return store.Component{}, faults.Permanentf("",
			"dependency %s %s has no available Component in this Site\n"+
				"  → deploy it first: mechctl component deploy %s",
			dep.Name, dep.Version, dep.Name)
	default:
		names := make([]string, 0, len(candidates))
		for _, c := range candidates {
			names = append(names, c.Name)
		}
		sort.Strings(names)
		return store.Component{}, faults.Permanentf("",
			"dependency %s has multiple candidates: %s\n"+
				"  → use --require %s=<component> to specify which one to bind",
			dep.Name, strings.Join(names, ", "), dep.Name)
	}
}

// bindingFor 渲染 provider 并取出它的导出与拓扑。
func (s *Service) bindingFor(
	ctx context.Context, site store.Site, dep pack.PackRequire, provider store.Component,
) (render.Binding, error) {
	pe, err := s.Packs.Resolve(provider.Pack.Name, "="+provider.Pack.Version)
	if err != nil {
		return render.Binding{}, err
	}
	pp := pe.Pack
	insts, byID, err := s.existingInstances(ctx, provider.ID)
	if err != nil {
		return render.Binding{}, err
	}
	if len(insts) == 0 {
		return render.Binding{}, faults.Permanentf("",
			"dependency %s is bound to Component %s, but it has no instances yet",
			dep.Name, provider.Name)
	}

	// **消费方要等提供方解析完**：exports 里的 `{{ .Params.app_password }}`
	// 要求 provider 的参数已经解析完毕（15-render-pipeline §5）
	res, err := s.renderComponent(ctx, site, provider, pp,
		inputsForExisting(insts, byID), false)
	if err != nil {
		return render.Binding{}, faults.Permanentf("", "resolving dependency %s: %w", provider.Name, err)
	}

	b := render.Binding{
		Pack: provider.Pack.Name, Component: provider.Name,
		Version: provider.Pack.Version, Scope: dep.EffectiveScope(),
		Exports: res.Exports,
	}

	if dep.EffectiveScope() == pack.ScopeNode {
		// scope:node 的依赖在**本机**，消费方要的是它的路径。
		// 取任意一个实例的即可——同一个 Component 在各节点上的路径
		// 由同一份声明渲染，差异只来自 Node.Roots。
		for _, key := range res.Order {
			sp := res.Specs[key]
			b.Paths = map[string][]string{}
			for name, pv := range sp.Paths {
				b.Paths[name] = pv.Values
			}
			// current 是派生值，规格里没有，但消费方几乎总是引用它
			if home := sp.Paths["home"]; len(home.Values) > 0 {
				b.Paths["current"] = []string{filepath.ToSlash(
					filepath.Join(home.Values[0], "current"))}
			}
			//lint:ignore SA4004 只取第一个键就够——同一个 Component 在各节点上的路径由同一份声明渲染，取哪个实例都一样。
			break
		}
		return b, nil
	}

	// scope:site：给出提供方的拓扑快照
	for _, ri := range insts {
		n := byID[ri.NodeID]
		b.Topology = append(b.Topology, render.Peer{
			Node: n.Name, Address: n.Address, Ordinal: ri.Ordinal,
			Role: ri.Role, Labels: n.Labels,
		})
	}
	return b, nil
}

func versionSatisfies(have, constraint string) (bool, error) {
	c, err := pack.ParseConstraint(constraint)
	if err != nil {
		return false, err
	}
	return c.Matches(pack.ParseVersion(have)), nil
}

// ── 载荷 ────────────────────────────────────────────────────────────────

// ── 参数覆盖 ────────────────────────────────────────────────────────────

func overridesFrom(comp store.Component, groups []store.ConfigGroup) render.Overrides {
	o := render.Overrides{Component: comp.Params}
	for _, g := range groups {
		if len(g.Params) == 0 {
			continue
		}
		if o.Group == nil {
			o.Group = map[string]map[string]any{}
		}
		o.Group[g.Name] = g.Params
	}
	return o
}

func groupNameFor(groups []store.ConfigGroup, role, node string) string {
	for _, g := range groups {
		if g.Role != role {
			continue
		}
		for _, m := range g.Members {
			if m == node {
				return g.Name
			}
		}
	}
	return "default"
}

// pathBindingsFor 目前恒为空：按卷名绑定多盘要等 ConfigGroup 的
// paths 字段落到存储层，那是 M5 的 config 命令一起做的事。
// pathBindingsFor 返回某个实例所属配置组上的多盘绑定（spec §8.6）。
//
// **这个函数在 M7 之前一直是个 `return nil` 的桩**，因此 ADR-0021 的
// 原始动机场景（20 台 4 盘、5 台 12 盘的 DataNode）从来没能真的配出来
// ——求值代码（render/paths.go 的 resolveOnePath）倒是从 M2 起就写好了，
// 它只是永远收不到绑定。
func pathBindingsFor(groups []store.ConfigGroup, role, node string) map[string][]string {
	for _, g := range groups {
		if g.Role != role {
			continue
		}
		for _, m := range g.Members {
			if m == node {
				return g.Paths
			}
		}
	}
	return nil
}

// renderNode 把节点记录变成渲染上下文里的节点。
//
// facts 由调用方传入**放置时的快照**，而不是从 n 上取实时值：
// 配置取值跟随实时事实会让一次加内存变成一次业务时间的重启（spec §9.4.1）。
func renderNode(n store.Node, facts map[string]any) render.Node {
	vols := map[string]render.Volume{}
	for _, v := range n.Volumes {
		vols[v.Name] = render.Volume{Path: v.Path, Class: v.Class}
	}
	return render.Node{
		Name: n.Name, Address: n.Address, Labels: n.Labels,
		Roots: n.Roots, Volumes: vols, Facts: facts,
	}
}

func sortedNodeNames(m map[string]store.Node) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── 通知 / 审计 ─────────────────────────────────────────────────────────

func (s *Service) notifyNodes(instances []placement.Assignment) {
	if s.Notify == nil {
		return
	}
	seen := map[string]bool{}
	for _, a := range instances {
		if seen[a.Node.Name] {
			continue
		}
		seen[a.Node.Name] = true
		s.Notify.Notify(a.Node.Name)
	}
}

func (s *Service) audit(ctx context.Context, actor, action, target string, p *pack.Pack, result string) {
	ref := ""
	if p != nil {
		ref = p.Name + " " + p.Version
	}
	if actor == "" {
		actor = "unknown"
	}
	if err := s.Repos.Events().Audit(ctx, store.AuditEntry{
		At: s.now(), Actor: actor, Action: action,
		Target: target, PackRef: ref, Result: result,
	}); err != nil {
		// 审计写失败不该让动作失败——那会让一次磁盘满变成整个控制面停摆
		s.log().Warn("failed to write audit entry", "action", action, "err", err)
	}
}

func (s *Service) event(ctx context.Context, siteID int64, kind, subject string, payload map[string]any) {
	if err := s.Repos.Events().Append(ctx, store.Event{
		At: s.now(), SiteID: siteID, Kind: kind, Subject: subject, Payload: payload,
	}); err != nil {
		s.log().Warn("failed to write event", "kind", kind, "err", err)
	}
}

// ── 干跑用的一次性密钥 ──────────────────────────────────────────────────

// ephemeralSecrets 是 dry-run / diff 用的密钥来源。
//
// 值只活在本次调用里，**不写 Vault**：一次「先看看会发生什么」不该
// 留下副作用。代价是 digest 与真实部署不可比——调用方需要知道这一点。
type ephemeralSecrets struct{ values map[string]string }

func newEphemeralSecrets() *ephemeralSecrets {
	return &ephemeralSecrets{values: map[string]string{}}
}

func (e *ephemeralSecrets) Ensure(component, param string, g pack.Generate) (render.StoredSecret, error) {
	if _, ok := e.values[param]; !ok {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			return render.StoredSecret{}, err
		}
		e.values[param] = "dryrun-" + hex.EncodeToString(buf)
	}
	return render.StoredSecret{ID: "dryrun." + param, Value: e.values[param]}, nil
}

func (e *ephemeralSecrets) Store(component, param, value string) (render.StoredSecret, error) {
	e.values[param] = value
	return render.StoredSecret{ID: "dryrun." + param, Value: value}, nil
}

// ── 实例输入 ────────────────────────────────────────────────────────────

// instanceInput 是渲染一个实例所需的全部信息。
//
// 它比 placement.Assignment 多一样东西：**放置时的事实快照**。
// 那是解析管线与实时事实之间的隔离层——没有它，一次心跳带回的新内存值
// 就会改变 heap、改变 digest、在业务时间重启服务（spec §9.4.1）。
type instanceInput struct {
	Role    string
	Ordinal int
	Node    store.Node
	Facts   map[string]any
}

// inputsForExisting 用**已固化**的事实快照构造渲染输入。
func inputsForExisting(
	insts []store.RoleInstance, byID map[int64]store.Node,
) []instanceInput {
	out := make([]instanceInput, 0, len(insts))
	for _, ri := range insts {
		out = append(out, instanceInput{
			Role: ri.Role, Ordinal: ri.Ordinal,
			Node: byID[ri.NodeID], Facts: ri.Facts,
		})
	}
	sortInputs(out)
	return out
}

// freezeFacts 为本次放置的每个实例取一份事实快照。
//
// 已有实例保留原快照——**它们的配置不该因为一次心跳而变**；
// 新实例取当前值并写库。
func (s *Service) freezeFacts(
	ctx context.Context, componentID int64,
	assigned []placement.Assignment, existing []store.RoleInstance, persist bool,
) ([]instanceInput, error) {
	frozen := map[string]map[string]any{}
	idOf := map[string]int64{}
	for _, ri := range existing {
		frozen[ri.Role+"#"+fmt.Sprint(ri.NodeID)] = ri.Facts
	}
	if persist {
		cur, err := s.Repos.Instances().List(ctx, componentID)
		if err != nil {
			return nil, err
		}
		for _, ri := range cur {
			idOf[ri.Role+"#"+fmt.Sprint(ri.NodeID)] = ri.ID
			if _, ok := frozen[ri.Role+"#"+fmt.Sprint(ri.NodeID)]; !ok {
				frozen[ri.Role+"#"+fmt.Sprint(ri.NodeID)] = ri.Facts
			}
		}
	}

	out := make([]instanceInput, 0, len(assigned))
	for _, a := range assigned {
		key := a.Role + "#" + fmt.Sprint(a.Node.ID)
		facts := frozen[key]
		if len(facts) == 0 {
			// 新实例：取当前上报的事实并固化
			f, err := s.Repos.Status().GetFacts(ctx, a.Node.ID)
			if err == nil {
				facts = f.Facts
			} else if !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
			if persist {
				if id, ok := idOf[key]; ok {
					if err := s.Repos.Instances().SetFacts(ctx, id, facts); err != nil {
						return nil, err
					}
				}
			}
		}
		out = append(out, instanceInput{
			Role: a.Role, Ordinal: a.Ordinal, Node: a.Node, Facts: facts,
		})
	}
	sortInputs(out)
	return out, nil
}

func sortInputs(in []instanceInput) {
	sort.Slice(in, func(i, j int) bool {
		if in[i].Role != in[j].Role {
			return in[i].Role < in[j].Role
		}
		return in[i].Ordinal < in[j].Ordinal
	})
}

// notifyTouched 通知本次操作涉及的节点。
//
// 只通知涉及的那些：一次单节点维护操作把整个 Site 的规格重推一遍，
// 在几百个节点的站点上是一次可观的抖动，而且毫无必要。
func (s *Service) notifyTouched(
	insts []store.RoleInstance, byID map[int64]store.Node, role, node string,
) {
	if s.Notify == nil {
		return
	}
	seen := map[string]bool{}
	for _, ri := range insts {
		if role != "" && ri.Role != role {
			continue
		}
		name := byID[ri.NodeID].Name
		if node != "" && name != node {
			continue
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		s.Notify.Notify(name)
	}
}
