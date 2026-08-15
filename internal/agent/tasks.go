package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/mecharion/mecharion/internal/protocol"
	"github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/state"
)

// RunTask 执行一条 ad-hoc 命令。实现 protocol.TaskHandler。
func (a *Agent) RunTask(ctx context.Context, cmd protocol.TaskCommand) protocol.TaskResult {
	switch cmd.Kind {
	case protocol.TaskRestart:
		return a.restart(ctx, cmd)
	default:
		// 不认识的类型**如实拒绝**：一个新 mechd 发来的新命令，旧 mechlet
		// 做不了。静默成功会让中心以为它生效了。
		return protocol.TaskResult{
			Message: fmt.Sprintf("本 mechlet 不认识命令类型 %q，请升级 mechlet", cmd.Kind),
		}
	}
}

// restart 停掉再拉起一个实例的工作负载。
//
// **它绕过调和，不改期望状态**（06-state-and-drift）：期望状态没有变，
// 变的只是「现在把它踢一下」。因此这里既不动 generation、也不写台账，
// 下一轮调和看到的仍是同一份规格、同一个 digest。
//
// 与 cordon 的关系：**被 cordon 的机器仍然接受 restart**。cordon 说的是
// 「别让 mechlet 把我的改动改回去」，而 restart 是人**当场**在敲的命令
// ——把它也挡掉，会让「我想重启一下看看」在最需要的时候做不到。
func (a *Agent) restart(ctx context.Context, cmd protocol.TaskCommand) protocol.TaskResult {
	started := time.Now()
	key := state.InstanceKey(cmd.Component, cmd.Role)

	// 占住这个实例：正在调和时插一脚会与它抢同一个 unit
	if !a.acquire(key) {
		return protocol.TaskResult{
			Message: "该实例正在调和中，请稍后重试",
		}
	}
	defer a.release(key)

	// 从本机的期望状态里找那份规格。
	//
	// **不从台账里找**：台账记的是「装过什么」，而重启要针对的是「现在
	// 该跑的那一份」——一个刚被 stop 掉的实例，台账与期望状态说的是
	// 两件不同的事。
	specs, err := a.opts.Desired.Load()
	if err != nil {
		return protocol.TaskResult{Message: fmt.Sprintf("读本机期望状态: %v", err)}
	}
	var sp *spec.ResolvedSpec
	for _, in := range specs {
		if in.Spec.InstanceKey() == key {
			sp = in.Spec
			break
		}
	}
	if sp == nil {
		return protocol.TaskResult{
			Message: fmt.Sprintf("本机没有实例 %s", key),
		}
	}
	if sp.Workload == nil {
		// 纯配置分发的角色没有进程可重启。**说清楚而不是假装成功**。
		return protocol.TaskResult{
			Message: fmt.Sprintf("%s 没有工作负载，无从重启", key),
		}
	}

	rt, err := a.opts.Reconciler.Runtimes.For(sp.Workload)
	if err != nil {
		return protocol.TaskResult{Message: err.Error()}
	}
	ref, err := rt.RefFor(runtime.WorkloadSpec{
		Site: sp.Site.Name, Component: sp.Component, Role: sp.Role,
		ConfigGroup: sp.ConfigGroup, Workload: sp.Workload,
	})
	if err != nil {
		return protocol.TaskResult{Message: err.Error()}
	}

	if err := rt.Stop(ctx, ref, runtime.StopOpts{}); err != nil {
		return protocol.TaskResult{
			Message:  fmt.Sprintf("停止失败: %v", err),
			Duration: time.Since(started),
		}
	}
	if err := rt.Start(ctx, ref); err != nil {
		// **停掉了但没起来**：这是最要紧的一种结果，必须原样说出来
		// ——运维需要知道现在那台机器上的服务是**停着**的。
		return protocol.TaskResult{
			Message:  fmt.Sprintf("已停止但启动失败: %v（该实例现在是停止状态）", err),
			Duration: time.Since(started),
		}
	}

	a.opts.Log.Info("restarted instance on command", "instance", key, "unit", ref.Native)
	return protocol.TaskResult{OK: true, Duration: time.Since(started)}
}
