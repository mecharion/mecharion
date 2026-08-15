// Package reconcile 是调和器：把一份已解析规格变成机器上的实际状态。
//
// 它编排七个阶段（docs/design/11-resource-engine.md §3），并独占三件
// 资源引擎与 Runtime 都不管的事：**generation 的分配与切换、notify 的
// 聚合、本地状态的读写**。
//
//	① 解析并创建 paths 声明的目录
//	② 资源（shared 在前、role 在后）Read → Diff → Apply
//	②′ linkInto：载荷自带目录改名 + 建软链   ← 必须在载荷解开之后
//	③ Runtime.Materialize
//	④ generation 切换（若需要）：Stop → 切 current → Start
//	⑤ notify 聚合执行
//	⑥ 健康检查
//	⑦ 上报观测状态
//
// M2 的入口是 `mechlet apply -f <已解析规格>`；M3 换成 mechd 的 gRPC
// 下发，**这个包一行不改**——两者读的是同一个结构。
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/mecharion/mecharion/internal/command"
	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/health"
	"github.com/mecharion/mecharion/internal/hook"
	"github.com/mecharion/mecharion/internal/resource"
	"github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/state"
)

// Reconciler 执行调和。
type Reconciler struct {
	// Store 是 mechlet 的本地状态。
	Store *state.Store
	// Runtimes 是已注册的 Runtime。
	Runtimes *runtime.Registry
	// BlobDir 是内容寻址的载荷根目录。
	BlobDir string
	// PackDir 是解开的 Pack 逻辑根目录；每个 Pack 一个子目录。
	PackDir string
	// Runner 执行外部命令。
	Runner command.Runner
	// HookRunDir 是 hook 敏感参数临时文件的父目录，为空时用
	// hook.DefaultRunDir（/run/mecharion/hooks，tmpfs）。
	HookRunDir string
	// Log 记录调和过程。
	Log *slog.Logger

	// Now 可替换，供测试固定时间。
	Now func() time.Time
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Reconciler) runner() command.Runner {
	if r.Runner == nil {
		return command.Exec{}
	}
	return r.Runner
}

