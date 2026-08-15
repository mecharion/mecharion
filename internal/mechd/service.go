// Package mechd 是控制面的应用服务层。
//
// 设计见 docs/design/13-mechd.md。它把前面几步交付的库串成用户能用的动作：
//
//	store      期望状态与观测状态
//	vault      密钥
//	packindex  本地 Pack 集合
//	placement  放置校验与 ordinal
//	render     解析管线
//	protocol   与 mechlet 的 gRPC
//
// **mechd 不执行任何部署动作**——那是 mechlet 的事（ADR-0002）。
// 它退场时单机能力不受影响，这条边界决定了这里的全部划分。
package mechd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/packindex"
	"github.com/mecharion/mecharion/internal/placement"
	"github.com/mecharion/mecharion/internal/render"
	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/store"
	"github.com/mecharion/mecharion/internal/vault"
)

// Notifier 让服务层在期望状态变化后唤醒受影响的节点。
//
// 抽成接口是为了让服务层可以脱离 gRPC 单独测：deploy 的正确性
// （放置、渲染、落库顺序）与「谁被唤醒了」无关。
type Notifier interface {
	Notify(node string)
}

// Presence 回答「这台机器此刻还挂在线上吗」。
//
// 与 Notifier 分开是因为**读者不同**：Notify 是服务层推给节点，
// Presence 是服务层向传输层要一个事实。合成一个接口会让
// 「实现它需要什么」变得含糊。
type Presence interface {
	Connected(node string) bool
}

// 节点状态的三个值，是**显示层**的枚举，与 store 那三个值（reserved /
// pending / seen）不是一一对应：store 侧 reserved 与「已发证书未连接」的
// pending 在这里显示成同一个 NodePending——两者对运维呈现的都是「还没
// 起来」，区分它们是 Join 内部的身份把关，不是给人看的状态
// （22-multi-node §6.16）。online 与 offline 的区分在读的这一刻算出来。
const (
	NodePending = "pending"
	NodeOnline  = "online"
	NodeOffline = "offline"
)

// nodeStatus 算出一台机器此刻该显示成什么。
//
// **三个值不能并成两个**：「还没装」与「装了但死了」对运维是完全不同的
// 两件事——前者去那台机器上敲 join，后者去查它为什么掉了。合并之后唯一
// 的线索是「最后上报时间是不是空的」，那要求看的人知道该去比对哪个字段。
func nodeStatus(seen, connected bool) string {
	if connected {
		return NodeOnline
	}
	if !seen {
		return NodePending
	}
	return NodeOffline
}

// connected 问传输层某台机器在不在线。
//
// Presence 为空时一律答「不在线」而不是「沿用存的那个值」：
// 沿用会让一次接线遗漏（忘了给 Service 装上 Presence）表现成
// 「状态看着挺正常」，而那正是 §6.13 里那个只进不出的门的症状。
// 答 offline 是错的方向里安全的那个——它会立刻被人看见。
func (s *Service) connected(name string) bool {
	if s.Presence == nil {
		return false
	}
	return s.Presence.Connected(name)
}

