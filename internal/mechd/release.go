package mechd

import (
	"context"
	"errors"
	"sort"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/render"
	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/store"
)

// releaseState 说明一个组件此刻处在什么发布状态。
//
// 没有进行中的 Rollout 时它是零值——那是绝大多数时候的情况，
// 下发照常按 Component 当前版本渲染。
type releaseState struct {
	// Active 为 false 表示没有分批中的变更。
	Active bool
	// FromVersion 是还没轮到的实例应当停留的版本。
	FromVersion string
	// Released 是已经放行的实例 id。
	Released map[int64]bool
	// Batches 是全部批次，供推进与展示。
	Batches []store.RolloutBatch
	Rollout store.Rollout
}

// releaseStateOf 取一个组件当前的发布状态。
//
// **这是分批下发的全部机制**：Assignment 按它决定每个实例拿哪个版本的
// 规格。没有它的话，「分批」就只是一个观测构造——status 说着「第 1/4 批」
// 而四批的机器早已全部升完，那比没有分批更糟。
//
// 它看的是**最近一条** Rollout，不是「进行中」那条。差别在失败之后：
// 一条刚被判失败的 Rollout 不再是 active，若门禁跟着消失，剩下的批次会在
// 下一次心跳里**一起升上去**——正好是这道门禁存在的全部理由的反面。
// 「停止下发后续批次」（§2.6）要求那个门在失败之后继续关着。
func (s *Service) releaseStateOf(
	ctx context.Context, comp store.Component,
) (releaseState, error) {
	list, err := s.Repos.Rollouts().List(ctx, comp.ID, 1)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return releaseState{}, nil
		}
		return releaseState{}, err
	}
	if len(list) == 0 {
		return releaseState{}, nil
	}
	ro := list[0]
	if !gatesDelivery(ro.State) {
		return releaseState{}, nil
	}
	return s.releaseStateOfRollout(ctx, ro)
}

// gatesDelivery 报告某个状态的 Rollout 还管不管下发。
//
//	running / paused / halted / failed  管——没轮到的实例继续停在旧版
//	succeeded                           不管（每一批都放行过了，一个样）
//	aborted                             不管（一定已被更新的一条取代）
//
// **halted 与 failed 也管**是这里唯一不显然的一条，理由见 releaseStateOf：
// 停下来之后门禁必须继续关着，否则剩下的批次会在下一次心跳里一起升上去。
func gatesDelivery(state string) bool {
	switch state {
	case store.RolloutRunning, store.RolloutPaused,
		store.RolloutHalted, store.RolloutFailed:
		return true
	}
	return false
}

// releaseStateOfRollout 取**指定**那一条 Rollout 的发布状态。
//
// 已经结束的也读得出来：`rollout status` 在事后被敲的次数远多于事中，
// 而那时最想知道的正是「一共几批、跳过了谁」。
func (s *Service) releaseStateOfRollout(
	ctx context.Context, ro store.Rollout,
) (releaseState, error) {
	batches, err := s.Repos.RolloutBatches().List(ctx, ro.ID)
	if err != nil {
		return releaseState{}, err
	}
	if len(batches) == 0 {
		// 没有批次记录：单机形态，或者分批那一步失败过。
		// **一次性下发**而不是全部卡住——分批是为了控制影响面，
		// 而「谁都不动」不是更安全的默认，是更糟的默认。
		return releaseState{}, nil
	}

	st := releaseState{
		Active: true, FromVersion: ro.FromVersion,
		Released: map[int64]bool{}, Batches: batches, Rollout: ro,
	}
	for _, b := range batches {
		// **failed 也算放行过。**
		//
		// 那一批的机器上已经装着新版了。把规格抽回旧版等于替人做了一次
		// 「整体回滚」的决定，而 §2.6 说得很清楚：那是一次新的、更大的
		// 变更，由人决定（`rollout abort` 会真的做，只是需要人点一下）。
		// 何况被门禁拦下的机器多半正在崩，再给它一次版本切换只会更糟。
		if b.State != store.BatchReleased &&
			b.State != store.BatchDone && b.State != store.BatchFailed {
			continue
		}
		for _, t := range b.Targets {
			st.Released[t.InstanceID] = true
		}
	}
	return st, nil
}