// Reconcile 把一份已解析规格调和到机器上。
//
// **返回 Report 与 error 两者**：失败时 Report 仍然带着已经发生的部分，
// 那是排障最需要的东西——只返回一个 error 等于把「走到哪一步失败的」丢掉了。
func (r *Reconciler) Reconcile(ctx context.Context, s *spec.ResolvedSpec) (*Report, error) {
	started := r.now()
	rep := &Report{
		Component: s.Component, Role: s.Role, ConfigGroup: s.ConfigGroup,
		Node: s.Node.Name, Digest: s.Digest, StartedAt: started,
		Result: ResultFailed,
	}
	finish := func(err error) (*Report, error) {
		rep.Duration = r.now().Sub(started)
		if err != nil {
			rep.Error = err.Error()
			rep.Result = ResultFailed
		}
		return rep, err
	}

	if err := spec.Validate(s); err != nil {
		return finish(faults.Wrap(faults.Permanent, "校验规格", err))
	}
	opts := s.Reconcile.WithDefaults()

	// ── 本地状态：路径固化校验 ──────────────────────────────────────
	key := s.InstanceKey()
	in, err := r.Store.LoadInstance(key)
	if err != nil {
		return finish(faults.Wrap(faults.Transient, "读取本地状态", err))
	}
	if in == nil {
		in = &state.Instance{
			Component: s.Component, Role: s.Role, ConfigGroup: s.ConfigGroup,
		}
	}
	// ── 卸载：runState: removed ─────────────────────────────────────
	//
	// **必须在 CheckPaths 与 planGeneration 之前分叉。** 往下走一步就是
	// 「分配 generation、物化、切软链」——为一个马上要被删掉的实例装一遍
	// 新版本，是最荒唐的一种浪费，而中途失败会把「删除」变成「装了一半」。
	//
	// 也绕开 CheckPaths：那条固化校验是为「别让已装的组件静默搬家」设的，
	// 而卸载要拆的正是盘上那一份。让它挡在这里，一个路径漂移过的实例
	// 就再也删不掉了。
	if s.WantsRemoved() {
		if err := r.uninstall(ctx, s, in, rep); err != nil {
			return finish(err)
		}
		rep.Duration = r.now().Sub(started)
		return rep, nil
	}

	if err := in.CheckPaths(pathSnapshot(s)); err != nil {
		// 路径变了就拒绝调和，不自动迁移。若不固化，用户改了 Node.Roots
		// 或 Pack 改了默认路径，已装组件会静默搬家，旧数据变成孤儿。
		return finish(faults.Wrap(faults.Permanent, "校验路径", err))
	}

	home := homeOf(s)
	pl, err := planGeneration(in, s, home)
	if err != nil {
		return finish(err)
	}
	rep.Generation, rep.GenerationDir = pl.Seq, pl.Dir
	rep.Rollback = pl.Rollback

	// 这个版本上次失败过：**停下来，不按这份规格做任何事**。
	//
	// 不能只是「不切软链」而照常应用资源——那会把新版本的配置写到
	// 旧版本的二进制旁边，得到一个从没被测试过的组合。
	//
	// 被锁住的只有「切到这一版」这个动作。机器不会因此失去维护：agent
	// 拿到 Blocked 之后会按**最后一次成功的规格**再调和一轮，旧版上的
	// 漂移照常纠正（internal/agent 的 fallback）。那一轮是「维持」而不是
	// 「回滚」——机器一步没动过。
	if pl.Blocked {
		rep.Blocked = true
		rep.Result = ResultFailed
		rep.Duration = r.now().Sub(started)
		err := faults.Permanentf("升级",
			"版本 %s（generation %04d）上次部署失败并已停在此处，不再自动重试\n"+
				"  用 mechctl component rollback 回到旧版，\n"+
				"  或改一个参数 / 换一个版本重新尝试",
			s.Pack.Version, pl.Seq)
		rep.Error = err.Error()
		// **Debug 而不是 Warn**：这是一个稳定状态，会一直保持到有人来处理。
		// 每个调和周期喊一次，只会让真正发生变化的那一刻淹掉。进入这个
		// 状态时的那一声由 agent 喊（它才知道上一轮是什么样），而
		// 「现在停在这里」这件事由报告与 `component status` 长期可见。
		r.log().Debug("this version failed last time, not retrying this round",
			"component", s.Component, "role", s.Role,
			"generation", pl.Seq, "digest", s.Digest)
		return rep, err
	}

	// 占位符替换：generation 序号是 mechlet 本地分配的，mechd 无从得知，
	// 因此这是**唯一**留到此刻才能填的值。字面量替换，不是第二次渲染。
	resolved, err := spec.ResolveGeneration(s, pl.Dir)
	if err != nil {
		return finish(faults.Wrap(faults.Permanent, "替换 generation 占位符", err))
	}
	if spec.HasUnresolvedPlaceholder(resolved) {
		return finish(faults.Permanentf("替换 generation 占位符",
			"替换后仍有残留——组件没有 home 路径？"))
	}

	env := resource.EnvFor(resolved, r.packRoot(s), r.BlobDir)
	env.Runner = r.runner()

	// 摘要缓存让「每轮全量哈希」降为「每轮一次 stat」——一个装了十个组件、
	// 各带 50MB 二进制的节点，否则每分钟要读并哈希 500MB，而绝大多数轮次
	// 什么都没变。
	digests := newDigestCache(in)
	env.Digests = digests

	// hook 的执行环境每轮都可能不同（升级会换 generation），因此每轮新建。
	hooks := r.hookRunner(env, s, pl.Dir)

	// 首装与升级走不同的 hook 点。**调和器知道自己在做哪一种**
	// （generation 是否是新的、之前有没有装过），因此这个判断不需要额外信息。
	pre, post := hook.PreInstall, hook.PostInstall
	if pl.New && len(in.Generations) > 0 {
		pre, post = hook.PreUpgrade, hook.PostUpgrade
	}

	// generation 目录要在 **preInstall 之前**建出来。
	//
	// hook 的执行环境约定 cwd 就是 generation 目录（18-hooks §3），而
	// preInstall 是第一个 hook 点——目录还没建，cwd 就指向一个不存在的
	// 路径。那时 Go 的 fork/exec 报的是
	//
	//	fork/exec <脚本路径>: no such file or directory
	//
	// **它指向脚本，而脚本明明在那儿**——排查会一直盯着 hook 文件看。
	// 建一个空目录不算「物化」：台账要到最后才写，这一步失败留下的空目录
	// 会被下一轮以同一个序号复用。
	if pl.Dir != "" {
		if err := makeGenerationDir(pl.Dir); err != nil {
			return finish(err)
		}
	}

	// ── 阶段⓪：preInstall / preUpgrade ──────────────────────────────
	//
	// 失败即中止，**不物化、不切换**：升级场景因此保持在旧 generation 上，
	// 服务不受影响（18-hooks §4）。
	if err := r.runHooks(ctx, hooks, resolved, pre, rep); err != nil {
		return finish(err)
	}

	// ── 阶段①：创建 paths 声明的目录 ────────────────────────────────
	if err := createPaths(ctx, env, resolved); err != nil {
		return finish(err)
	}

	// ── 阶段②：资源 ────────────────────────────────────────────────
	//
	// driftPolicy 只在**期望状态没变**时才有话语权。复用当前 generation
	// 意味着 digest 没变，此时的差异是有人动了机器——那才叫漂移。
	// 新 generation 或回滚时的差异来自新的期望状态，必须无条件落地，
	// 否则一个 driftPolicy: report 的模板会让配置变更永远发不出去。
	notify := newNotifySet()
	if err := r.applyResources(ctx, env, resolved, rep, notify, driftContext{
		governed:     pl.Reused(),
		instance:     in,
		spec:         resolved,
		now:          r.now(),
		allowRestart: opts.AllowDriftRestart,
	}); err != nil {
		rep.Error = err.Error()
		r.recordFailure(in, pl, s, rep)
		return finish(err)
	}

	// ── 阶段②′：linkInto ───────────────────────────────────────────
	if err := linkPaths(resolved, pl.Dir); err != nil {
		rep.Error = err.Error()
		r.recordFailure(in, pl, s, rep)
		return finish(err)
	}

	// ── 阶段②″：postInstall / postUpgrade ──────────────────────────
	//
	// 排在资源之后、启动之前（18-hooks §2）。需要服务已经在跑的动作
	// （建角色、建库）应当声明在 postStart，不是这里。
	if err := r.runHooks(ctx, hooks, resolved, post, rep); err != nil {
		rep.Error = err.Error()
		r.recordFailure(in, pl, s, rep)
		return finish(err)
	}

	// ── 阶段③④：Runtime ───────────────────────────────────────────
	if err := r.applyWorkload(ctx, resolved, pl, home, rep, notify, hooks); err != nil {
		rep.Error = err.Error()
		r.recordFailure(in, pl, s, rep)
		return finish(err)
	}

	// ── 阶段⑥：健康检查 ────────────────────────────────────────────
	//
	// **必须在标记 active 之前。** 早先它排在写台账之后，于是一个健康检查
	// 没过的 generation 已经被记成当前版本——下一轮读到「当前 digest ==
	// 期望 digest」，判定为复用，不再切换也不再重试。机器停在一个坏掉的
	// 版本上，而台账说一切正常。
	if err := r.checkHealth(ctx, resolved, rep); err != nil {
		rep.Error = err.Error()
		r.recordFailure(in, pl, s, rep)
		return finish(err)
	}

	// ── 台账：健康之后才记，失败的 generation 不该被当成可回滚的落脚点 ──
	in.Paths = pathSnapshot(s)
	in.ConfigGroup = s.ConfigGroup
	in.AppliedResources = appliedRefs(resolved)
	if pl.New {
		in.AddGeneration(state.Generation{
			Seq: pl.Seq, Digest: s.Digest,
			Version: s.Pack.Version, Revision: s.Pack.Revision,
			Dir: pl.Dir, MaterializedAt: r.now(), State: state.GenActive,
		})
	}
	// 引用每轮都刷：一台从旧版本 mechlet 升上来的机器，台账里的老记录没有
	// 这两个字段，只在 pl.New 时写会让它们永远补不上——而「引用集不全」
	// 会让回收删掉还在用的东西。
	setGenerationRefs(in, pl.Seq, blobSums(s), rep.Images)
	in.SetActive(pl.Seq)

	var junk garbage
	rep.Pruned, junk = pruneGenerations(in, opts.RetainGenerations)
	r.addGarbage(junk)
	digests.commit(in)

	// 纠正动作记进台账：上报是周期快照，把它当事件发必然丢
	if rep.WorkloadAction != "" {
		in.LastWorkload = &state.WorkloadEvent{
			Action: string(rep.WorkloadAction), At: r.now(),
		}
	}
	if e := in.LastWorkload; e != nil {
		rep.LastWorkloadAction = e.Action
		rep.LastWorkloadAt = e.At.UTC().Format(time.RFC3339)
	}

	in.LastReconcile = &state.ReconcileRecord{
		At: r.now(), Result: string(resultOf(rep)),
		DurationMs: r.now().Sub(started).Milliseconds(),
	}
	if err := r.Store.SaveInstance(in); err != nil {
		return finish(faults.Wrap(faults.Transient, "写入本地状态", err))
	}

	rep.Result = resultOf(rep)
	rep.Duration = r.now().Sub(started)
	r.log().Info("reconcile complete", "summary", rep.Summary())
	return rep, nil
}

