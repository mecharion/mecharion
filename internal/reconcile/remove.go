package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"sort"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/hook"
	"github.com/mecharion/mecharion/internal/resource"
	"github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/state"
)

// ── 卸载 ────────────────────────────────────────────────────────────────

// uninstall 执行 runState: removed（24-lifecycle §2.3）。顺序照 10-cli §4.3：
//
//	preStop → Stop → postStop → preRemove → Runtime.Remove
//	        → 删 generation / config → 数据按开关处理 → postRemove
//
// **幂等**：没装过、装了一半、已经卸过——都走到同一个终态并报成功。
// 这是必须的，因为 removed 是一个会被反复下发的**状态**：mechd 要等到
// 全部实例都报「已卸载」才删记录，而在那之前每一轮调和都会再来一次。
// 任何一处「已经没了 → 报错」都会让组件永远卡在 removing。
//
// 用的是**固化在本地状态里的路径**，不是规格里的。规格里的路径可能因为
// 有人改了 Node.Roots 而与盘上的对不上，而卸载要拆的是**盘上真实存在的
// 那一份**。正常路径上 CheckPaths 会拒绝这种不一致，卸载则必须放行——
// 否则一个路径漂移过的实例就再也删不掉了。
func (r *Reconciler) uninstall(
	ctx context.Context, s *spec.ResolvedSpec, in *state.Instance, rep *Report,
) error {
	opts := s.Removal
	if opts == nil {
		// 零值就是安全默认：配置删、数据留、用户留。
		// 一份漏传 Removal 的规格不会多删任何东西。
		opts = &spec.Removal{}
	}
	rep.Removed = &RemovalReport{}

	pinned := in.Paths
	if len(pinned) == 0 {
		// 没固化过路径 = 从没成功调和过。仍然要往下走：Runtime 那一侧
		// 可能已经建过 unit / 容器（物化在写台账之前），只是没走到台账。
		pinned = pathSnapshot(s)
	}
	home := firstOf(pinned["home"])
	if home == "" {
		home = homeOf(s)
	}

	// ① 停 + ② 卸工作负载 ────────────────────────────────────────────
	//
	// hook 的 cwd 是 generation 目录（18-hooks §3），因此这一段必须
	// **早于删 home**——那之后 cwd 就不存在了，Go 的 fork/exec 会报
	// 「脚本不存在」而脚本明明在那儿。
	genDir := ""
	if g := in.Active(); g != nil {
		genDir = g.Dir
	}
	hooks := r.removeHooks(s, genDir)

	if err := r.tearDownWorkload(ctx, s, in, hooks, rep); err != nil {
		return err
	}

	// ③ 处置目录 ──────────────────────────────────────────────────────
	//
	// 排在 preRemove/Runtime.Remove 之后：hook 与 Runtime 都可能还要读
	// 配置目录（容器的 mount 源就在里面）。
	retained, err := r.disposePaths(pinned, home, opts, rep)
	if err != nil {
		return err
	}

	// ④ --purge-user ─────────────────────────────────────────────────
	if opts.PurgeUser {
		r.purgeIdentities(ctx, s, rep)
	}

	// ⑤ postRemove ───────────────────────────────────────────────────
	//
	// **cwd 换成空**：generation 目录刚被删掉，还指着它的话这个 hook
	// 必然失败，而失败的位置会指向脚本，把人引向完全错误的方向。
	// 脚本本身在 Pack 根下（不随实例删），因此照常找得到。
	if err := r.runHooks(ctx, r.removeHooks(s, ""), s, hook.PostRemove, rep); err != nil {
		return err
	}

	// ⑥ 本地状态 ──────────────────────────────────────────────────────
	if err := r.recordRemoval(s, in, retained); err != nil {
		return err
	}

	rep.Result = ResultChanged
	r.log().Info("instance uninstalled",
		"component", s.Component, "role", s.Role,
		"retained", len(retained), "purged", len(rep.Removed.PurgedPaths))
	return nil
}

