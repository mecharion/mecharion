package mechd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/store"
)

// RolloutTimeout 是「多久不收敛就判失败」。
//
// 不做成配置项：它不是一个用户需要调的旋钮，而是一条**判据**——
// 超过它还没收敛，就该有人来看，而不是继续等。真的需要更久时用
// `rollout pause` 把这个时钟冻住，那比调一个全局默认值精确得多。
const RolloutTimeout = 10 * time.Minute

// startRollout 为一次版本变更开一条 Rollout 记录。
//
// 已有进行中的 Rollout 时**接管它**而不是并行开一条：同一个 Component 上
// 两条同时推进的 Rollout 没有意义，而两条记录会让 `rollout status` 无法
// 回答「现在到底在做什么」。
func (s *Service) startRollout(
	ctx context.Context, comp store.Component, kind, from, to string,
) {
	now := s.now()
	if cur, err := s.Repos.Rollouts().Active(ctx, comp.ID); err == nil {
		reason := fmt.Sprintf("superseded by a new %s (-> %s)", kind, to)
		if err := s.Repos.Rollouts().SetState(ctx,
			cur.ID, store.RolloutAborted, reason, &now); err != nil {
			s.log().Warn("failed to end the previous Rollout", "id", cur.ID, "err", err)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		s.log().Warn("failed to query the in-progress Rollout", "component", comp.Name, "err", err)
	}

	ro, err := s.Repos.Rollouts().Create(ctx, store.Rollout{
		ComponentID: comp.ID, State: store.RolloutRunning,
		Kind: kind, FromVersion: from, ToVersion: to, StartedAt: now,
	})
	if err != nil {
		// 记不下 Rollout 不该让升级本身失败——版本已经改了、规格已经推了。
		// 但要说出来：升级过程会因此不可见。
		s.log().Error("failed to create Rollout record, this upgrade will not be visible",
			"component", comp.Name, "err", err)
		return
	}

	// **批次在这里一次算好并落盘**，不是边走边算（22-multi-node §4）。
	//
	// 算好之后 `rollout status` 才回答得了「一共几批、现在第几批」；
	// 边走边算只能回答「现在这批」。中途有节点上线/下线也不改已算好的
	// 批次——那会让「一共几批」这个答案在执行过程中变化。
	if err := s.planAndSaveBatches(ctx, comp, ro.ID); err != nil {
		s.log().Error("failed to compute batches, this change will be dispatched all at once",
			"component", comp.Name, "err", err)
	}
}

// abortStartedRollout 收掉一条刚开出来、但下发失败了的 Rollout。
//
// 不收的话它会一直挂在「进行中」，而机器上什么都没变——`rollout status`
// 会说「第 1/3 批进行中」，运维盯着它等一个永远不会来的收敛。
func (s *Service) abortStartedRollout(
	ctx context.Context, comp store.Component, cause error,
) {
	ro, err := s.Repos.Rollouts().Active(ctx, comp.ID)
	if err != nil {
		return
	}
	now := s.now()
	if err := s.Repos.Rollouts().SetState(ctx, ro.ID, store.RolloutAborted,
		"deployment failed, never started: "+cause.Error(), &now); err != nil {
		s.log().Warn("failed to abort the never-started Rollout", "id", ro.ID, "err", err)
	}
}

// planAndSaveBatches 算出这次变更的阶段与批次并落盘。
func (s *Service) planAndSaveBatches(
	ctx context.Context, comp store.Component, rolloutID int64,
) error {
	entry, err := s.Packs.Resolve(comp.Pack.Name, "="+comp.Pack.Version)
	if err != nil {
		return faults.Permanentf("", "parsing Pack: %w", err)
	}
	insts, byID, err := s.existingInstances(ctx, comp.ID)
	if err != nil {
		return err
	}

	in := batchInput{
		Roles:          map[string][]store.BatchTarget{},
		Quorum:         map[string]bool{},
		Cordoned:       map[string]bool{},
		MaxUnavailable: comp.RolloutMaxUnavailable,
		Canary:         comp.RolloutCanary,
	}
	roles := entry.Pack.RolesForProfile(comp.Profile)
	for _, r := range roles {
		in.Quorum[r.Name] = r.Quorum
	}
	in.Order = roleOrder(roles)

	for _, ri := range insts {
		n := byID[ri.NodeID]
		if n.Cordoned() {
			in.Cordoned[n.Name] = true
		}
		in.Roles[ri.Role] = append(in.Roles[ri.Role], store.BatchTarget{
			InstanceID: ri.ID, Role: ri.Role, Node: n.Name, Ordinal: ri.Ordinal,
		})
	}

	batches, skipped, err := planBatches(in)
	if err != nil {
		return err
	}
	for _, b := range batches {
		b.RolloutID = rolloutID
		if _, err := s.Repos.RolloutBatches().Create(ctx, b); err != nil {
			return err
		}
	}
	if len(skipped) > 0 {
		// **跳过的也落盘**，作为一条 seq=0 的记录。
		//
		// 只写日志是不够的：运维事后问的是「为什么这台还是旧版」，
		// 而那时日志已经翻过去了。seq=0 让它不进「第几/共几批」的
		// 分母——它不是一批，是一份名单。
		if _, err := s.Repos.RolloutBatches().Create(ctx, store.RolloutBatch{
			RolloutID: rolloutID, Seq: 0, Stage: 0, Role: "",
			Targets: skipped, State: store.BatchSkipped,
		}); err != nil {
			return err
		}
		s.log().Info("these nodes are cordoned, excluded from this change",
			"component", comp.Name, "nodes", strings.Join(nodesOf(skipped), ","))
	}
	s.log().Info("batched", "component", comp.Name,
		"batches", len(batches), "skipped", len(skipped))
	return nil
}

// AdvanceRollout 按**观测到的状态**推进一条 Rollout。
//
// 判据来自实例上报，不来自「mechd 发过什么」——状态可以重复确认，
// 事件丢一次就永远丢了（这与 digest 作为收敛判据是同一条理由）。
//
// 它在每次节点上报时被调用，因此不需要一条后台循环：上报本来就是
// 15 秒一次的心跳，正好是推进 Rollout 的天然节拍。
func (s *Service) AdvanceRollout(ctx context.Context, comp store.Component) {
	ro, err := s.Repos.Rollouts().Active(ctx, comp.ID)
	if err != nil {
		return // 没有进行中的，或查不到——都不该打断上报
	}
	if ro.State == store.RolloutPaused || ro.State == store.RolloutHalted {
		// **冻住判定**：人正在查，不要替他宣布失败。
		// 也因此不放行下一批——pause 的语义是「别再往前走了」，
		// 一个只冻结判定却继续推批次的实现等于没有 pause。
		//
		// halted 走同一条路：它也是「停下来等人」，只是停的人是系统。
		// 要往前走得由人显式 `rollout resume`。
		return
	}

	view, err := s.Status(ctx, "", comp.Name)
	if err != nil {
		return
	}

	now := s.now()
	if rolledBack(view) {
		_ = s.Repos.Rollouts().SetState(ctx, ro.ID, store.RolloutFailed,
			"nodes automatically rolled back to the last available version", &now)
		s.event(ctx, comp.SiteID, "rollout.failed", comp.Name, map[string]any{
			"to": ro.ToVersion, "reason": "automatic rollback",
		})
		return
	}

	// 推进批次：当前这批收敛了就放行下一批。
	//
	// **判据是观测到的收敛，不是「已经发出去了」**——按后者推进等于一次
	// 性下发，只是把时间摊开了：批次会一批接一批地立刻放行，谁都没等。
	done, batched, err := s.advanceBatches(ctx, comp, ro, view)
	if err != nil {
		s.log().Warn("failed to advance batch", "component", comp.Name, "err", err)
		return
	}

	switch {
	// **判据只有 done 一个**，不再叠加「整体已收敛」。
	//
	// 叠加的话，被 cordon 的机器会让升级永远不成功：它们按定义就停在旧版，
	// `view.Converged` 因此永远为假，而这次变更明明把该做的都做完了。
	// 那会让每一次「先 cordon 掉一台再升级」都以超时失败收场。
	//
	// done 本身已经足够严：它要求每一批都走到 Done，而一批只有在它的
	// 目标**全部收敛到新版**时才进 Done。
	case done:
		_ = s.Repos.Rollouts().SetState(ctx, ro.ID, store.RolloutSucceeded, "", &now)
		s.event(ctx, comp.SiteID, "rollout.succeeded", comp.Name, map[string]any{
			"from": ro.FromVersion, "to": ro.ToVersion,
		})

	// **有批次时全局超时不生效**，那是 BatchTimeout 的活。
	//
	// 一次 4 批的升级合法地要花 4×（物化 + 稳定窗口），全局 10 分钟会在
	// 第三批上误判失败——而且它给出的是「10 分钟内未收敛」这句说不出该看
	// 哪台机器的废话，正好盖掉 BatchTimeout 那条指名道姓的。
	case !batched && now.Sub(ro.StartedAt) > RolloutTimeout:
		_ = s.Repos.Rollouts().SetState(ctx, ro.ID, store.RolloutFailed,
			fmt.Sprintf("did not converge within %s", RolloutTimeout), &now)
		s.event(ctx, comp.SiteID, "rollout.failed", comp.Name, map[string]any{
			"to": ro.ToVersion, "reason": "timed out without converging",
		})
	}
}

// advanceBatches 放行下一批。
//
// 返回 (done, batched)：done 是「这次变更做完了没有」，batched 说明这条
// Rollout 到底有没有批次记录——后者决定全局 RolloutTimeout 还算不算数。
//
// 一次调用最多走一步：要么把当前批标成完成并放行下一批，要么什么都不做。
// **不在一次调用里连推多批**——那会让「当前批已收敛」这个判据用的是放行
// 之前的观测，等于没判。
//
// **它是「这次变更成了没有」的唯一判据**，因此没有批次记录时也必须回答得
// 准：那时变更是一次性下发的，判据回落到整体收敛。
func (s *Service) advanceBatches(
	ctx context.Context, comp store.Component, ro store.Rollout, view *StatusView,
) (done, batched bool, err error) {
	rel, err := s.releaseStateOf(ctx, comp)
	if err != nil {
		return false, false, err
	}
	if !rel.Active {
		// 没有批次记录：分批那一步失败过。此时变更是一次性下发的，
		// 判据回落到整体收敛，全局超时继续管着它。
		return view.Converged && view.Version == ro.ToVersion, false, nil
	}

	gateStates := map[int64]instanceGateState{}
	for _, iv := range view.Instances {
		gateStates[iv.ID] = gateStateOf(iv)
	}
	now := s.now()

	batches := rel.Batches
	for i := range batches {
		b := batches[i]
		switch b.State {
		case store.BatchDone, store.BatchSkipped:
			continue

		case store.BatchReleased:
			// **健康门禁**（22-multi-node §2.5）：收敛、健康、没崩、稳住。
			//
			// 只看收敛的话，Rollout 会用一台正在崩溃循环里的机器去批准
			// 下一批——故障因此被逐批放大。
			v := checkGate(b, gateStates, now)
			if err := s.saveGate(ctx, b, v); err != nil {
				return false, true, err
			}
			if !v.Passed {
				if timedOut(b, now) {
					return false, true, s.failBatch(ctx, comp, ro, b, v)
				}
				return false, true, nil // 还在等这一批
			}
			if err := s.Repos.RolloutBatches().SetState(ctx,
				b.ID, store.BatchDone); err != nil {
				return false, true, err
			}
			s.log().Info("batch passed gate", "component", comp.Name,
				"batch", b.Seq, "total", len(batches),
				"role", b.Role, "nodes", batchNodes(b),
				"stable_for", now.Sub(b.HealthySince).Round(time.Second).String())
			// 继续往下：把下一批放行

		case store.BatchPending:
			if err := s.Repos.RolloutBatches().Release(ctx, b.ID, now); err != nil {
				return false, true, err
			}
			s.log().Info("batch released", "component", comp.Name,
				"batch", b.Seq, "total", len(batches),
				"role", b.Role, "nodes", batchNodes(b), "version", ro.ToVersion)
			s.event(ctx, comp.SiteID, "rollout.batch", comp.Name, map[string]any{
				"seq": b.Seq, "total": len(batches),
				"role": b.Role, "nodes": batchNodes(b), "to": ro.ToVersion,
			})
			// **立刻唤醒这一批的节点**：不唤醒的话它们要等下一次轮询才
			// 拿到新规格，每批都白等一个心跳周期。
			if s.Notify != nil {
				for _, t := range b.Targets {
					s.Notify.Notify(t.Node)
				}
			}
			return false, true, nil

		default:
			// 失败的批次：停在这里，等人处理（第 9 步接管）
			return false, true, nil
		}
	}
	return true, true, nil
}

// saveGate 把本轮算出来的窗口落盘。
//
// **只在真的变了的时候写**：门禁每 3 秒被算一次，每次都写一遍会让这张表
// 在一次升级里被改上百次，而其中绝大多数是同一个值。
func (s *Service) saveGate(
	ctx context.Context, b store.RolloutBatch, v gateVerdict,
) error {
	if v.Since.Equal(b.HealthySince) {
		return nil
	}
	return s.Repos.RolloutBatches().SetGate(ctx, b.ID, v.Since, v.Baseline)
}

// failBatch 判一批没过，并把整条 Rollout **停下来等人**。
//
// 状态是 `halted` 而不是 `failed`（22-multi-node §2.6）：它有前路——修好
// 机器之后 `rollout resume` 从这一批续做。`failed` 留给真正走不下去的。
//
// **指名道姓**：错误信息说清是哪台、卡在哪一条。「10 分钟内未收敛」说不出
// 该去看哪台机器，而那正是运维在这一刻唯一需要的东西。
//
// 已完成的批次不动（§2.6）：那是一次新的、更大的变更，由人决定。集群
// 此刻是混合版本的，`rollout status` 要把它显示出来。
//
// **不写 ended_at**：这次变更还没结束，只是停着。写了的话 history 里它
// 看起来就像已经收场了。
func (s *Service) failBatch(
	ctx context.Context, comp store.Component, ro store.Rollout,
	b store.RolloutBatch, v gateVerdict,
) error {
	reason := blockerText(b, v.Blockers)
	if err := s.Repos.RolloutBatches().SetState(ctx, b.ID, store.BatchFailed); err != nil {
		return err
	}
	if err := s.Repos.Rollouts().SetState(ctx,
		ro.ID, store.RolloutHalted, reason, nil); err != nil {
		return err
	}
	s.log().Error("batch failed the gate, stopped subsequent batches, waiting for manual handling",
		"component", comp.Name, "batch", b.Seq, "reason", reason)
	s.event(ctx, comp.SiteID, "rollout.halted", comp.Name, map[string]any{
		"to": ro.ToVersion, "reason": reason, "batch": b.Seq,
	})
	return nil
}

// batchNodes 返回一批涉及的节点名，逗号分隔。
func batchNodes(b store.RolloutBatch) string {
	return strings.Join(nodesOf(b.Targets), ",")
}

// nodesOf 抽出目标的节点名。
func nodesOf(ts []store.BatchTarget) []string {
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.Node)
	}
	return names
}