// packRoot 返回该 Pack 解开后的目录。
func (r *Reconciler) packRoot(s *spec.ResolvedSpec) string {
	if r.PackDir == "" {
		return ""
	}
	name := s.Pack.Name
	if name == "" {
		name = s.Component
	}
	return r.PackDir + "/" + name
}

// resultOf 由报告内容推出总体结论。
func resultOf(rep *Report) Result {
	if rep.Result == ResultFailed && rep.Error != "" {
		return ResultFailed
	}
	drift := false
	for _, rr := range rep.Resources {
		if rr.Action == ActionReported {
			drift = true
		}
	}
	switch {
	case drift:
		return ResultDrift
	case rep.Changed():
		return ResultChanged
	default:
		return ResultOK
	}
}

// recordFailure 在调和失败时把这次 generation 记为 failed。
//
// 记下来而不是丢掉：失败的 generation 目录保留一个供诊断（state.Prunable
// 会留最近一个 failed），而 state=failed 保证它不会被当成回滚的落脚点。
func (r *Reconciler) recordFailure(in *state.Instance, pl plan, s *spec.ResolvedSpec, rep *Report) {
	if !pl.New {
		return
	}
	in.Paths = pathSnapshot(s)
	in.AddGeneration(state.Generation{
		Seq: pl.Seq, Digest: s.Digest,
		Version: s.Pack.Version, Revision: s.Pack.Revision,
		Dir: pl.Dir, MaterializedAt: r.now(), State: state.GenFailed,
	})
	in.LastReconcile = &state.ReconcileRecord{
		At: r.now(), Result: string(ResultFailed), Message: rep.Error,
	}
	if err := r.Store.SaveInstance(in); err != nil {
		r.log().Warn("failed to write state after failure too", "component", s.Component, "err", err)
	}
}