// Service 是控制面的应用服务。
type Service struct {
	Store *store.Store
	Repos store.Repos
	Vault *vault.Vault
	Packs *packindex.Index
	// BlobDir 是内容寻址的载荷根目录。
	BlobDir  string
	Notify   Notifier
	Presence Presence
	// Tasks 发 ad-hoc 命令并把结果拿回来（ADR-0038）。
	//
	// 可以为 nil：服务层的其余部分与「能不能发命令」无关，而单机形态下
	// 这条通道同样存在（mechlet 照样是拨出来的）。nil 时 restart 明确
	// 报错，不静默。
	Tasks Tasker
	// Watch 让正在看的浏览器知道「有东西变了」（23-web-ui §4.5.2）。
	// 可以为 nil：服务层的正确性与「谁在看」无关。
	Watch *Hub
	Log   *slog.Logger
	// Now 可替换，供测试固定时间。
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// ── 请求与结果 ──────────────────────────────────────────────────────────

// DeployRequest 是一次部署请求。
type DeployRequest struct {
	Site string
	// Pack 是 Pack 名；Component 缺省时等于它。
	//
	// 默认相等是刻意的：同一个 Pack 可以在一个 Site 里部署多份
	// （pg-main 与 pg-report），但那要用户主动改名——只有那时他才是知情的。
	Pack      string
	Component string
	Profile   string

	// Roles 是 角色 → 节点名。未列出的角色按 cardinality 下限判定。
	Roles map[string][]string
	// Nodes 是「所有角色都放这些节点上」的简写，Roles 为空时生效。
	Nodes []string

	// Set 是 Component 级参数覆盖。
	Set map[string]any
	// Require 是显式的依赖绑定：依赖名 → Component 名。
	Require map[string]string

	// Update 允许覆盖已存在的 Component。默认拒绝——
	// 一次手滑的重复 deploy 不该悄悄改掉线上参数。
	Update bool
	// AllowRemove 允许缩容。默认拒绝——「我少写了一个节点名」
	// 不该导致一个实例被卸载（14-placement §6）。
	AllowRemove bool
	DryRun      bool
	Actor       string
}

// DeployResult 是一次部署的结果。
type DeployResult struct {
	Component string
	Plan      *placement.Plan
	// Specs 是渲染出的规格，键为 "<role>@<node>"。
	Specs map[string]*spec.ResolvedSpec
	// Digests 是每个实例的 digest，用于展示与收敛判定。
	Digests  map[string]string
	Warnings []string
	// DryRun 为 true 时什么都没落库、没下发。
	DryRun bool
}

// ── deploy ──────────────────────────────────────────────────────────────

// Deploy 执行一次部署（13-mechd §2）。
//
// 顺序上有两条硬要求：
//
//	② 组件级拓扑排序早于 ③ 渲染——消费方要等提供方解析完
//	④ 落库早于 ⑤ 下发——反过来的话 mechd 崩溃会留下「机器上装了、
//	  库里没有」的孤儿，而机器上没有主人的东西最难清理
func (s *Service) Deploy(ctx context.Context, req DeployRequest) (*DeployResult, error) {
	site, err := s.resolveSite(ctx, req.Site)
	if err != nil {
		return nil, err
	}

	// 重扫一次：新放进 pack-dir 的 Pack 应当立刻可用，不该要求重启 mechd
	if err := s.Packs.Reload(); err != nil {
		s.log().Warn("failed to rescan Pack collection, keeping the previous index", "err", err)
	}

	entry, err := s.Packs.Resolve(req.Pack, "")
	if err != nil {
		return nil, faults.Permanentf("", "parsing Pack %s: %w", req.Pack, err)
	}
	p := entry.Pack
	name := req.Component
	if name == "" {
		name = req.Pack
	}

	// ── ① 校验 ──
	comp, existed, err := s.upsertComponentDraft(ctx, site, name, p, req)
	if err != nil {
		return nil, err
	}
	nodes, err := s.resolveNodes(ctx, site.ID, req, p)
	if err != nil {
		return nil, err
	}

	existing, byID, err := s.existingInstances(ctx, comp.ID)
	if err != nil {
		return nil, err
	}

	// ── ② 放置 ──
	plan, err := placement.Compute(placement.Input{
		Component: name, Pack: p, Profile: comp.Profile,
		Nodes: nodes, Existing: existing, NodeByID: byID,
	})
	if err != nil {
		return nil, err
	}
	if len(plan.Remove) > 0 && !req.AllowRemove && !req.DryRun {
		return nil, s.removeRefusal(ctx, name, plan, byID)
	}

	out := &DeployResult{
		Component: name, Plan: plan, DryRun: req.DryRun,
		Digests: map[string]string{}, Warnings: plan.Warnings,
	}

	// ── ③④⑤ 落库并渲染：一个 Unit of Work ──
	//
	// ordinal 必须在渲染之前分配：模板要用它生成节点身份
	// （ZooKeeper 的 myid、Kafka 的 node.id）。
	//
	// **Component 落库、实例 ensure/delete、事实冻结、渲染（含渲染管线
	// 里顺手固化的依赖绑定）此前是分步提交**：任何一步失败，前面已经
	// 写完的部分留在库里不会回退——一个部分放置的组件、事实没冻结的
	// 实例，或者固化了绑定却没能渲染出规格的悬空状态。
	//
	// 现在这一整块包进 `s.Store.InTx`：失败自动回滚，成功才提交。渲染
	// 本身除了固化绑定不写 SQL（密钥进 Vault，是另一套独立存储，不参与
	// 这个事务——生成了但没被引用的密钥是无害孤儿，与 A2 里签发了但
	// 没被用上的证书是同一个道理）。渲染管线的 13 处调用点都不需要
	// 跟着改：事务通过 context 传递，`resolveBindings` 里那次
	// `Bindings().Create` 认出 ctx 里挂着的事务就自动参与进来，不认得
	// 时照旧各写各的——这条路径以外的调用方行为完全不变。
	assigned := append([]placement.Assignment{}, plan.Keep...)
	var instances []instanceInput
	var res *render.Result

	if req.DryRun {
		// 干跑不写任何东西。给 Add 一个**预演序号**：从当前最大值往后排。
		// 不这么做的话渲染会拿到 -1，而 myid=-1 的配置毫无参考价值。
		assigned = append(assigned, previewOrdinals(plan, existing)...)
		instances, err = s.freezeFacts(ctx, comp.ID, assigned, existing, false)
		if err != nil {
			return nil, err
		}
		res, err = s.renderComponent(ctx, site, comp, p, instances, true)
		if err != nil {
			return nil, err
		}
	} else {
		err = s.Store.InTx(ctx, func(ctx context.Context) error {
			if !existed || req.Update {
				comp, err = s.persistComponent(ctx, comp, existed)
				if err != nil {
					return err
				}
			}
			assigned, err = s.ensureInstances(ctx, comp.ID, plan)
			if err != nil {
				return err
			}
			// 事实快照：已有实例保留原值，新实例取当前上报值并固化。
			// 这是解析管线与实时事实之间的隔离层（spec §9.4.1）。
			instances, err = s.freezeFacts(ctx, comp.ID, assigned, existing, true)
			if err != nil {
				return err
			}
			res, err = s.renderComponent(ctx, site, comp, p, instances, false)
			return err
		})
		if err != nil {
			return nil, err
		}
	}

	out.Specs = res.Specs
	out.Warnings = append(out.Warnings, res.Warnings...)
	for k, sp := range res.Specs {
		out.Digests[k] = sp.Digest
	}

	if req.DryRun {
		return out, nil
	}

	// ── ⑥ 下发：唤醒受影响的节点 ──
	s.audit(ctx, req.Actor, "deploy", name, p, "ok")
	s.event(ctx, site.ID, "component.deployed", name, map[string]any{
		"pack": p.Name, "version": p.Version, "instances": len(res.Specs),
	})
	s.notifyNodes(assigned)
	return out, nil
}

// removeRefusal 构造缩容被拒绝时的错误。
//
// **缩小规模必须是显式意图**，不能是「我少写了一个节点名」的后果
// （14-placement §6）。因此错误里要列清被移除的是哪些实例。
func (s *Service) removeRefusal(
	ctx context.Context, name string, plan *placement.Plan, byID map[int64]store.Node,
) error {
	var lines []string
	for _, ri := range plan.Remove {
		lines = append(lines, fmt.Sprintf("  %s@%s (ordinal %d)",
			ri.Role, byID[ri.NodeID].Name, ri.Ordinal))
	}
	return faults.Permanentf("",
		"deploying %s would remove %d existing instance(s):\n%s\n"+
			"  scaling down must be an explicit intent. add --allow-remove to confirm;\n"+
			"  if you just forgot a node name, add it back and retry",
		name, len(plan.Remove), strings.Join(lines, "\n"))
}

// ── 渲染 ────────────────────────────────────────────────────────────────

// renderComponent 跑一遍解析管线。
//
// 每次按需渲染而不缓存：管线是纯函数，同样的输入必然产出同样的 digest，
// 因此重算是安全的，而缓存要处理失效——参数改了、依赖升级了、节点事实
// 刷新了都得失效，漏一条就是「改了不生效」。M3 的规模下重算是毫秒级。
func (s *Service) renderComponent(
	ctx context.Context, site store.Site, comp store.Component, p *pack.Pack,
	instances []instanceInput, dryRun bool,
) (*render.Result, error) {
	return s.renderWith(ctx, site, comp, p, instances, dryRun, nil)
}

// renderWith 是 renderComponent 带密钥视图覆盖的版本。
func (s *Service) renderWith(
	ctx context.Context, site store.Site, comp store.Component, p *pack.Pack,
	instances []instanceInput, dryRun bool, secrets render.SecretStore,
) (*render.Result, error) {
	return s.renderWithGroups(ctx, site, comp, p, instances, dryRun, secrets, nil)
}

// renderWithGroups 再多一层覆盖：**用一份指定的配置组集合渲染**。
//
// 它是「先看看这次组变更会发生什么」的基础——那份组集合还没落库，
// 从库里读是读不到的。groups 为 nil 时照常从库里读。
func (s *Service) renderWithGroups(
	ctx context.Context, site store.Site, comp store.Component, p *pack.Pack,
	instances []instanceInput, dryRun bool, secrets render.SecretStore,
	groups []store.ConfigGroup,
) (*render.Result, error) {
	if groups == nil {
		var err error
		groups, err = s.Repos.ConfigGroups().List(ctx, comp.ID)
		if err != nil {
			return nil, err
		}
	}

	req := render.Request{
		Site:      spec.SiteRef{Name: site.Name, Kind: site.Kind, Labels: site.Labels},
		Component: comp.Name, Pack: p,
		PackRef: spec.PackRef{
			Name: p.Name, Version: p.Version, Revision: p.Revision,
		},
		Profile:   comp.Profile,
		Overrides: overridesFrom(comp, groups),
		// 站点侧的漂移策略覆盖：现场的人说了算（06-state-and-drift §4.2）
		DriftPolicy: comp.DriftPolicy,
	}
	for _, a := range instances {
		req.Instances = append(req.Instances, render.Instance{
			Role: a.Role, Ordinal: a.Ordinal,
			ConfigGroup:  groupNameFor(groups, a.Role, a.Node.Name),
			Node:         renderNode(a.Node, a.Facts),
			PathBindings: pathBindingsFor(groups, a.Role, a.Node.Name),
		})
	}

	// 密钥：干跑时用一次性值，不往 Vault 里写。
	// 一次「先看看会发生什么」不该留下副作用。
	//
	// **一次性值只适合「这个组件还不存在」的干跑**（deploy --dry-run）：
	// 它每次生成新随机值，因此两次渲染的 digest 必然不同。要对比
	// 「改之前 / 改之后」时不能用它，那种场合用 previewSecrets
	// （读真的、写假的），由调用方经 secretsOverride 传进来。
	switch {
	case secrets != nil:
		req.Secrets = secrets
	case dryRun:
		req.Secrets = newEphemeralSecrets()
	default:
		req.Secrets = vault.NewRenderStore(ctx, s.Vault, comp.ID)
	}

	binds, err := s.resolveBindings(ctx, site, comp, p, dryRun)
	if err != nil {
		return nil, err
	}
	req.Requires = binds

	req.Blobs = render.BlobsFor(p, render.DefaultPlatform)

	return render.Render(req)
}

// ── 状态 ────────────────────────────────────────────────────────────────

// InstanceView 是一个实例在 status 里的呈现。
type InstanceView struct {
	// ID 只用于内部关联（批次目标 ↔ 观测），不出现在任何输出里。
	ID      int64  `json:"-"`
	Role    string `json:"role"`
	Node    string `json:"node"`
	Ordinal int    `json:"ordinal"`
	// Want 是期望的 digest，Got 是该实例上报的。
	Want string `json:"want"`
	Got  string `json:"got,omitempty"`
	// Converged 是**收敛判据**：digest 一致且健康。
	// 靠状态而非「mechlet 说我成功了」这个事件来判定。
	Converged bool   `json:"converged"`
	Result    string `json:"result,omitempty"`
	Workload  string `json:"workload,omitempty"`
	// Restarts 是工作负载的累计重启次数。
	//
	// 显示出来是因为它本来就该看得见：一个每分钟崩一次又被拉起的服务，
	// 在「workload=running、health=healthy」这两列里与健康的没有区别。
	Restarts   int    `json:"restarts,omitempty"`
	Health     string `json:"health,omitempty"`
	Generation int    `json:"generation,omitempty"`
	ReportedAt string `json:"reportedAt,omitempty"`
	// RunState 是**期望**运行态（running | stopped），不是观测到的状态。
	//
	// 少了它，一个被运维显式停掉的实例在 status 里只显示
	// `workload=stopped`，与「它挂了」长得一模一样。
	RunState string `json:"runState,omitempty"`
	// WorkloadAction 是调和器上一轮对工作负载做的事：restored | stopped。
	//
	// 它让「服务被人停了又被拉起来」在中心看得见——否则那一轮报的是 ok。
	WorkloadAction   string `json:"workloadAction,omitempty"`
	WorkloadActionAt string `json:"workloadActionAt,omitempty"`
	// RolledBack 表示节点把这个实例回滚掉了：它跑的不是期望的那一版。
	//
	// **只有节点知道它做了什么**，mechd 只能观测——因此这条来自上报，
	// 而不是 mechd 自己推断。
	RolledBack bool `json:"rolledBack,omitempty"`
	// PendingVersion 非空表示这个实例**还没轮到**，此刻按这个版本运行。
	//
	// 它说明这台**为什么**没收敛：在排队，不是出了问题。这两种「没收敛」
	// 在滚动升级中最需要分清——一个只要等，一个要人去看。
	PendingVersion string `json:"pendingVersion,omitempty"`
	// Drift 是该实例上被检出的漂移。
	Drift []DriftView `json:"drift,omitempty"`
	// Suppressed 是被 ack-drift 抑制的资源，"" 表示整个实例。
	Suppressed []string `json:"suppressed,omitempty"`
}

// StatusView 是一个 Component 的状态。
type StatusView struct {
	Component string         `json:"component"`
	Pack      string         `json:"pack"`
	Version   string         `json:"version"`
	Profile   string         `json:"profile,omitempty"`
	Instances []InstanceView `json:"instances"`
	// Converged 为 true 表示全部实例都已收敛。
	Converged bool     `json:"converged"`
	Warnings  []string `json:"warnings,omitempty"`
}

// Status 汇总一个 Component 的期望与观测。
func (s *Service) Status(ctx context.Context, siteName, name string) (*StatusView, error) {
	site, comp, _, err := s.load(ctx, siteName, name)
	if err != nil {
		return nil, err
	}

	existing, byID, err := s.existingInstances(ctx, comp.ID)
	if err != nil {
		return nil, err
	}
	if len(existing) == 0 {
		return &StatusView{
			Component: comp.Name, Pack: comp.Pack.Name,
			Version: comp.Pack.Version, Profile: comp.Profile, Converged: true,
		}, nil
	}

	// **按实际下发的版本判收敛**，不是按组件的目标版本：分批期间还没
	// 轮到的实例拿的是旧版规格，拿它去比新版 digest，会把「按计划等着」
	// 显示成「没收敛」——那正是运维在滚动升级中最需要分清的两件事。
	g, err := s.renderGated(ctx, site, comp, inputsForExisting(existing, byID))
	if err != nil {
		return nil, err
	}
	res := g.result()

	reported, err := s.Repos.Status().ListByComponent(ctx, comp.ID)
	if err != nil {
		return nil, err
	}
	byInstance := map[int64]store.InstanceStatus{}
	for _, st := range reported {
		byInstance[st.InstanceID] = st
	}

	drift, err := s.Repos.Status().ListDrift(ctx, comp.ID)
	if err != nil {
		return nil, err
	}
	sup, err := s.Repos.Suppressions().ListActive(ctx, comp.ID, s.now())
	if err != nil {
		return nil, err
	}

	view := &StatusView{
		Component: comp.Name, Pack: comp.Pack.Name,
		Version: comp.Pack.Version, Profile: comp.Profile,
		Converged: true, Warnings: res.Warnings,
	}
	for _, ri := range existing {
		node := byID[ri.NodeID]
		key := ri.Role + "@" + node.Name
		want, atVersion := "", g.rel.versionFor(ri.ID, comp.Pack.Version)
		if sp := g.specFor(ri.ID, key); sp != nil {
			want = sp.Digest
		}
		iv := InstanceView{
			ID:   ri.ID,
			Role: ri.Role, Node: node.Name, Ordinal: ri.Ordinal, Want: want,
			RunState: ri.RunState,
		}
		if atVersion != comp.Pack.Version {
			// 说出来它为什么还是旧版：不说的话，「这台怎么没升」会变成
			// 一次排查，而它只是在按计划排队。
			iv.PendingVersion = atVersion
		}
		if st, ok := byInstance[ri.ID]; ok {
			iv.Got, iv.Result = st.Digest, st.Result
			iv.WorkloadAction, iv.WorkloadActionAt = st.WorkloadAction, st.WorkloadActionAt
			iv.RolledBack = st.RolledBackFrom != ""
			iv.Workload, iv.Health = st.WorkloadState, st.Health
			iv.Restarts = st.Restarts
			iv.Generation = st.Generation
			iv.ReportedAt = store.FormatTime(st.ReportedAt)
			// **收敛 = 跑着目标版本、digest 一致、且健康**
			//
			// 「跑着目标版本」这一条不能省：分批期间还没轮到的实例拿的是
			// 旧版规格，它与自己的期望是对得上的。少了这一条，
			// `component status` 会在第一批还没放行时就说「1.3.0 已收敛」，
			// 而三台机器上跑的全是 1.2.0——那是最坏的一种谎。
			iv.Converged = want != "" && st.Digest == want &&
				st.Health != "unhealthy" && iv.PendingVersion == ""
		}
		for _, d := range drift {
			if d.InstanceID != ri.ID {
				continue
			}
			iv.Drift = append(iv.Drift, DriftView{
				Resource: d.ResourceID,
				// 策略取自**刚渲染出来的规格**，而不是上报回来的东西：
				// 站点覆盖是 mechd 侧算的，节点只是照做。问节点等于
				// 让答案绕一圈回来，而中间任何一次推送延迟都会让它过时。
				Policy: policyOf(res.Specs[key], d.ResourceID),
				Detail: firstDetail(d.Changes),
			})
		}
		for _, sp := range sup {
			if sp.InstanceID == ri.ID {
				iv.Suppressed = append(iv.Suppressed, sp.ResourceID)
			}
		}
		if !iv.Converged {
			view.Converged = false
		}
		view.Instances = append(view.Instances, iv)
	}
	sort.Slice(view.Instances, func(i, j int) bool {
		if view.Instances[i].Role != view.Instances[j].Role {
			return view.Instances[i].Role < view.Instances[j].Role
		}
		return view.Instances[i].Ordinal < view.Instances[j].Ordinal
	})
	return view, nil
}

// ── diff ────────────────────────────────────────────────────────────────

// DiffEntry 是一个实例的期望与现状之差。
type DiffEntry struct {
	Role string `json:"role"`
	Node string `json:"node"`
	// Change 取 create | update | none | drift。
	Change string   `json:"change"`
	Want   string   `json:"want"`
	Got    string   `json:"got,omitempty"`
	Drift  []string `json:"drift,omitempty"`
}

// DiffView 是一次 diff 的结果。
type DiffView struct {
	Component string      `json:"component"`
	Entries   []DiffEntry `json:"entries"`
	// Changed 为 true 表示存在需要下发的变化。
	Changed  bool     `json:"changed"`
	Warnings []string `json:"warnings,omitempty"`
}

// Diff 跑完整的解析管线但**不落库、不下发**。
//
// 这是「先看看会发生什么」的唯一正确实现方式：不是另写一套预演逻辑，
// 而是同一条管线少走最后两步。两套实现迟早会不一致，而不一致的预演
// 比没有预演更糟（13-mechd §3.1）。
func (s *Service) Diff(ctx context.Context, siteName, name string) (*DiffView, error) {
	site, comp, p, err := s.load(ctx, siteName, name)
	if err != nil {
		return nil, err
	}
	existing, byID, err := s.existingInstances(ctx, comp.ID)
	if err != nil {
		return nil, err
	}
	if len(existing) == 0 {
		return &DiffView{Component: comp.Name}, nil
	}

	// **用真实的 Vault，不用一次性密钥。**
	//
	// diff 要拿期望 digest 与上报的比。一次性密钥会让 secretRefs 的 id
	// 与版本每次都不同，于是**任何带 generate 参数的组件都会永远显示有变化**——
	// 一个永远报「有待下发的变更」的 diff 比没有 diff 更糟。
	//
	// 这不引入副作用：密钥已经固化，读它什么都不写。真正有副作用的是
	// **首次生成**，而那只发生在 deploy 上（那条路径确实用一次性值）。
	res, err := s.renderComponent(ctx, site, comp, p, inputsForExisting(existing, byID), false)
	if err != nil {
		return nil, err
	}
	reported, err := s.Repos.Status().ListByComponent(ctx, comp.ID)
	if err != nil {
		return nil, err
	}
	byInstance := map[int64]store.InstanceStatus{}
	for _, st := range reported {
		byInstance[st.InstanceID] = st
	}
	drift, err := s.Repos.Status().ListDrift(ctx, comp.ID)
	if err != nil {
		return nil, err
	}

	out := &DiffView{Component: comp.Name, Warnings: res.Warnings}
	for _, ri := range existing {
		node := byID[ri.NodeID]
		key := ri.Role + "@" + node.Name
		e := DiffEntry{Role: ri.Role, Node: node.Name, Change: "create"}
		if sp := res.Specs[key]; sp != nil {
			e.Want = sp.Digest
		}
		if st, ok := byInstance[ri.ID]; ok {
			e.Got = st.Digest
			switch {
			case st.Digest == e.Want:
				e.Change = "none"
			default:
				e.Change = "update"
			}
		}
		for _, d := range drift {
			if d.InstanceID == ri.ID {
				e.Drift = append(e.Drift, d.ResourceID)
			}
		}
		if len(e.Drift) > 0 && e.Change == "none" {
			// digest 一致却有差异：那是有人动了机器，不是期望状态变了
			e.Change = "drift"
		}
		if e.Change != "none" {
			out.Changed = true
		}
		out.Entries = append(out.Entries, e)
	}
	return out, nil
}

// ── ack-drift ───────────────────────────────────────────────────────────

// AckDriftRequest 是一次漂移确认。
type AckDriftRequest struct {
	Site      string
	Component string
	// Role 与 Node 为空时作用于该 Component 的全部实例。
	Role string
	Node string
	// Resource 为空表示抑制整个实例，用于整机维护窗口。
	Resource string
	Duration time.Duration
	Reason   string
	Actor    string
}

// AckDrift 给「临时修改」一个名分（06-state-and-drift §4.1）。
//
// 运维凌晨救火改了一个值，此前只能要么被永远报成异常、要么走一次正式变更。
// 抑制**有期限**（到点自动恢复告警，不会悄悄变永久）、**有理由**（进审计）、
// **仍然检测**（只是不告警，status 里照常显示「已抑制」）。
func (s *Service) AckDrift(ctx context.Context, req AckDriftRequest) (int, error) {
	if strings.TrimSpace(req.Reason) == "" {
		// 没有理由的抑制半年后没人知道为什么——那时它和「忘了处理」
		// 无法区分，而这正是抑制机制最容易退化成的样子
		return 0, faults.Permanentf("", "ack-drift requires --reason")
	}
	if req.Duration <= 0 {
		return 0, faults.Permanentf("", "ack-drift requires a positive --duration (e.g. 4h)")
	}

	site, err := s.resolveSite(ctx, req.Site)
	if err != nil {
		return 0, err
	}
	comp, err := s.componentForWrite(ctx, site.ID, req.Component)
	if err != nil {
		return 0, err
	}
	existing, byID, err := s.existingInstances(ctx, comp.ID)
	if err != nil {
		return 0, err
	}

	now := s.now()
	n := 0
	for _, ri := range existing {
		if req.Role != "" && ri.Role != req.Role {
			continue
		}
		if req.Node != "" && byID[ri.NodeID].Name != req.Node {
			continue
		}
		if _, err := s.Repos.Suppressions().Create(ctx, store.Suppression{
			InstanceID: ri.ID, ResourceID: req.Resource,
			Reason: req.Reason, CreatedBy: req.Actor,
			CreatedAt: now, ExpiresAt: now.Add(req.Duration),
		}); err != nil {
			return n, err
		}
		n++
	}
	if n == 0 {
		return 0, faults.Permanentf("", "no matching instances (component=%s role=%q node=%q)",
			req.Component, req.Role, req.Node)
	}

	s.audit(ctx, req.Actor, "ack-drift", req.Component, nil, "ok")
	s.event(ctx, site.ID, "drift.acknowledged", req.Component, map[string]any{
		"resource": req.Resource, "reason": req.Reason,
		"until": store.FormatTime(now.Add(req.Duration)), "instances": n,
	})
	// 立刻推一次：抑制随规格下发，不推的话要等下一个推送周期才生效——
	// 而运维敲 ack-drift 的时候正想让告警**马上**停下来
	s.notifyTouched(existing, byID, req.Role, req.Node)
	return n, nil
}

// ── 列表 ────────────────────────────────────────────────────────────────

// ComponentView 是列表里的一个 Component。
type ComponentView struct {
	Name      string `json:"name"`
	Pack      string `json:"pack"`
	Version   string `json:"version"`
	Profile   string `json:"profile,omitempty"`
	Instances int    `json:"instances"`
	// Removing 表示它正在被移除，还在等实例报告拆干净。
	//
	// **必须在列表里就看得见。** 一个正在被删的组件不接受任何写操作，
	// 而列表上若与正常组件长得一样，用户点进去改配置只会撞上一句
	// 「不接受其它写操作」——那时他才知道发生了什么。
	Removing bool `json:"removing,omitempty"`
}

// ListComponents 列出一个 Site 内的 Component。
func (s *Service) ListComponents(ctx context.Context, siteName string) ([]ComponentView, error) {
	site, err := s.resolveSite(ctx, siteName)
	if err != nil {
		return nil, err
	}
	comps, err := s.Repos.Components().List(ctx, site.ID)
	if err != nil {
		return nil, err
	}
	out := make([]ComponentView, 0, len(comps))
	for _, c := range comps {
		insts, err := s.Repos.Instances().List(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, ComponentView{
			Name: c.Name, Pack: c.Pack.Name, Version: c.Pack.Version,
			Profile: c.Profile, Instances: len(insts),
			Removing: c.Removing(),
		})
	}
	return out, nil
}

// NodeView 是列表里的一个节点。
type NodeView struct {
	Name    string            `json:"name"`
	Address string            `json:"address"`
	Status  string            `json:"status"`
	Labels  map[string]string `json:"labels,omitempty"`
	// Cordoned / Revoked 是两件不同的事，因此分成两个字段而不是塞进
	// Status：一台机器可以既在线又被暂停调和，把它们并成一列会丢信息。
	Cordoned bool `json:"cordoned,omitempty"`
	Revoked  bool `json:"revoked,omitempty"`
	// Orphans 是机器上还在、但下发里没有的实例。
	//
	// 它必须在**节点**这一层呈现：mechd 里可能已经没有那个 Component 了，
	// `component status` 根本查不到它。
	Orphans []OrphanView `json:"orphans,omitempty"`
}

// OrphanView 是一条孤儿实例。
type OrphanView struct {
	Instance string `json:"instance"`
	// FirstSeen 回答「什么时候被落下的」——多半对得上某次变更。
	FirstSeen string `json:"firstSeen"`
}

// ListNodes 列出一个 Site 内的节点。
// AddNode 把一台机器登记进册子。
//
// **登记与加入是两件事。** 这里只在库里留一行「这台机器属于本 Site」；
// 那台机器真正连上来还需要一张证书（M7 第 3 步的 join token，或
// `mechd ca issue` 的离线路径）。分开是因为它们的授权者不同：登记是
// 控制面的管理动作，加入需要那台机器上有人。
//
// `Register` 拒绝不在册的节点（ADR-0034 的名字占用规则），而在 M7 之前
// **全仓库没有任何地方能建出这一行**——错误信息让用户去敲一条不存在的
// 命令。这个方法补的正是那个洞。
func (s *Service) AddNode(
	ctx context.Context, siteName, name, address, actor string,
) (NodeView, error) {
	if name == "" {
		return NodeView{}, faults.Permanentf("registering node", "node name must not be empty")
	}
	site, err := s.resolveSite(ctx, siteName)
	if err != nil {
		return NodeView{}, err
	}
	if _, err := s.Repos.Nodes().GetByName(ctx, site.ID, name); err == nil {
		return NodeView{}, faults.Permanentf("registering node",
			"node %s already exists in Site %s", name, site.Name)
	} else if !errors.Is(err, store.ErrNotFound) {
		return NodeView{}, err
	}

	// 状态是 **reserved**：登记完还没有人对它执行过 join，没有签发过
	// 任何证书（22-multi-node §6.16）。它不是 NodePending——
	// pending 现在专指「已经拿到证书、还没连上来过」，两者的区别正是
	// Join 认领这一行时唯一要看的判据。
	n, err := s.Repos.Nodes().Upsert(ctx, store.Node{
		SiteID: site.ID, Name: name, Address: address, Status: store.NodeReserved,
	})
	if err != nil {
		return NodeView{}, err
	}
	s.audit(ctx, actor, "node-add", name, nil, site.Name)
	s.event(ctx, site.ID, "node.added", name, map[string]any{"address": address})
	return NodeView{
		Name: n.Name, Address: n.Address,
		Status: nodeStatus(n.Seen(), s.connected(n.Name)),
	}, nil
}

// RemoveNode 把一个节点从册子上抹掉。
//
// **它不去那台机器上卸载任何东西**——mechd 不执行部署动作（ADR-0002），
// 而且那台机器很可能已经联系不上了（换硬件、退役、被回收）。因此这里
// 只改册子，机器上留下的东西会在它下次上报时变成孤儿并被明确列出
// （20-continuous-reconcile §2.4）。
//
// 仍有实例时**拒绝**：删掉一个还跑着组件的节点，会让那些组件从中心的
// 视图里消失，而它们仍在机器上跑——那是最难发现的一类不一致。
func (s *Service) RemoveNode(
	ctx context.Context, siteName, name string, force bool, actor string,
) error {
	site, err := s.resolveSite(ctx, siteName)
	if err != nil {
		return err
	}
	node, err := s.Repos.Nodes().GetByName(ctx, site.ID, name)
	if err != nil {
		return err
	}
	instances, err := s.Repos.Nodes().InstancesOn(ctx, node.ID)
	if err != nil {
		return err
	}
	if len(instances) > 0 && !force {
		return faults.Permanentf("removing node",
			"node %s still has %d instances.\n"+
				"  uninstall them first with mechctl component remove,\n"+
				"  or add --force -- that only erases them from the register, **nothing on the machine gets uninstalled**,\n"+
				"  they will become orphans the next time it reports in",
			name, len(instances))
	}
	for _, in := range instances {
		if err := s.Repos.Instances().Delete(ctx, in.ID); err != nil {
			return err
		}
	}
	if err := s.Repos.Nodes().Delete(ctx, node.ID); err != nil {
		return err
	}
	s.audit(ctx, actor, "node-remove", name, nil, site.Name)
	s.event(ctx, site.ID, "node.removed", name, map[string]any{
		"instances": len(instances), "force": force,
	})
	return nil
}

func (s *Service) ListNodes(ctx context.Context, siteName string) ([]NodeView, error) {
	site, err := s.resolveSite(ctx, siteName)
	if err != nil {
		return nil, err
	}
	nodes, err := s.Repos.Nodes().List(ctx, site.ID)
	if err != nil {
		return nil, err
	}
	out := make([]NodeView, 0, len(nodes))
	for _, n := range nodes {
		v := NodeView{
			Name: n.Name, Address: n.Address, Labels: n.Labels,
			Status:   nodeStatus(n.Seen(), s.connected(n.Name)),
			Cordoned: n.Cordoned(), Revoked: n.Revoked(),
		}
		orphans, err := s.Repos.Status().ListOrphans(ctx, n.ID)
		if err != nil {
			return nil, err
		}
		for _, o := range orphans {
			v.Orphans = append(v.Orphans, OrphanView{
				Instance: o.InstanceKey, FirstSeen: store.FormatTime(o.FirstSeen),
			})
		}
		out = append(out, v)
	}
	return out, nil
}

// ── 期望运行态 ──────────────────────────────────────────────────────────

// SetRunStateRequest 是 `component stop` / `start` 的入参。
type SetRunStateRequest struct {
	Site      string
	Component string
	// Role 与 Node 为空时作用于该 Component 的全部实例。
	//
	// 逐实例可选是必要的：维护窗口经常只针对一台机器（滚动重启、单节点
	// 排障），而 Component 级的粒度表达不了那件事。
	Role string
	Node string
	// State 是 running 或 stopped。
	State string
	Actor string
}

// SetRunState 设置期望运行态（20-continuous-reconcile §2.2）。
//
// 这不是「执行一次 stop」，而是**改变期望**——调和器随后会去维持它，
// 包括把被人手工启动的实例停回去。区别在断连时最明显：一次性的 stop
// 会在下一轮调和被撤销，而改期望不会。
//
// **`removed` 不在这里。** 它是 RunState 的第三个值，但走 `component
// remove` 那条路——那条路有引用检查、影响面打印与二档确认（10-cli §4.3），
// 而这个动词一个都没有。放行它，`component stop --state removed` 就成了
// 一条绕开全部安全网的卸载命令。
func (s *Service) SetRunState(ctx context.Context, req SetRunStateRequest) (int, error) {
	switch req.State {
	case spec.RunStateRunning, spec.RunStateStopped:
	case spec.RunStateRemoved:
		return 0, faults.Permanentf("",
			"uninstalling does not go through this verb: it has no dependency check, impact analysis, or second confirmation\n"+
				"  use mechctl component remove %s", req.Component)
	default:
		return 0, faults.Permanentf("", "run state can only be %s or %s, got %q",
			spec.RunStateRunning, spec.RunStateStopped, req.State)
	}

	site, err := s.resolveSite(ctx, req.Site)
	if err != nil {
		return 0, err
	}
	comp, err := s.componentForWrite(ctx, site.ID, req.Component)
	if err != nil {
		return 0, err
	}
	existing, byID, err := s.existingInstances(ctx, comp.ID)
	if err != nil {
		return 0, err
	}

	n := 0
	for _, ri := range existing {
		if req.Role != "" && ri.Role != req.Role {
			continue
		}
		if req.Node != "" && byID[ri.NodeID].Name != req.Node {
			continue
		}
		if err := s.Repos.Instances().SetRunState(ctx, ri.ID, req.State); err != nil {
			return n, err
		}
		n++
	}
	if n == 0 {
		// **0 个匹配要报错而不是静默成功**：一句「已停止」而实际什么都没停，
		// 会让人以为维护窗口已经安全了
		return 0, faults.Permanentf("", "no matching instances (component=%s role=%q node=%q)",
			req.Component, req.Role, req.Node)
	}

	s.audit(ctx, req.Actor, "set-run-state", req.Component, nil, req.State)
	s.event(ctx, site.ID, "component."+req.State, req.Component, map[string]any{
		"role": req.Role, "node": req.Node, "instances": n,
	})
	// 立刻推一次：不推的话要等下一个调和周期，而运维刚敲完 stop 就去看
	// 服务还在跑，会以为命令没生效
	s.notifyTouched(existing, byID, req.Role, req.Node)
	return n, nil
}

// ── 漂移策略覆盖 ────────────────────────────────────────────────────────

// SetDriftPolicyRequest 是 `component set-drift-policy` 的入参。
type SetDriftPolicyRequest struct {
	Site      string
	Component string
	// Policy 是 report | ignore | 空（清除覆盖）。
	//
	// **不接受 reconcile**：那是最严的一档，作为覆盖只能是收紧。
	Policy string
	Actor  string
}

// SetDriftPolicy 设置站点侧对 Pack 声明的漂移策略覆盖。
//
// `driftPolicy` 写在 Pack 里，等于 **Pack 作者决定了运维现场的临时修改能
// 不能活下来**——这个权责关系本来就是反的，因此站点可以放松它
// （06-state-and-drift §4.2）。
func (s *Service) SetDriftPolicy(ctx context.Context, req SetDriftPolicyRequest) error {
	if err := spec.CheckDriftOverride(req.Policy); err != nil {
		return err
	}

	site, err := s.resolveSite(ctx, req.Site)
	if err != nil {
		return err
	}
	comp, err := s.componentForWrite(ctx, site.ID, req.Component)
	if err != nil {
		return err
	}

	comp.DriftPolicy = req.Policy
	if _, err := s.Repos.Components().Update(ctx, comp); err != nil {
		return err
	}

	s.audit(ctx, req.Actor, "set-drift-policy", req.Component, nil, req.Policy)
	s.event(ctx, site.ID, "component.drift-policy", req.Component, map[string]any{
		"policy": req.Policy,
	})
	// 必须重推。digest **不会**因此改变（driftPolicy 不参与摘要，见
	// ComputeDigest），因此 mechlet 不会切 generation、不会重启服务——
	// 但它本机保存的那份期望状态里还是旧策略，不推就要等到下一次推送。
	// 而运维刚放松完策略就去改文件，会以为命令没生效。
	insts, byID, err := s.existingInstances(ctx, comp.ID)
	if err != nil {
		return err
	}
	s.notifyTouched(insts, byID, "", "")
	return nil
}

// DriftView 是 status 里的一条漂移。
//
// 只给资源 id 是不够的：**用户看到「漂移: template:app.yaml」时，第一个
// 问题是「它会不会被改回去」**——而那取决于 driftPolicy，包括站点侧的覆盖。
// 不告诉他就等于让他去翻 Pack 源码，再翻一遍站点配置。
type DriftView struct {
	Resource string `json:"resource"`
	// Policy 是**生效的**策略（已合并站点覆盖）：report | reconcile。
	//
	// ignore 不会出现在这里——那种资源根本不比对。
	Policy string `json:"policy,omitempty"`
	// Detail 是调和器给出的原因。
	//
	// 最要紧的一种是「策略是 reconcile，但改回会连带重启，未显式允许」——
	// 少了这句话，现场看到的是「策略说自动改回，可它就是不改」。
	Detail string `json:"detail,omitempty"`
}

// policyOf 从已渲染的规格里取某条资源的生效策略。
func policyOf(sp *spec.ResolvedSpec, resourceID string) string {
	if sp == nil {
		return ""
	}
	for _, r := range sp.Resources {
		if r.ID == resourceID {
			if r.DriftPolicy == "" {
				return spec.DriftReport // 规范的默认
			}
			return r.DriftPolicy
		}
	}
	return ""
}

func firstDetail(changes []any) string {
	for _, c := range changes {
		if s, ok := c.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// ── 升级与回滚 ──────────────────────────────────────────────────────────

// UpgradeRequest 是 `component upgrade` 的入参。
type UpgradeRequest struct {
	Site      string
	Component string
	// Version 是目标版本；空表示「升到本地可用的最新一版」。
	Version string
	// Force 跳过 upgradePolicy.compatible 检查。
	//
	// **提供它不是为了让人绕过检查**，而是因为 Pack 作者可能把范围写窄了，
	// 而现场的人知道这次升级是安全的。它进审计。
	Force  bool
	DryRun bool
	Actor  string
}

// Upgrade 把一个 Component 换到另一个 Pack 版本。
//
// 它不是一条独立的部署路径：改完版本之后走的还是同一条解析管线，
// 产出新 digest，由节点按 generation 规则物化与切换。**升级与首装的差别
// 只在节点侧**（新 generation vs 首个 generation），mechd 这里没有分叉。
func (s *Service) Upgrade(ctx context.Context, req UpgradeRequest) (*DeployResult, error) {
	site, comp, _, err := s.load(ctx, req.Site, req.Component)
	if err != nil {
		return nil, err
	}
	cur := comp.Pack

	// 升级是**唯一需要新版本 Pack 的流程**：不重扫的话，用户把新版本放进
	// pack-dir 之后还得重启 mechd 才看得见。
	if err := s.Packs.Reload(); err != nil {
		s.log().Warn("failed to rescan Pack collection, keeping the previous index", "err", err)
	}

	constraint := ""
	if req.Version != "" {
		constraint = "=" + req.Version
	}
	entry, err := s.Packs.Resolve(comp.Pack.Name, constraint)
	if err != nil {
		return nil, faults.Permanentf("", "parsing Pack %s: %w", comp.Pack.Name, err)
	}
	target := entry.Pack

	if target.Version == cur.Version && target.Revision == cur.Revision {
		return nil, faults.Permanentf("", "%s is already %s revision %d, no upgrade needed",
			comp.Name, cur.Version, cur.Revision)
	}
	if err := checkUpgradeCompatible(comp.Name, cur, target, req.Force); err != nil {
		return nil, err
	}

	// 记住上一版——`rollback` 不带参数时回到它
	comp.PreviousVersion, comp.PreviousRevision = comp.Pack.Version, comp.Pack.Revision
	comp.Pack.Version, comp.Pack.Revision = target.Version, target.Revision

	// **先开 Rollout 再下发**，顺序要紧。
	//
	// 下发会唤醒受影响的节点，而它们醒来后立刻来拉规格。此时若还没有
	// 批次记录，每台都会拿到新版——三台机器在几十秒内一起换掉，而
	// `rollout status` 随后才开始说「第 1/3 批」。分批就成了一个纯粹的
	// 观测构造，比没有分批更糟：它给了一个假的安全感。
	//
	// 干跑不留痕迹：一次「先看看会发生什么」不该在历史里多出一条记录。
	if !req.DryRun {
		s.startRollout(ctx, comp, "upgrade", comp.PreviousVersion, target.Version)
	}
	res, err := s.applyComponentVersion(ctx, site, comp, target, req.DryRun)
	if err != nil {
		if !req.DryRun {
			s.abortStartedRollout(ctx, comp, err)
		}
		return nil, err
	}
	s.audit(ctx, req.Actor, "upgrade", comp.Name, target,
		fmt.Sprintf("%s → %s", req.previousLabel(comp), target.Version))
	s.event(ctx, site.ID, "component.upgraded", comp.Name, map[string]any{
		"from": comp.PreviousVersion, "to": target.Version,
		"revision": target.Revision, "force": req.Force, "dryRun": req.DryRun,
	})
	return res, nil
}

func (r UpgradeRequest) previousLabel(c store.Component) string {
	if c.PreviousVersion == "" {
		return "?"
	}
	return c.PreviousVersion
}

// checkUpgradeCompatible 实现 spec §4.2。
//
// 判据是**目标版本的 Pack 声明的 compatible 是否包含当前已安装的版本**，
// 不是反过来。方向要紧：新版本才知道自己能从哪些旧版本接管数据。
//
// PostgreSQL 16 → 17 是这条规则存在的理由：Mecharion 的升级模型是
// 「物化新 generation → 原子切换 → 数据目录不动」，而 PG 大版本升级需要
// pg_upgrade 与新的数据目录——直接换二进制会让 PG 17 去启动 PG 16 的数据。
func checkUpgradeCompatible(name string, cur store.PackRef, target *pack.Pack, force bool) error {
	if target.UpgradePolicy == nil || target.UpgradePolicy.Compatible == "" {
		return nil // 没声明就是不限制
	}
	c, err := pack.ParseConstraint(target.UpgradePolicy.Compatible)
	if err != nil {
		return faults.Permanentf("", "%s %s's upgradePolicy.compatible is invalid: %w",
			target.Name, target.Version, err)
	}
	if c.Matches(pack.ParseVersion(cur.Version)) {
		return nil
	}
	if force {
		return nil
	}
	return faults.Permanentf("",
		"cannot upgrade %s from %s to %s\n"+
			"  %s %s declares upgradePolicy.compatible = %q, which does not include %s\n"+
			"  crossing this version boundary usually requires a data migration; create a new Component and migrate the data;\n"+
			"  if you've confirmed it's safe, use --force to skip this check (this gets audited)",
		name, cur.Version, target.Version,
		target.Name, target.Version, target.UpgradePolicy.Compatible, cur.Version)
}

// RollbackRequest 是 `component rollback` 的入参。
type RollbackRequest struct {
	Site      string
	Component string
	// Version 是要回到的版本；空表示回到上一版。
	Version string
	DryRun  bool
	Actor   string
}

// Rollback 把一个 Component 换回某个旧版本。
//
// **它与 upgrade 是同一条路径**，只是方向相反：改版本 → 重新解析 → 下发。
// 秒级完成的原因不在这里，而在节点：解析是纯函数，回到旧版本会产出
// **与当年一模一样的 digest**，节点因此命中已保留的 generation，
// 只需切一次软链，不重新解压（04-paths-and-storage §2）。
//
// 也正因如此，回滚**不跳过** upgradePolicy 检查：从 17 退回 16 与从 16
// 升到 17 面对的是同一个数据目录问题，方向不改变它。
func (s *Service) Rollback(ctx context.Context, req RollbackRequest) (*DeployResult, error) {
	site, comp, _, err := s.load(ctx, req.Site, req.Component)
	if err != nil {
		return nil, err
	}
	cur := comp.Pack

	version := req.Version
	if version == "" {
		if comp.PreviousVersion == "" {
			return nil, faults.Permanentf("",
				"%s has no recorded previous version, cannot roll back automatically\n"+
					"  use --to-version to specify a target; see available versions with mechctl pack list %s",
				comp.Name, comp.Pack.Name)
		}
		version = comp.PreviousVersion
	}
	if version == cur.Version {
		return nil, faults.Permanentf("", "%s is already %s", comp.Name, version)
	}

	if err := s.Packs.Reload(); err != nil {
		s.log().Warn("failed to rescan Pack collection, keeping the previous index", "err", err)
	}
	entry, err := s.Packs.Resolve(comp.Pack.Name, "="+version)
	if err != nil {
		return nil, faults.Permanentf("", "parsing Pack %s %s: %w", comp.Pack.Name, version, err)
	}
	target := entry.Pack
	if err := checkUpgradeCompatible(comp.Name, cur, target, false); err != nil {
		return nil, err
	}

	comp.PreviousVersion, comp.PreviousRevision = comp.Pack.Version, comp.Pack.Revision
	comp.Pack.Version, comp.Pack.Revision = target.Version, target.Revision

	// 与 upgrade 同一条顺序：先开 Rollout 再下发（见那边的说明）。
	// 回退同样分批——一次「全部一起退回去」与它要修的那次事故没有区别。
	if !req.DryRun {
		s.startRollout(ctx, comp, "rollback", comp.PreviousVersion, target.Version)
	}
	res, err := s.applyComponentVersion(ctx, site, comp, target, req.DryRun)
	if err != nil {
		if !req.DryRun {
			s.abortStartedRollout(ctx, comp, err)
		}
		return nil, err
	}
	s.audit(ctx, req.Actor, "rollback", comp.Name, target,
		fmt.Sprintf("%s → %s", comp.PreviousVersion, target.Version))
	s.event(ctx, site.ID, "component.rolledback", comp.Name, map[string]any{
		"from": comp.PreviousVersion, "to": target.Version, "dryRun": req.DryRun,
	})
	return res, nil
}

// applyComponentVersion 落库新版本并重新解析下发。
//
// 干跑时**只解析不落库**：一次「先看看会发生什么」不该改变任何东西，
// 这与 deploy --dry-run 是同一条纪律（13-mechd §3.1）。
func (s *Service) applyComponentVersion(
	ctx context.Context, site store.Site, comp store.Component,
	target *pack.Pack, dryRun bool,
) (*DeployResult, error) {
	existing, byID, err := s.existingInstances(ctx, comp.ID)
	if err != nil {
		return nil, err
	}
	if len(existing) == 0 {
		return nil, faults.Permanentf("", "%s has not been deployed to any node yet", comp.Name)
	}

	if !dryRun {
		if _, err := s.Repos.Components().Update(ctx, comp); err != nil {
			return nil, err
		}
	}

	res, err := s.renderComponent(ctx, site, comp, target,
		inputsForExisting(existing, byID), dryRun)
	if err != nil {
		return nil, err
	}

	out := &DeployResult{
		Component: comp.Name, DryRun: dryRun,
		Digests: map[string]string{}, Warnings: res.Warnings,
	}
	for _, key := range res.Order {
		out.Digests[key] = res.Specs[key].Digest
	}
	out.Specs = res.Specs
	if !dryRun {
		s.notifyTouched(existing, byID, "", "")
	}
	return out, nil
}
