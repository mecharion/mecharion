package reconcile

import (
	"context"

	"github.com/mecharion/mecharion/internal/hook"
	"github.com/mecharion/mecharion/internal/resource"
	"github.com/mecharion/mecharion/internal/spec"
)

// HookReport 是一次 hook 执行的记录，进 Report 供排障。
type HookReport struct {
	Point  string `json:"point"`
	Script string `json:"script"`
	// Output 已按敏感值脱敏。
	Output   string `json:"output,omitempty"`
	ExitCode int    `json:"exitCode"`
	Millis   int64  `json:"ms"`
}

// hookRunner 构造本轮的 hook 执行器。
//
// PackRoot 与 GenerationDir 每轮都可能不同（升级会换 generation），
// 因此执行器是每轮新建的，不挂在 Reconciler 上。
func (r *Reconciler) hookRunner(
	env *resource.Env, s *spec.ResolvedSpec, genDir string,
) *hook.Executor {
	return &hook.Executor{
		Runner:        r.runner(),
		Lookup:        env,
		RunDir:        r.HookRunDir,
		Now:           r.Now,
		PackRoot:      r.packRoot(s),
		GenerationDir: genDir,
	}
}

// runHooks 执行某个生命周期点上的 hook 并记进 Report。
//
// 规格里出现的 hook 就是要跑的：`when: false` 的与 `scope: once` 已执行过的
// **在 mechd 侧就没下发**。mechlet 完全不理解 once 语义——那是个跨节点
// 概念，放在唯一有全局视角的地方，mechlet 才永远不需要相互查询。
func (r *Reconciler) runHooks(
	ctx context.Context, ex *hook.Executor, s *spec.ResolvedSpec,
	point string, rep *Report,
) error {
	results, err := ex.Run(ctx, s, point)
	for _, res := range results {
		rep.Hooks = append(rep.Hooks, HookReport{
			Point: res.Point, Script: res.Script,
			Output: res.Output, ExitCode: res.ExitCode,
			Millis: res.Duration.Milliseconds(),
		})
	}
	if err != nil {
		return err
	}
	for _, res := range results {
		r.log().Info("hook complete", "point", res.Point, "script", res.Script,
			"ms", res.Duration.Milliseconds())
	}
	return nil
}
