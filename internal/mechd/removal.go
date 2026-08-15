package mechd

import (
	"context"
	"slices"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/store"
)

// RemovalProgress 是一次移除的进度快照。
type RemovalProgress struct {
	// Total 是这个 Component 的实例总数。
	Total int
	// Done 是已经报告「拆干净了」的实例数。
	Done int
	// Pending 是还没报的实例，形如 "primary@n3"，按字典序。
	//
	// **只给数字是不够的**：一个停在 3/5 的移除，运维要问的是「哪两台」，
	// 而那两台通常正是失联的那两台。
	Pending []string
	// Retained 是已完成的实例保留下来的目录，去重后按字典序。
	Retained []string
}

// Complete 报告是否全部实例都拆完了。
//
// **Total==0 也算完成**：一个没有任何实例的 Component（放置为空、或
// 实例已被逐个清掉）不该卡在 removing 等一个永远不会来的上报。
func (p RemovalProgress) Complete() bool { return p.Done >= p.Total }

// removalProgress 统计一个正在移除的 Component 拆到哪一步了。
func (s *Service) removalProgress(
	ctx context.Context, comp store.Component,
) (RemovalProgress, error) {
	var p RemovalProgress

	insts, err := s.Repos.Instances().List(ctx, comp.ID)
	if err != nil {
		return p, err
	}
	stats, err := s.Repos.Status().ListByComponent(ctx, comp.ID)
	if err != nil {
		return p, err
	}
	byID := make(map[int64]store.InstanceStatus, len(stats))
	for _, st := range stats {
		byID[st.InstanceID] = st
	}

	nodes, err := s.nodeNames(ctx, comp.SiteID)
	if err != nil {
		return p, err
	}

	p.Total = len(insts)
	for _, ri := range insts {
		st, ok := byID[ri.ID]
		if ok && st.Removed {
			p.Done++
			p.Retained = append(p.Retained, st.RetainedPaths...)
			continue
		}
		// 没上报过 = 还没拆。**不能当成拆完了**：一台从没连上来的机器
		// 与一台报告拆完的机器，在库里的区别就只有这一条状态记录。
		p.Pending = append(p.Pending, ri.Role+"@"+nodes[ri.NodeID])
	}
	sortStrings(p.Pending)
	sortStrings(p.Retained)
	p.Retained = slices.Compact(p.Retained)
	return p, nil
}

// finishRemovalIfDone 在全部实例都报告拆完之后删掉 Component 记录。
//
// **不是在下发那一刻就删。** 删了之后那个实例就不在下发里了，节点再也
// 收不到「这个实例不该存在」——它会变成孤儿，而孤儿永不自动删
// （20-continuous-reconcile §2.4）。于是一次「删除」会变成「机器上永远
// 留着一个没人管的服务」。
func (s *Service) finishRemovalIfDone(ctx context.Context, comp store.Component) error {
	p, err := s.removalProgress(ctx, comp)
	if err != nil {
		return err
	}
	if !p.Complete() {
		return nil
	}
	return s.deleteComponent(ctx, comp, p, "all instances uninstalled")
}

// deleteComponent 真的删掉记录，并把这件事说清楚。
//
// 保留下来的目录**不需要在这里登记为孤儿**：记录一删，那些实例就不在
// 下发里了，节点侧现成的 refreshOrphans 会把留下的收据报上来。多写一份
// 中心侧的登记只会多一个可能与节点不一致的来源。
func (s *Service) deleteComponent(
	ctx context.Context, comp store.Component, p RemovalProgress, why string,
) error {
	// **先把节点名收集起来，再删记录。** 删完之后实例行也没了（外键级联），
	// 那时就再也问不出「这个组件在哪几台机器上」。
	affected, err := s.nodesOf(ctx, comp)
	if err != nil {
		// 收集不到就退化成「等下一次重连」——不该因为这个而删不掉组件
		s.log().Warn("could not determine affected nodes, will not proactively wake them after deletion",
			"component", comp.Name, "err", err)
	}

	if err := s.Repos.Components().Delete(ctx, comp.ID); err != nil {
		return err
	}
	s.log().Info("Component removed", "component", comp.Name, "reason", why,
		"instances", p.Total, "uninstalled", p.Done, "retained_dirs", len(p.Retained))

	// **必须唤醒那些节点。**
	//
	// 记录一删，节点就该收到一次不含这个实例的全量下发——那一次才会让
	// 它清掉本地的期望状态，把留下的收据报成孤儿。
	//
	// 不唤醒的话没有任何东西会报错，症状是**沉默的**：节点手里还攥着
	// 那份「runState: removed」的期望，于是每 60 秒重新卸载一遍一个
	// 早就拆干净的实例，而 `orphans list` 永远是空的——保留下来的数据
	// 因此永远无人认领。这条是在三台真机的验收里发现的，单元测试
	// （没有真正的下发循环）看不见它。
	for _, n := range affected {
		if s.Notify != nil {
			s.Notify.Notify(n)
		}
	}

	// 催一下正在看这个组件的浏览器：它刚刚消失了，这是最需要立刻可见的
	// 一种变化。丢了也只是晚一点（SSE 有定时兜底）。
	s.bump(comp.Name)
	return nil
}

