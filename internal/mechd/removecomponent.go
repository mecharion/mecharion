package mechd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/store"
)

// RemoveRequest 是一次 `component remove`（10-cli §4.3）。
type RemoveRequest struct {
	Site      string
	Component string

	// 三个处置开关，对应 10-cli §4.3 那张表。零值即安全默认。
	KeepConfig bool
	PurgeData  bool
	PurgeUser  bool

	// Force 跳过失联节点：直接删记录，不等它们报告拆完。
	//
	// 那些机器上的实例会变成孤儿——20-continuous-reconcile §2.4 定死了
	// 孤儿**永不自动删**，因此它们要靠 `orphans` 发现与清理，
	// **不会**在重新上线时自己消失。
	Force bool
	// IgnoreNotFound 让「移除一个不存在的 Component」静默成功，
	// 便于脚本无脑调用。
	IgnoreNotFound bool
	// DryRun 只算影响面，不动任何东西。二档确认之前就是靠它。
	DryRun bool

	Actor string
}

// RemovalImpact 是「这次 remove 会发生什么」。
//
// 它在**二档确认之前**打给人看。一个只说「确定要删 pg-main 吗」的提示
// 没有信息量——人要知道的是几台机器、几个实例、哪些目录会没、哪些会留。
type RemovalImpact struct {
	Component string
	Pack      string
	Version   string

	// Nodes 是受影响的节点名，按字典序。
	Nodes []string
	// Instances 是实例数。
	Instances int

	// Deleted / Retained 是会被删掉 / 会留下的目录，按字典序去重。
	//
	// **两边都要列。** 只列要删的，人无从知道盘上还会剩什么；只列要留的，
	// 人无从判断这次删除到底有多狠。
	Deleted  []string
	Retained []string

	// Dependents 非空时这次 remove 会被拒绝。
	Dependents []store.Dependent
	// Removing 表示它已经在移除中了（重复敲 remove）。
	Removing bool
	// Progress 只在 Removing 时有意义。
	Progress RemovalProgress
}

// RemoveResult 是一次 remove 的结果。
type RemoveResult struct {
	Impact RemovalImpact
	// NotFound 表示组件不存在且给了 --ignore-not-found。
	NotFound bool
	// Deleted 表示记录**已经删掉了**（--force，或压根没有实例）。
	Deleted bool
	// DryRun 时什么都没做。
	DryRun bool
}

