package mechd

import (
	"context"
	"fmt"
	"sort"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/store"
)

// RolloutPolicyView 是两个旋钮的当前值。
type RolloutPolicyView struct {
	Component      string `json:"component"`
	MaxUnavailable int    `json:"maxUnavailable"`
	Canary         int    `json:"canary"`
	// Note 说明这两个值下一次变更才生效。
	Note string `json:"note,omitempty"`
	// QuorumRoles 是声明了仲裁、因此**不受 maxUnavailable 控制**的角色。
	//
	// **必须说出来**：那个旋钮在 Component 上，而 quorum 在角色上——
	// 用户设了 2 却看到每批只动 1 台，不说的话他会以为设置没生效。
	// 静默压掉等于把他的意图丢了还不告诉他。
	QuorumRoles []string `json:"quorumRoles,omitempty"`
}

// RolloutPolicy 读出一个组件的滚动升级旋钮。
func (s *Service) RolloutPolicy(
	ctx context.Context, siteName, name string,
) (*RolloutPolicyView, error) {
	_, comp, _, err := s.load(ctx, siteName, name)
	if err != nil {
		return nil, err
	}
	return policyView(comp, s.policyNote(ctx, comp), s.quorumRoles(comp)), nil
}

// policyNote 在有变更进行中时提醒「这两个值对它不生效」。
//
// 读的时候也说：运维多半是在**变更进行中**才想起来看这两个值，
// 而那正是最容易误以为「改了就生效」的时刻。
func (s *Service) policyNote(ctx context.Context, comp store.Component) string {
	if _, err := s.Repos.Rollouts().Active(ctx, comp.ID); err != nil {
		return ""
	}
	return "a change is in progress, these two values do not take effect on it -- batches were already computed when it started"
}

// SetRolloutPolicy 改滚动升级的两个旋钮。
//
// **只影响下一次变更**：批次在 Rollout 创建时一次算好并落盘
// （22-multi-node §4），改旋钮不会重算已经在跑的那次。这是刻意的——
// 半路重算批次会让「一共几批」这个答案在执行过程中变化，而运维正是
// 靠它判断「还要多久」。
//
// nil 表示不改这一项：0 是 canary 的合法值（表示关掉金丝雀），
// 用 0 当「没传」会让人永远关不掉它。
func (s *Service) SetRolloutPolicy(
	ctx context.Context, siteName, name string,
	maxUnavailable, canary *int, actor string,
) (*RolloutPolicyView, error) {
	_, comp, _, err := s.load(ctx, siteName, name)
	if err != nil {
		return nil, err
	}
	if err := s.refuseIfRemoving(ctx, comp); err != nil {
		return nil, err
	}

	mu, cn := comp.RolloutMaxUnavailable, comp.RolloutCanary
	if maxUnavailable != nil {
		if *maxUnavailable < 1 {
			return nil, faults.Permanentf("", "--max-unavailable must be at least 1, got %d\n"+
				"  setting it to 0 means nothing is ever allowed to move, which isn't a safer default",
				*maxUnavailable)
		}
		mu = *maxUnavailable
	}
	if canary != nil {
		if *canary < 0 {
			return nil, faults.Permanentf("", "--canary cannot be negative, got %d", *canary)
		}
		cn = *canary
	}
	if mu == comp.RolloutMaxUnavailable && cn == comp.RolloutCanary {
		return policyView(comp, s.policyNote(ctx, comp), s.quorumRoles(comp)), nil
	}

	if err := s.Repos.Components().SetRollout(ctx, comp.ID, mu, cn, s.now()); err != nil {
		return nil, err
	}
	s.audit(ctx, actor, "set-rollout", name, nil,
		fmt.Sprintf("maxUnavailable=%d canary=%d", mu, cn))

	// 说出来：不说的话，运维会以为改完就生效，然后盯着一张按旧值
	// 分好的批次表纳闷。
	comp.RolloutMaxUnavailable, comp.RolloutCanary = mu, cn
	return policyView(comp, s.policyNote(ctx, comp), s.quorumRoles(comp)), nil
}

func policyView(comp store.Component, note string, quorum []string) *RolloutPolicyView {
	mu, cn := comp.RolloutMaxUnavailable, comp.RolloutCanary
	if mu <= 0 {
		mu = DefaultMaxUnavailable
	}
	return &RolloutPolicyView{
		Component: comp.Name, MaxUnavailable: mu, Canary: cn,
		Note: note, QuorumRoles: quorum,
	}
}

// quorumRoles 列出这个组件里声明了仲裁的角色。
//
// 解析不出 Pack 时返回空：这只是一句提示，不该让读取旋钮本身失败。
func (s *Service) quorumRoles(comp store.Component) []string {
	entry, err := s.Packs.Resolve(comp.Pack.Name, "="+comp.Pack.Version)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range entry.Pack.RolesForProfile(comp.Profile) {
		if r.Enabled && r.Quorum {
			out = append(out, r.Name)
		}
	}
	sort.Strings(out)
	return out
}