// ── 阶段② ───────────────────────────────────────────────────────────────

// applyResources 按声明顺序 Read → Diff → Apply。
//
// 顺序即依赖：`shared` 在前（建用户、解载荷），`role` 在后。mechd 下发时
// 已排好，这里原样执行。
//
// 某条资源 Apply 失败则**同阶段后续资源不再执行**——它们之间可能有隐含
// 依赖，硬着头皮往下做只会让现场更难判断。已执行的不回滚：文件系统操作
// 本来就没有事务，假装有会导致「回滚到一半又失败」这种更难诊断的状态。
// 回滚能力来自 generation 的不可变与原子软链切换。
func (r *Reconciler) applyResources(
	ctx context.Context, env *resource.Env, s *spec.ResolvedSpec,
	rep *Report, notify *notifySet, dc driftContext,
) error {
	resources, err := resource.Build(env, s.Resources)
	if err != nil {
		return err
	}

	for i, res := range resources {
		decl := s.Resources[i]
		rr := ResourceReport{ID: res.ID(), Type: res.Type()}

		// ignore + 期望状态没变 → **连读都不读**。
		//
		// 两个条件缺一不可。早先只判 ignore 就 continue，后果是一个标了
		// ignore 的配置文件**从来不会被创建**——Pack 作者标它是因为
		// 「应用自己会改写这个文件」，但那个文件仍然得先有个初值；
		// 升级换 generation 时它也不会更新。而 §4 明说
		// **driftPolicy 只管漂移，不管期望变更**。
		//
		// 加了 governed 之后：首装与升级无条件收敛，之后才真的不比对——
		// 而「不读」正是 ignore 要省下的那笔开销（一个被应用反复改写的
		// 大文件，每轮哈希一遍没有意义）。
		if decl.DriftPolicy == "ignore" && dc.governed {
			rr.Action, rr.State = ActionIgnored, "-"
			rep.Resources = append(rep.Resources, rr)
			continue
		}

		obs, err := res.Read(ctx)
		if err != nil {
			rr.Action, rr.Error = ActionFailed, err.Error()
			rep.Resources = append(rep.Resources, rr)
			return fmt.Errorf("资源 %s: %w", res.ID(), err)
		}
		rr.State = obs.State.String()

		if obs.State == resource.StateUnknown {
			// 读不到不是漂移，是读不到。跳过并单独归类，不猜。
			rr.Action, rr.Reason = ActionSkipped, obs.Reason
			rep.Resources = append(rep.Resources, rr)
			r.log().Warn("could not read resource state, skipping this round",
				"id", res.ID(), "reason", obs.Reason)
			continue
		}

		changes := res.Diff(obs)
		rr.Changes = changeReports(changes)
		if len(changes) == 0 {
			rr.Action = ActionNone
			rep.Resources = append(rep.Resources, rr)
			continue
		}

		// 首次物化（Absent）一律创建；已存在的**漂移**才受 driftPolicy 约束。
		if dc.governed && obs.State == resource.StatePresent {
			act, note := dc.decide(decl, res.ID())
			if act != ActionApplied {
				rr.Action, rr.Reason = act, note
				rep.Resources = append(rep.Resources, rr)
				if act == ActionSuppressed {
					r.log().Debug("drift detected (acknowledged, not alerting)",
						"id", res.ID(), "reason", note)
				} else {
					r.log().Info("drift detected (report-only per policy)",
						"id", res.ID(), "changes", len(changes))
				}
				continue
			}
		}

		if err := res.Apply(ctx); err != nil {
			rr.Action, rr.Error = ActionFailed, err.Error()
			rep.Resources = append(rep.Resources, rr)
			return fmt.Errorf("资源 %s: %w", res.ID(), err)
		}
		rr.Action = ActionApplied
		rep.Resources = append(rep.Resources, rr)

		// notify 只在这里产生——确有差异且已 Apply
		notify.add(decl.Notify, res.ID())
	}
	return nil
}