// rolledBack 报告是否有实例已经被节点自动回滚。
//
// 判据是实例上报的 result / message 里带着回滚标记——**节点才知道它做了
// 什么**，mechd 只能观测。
func rolledBack(view *StatusView) bool {
	for _, in := range view.Instances {
		if in.RolledBack {
			return true
		}
	}
	return false
}

// RolloutView 是 `rollout status` / `history` 的一行。
type RolloutView struct {
	ID    int64  `json:"id"`
	State string `json:"state"`
	Kind  string `json:"kind"`
	From  string `json:"from,omitempty"`
	To    string `json:"to"`
	// Reason 说明它为什么停在这个状态。
	Reason    string `json:"reason,omitempty"`
	StartedAt string `json:"startedAt"`
	EndedAt   string `json:"endedAt,omitempty"`
	// Batch / Batches 是「第 N / 共 M 批」。Batches 为 0 表示没有分批。
	Batch   int `json:"batch,omitempty"`
	Batches int `json:"batches,omitempty"`
	// Current 是当前这一批的说明，形如 `zk n2`。
	Current string `json:"current,omitempty"`
	// Skipped 是被 cordon 而不参与本次变更的节点。
	//
	// **必须列出来**：不列的话，「为什么这台还是旧版」会变成一次排查，
	// 而答案早就有人明确说过了。
	Skipped []string `json:"skipped,omitempty"`
	// Mixed 是失败之后集群里的混合版本分布：版本 → 节点名。
	//
	// **失败时必须显示出来**（22-multi-node §2.6）。已完成的批次不自动
	// 回滚——那是一次新的、更大的变更，由人决定。但那意味着集群此刻是
	// 混合版本的，不说出来的话，运维得自己一台台去看才知道停在哪儿。
	Mixed map[string][]string `json:"mixed,omitempty"`
}