// RemoveComponent 执行 `component remove`（10-cli §4.3）。
//
// **它不直接删记录**，而是把 Component 置为 removing：`removed` 只是期望，
// 记录要留到全部实例都报告拆干净（24-lifecycle §2.2）。`--force` 是唯一的
// 例外，代价是那些机器上的实例变成孤儿。
//
// 二档确认**不在这里**：那是 CLI 与 UI 的事。服务层没有「人在不在终端前」
// 这个概念，把确认放进来只会让它在 API 调用时形同虚设——而形同虚设的
// 确认比没有确认更糟，因为它让人以为有一道防线。
func (s *Service) RemoveComponent(
	ctx context.Context, req RemoveRequest,
) (*RemoveResult, error) {
	site, err := s.resolveSite(ctx, req.Site)
	if err != nil {
		return nil, err
	}

	comp, err := s.Repos.Components().GetByName(ctx, site.ID, req.Component)
	if errors.Is(err, store.ErrNotFound) {
		if req.IgnoreNotFound {
			// 静默成功：脚本可以无脑调用，不必先查一遍
			return &RemoveResult{NotFound: true, DryRun: req.DryRun}, nil
		}
		return nil, faults.Permanentf("", "component %s does not exist\n"+
			"  add --ignore-not-found in scripts to make this succeed silently", req.Component)
	}
	if err != nil {
		return nil, err
	}

	// ── ① 前置校验：引用计数 ──
	//
	// 排在最前面。一个还被别人依赖着的组件，无论后面的确认怎么走都不该删。
	deps, err := s.Repos.Bindings().ListDependents(ctx, comp.ID)
	if err != nil {
		return nil, err
	}
	if len(deps) > 0 {
		return nil, errHasDependents(comp.Name, deps)
	}

	impact, err := s.removalImpact(ctx, site, comp, req)
	if err != nil {
		return nil, err
	}
	out := &RemoveResult{Impact: impact, DryRun: req.DryRun}
	if req.DryRun {
		return out, nil
	}

	// ── 已经在移除中：只接受推进，不接受改开关 ──
	if comp.Removing() {
		if !req.Force {
			return nil, errAlreadyRemoving(comp, impact.Progress, req)
		}
		if err := s.deleteComponent(ctx, comp, impact.Progress, "--force"); err != nil {
			return nil, err
		}
		out.Deleted = true
		s.audit(ctx, req.Actor, "remove-force", comp.Name, nil,
			fmt.Sprintf("%d/%d uninstalled", impact.Progress.Done, impact.Progress.Total))
		return out, nil
	}

	// ── ② 置为 removing，下发卸载意图 ──
	rm := store.ComponentRemoval{
		KeepConfig: req.KeepConfig, PurgeData: req.PurgeData, PurgeUser: req.PurgeUser,
	}
	if err := s.Repos.Components().StartRemoval(ctx, comp.ID, rm, s.now()); err != nil {
		return nil, err
	}
	s.audit(ctx, req.Actor, "remove", comp.Name, nil, removalNote(req))
	s.event(ctx, site.ID, "component.removing", comp.Name, map[string]any{
		"instances": impact.Instances, "purgeData": req.PurgeData,
		"keepConfig": req.KeepConfig, "purgeUser": req.PurgeUser,
	})

	// **没有实例的组件当场就删掉。** 它永远等不到任何上报——放置为空、
	// 或实例被逐个清掉过的组件都会落到这里，而卡在 removing 的它
	// 既不能改也不会消失。
	if impact.Instances == 0 {
		if err := s.deleteComponent(ctx, comp, RemovalProgress{}, "no instances"); err != nil {
			return nil, err
		}
		out.Deleted = true
		return out, nil
	}

	// 立刻唤醒：不推的话要等下一个调和周期，而运维刚敲完 remove 就去看
	// 服务还在跑，会以为命令没生效
	if insts, byID, ierr := s.existingInstances(ctx, comp.ID); ierr == nil {
		s.notifyTouched(insts, byID, "", "")
	}
	s.bump(comp.Name)
	return out, nil
}

// removalImpact 算出这次 remove 会发生什么。
//
// 目录清单**由渲染出的规格算出来**，用的是与节点侧同一张归类表
// （spec.DispositionOf）。两处各写一份的话，预览迟早会与真正发生的事
// 不一致——而那正是二档确认唯一的价值所在。
func (s *Service) removalImpact(
	ctx context.Context, site store.Site, comp store.Component, req RemoveRequest,
) (RemovalImpact, error) {
	out := RemovalImpact{
		Component: comp.Name, Pack: comp.Pack.Name, Version: comp.Pack.Version,
		Removing: comp.Removing(),
	}

	insts, err := s.Repos.Instances().List(ctx, comp.ID)
	if err != nil {
		return out, err
	}
	out.Instances = len(insts)

	names, err := s.nodeNames(ctx, site.ID)
	if err != nil {
		return out, err
	}
	for _, ri := range insts {
		out.Nodes = append(out.Nodes, names[ri.NodeID])
	}
	sortStrings(out.Nodes)
	out.Nodes = slices.Compact(out.Nodes)

	if comp.Removing() {
		if out.Progress, err = s.removalProgress(ctx, comp); err != nil {
			return out, err
		}
		// 已经在移除中时，开关早已定下——按库里那份算，不按这次的入参。
		// 否则预览会显示一个不会发生的结果。
		req.KeepConfig = comp.Removal.KeepConfig
		req.PurgeData = comp.Removal.PurgeData
		req.PurgeUser = comp.Removal.PurgeUser
	}

	// 渲染一次拿路径。**失败不致命**：Pack 可能已经不在本地了，而那不该
	// 让「删掉它」变成做不到的事——路径清单只是给人看的辅助信息。
	opts := &spec.Removal{
		KeepConfig: req.KeepConfig, PurgeData: req.PurgeData, PurgeUser: req.PurgeUser,
	}
	specs, rerr := s.renderForImpact(ctx, site, comp, insts)
	if rerr != nil {
		s.log().Warn("could not compute which directories removal would affect, omitting paths from the impact preview",
			"component", comp.Name, "err", rerr)
		return out, nil
	}
	for _, sp := range specs {
		for name, pv := range sp.Paths {
			drop := spec.DispositionOf(name).Drops(opts)
			for _, dir := range pv.Values {
				if dir == "" || !strings.HasPrefix(dir, "/") {
					continue
				}
				if drop {
					out.Deleted = append(out.Deleted, dir)
				} else {
					out.Retained = append(out.Retained, dir)
				}
			}
		}
	}
	sortStrings(out.Deleted)
	sortStrings(out.Retained)
	out.Deleted = slices.Compact(out.Deleted)
	out.Retained = slices.Compact(out.Retained)
	return out, nil
}