// driftContext 是判定「这处漂移该怎么办」所需的全部输入。
type driftContext struct {
	// governed 为 true 表示期望状态未变（复用当前 generation），
	// 此时的差异才叫漂移；新 generation / 回滚时一律无条件收敛。
	governed bool
	instance *state.Instance
	// spec 是本轮的期望状态，抑制从这里读。
	spec *spec.ResolvedSpec
	now  time.Time
	// allowRestart 控制「自动改回」是否可以顺带重启服务。
	allowRestart bool
}

// decide 返回对一处漂移应采取的动作。
//
// 三道闸，顺序不能换：
//
//	① 已被显式确认（ack-drift）→ 不动、不告警。**运维现场说了算**
//	② 策略不是 reconcile        → 只上报。默认如此：自动改回一个为救火而
//	                              临时修改的配置，比漂移本身更严重
//	③ 要改回、但会连带重启      → 除非显式允许，否则降级为上报
func (dc driftContext) decide(decl spec.Resource, id string) (ResourceAction, string) {
	// 抑制来自**规格**，不是 mechlet 的本地状态。
	//
	// 早先这里读的是 state.Instance.Suppressions，而那个字段**从来没有
	// 任何代码写过**——`ack-drift` 存进 mechd 的库，节点永远不知道。
	// 于是「已抑制」在 status 里显示得好好的，实际却照常告警：
	// 一个看起来生效、实际是死代码的功能。
	if s, ok := dc.spec.SuppressedAt(id, dc.now); ok {
		return ActionSuppressed, fmt.Sprintf("已确认：%s（至 %s）", s.Reason, s.Until)
	}
	if decl.DriftPolicy != "reconcile" {
		return ActionReported, ""
	}
	if !dc.allowRestart && decl.Notify == NotifyRestart {
		// 运维只是想试个参数，服务却在他手底下重启了——这是最不该
		// 由工具自作主张的一类动作。
		return ActionReported, "该资源改回后会重启服务，未显式允许，本轮只上报"
	}
	return ActionApplied, ""
}

// appliedRefs 记下本轮声明过的资源，供「已不再声明但仍存在」的比对。
func appliedRefs(s *spec.ResolvedSpec) []state.ResourceRef {
	out := make([]state.ResourceRef, 0, len(s.Resources))
	for _, res := range s.Resources {
		out = append(out, state.ResourceRef{ID: res.ID, Type: res.Type})
	}
	return out
}

// ── 阶段③④ ─────────────────────────────────────────────────────────────

// applyWorkload 物化并按需切换、启动工作负载。
func (r *Reconciler) applyWorkload(
	ctx context.Context, s *spec.ResolvedSpec, pl plan, home string,
	rep *Report, notify *notifySet, hooks *hook.Executor,
) error {
	if s.Workload == nil {
		// 无 workload 的角色只落文件不起进程（纯主机配置、客户端配置分发）。
		// 切软链仍要做——generation 目录里可能有别的组件要引用的东西。
		if pl.Switch {
			if err := switchCurrent(home, pl.Dir); err != nil {
				return err
			}
			rep.Switched = true
		}
		return nil
	}

	rt, err := r.Runtimes.For(s.Workload)
	if err != nil {
		return err
	}

	w := runtime.WorkloadSpec{
		Site: s.Site.Name, Component: s.Component, Role: s.Role,
		ConfigGroup: s.ConfigGroup, Generation: pl.Seq,
		GenerationDir: pl.Dir, Home: home, Workload: s.Workload,
		// 容器不可变，docker runtime 靠它判断要不要重建（19-container-runtime §4.2）
		SpecDigest: s.Digest,
		Blobs:      blobPaths(s, r.BlobDir),
	}
	ref, err := rt.Materialize(ctx, w)
	// **失败也要记**：docker load 成功、建容器失败，镜像已经在库里了。
	// 只在成功路径上记，那个镜像就再也没人认领。
	rep.Images = append(rep.Images, ref.Images...)
	if err != nil {
		return err
	}

	before, err := rt.Observe(ctx, ref)
	if err != nil {
		return err
	}

	// stop / start 各自包一层 hook。包成闭包而不是在三个分支里各写一遍：
	// 漏掉一处的后果是「升级时 preStop 不跑，手工重启时跑」这类只在
	// 特定路径上出现的差异，几乎不可能被测出来。
	stop := func(opts runtime.StopOpts) error {
		if err := r.runHooks(ctx, hooks, s, hook.PreStop, rep); err != nil {
			return err
		}
		if err := rt.Stop(ctx, ref, opts); err != nil {
			return err
		}
		return r.runHooks(ctx, hooks, s, hook.PostStop, rep)
	}
	start := func() error {
		if err := r.runHooks(ctx, hooks, s, hook.PreStart, rep); err != nil {
			return err
		}
		if err := rt.Start(ctx, ref); err != nil {
			return err
		}
		// postStart 失败标记调和失败，但**不自动停掉服务**——
		// 它已经起来了，替用户做停机决定不是引擎的职责（18-hooks §4）
		return r.runHooks(ctx, hooks, s, hook.PostStart, rep)
	}

	// **期望停止的组件仍然要物化与切软链**，只是不启动：
	// 配置照常更新、载荷照常就位，`component start` 时立刻就是新版本。
	// 「停着 = 不管它」会让一次停机变成一次静默的版本落后。
	if !s.WantsRunning() {
		if err := r.enforceStopped(ctx, rt, ref, s, pl, home, before, rep, stop); err != nil {
			return err
		}
		after, err := rt.Observe(ctx, ref)
		if err != nil {
			return err
		}
		rep.Workload = &after
		return nil
	}

	switch {
	case pl.Switch:
		// generation 切换：停 → 切软链 → 起。**切软链是唯一不可分割的时刻。**
		if before.State != runtime.StateAbsent && before.State != runtime.StateStopped {
			if err := stop(runtime.StopOpts{}); err != nil {
				return err
			}
		}
		if err := switchCurrent(home, pl.Dir); err != nil {
			return err
		}
		rep.Switched = true
		if err := start(); err != nil {
			return err
		}
		// 进程刚起来，本轮的 notify 已经被这次启动涵盖了
		notify.actions = map[string]bool{}

	case settling(before.State):
		// **Runtime 自己正在处理，让路。**
		//
		// systemd 的 Restart=always 与 docker 的 --restart 会自己把崩掉的
		// 进程拉起来。那期间状态是 Starting / restarting——此时再插一脚
		// `start` 只会与它抢，而且每 60 秒抢一次。
		//
		// 判据是**观测到的状态**而不是「Pack 声明了重启策略」：后者说明
		// 它打算重启，前者才说明它此刻真的在重启。
		r.log().Debug("runtime is self-recovering, not intervening this round",
			"unit", ref.Native, "state", before.State)

	case !before.Running():
		// 没在跑就拉起来——服务被人手工 stop 了，这正是常驻 Agent 的价值
		r.log().Info("workload not running, starting it", "unit", ref.Native, "state", before.State)
		if err := start(); err != nil {
			return err
		}
		rep.WorkloadAction = WorkloadRestored
		notify.actions = map[string]bool{}

	default:
		if err := r.runNotify(ctx, rt, ref, rep, notify, s, stop, start); err != nil {
			return err
		}
	}

	after, err := rt.Observe(ctx, ref)
	if err != nil {
		return err
	}
	rep.Workload = &after
	return nil
}