// versionFor 返回某个实例此刻该拿哪个版本。
//
// **被 cordon 而没进任何批次的实例也停在旧版**：它们不在 Released 里，
// 因此自然落到 FromVersion 那一侧。那正是 cordon 的语义——别动这台。
func (st releaseState) versionFor(instanceID int64, current string) string {
	if !st.Active || st.Released[instanceID] {
		return current
	}
	if st.FromVersion == "" {
		// 没记住起始版本（首次部署走的不是升级路径）——只能按当前版本发。
		return current
	}
	return st.FromVersion
}

// current 返回**现在卡在哪一批**；没有的话返回 nil。
//
// 先找正在等收敛的（released），再找没过门禁停下的（failed）。后者不能漏：
// 停下来之后运维问的第一句就是「停在第几批」，而那时那一批已经不是
// released 了——只认 released 的话 `rollout status` 会说「第 0/3 批」。
func (st releaseState) current() *store.RolloutBatch {
	for i := range st.Batches {
		if st.Batches[i].State == store.BatchReleased {
			return &st.Batches[i]
		}
	}
	for i := range st.Batches {
		if st.Batches[i].State == store.BatchFailed {
			return &st.Batches[i]
		}
	}
	return nil
}

// progress 返回「第 N / 共 M 批」里的两个数。
//
// N 是**已经完成的批数 + 1**（还在进行时），全部完成时等于 M。
// 分母是全局的，跨阶段连续——运维问的是「还剩几批」，不是
// 「这个阶段还剩几批」。
func (st releaseState) progress() (done, total int) {
	for _, b := range st.Batches {
		if b.Seq == 0 {
			continue // 被 cordon 的名单，不是一批
		}
		total++
		if b.State == store.BatchDone {
			done++
		}
	}
	return done, total
}

// mixedVersions 把集群此刻的版本分布按「版本 → 节点」列出来。
//
// 放行过的批次（含失败的那一批）在新版，其余在旧版。**这是失败之后运维
// 最需要的一张图**：已完成的批次不自动回滚（§2.6），因此集群一定是混合
// 版本的，而不说出来的话他得一台台机器去看。
func (st releaseState) mixedVersions(from, to string) map[string][]string {
	if !st.Active || from == "" || from == to {
		return nil
	}
	out := map[string][]string{}
	for _, b := range st.Batches {
		v := from
		if b.Seq > 0 && (b.State == store.BatchReleased ||
			b.State == store.BatchDone || b.State == store.BatchFailed) {
			v = to
		}
		out[v] = append(out[v], nodesOf(b.Targets)...)
	}
	for v := range out {
		sort.Strings(out[v])
	}
	return out
}

// skippedNodes 返回被 cordon 而不参与本次变更的节点。
func (st releaseState) skippedNodes() []string {
	for _, b := range st.Batches {
		if b.Seq == 0 && b.State == store.BatchSkipped {
			return nodesOf(b.Targets)
		}
	}
	return nil
}

// versionsInPlay 返回这次下发要渲染哪几个版本。
//
// 没有分批中的变更时只有一个——那是绝大多数时候的情况，多渲染一遍是
// 白花的力气。分批期间最多两个：新版与起始版。
func (s *Service) versionsInPlay(comp store.Component, rel releaseState) []string {
	out := []string{comp.Pack.Version}
	if rel.Active && rel.FromVersion != "" && rel.FromVersion != comp.Pack.Version {
		out = append(out, rel.FromVersion)
	}
	return out
}