// nodesOf 返回一个组件落在哪些节点上，按字典序去重。
func (s *Service) nodesOf(ctx context.Context, comp store.Component) ([]string, error) {
	insts, err := s.Repos.Instances().List(ctx, comp.ID)
	if err != nil {
		return nil, err
	}
	names, err := s.nodeNames(ctx, comp.SiteID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(insts))
	for _, ri := range insts {
		if n := names[ri.NodeID]; n != "" {
			out = append(out, n)
		}
	}
	sortStrings(out)
	return slices.Compact(out), nil
}

// nodeNames 返回 nodeID → 节点名。
func (s *Service) nodeNames(ctx context.Context, siteID int64) (map[int64]string, error) {
	nodes, err := s.Repos.Nodes().List(ctx, siteID)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]string, len(nodes))
	for _, n := range nodes {
		out[n.ID] = n.Name
	}
	return out, nil
}

// componentForWrite 取一个 Component，并挡住对正在移除者的写操作。
//
// **做成一个取记录的函数，而不是十一处各写一遍 if。** 写动词有十一个
// （deploy / upgrade / rollback / config set / 配置组的增删 / drift 策略 /
// rollout 旋钮 / 暂停 / ack-drift / stop-start），漏掉任何一处都会开出
// 一条绕过闸门的路——而漏掉的那一处不会有任何症状，直到有人真的用它。
//
// 让「取记录」这个动作本身带上闸门，新加的写动词就只能默认是安全的：
// 它想拿到 Component，就必须走这里。
func (s *Service) componentForWrite(
	ctx context.Context, siteID int64, name string,
) (store.Component, error) {
	comp, err := s.Repos.Components().GetByName(ctx, siteID, name)
	if err != nil {
		return comp, err
	}
	if !comp.Removing() {
		return comp, nil
	}
	p, perr := s.removalProgress(ctx, comp)
	if perr != nil {
		// 进度算不出来也要拦住——**拿不到细节不是放行的理由**
		return comp, faults.Permanentf("", "Component %q is being removed, no other writes are accepted", name)
	}
	return comp, errRemoving(comp, p)
}

// refuseIfRemoving 挡住对正在移除的组件的写操作。
//
// 给那些已经从别处拿到了 Component 的写动词用（rollout 的两个旋钮就是
// 这样：它们走 s.load，而那条路读写共用）。
func (s *Service) refuseIfRemoving(ctx context.Context, comp store.Component) error {
	if !comp.Removing() {
		return nil
	}
	p, err := s.removalProgress(ctx, comp)
	if err != nil {
		return faults.Permanentf("", "Component %q is being removed, no other writes are accepted", comp.Name)
	}
	return errRemoving(comp, p)
}

// errRemoving 是「这个组件正在被删，不接受写操作」的统一说法。
//
// **一个正在被删的东西不该还能改。** 允许的话会出现「配置改了，但那台
// 已经拆完的机器上没有」这种事后完全解释不清的状态。
func errRemoving(comp store.Component, p RemovalProgress) error {
	return faults.Permanentf("",
		"Component %q is being removed (%d/%d done), no other writes are accepted\n"+
			"  pending: %v\n"+
			"  · to proceed: mechctl component remove %s --force (skips unreachable nodes)",
		comp.Name, p.Done, p.Total, p.Pending, comp.Name)
}