// runNotify 是阶段⑤：执行聚合后的 notify 动作。
//
// notify 失败算调和失败——配置改了但服务没重载，等于变更没生效，
// 而机器上的文件已经变了，这种「一半生效」的状态必须让人知道。
func (r *Reconciler) runNotify(
	ctx context.Context, rt runtime.Runtime, ref runtime.Ref,
	rep *Report, notify *notifySet, s *spec.ResolvedSpec,
	stop func(runtime.StopOpts) error, start func() error,
) error {
	if notify.Empty() {
		return nil
	}
	rep.Absorbed = notify.absorbed()

	for _, action := range notify.resolved() {
		switch action {
		case NotifyRestart:
			r.log().Info("notify: restart", "unit", ref.Native,
				"causes", notify.causesOf(action))
			if err := stop(runtime.StopOpts{}); err != nil {
				return err
			}
			if err := start(); err != nil {
				return err
			}

		case NotifyReload:
			r.log().Info("notify: reload", "unit", ref.Native,
				"causes", notify.causesOf(action))
			err := rt.Reload(ctx, ref)
			if errors.Is(err, runtime.ErrReloadUnsupported) {
				// 降级为重启：Pack 声明了 reload 但工作负载不支持，
				// 什么都不做等于变更没生效。
				r.log().Info("workload does not support reload, downgrading to restart", "unit", ref.Native)
				if err := stop(runtime.StopOpts{}); err != nil {
					return err
				}
				if err := start(); err != nil {
					return err
				}
				action = NotifyRestart
			} else if err != nil {
				return err
			}

		default:
			// hook 名：notify 指向一个生命周期点之外的、由资源变更触发的动作。
			//
			// 它没有对应的生命周期点，因此按名字在 Hooks 里找同名脚本。
			if err := r.notifyHook(ctx, s, action, rep); err != nil {
				return err
			}
		}
		rep.Notified = append(rep.Notified, action)
	}
	return nil
}