func rolloutView(r store.Rollout) RolloutView {
	v := RolloutView{
		ID: r.ID, State: r.State, Kind: r.Kind,
		From: r.FromVersion, To: r.ToVersion, Reason: r.Reason,
		StartedAt: store.FormatTime(r.StartedAt),
	}
	if r.EndedAt != nil {
		v.EndedAt = store.FormatTime(*r.EndedAt)
	}
	return v
}

// RolloutStatus 返回进行中的 Rollout；没有则返回最近一条。
//
// **没有进行中的也要给出最近一条**：运维敲 `rollout status` 多半是因为刚
// 做过一次升级，而「没有进行中的 Rollout」这句回答等于什么都没说。
func (s *Service) RolloutStatus(ctx context.Context, siteName, name string) (*RolloutView, error) {
	_, comp, _, err := s.load(ctx, siteName, name)
	if err != nil {
		return nil, err
	}
	if ro, err := s.Repos.Rollouts().Active(ctx, comp.ID); err == nil {
		v := rolloutView(ro)
		s.fillBatches(ctx, ro, &v)
		return &v, nil
	}
	list, err := s.Repos.Rollouts().List(ctx, comp.ID, 1)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, faults.Permanentf("", "%s has never had a version change", name)
	}
	v := rolloutView(list[0])
	s.fillBatches(ctx, list[0], &v)
	return &v, nil
}