// tearDownWorkload 走 preStop → Stop → postStop → preRemove → Runtime.Remove。
//
// 没有 workload 的角色（纯配置分发）跳过 Runtime，但 **preRemove /
// postRemove 照跑**：它们是「这个实例要没了」的通知，与有没有进程无关。
func (r *Reconciler) tearDownWorkload(
	ctx context.Context, s *spec.ResolvedSpec, in *state.Instance,
	hooks *hook.Executor, rep *Report,
) error {
	if s.Workload == nil {
		return r.runHooks(ctx, hooks, s, hook.PreRemove, rep)
	}

	rt, err := r.Runtimes.For(s.Workload)
	if err != nil {
		return err
	}
	gen := 0
	if g := in.Active(); g != nil {
		gen = g.Seq
	}
	// **RefFor 而不是 Materialize**：拆之前不能先装一遍。
	ref, err := rt.RefFor(runtime.WorkloadSpec{
		Site: s.Site.Name, Component: s.Component, Role: s.Role,
		ConfigGroup: s.ConfigGroup, Generation: gen,
		Workload: s.Workload,
	})
	if err != nil {
		return err
	}

	st, err := rt.Observe(ctx, ref)
	if err != nil {
		return err
	}
	// 已经不在跑就别走 stop hook：preStop 的语义是「服务马上要停了」，
	// 对一个早就停着的东西喊一遍，会让 hook 作者写的「摘流量」之类动作
	// 在每一轮重复的卸载里反复执行。
	if st.State != runtime.StateAbsent && st.State != runtime.StateStopped {
		if err := r.runHooks(ctx, hooks, s, hook.PreStop, rep); err != nil {
			return err
		}
		if err := rt.Stop(ctx, ref, runtime.StopOpts{}); err != nil {
			return err
		}
		if err := r.runHooks(ctx, hooks, s, hook.PostStop, rep); err != nil {
			return err
		}
	}

	if err := r.runHooks(ctx, hooks, s, hook.PreRemove, rep); err != nil {
		return err
	}
	if err := rt.Remove(ctx, ref); err != nil {
		return err
	}
	rep.Removed.Native = ref.Native
	rep.Removed.Runtime = ref.Runtime
	return nil
}

// disposePaths 按 §2.3 那张表处置全部固化路径，返回保留下来的那些。
func (r *Reconciler) disposePaths(
	pinned map[string][]string, home string, opts *spec.Removal, rep *Report,
) ([]string, error) {
	var retained []string

	names := make([]string, 0, len(pinned))
	for n := range pinned {
		names = append(names, n)
	}
	sort.Strings(names) // 顺序稳定，报告与日志才好比对

	for _, name := range names {
		// 归类表在 spec 里，两端共用：中心侧要用同一份算出「这次 remove
		// 会留下什么」并在二档确认之前打给人看。各写一份的话，预览迟早
		// 会与真正发生的事不一致——而那正是确认这个动作唯一的价值。
		drop := spec.DispositionOf(name).Drops(opts)

		for _, dir := range pinned[name] {
			// 非绝对路径一律跳过，而这**不是**防御性代码：固化的路径存的是
			// **未替换 generation 占位符**的形态（正常路径上的 CheckPaths
			// 也是拿这个形态比对的）。因此 `layout: inline` 的配置在这里
			// 长成 `{{ .Paths.Generation }}/config`——不是绝对路径。
			//
			// 跳过它是对的：inline 的物理位置就在 generation 里，也就是
			// home 之下，删 home 已经把它带走了。
			if dir == "" || !filepath.IsAbs(dir) {
				continue
			}
			// **home 之下的东西不单独处理**：删 home 已经把它们带走了，
			// 而 inline 布局的 config 正好在 home 里。再删一次不会错，
			// 但「保留」一个已经随 home 消失的目录会登记出一个不存在的
			// 孤儿——那比漏登记更糟，因为它把人指向一个空地址。
			if home != "" && name != "home" && under(home, dir) {
				continue
			}

			// **不存在的目录一条都不记。**
			//
			// 「保留」一个从没被建出来的目录，orphans 里就会出现一条指向
			// 空地址的记录——而运维会真的跑去那台机器上找它。一个从没
			// 装成功过的实例（部署失败之后才收到 removed）正好走这条路。
			//
			// 「删掉」也同理：报告里列一堆其实什么也没发生的路径，会让人
			// 以为这次卸载清掉了很多东西。
			if _, err := os.Lstat(dir); err != nil {
				continue
			}

			if !drop {
				retained = append(retained, dir)
				continue
			}
			if err := os.RemoveAll(dir); err != nil {
				return nil, faults.Wrap(faults.Transient, "删除 "+name+" 目录 "+dir, err)
			}
			rep.Removed.PurgedPaths = append(rep.Removed.PurgedPaths, dir)
		}
	}

	retained = dedup(retained)
	rep.Removed.RetainedPaths = retained
	return retained, nil
}