// ── 错误 ────────────────────────────────────────────────────────────────

// errHasDependents 拒绝删掉一个还被依赖着的组件。
func errHasDependents(name string, deps []store.Dependent) error {
	var b strings.Builder
	fmt.Fprintf(&b, "component %s is still depended on by %d component(s), cannot remove it:\n", name, len(deps))
	for _, d := range deps {
		fmt.Fprintf(&b, "  %s (require: %s)\n", d.Component, d.Require)
	}
	b.WriteString("  remove these dependents first, or change their dependency binding")
	return faults.Permanentf("", "%s", b.String())
}

// errAlreadyRemoving 说明「已经在删了，第二次 remove 只能推进」。
//
// 三个开关一律忽略：它们是**逐节点生效**的，改到一半会得到「一半节点
// 删了数据、一半留着」的集群，而那种不一致事后几乎无法排查——运维看到
// 同一个组件在不同机器上留下了不同的东西，却没有任何记录说明为什么。
func errAlreadyRemoving(comp store.Component, p RemovalProgress, req RemoveRequest) error {
	var b strings.Builder
	fmt.Fprintf(&b, "component %s is being removed (%d/%d done)\n", comp.Name, p.Done, p.Total)
	if changed := switchesGiven(req); changed != "" {
		fmt.Fprintf(&b, "  %s ignored: the switches were fixed on the first remove\n", changed)
		b.WriteString("  nodes that already finished were handled per those switches; changing them now would create inconsistency\n")
	}
	if len(p.Pending) > 0 {
		fmt.Fprintf(&b, "  pending: %v\n", p.Pending)
	}
	fmt.Fprintf(&b, "  · to proceed:      mechctl component remove %s --force (skips unreachable nodes)\n", comp.Name)
	b.WriteString("  · to clear data:   once removal finishes, mechctl orphans purge")
	return faults.Permanentf("", "%s", b.String())
}

// switchesGiven 返回这次给了哪些开关，供「已忽略」的提示用。
func switchesGiven(req RemoveRequest) string {
	var on []string
	if req.PurgeData {
		on = append(on, "--purge-data")
	}
	if req.KeepConfig {
		on = append(on, "--keep-config")
	}
	if req.PurgeUser {
		on = append(on, "--purge-user")
	}
	return strings.Join(on, " ")
}

func removalNote(req RemoveRequest) string {
	if s := switchesGiven(req); s != "" {
		return s
	}
	return "default switches (config deleted, data kept, user kept)"
}

// renderForImpact 渲染一遍，只为拿到每个实例的路径。
//
// 走的是与真实下发**同一条解析管线**：路径是模板算出来的
// （`{{ .Node.Roots.data }}/apps/{{ .Component }}`），另写一套推导迟早
// 会与真实结果分叉，而分叉的那一次正好是人依赖这个预览做决定的那一次。
func (s *Service) renderForImpact(
	ctx context.Context, site store.Site, comp store.Component,
	insts []store.RoleInstance,
) (map[string]*spec.ResolvedSpec, error) {
	if len(insts) == 0 {
		return nil, nil
	}
	_, byID, err := s.existingInstances(ctx, comp.ID)
	if err != nil {
		return nil, err
	}
	entry, err := s.Packs.Resolve(comp.Pack.Name, "="+comp.Pack.Version)
	if err != nil {
		return nil, err
	}
	res, err := s.renderComponent(ctx, site, comp, entry.Pack,
		inputsForExisting(insts, byID), true)
	if err != nil {
		return nil, err
	}
	return res.Specs, nil
}