// notifyHook 执行 notify 指名的那个 hook。
//
// 与生命周期点上的 hook 不同，它是**资源变更驱动**的：某个模板变了才跑。
// 匹配按脚本名（不含目录与扩展名），因为 Pack 作者在 notify 里写的是
// 一个短名字而非路径。
func (r *Reconciler) notifyHook(
	ctx context.Context, s *spec.ResolvedSpec, name string, rep *Report,
) error {
	target := findNotifyHook(s, name)
	if target == nil {
		// 明确报错而不是静默跳过——静默会让 Pack 作者以为它生效了，
		// 而问题要到生产上才暴露。lint 的 R23 在 Pack 侧也查一遍。
		return faults.Permanentf("执行 notify",
			"notify: %s 既不是内建动作（restart / reload），"+
				"也不对应任何已下发的 hook\n"+
				"  已下发的 hook: %s", name, hookNames(s))
	}

	env := resource.EnvFor(s, r.packRoot(s), r.BlobDir)
	env.Runner = r.runner()
	ex := r.hookRunner(env, s, rep.GenerationDir)

	// 借用 hook 执行器：临时造一份只含这一个 hook 的规格，
	// 让密钥注入、脱敏、超时、身份全部复用同一条路径
	one := *s
	one.Hooks = []spec.Hook{*target}
	one.Hooks[0].Point = notifyPoint
	return r.runHooks(ctx, ex, one.WithSecrets(s.SecretParams()), notifyPoint, rep)
}

// notifyPoint 是 notify 触发的 hook 在 Report 里显示的「点」。
//
// 不复用真实的生命周期点名：那会让排障时分不清「这是 postStart 跑的」
// 还是「这是配置变更触发的」。
const notifyPoint = "notify"

func findNotifyHook(s *spec.ResolvedSpec, name string) *spec.Hook {
	for i := range s.Hooks {
		if hookShortName(s.Hooks[i].Script) == name {
			return &s.Hooks[i]
		}
	}
	return nil
}

// hookShortName 取脚本的短名：hooks/reload-certs.sh → reload-certs。
func hookShortName(script string) string {
	base := script
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	return base
}

func hookNames(s *spec.ResolvedSpec) string {
	if len(s.Hooks) == 0 {
		return "（无）"
	}
	out := make([]string, 0, len(s.Hooks))
	for _, h := range s.Hooks {
		out = append(out, hookShortName(h.Script))
	}
	return strings.Join(out, ", ")
}

// ── 阶段⑥ ───────────────────────────────────────────────────────────────

// checkHealth 执行健康探针。
func (r *Reconciler) checkHealth(ctx context.Context, s *spec.ResolvedSpec, rep *Report) error {
	if s.Health == nil || s.Workload == nil {
		return nil
	}
	// 期望就是停着的，没什么可探的。
	if !s.WantsRunning() {
		return nil
	}

	// **不因为「没在跑」就跳过。**
	//
	// 早先这里在 `!Running()` 时直接返回 nil，理由是「探针必然失败，而真正的
	// 原因是进程没起来」。那个理由只对了一半：**它让「没起来」与「健康」
	// 变得不可区分**。
	//
	// 最坏的一种组合是崩溃重启循环：systemd 的 Restart=on-failure 让状态
	// 一直停在 Starting，调和器按「Runtime 正在自行恢复」让路，健康检查又
	// 按「没在跑」跳过——于是一个**永远起不来的新版本被报成 ok**，
	// 中心看到的是升级成功。M6 的自动回滚也因此永远不会触发。
	//
	// 让探针照常跑：`startupGrace` 本来就是给「刚起来还没就绪」用的，
	// 它同样是「一直起不来」的上界。错误信息里带上工作负载状态，
	// 「进程没起来」这个真正的原因不会被盖住。
	if rep.Workload != nil && !rep.Workload.Running() {
		r.log().Debug("workload not running yet, health check will wait until startupGrace is exhausted",
			"state", rep.Workload.State)
	}

	checker, err := health.New(s.Health, r.execContext(s, rep))
	if err != nil {
		return faults.Wrap(faults.Permanent, "构造健康探针", err)
	}
	if checker == nil {
		return nil
	}

	hr := &HealthReport{Probe: checker.Prober.Describe()}
	if err := checker.WaitReady(ctx); err != nil {
		hr.Healthy, hr.Error = false, err.Error()
		rep.Health = hr
		// 带上工作负载状态：探针失败时第一个要分辨的是
		// 「服务在跑但没就绪」还是「它压根没起来」——两者的下一步动作
		// 完全不同，而只报一句「连不上」区分不了。
		if rep.Workload != nil && !rep.Workload.Running() {
			return faults.Wrap(faults.Transient, "健康检查",
				fmt.Errorf("工作负载状态为 %s（不是 running）: %w",
					rep.Workload.State, err))
		}
		return faults.Wrap(faults.Transient, "健康检查", err)
	}
	hr.Healthy = true
	rep.Health = hr
	return nil
}