// gated 是一次「按批次分版本」的解析结果。
//
// **两次渲染，不是一次渲染再改改**：解析管线是纯函数，同一份输入必然
// 产出同样的 digest。想在一次渲染的结果上「把版本号改回去」，得到的是
// 一个从没被算出来过的规格——它的 digest 与任何一边都对不上，节点会
// 当成第三个版本反复物化。
//
// 代价（写在这里是因为它是这次设计的固有成本，不是疏漏）：混版期间
// 每个实例看到的 `topology.…` 视图来自**它自己那一版**的 Pack。新版
// 实例眼里所有同伴都是新版布局，旧版实例眼里都是旧版布局。要让两边
// 看到同一份混合视图，就得让渲染依赖「别人现在是哪一版」——那会让
// 每台机器的规格随别人的进度变化，滚动过程中反复重算，比这个偏差
// 糟得多。
type gated struct {
	rel releaseState
	// byVersion 是版本 → 该版本下**全部实例**的解析结果。
	byVersion map[string]*render.Result
	// current 是组件当前（目标）版本。
	current  string
	Warnings []string
}

// renderGated 按发布状态解析组件：已放行的实例用新版，其余停在旧版。
func (s *Service) renderGated(
	ctx context.Context, site store.Site, comp store.Component,
	inputs []instanceInput,
) (*gated, error) {
	rel, err := s.releaseStateOf(ctx, comp)
	if err != nil {
		// 查不到发布状态时按当前版本下发：分批是为了控制影响面，
		// 而「谁都不动」不是更安全的默认，是更糟的默认。
		s.log().Error("failed to query release state, resolving against the current version this time",
			"component", comp.Name, "err", err)
		rel = releaseState{}
	}

	g := &gated{rel: rel, byVersion: map[string]*render.Result{},
		current: comp.Pack.Version}
	var firstErr error
	for _, v := range s.versionsInPlay(comp, rel) {
		entry, err := s.Packs.Resolve(comp.Pack.Name, "="+v)
		if err != nil {
			if v == comp.Pack.Version {
				firstErr = faults.Permanentf("", "parsing Pack %s@%s: %w", comp.Pack.Name, v, err)
			} else {
				// 旧版 Pack 已经不在本地集合里了——那一侧的实例这次拿不到
				// 规格。**不能就此把新版发给它们**：那等于绕开分批。
				s.log().Error("starting version's Pack is not in the local collection, not dispatched to not-yet-released instances this round",
					"component", comp.Name, "version", v, "err", err)
			}
			continue
		}
		res, err := s.renderComponent(ctx, site, packAt(comp, v), entry.Pack, inputs, false)
		if err != nil {
			if v == comp.Pack.Version {
				firstErr = err
			} else {
				s.log().Error("failed to render starting version, not dispatched to not-yet-released instances this round",
					"component", comp.Name, "version", v, "err", err)
			}
			continue
		}
		g.byVersion[v] = res
		if v == comp.Pack.Version {
			g.Warnings = res.Warnings
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return g, nil
}

// packAt 返回把版本换成 v 的组件副本。
func packAt(comp store.Component, v string) store.Component {
	comp.Pack.Version = v
	return comp
}

// specFor 返回某个实例此刻该拿的规格；拿不到时返回 nil。
func (g *gated) specFor(instanceID int64, key string) *spec.ResolvedSpec {
	res := g.byVersion[g.rel.versionFor(instanceID, g.current)]
	if res == nil {
		return nil
	}
	return res.Specs[key]
}

// secretsFor 返回某个实例该拿的密钥表；与 specFor 取自同一次解析。
func (g *gated) secretsFor(instanceID int64) map[string]string {
	res := g.byVersion[g.rel.versionFor(instanceID, g.current)]
	if res == nil {
		return nil
	}
	return res.Secrets
}

// result 返回目标版本的完整解析结果，供不分实例看的用途（导出、诊断）。
func (g *gated) result() *render.Result { return g.byVersion[g.current] }