// purgeIdentities 删掉规格声明过的系统用户与组（--purge-user）。
//
// **失败只警告，不让卸载失败。** userdel 在用户还有进程、或还是别处
// 文件属主时会拒绝，而那是常态而非异常。让它中止卸载，组件就会永远卡在
// removing——为一个可选的清理动作赌上整条删除路径，不划算。
//
// 不加 -r：用户的 home 可能正是刚被判定为「保留」的数据目录。
func (r *Reconciler) purgeIdentities(ctx context.Context, s *spec.ResolvedSpec, rep *Report) {
	run := r.runner()
	// 先用户后组：组还被用户引用时 groupdel 会拒绝。
	for _, pass := range []struct {
		typ string
		cmd string
	}{{resource.TypeUser, "userdel"}, {resource.TypeGroup, "groupdel"}} {
		for _, res := range s.Resources {
			if res.Type != pass.typ {
				continue
			}
			name := resource.IdentityName(res)
			if name == "" || name == "root" {
				continue
			}
			out, err := run.Run(ctx, pass.cmd, name)
			if err == nil && out.ExitCode == 0 {
				rep.Removed.PurgedIdentities = append(rep.Removed.PurgedIdentities, name)
				continue
			}
			msg := name + ": " + firstLine(out.Message())
			rep.Removed.Warnings = append(rep.Removed.Warnings, pass.cmd+" 失败 "+msg)
			r.log().Warn("failed to delete identity, skipped",
				"cmd", pass.cmd, "name", name, "component", s.Component,
				"hint", "it may still own files elsewhere, or a process may still be running")
		}
	}
}

// recordRemoval 落本地状态：留了东西写收据，什么都没留就整个删掉。
func (r *Reconciler) recordRemoval(
	s *spec.ResolvedSpec, in *state.Instance, retained []string,
) error {
	key := s.InstanceKey()
	if len(retained) == 0 {
		// 干净的卸载不留痕迹——包括这张收据。
		if err := r.Store.DeleteInstance(key); err != nil {
			return faults.Wrap(faults.Transient, "删除本地实例状态", err)
		}
		return nil
	}

	// 只留收据：台账、资源清单、摘要缓存全部清掉。留着它们会让这条记录
	// 看起来仍像一个装着的实例，而 `orphans` 与回收都会据此做出错误判断。
	in.Component, in.Role = s.Component, s.Role
	in.ConfigGroup = s.ConfigGroup
	in.CurrentGeneration = 0
	in.Generations = nil
	in.AppliedResources = nil
	in.Digests = nil
	in.LastWorkload = nil
	in.Paths = map[string][]string{}
	in.Removed = &state.Removal{
		At: r.now(), Pack: s.Pack.Name, Version: s.Pack.Version,
		RetainedPaths: retained,
	}
	if err := r.Store.SaveInstance(in); err != nil {
		return faults.Wrap(faults.Transient, "保存卸载收据", err)
	}
	return nil
}

// removeHooks 建一个卸载用的 hook 执行器。
//
// 与正常路径的区别只有 Lookup：卸载不解析资源，因而没有 resource.Env。
// hook 拿得到的仍是规格里的参数与密钥——那是 hook.Executor 自己的事。
func (r *Reconciler) removeHooks(s *spec.ResolvedSpec, genDir string) *hook.Executor {
	return &hook.Executor{
		Runner:        r.runner(),
		RunDir:        r.HookRunDir,
		Now:           r.Now,
		PackRoot:      r.packRoot(s),
		GenerationDir: genDir,
	}
}

// ── 小工具 ──────────────────────────────────────────────────────────────

func firstOf(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// under 报告 dir 是否在 root 之下（或就是 root）。
func under(root, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(dir))
	if err != nil {
		return false
	}
	return rel != ".." && !hasPrefixSep(rel, "..")
}

func hasPrefixSep(s, p string) bool {
	return len(s) > len(p) && s[:len(p)] == p && os.IsPathSeparator(s[len(p)])
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