// fillBatches 补上「第 N / 共 M 批」与被跳过的节点。
//
// 查不到批次不该让 `rollout status` 整个失败：批次是这条记录的**细节**，
// 而运维敲这条命令首先要看的是「成了没有」。
func (s *Service) fillBatches(
	ctx context.Context, ro store.Rollout, v *RolloutView,
) {
	rel, err := s.releaseStateOfRollout(ctx, ro)
	if err != nil || !rel.Active {
		return
	}
	done, total := rel.progress()
	v.Batches = total
	v.Batch = done
	if cur := rel.current(); cur != nil {
		v.Batch = done + 1 // 有一批正在做，它就是「现在第几批」
		v.Current = cur.Role + " " + batchNodes(*cur)
	}
	v.Skipped = rel.skippedNodes()
	// **停下来的时候才列**：正常推进中每一秒的分布都在变，列出来只是噪声；
	// 而停住之后它是运维最需要的一张图——已完成的批次不自动回滚（§2.6），
	// 集群一定是混着的。
	if v.State == store.RolloutHalted || v.State == store.RolloutFailed {
		v.Mixed = rel.mixedVersions(v.From, v.To)
	}
}

// RolloutHistory 列出历史 Rollout，最近的在前。
func (s *Service) RolloutHistory(
	ctx context.Context, siteName, name string, limit int,
) ([]RolloutView, error) {
	_, comp, _, err := s.load(ctx, siteName, name)
	if err != nil {
		return nil, err
	}
	list, err := s.Repos.Rollouts().List(ctx, comp.ID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]RolloutView, 0, len(list))
	for _, r := range list {
		out = append(out, rolloutView(r))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// SetRolloutPaused 冻结或恢复判定。
func (s *Service) SetRolloutPaused(
	ctx context.Context, siteName, name string, paused bool, actor string,
) (*RolloutView, error) {
	_, comp, _, err := s.load(ctx, siteName, name)
	if err != nil {
		return nil, err
	}
	if err := s.refuseIfRemoving(ctx, comp); err != nil {
		return nil, err
	}
	ro, err := s.Repos.Rollouts().Active(ctx, comp.ID)
	if err != nil {
		return nil, faults.Permanentf("", "%s has no version change in progress", name)
	}

	want := store.RolloutRunning
	reason := ""
	action := "resume"
	if paused {
		want, action = store.RolloutPaused, "pause"
		reason = "manually paused"
		// **停下来等人的时候不该还能「暂停」**：它已经停了，pause 什么
		// 都不会改变，却会把「为什么停的」那句原因冲掉——而那正是运维
		// 此刻唯一需要的信息。
		if ro.State == store.RolloutHalted {
			return nil, faults.Permanentf("",
				"%s's version change has already halted due to a failure, pause is not needed\n"+
					"  reason: %s\n"+
					"  once it's fixed, use rollout resume to continue, or rollout abort to roll it all back",
				name, ro.Reason)
		}
	}
	if ro.State == want {
		return nil, faults.Permanentf("", "%s's version change is already %s", name, want)
	}

	// 从 halted 恢复要**把断点重新支起来**，不只是改个状态字段。
	if !paused && ro.State == store.RolloutHalted {
		if err := s.rewindFailedBatch(ctx, comp, ro); err != nil {
			return nil, err
		}
	}

	if err := s.Repos.Rollouts().SetState(ctx, ro.ID, want, reason, nil); err != nil {
		return nil, err
	}

	s.audit(ctx, actor, "rollout-"+action, name, nil, want)
	s.event(ctx, comp.SiteID, "rollout."+action, name, map[string]any{"to": ro.ToVersion})

	ro.State, ro.Reason = want, reason
	v := rolloutView(ro)
	s.fillBatches(ctx, ro, &v)
	return &v, nil
}

// rewindFailedBatch 把没过门禁的那一批重新支起来，供 resume 用。
//
// 三件事，一件不多（22-multi-node §2.6）：
//
//	① 状态退回 released —— 它重新成为「正在等它收敛」的那一批
//	② 门禁窗口清零 —— 修好之后要重新稳住 StableFor 才算过，
//	   不能靠故障之前攒的时间过关
//	③ 放行时刻重置 —— batchTimeout 从头算，否则刚 resume 就会立刻再超时
//
// **已完成的批次一个都不碰**，这就是「断点续做，不重做」。
func (s *Service) rewindFailedBatch(
	ctx context.Context, comp store.Component, ro store.Rollout,
) error {
	rel, err := s.releaseStateOfRollout(ctx, ro)
	if err != nil {
		return err
	}
	now := s.now()
	for _, b := range rel.Batches {
		if b.State != store.BatchFailed {
			continue
		}
		// Release 一次写状态与放行时刻，正好是 ①③
		if err := s.Repos.RolloutBatches().Release(ctx, b.ID, now); err != nil {
			return err
		}
		// ② 窗口清零：零值表示「还没开始攒」
		if err := s.Repos.RolloutBatches().SetGate(ctx, b.ID, time.Time{}, nil); err != nil {
			return err
		}
		s.log().Info("resuming batch", "component", comp.Name,
			"batch", b.Seq, "role", b.Role, "nodes", batchNodes(b))
		// **立刻唤醒这一批的节点**：它们可能在故障期间断过连
		if s.Notify != nil {
			for _, t := range b.Targets {
				s.Notify.Notify(t.Node)
			}
		}
	}
	return nil
}

// AbortRollout 中止并回退到起始版本。
//
// **它真的会回退**，不只是把记录标成中止：一条只改状态字段的 abort 会让
// 运维以为世界回到了升级前，而机器上跑的还是新版——那比没有这个命令更糟。
func (s *Service) AbortRollout(
	ctx context.Context, siteName, name, actor string,
) (*RolloutView, error) {
	_, comp, _, err := s.load(ctx, siteName, name)
	if err != nil {
		return nil, err
	}
	ro, err := s.Repos.Rollouts().Active(ctx, comp.ID)
	if err != nil {
		return nil, faults.Permanentf("", "%s has no version change in progress", name)
	}
	if ro.FromVersion == "" {
		return nil, faults.Permanentf("", "%s's current change has no recorded starting version, cannot roll back automatically\n"+
			"  use mechctl component rollback --to-version to specify a target", name)
	}

	if _, err := s.Rollback(ctx, RollbackRequest{
		Site: siteName, Component: name, Version: ro.FromVersion, Actor: actor,
	}); err != nil {
		return nil, faults.Permanentf("", "rolling back to %s failed: %w", ro.FromVersion, err)
	}

	// Rollback 会新开一条 Rollout 并把这条标成 aborted（startRollout 的接管
	// 逻辑），因此这里只补上更准确的原因。
	now := s.now()
	_ = s.Repos.Rollouts().SetState(ctx, ro.ID, store.RolloutAborted,
		fmt.Sprintf("manually aborted, rolled back to %s", ro.FromVersion), &now)
	s.audit(ctx, actor, "rollout-abort", name, nil, ro.FromVersion)

	ro.State = store.RolloutAborted
	ro.Reason = fmt.Sprintf("manually aborted, rolled back to %s", ro.FromVersion)
	ro.EndedAt = &now
	v := rolloutView(ro)
	return &v, nil
}