// execContext 返回「在这个工作负载的上下文里执行命令」的方式。
//
// **只有 exec 探针用得上**（ADR-0032）：systemd 下它就是在宿主机上跑，
// docker 下要 `docker exec` 进容器。把这一步交给 Runtime，其余的探针
// 编排（重试、阈值、超时）仍然三个 Runtime 共用一份实现。
//
// 拿不到 Runtime 时返回 nil，health 会退回「在本机执行」——那正是
// systemd 的语义，也是 `mechlet apply -f` 这条调试路径需要的。
func (r *Reconciler) execContext(s *spec.ResolvedSpec, rep *Report) health.ExecContext {
	if s.Workload == nil || r.Runtimes == nil {
		return nil
	}
	rt, err := r.Runtimes.For(s.Workload)
	if err != nil {
		return nil
	}

	ref := runtime.Ref{
		Runtime: rt.Name(), Component: s.Component, Role: s.Role,
	}
	// **Native 不能省**：systemd 的 ExecIn 用不到它（就在宿主机上跑），
	// 但 docker 要靠它知道进哪个容器。少了这一项，`docker exec "" …`
	// 会以一句看不懂的话失败。
	//
	// 它来自刚刚那次 Observe——Runtime 自己填的原生标识，比在这里
	// 按命名规则拼一个更可靠（拼错了要到探针失败时才发现）。
	if rep != nil && rep.Workload != nil {
		ref.Native = rep.Workload.Native
	}

	return func(ctx context.Context, cmd []string) (command.Result, error) {
		return rt.ExecIn(ctx, ref, cmd)
	}
}

// blobSums 是规格引用的全部载荷摘要。
func blobSums(s *spec.ResolvedSpec) []string {
	if len(s.Blobs) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.Blobs))
	for _, b := range s.Blobs {
		if b.SHA256 != "" {
			out = append(out, b.SHA256)
		}
	}
	return dedup(out)
}

// addGarbage 把被回收的引用写进节点级清单。
//
// **写失败只警告不失败**：调和本身已经成功了，机器上的状态是对的。
// 让一次「清单写不下去」把一次成功的升级判成失败，是把小事换成大事。
// 代价是这些镜像可能留在盘上——下次同一代再被 prune 时不会重来，
// 因此这条日志要说清楚，而不是悄悄咽掉。
func (r *Reconciler) addGarbage(junk garbage) {
	if junk.empty() || r.Store == nil {
		return
	}
	g, err := r.Store.LoadGarbage()
	if err != nil {
		r.log().Warn("failed to read garbage manifest, this round's images and blobs will stay on disk", "err", err)
		return
	}
	g.Add(r.now(), junk.images, junk.blobs)
	if err := r.Store.SaveGarbage(g); err != nil {
		r.log().Warn("failed to write garbage manifest, this round's images and blobs will stay on disk", "err", err)
	}
}

// blobPaths 把规格引用的载荷映射到本机路径。
//
// systemd 的载荷由资源引擎解压，Runtime 碰不到；docker 的镜像却要
// Runtime 自己 `docker load`。布局与资源引擎一致：
// <blobDir>/sha256/<前两位>/<完整摘要>。
func blobPaths(s *spec.ResolvedSpec, blobDir string) map[string]string {
	if blobDir == "" || len(s.Blobs) == 0 {
		return nil
	}
	out := make(map[string]string, len(s.Blobs))
	for _, b := range s.Blobs {
		if len(b.SHA256) < 2 {
			continue
		}
		out[b.Name] = filepath.Join(blobDir, "sha256", b.SHA256[:2], b.SHA256)
	}
	return out
}

// settling 报告 Runtime 是否正在自行把工作负载拉起来。
//
// systemd 的 `Restart=always` 与 docker 的 `--restart` 会自己重启崩掉的
// 进程。那期间观测到的是 Starting / restarting——这时调和器再 `start`
// 一次只会与它抢，而且每个周期抢一次。
func settling(st runtime.State) bool {
	return st == runtime.StateStarting
}

// enforceStopped 维持「期望停止」。
//
// 三件事，顺序要紧：
//
//	① 该物化的照常物化（上面已做），该切的软链照常切
//	② 在跑就停掉——**包括别人手工启动的**
//	③ 不执行任何 notify：那些动作全是 restart / reload，对一个应当停着的
//	   服务毫无意义，而 reload 一个停着的 unit 在 systemd 上会直接失败
//
// ②里「别人手工启动的也要停回去」不能省。只做「停了就别拉起来」的话，
// `component stop` 就成了一句没人执行的声明——有人手工把它起来，
// 系统会一直默认那是对的（20-continuous-reconcile §2.2）。
func (r *Reconciler) enforceStopped(
	ctx context.Context, rt runtime.Runtime, ref runtime.Ref,
	s *spec.ResolvedSpec, pl plan, home string, before runtime.Status,
	rep *Report, stop func(runtime.StopOpts) error,
) error {
	if pl.Switch {
		if err := switchCurrent(home, pl.Dir); err != nil {
			return err
		}
		rep.Switched = true
	}

	switch before.State {
	case runtime.StateAbsent, runtime.StateStopped:
		r.log().Debug("expected stopped, and it really isn't running", "unit", ref.Native)
		return nil
	}

	r.log().Info("expected stopped but running, stopping it",
		"unit", ref.Native, "state", before.State,
		"hint", "an operator ran component stop; use component start to resume")
	if err := stop(runtime.StopOpts{}); err != nil {
		return err
	}
	rep.WorkloadAction = WorkloadStopped
	return nil
}
