package agent

import (
	"context"

	"github.com/mecharion/mecharion/internal/reclaim"
	"github.com/mecharion/mecharion/internal/runtime"
)

// collectGarbage 删掉已经没有任何 generation 引用的镜像与载荷。
//
// 真正的判据在 internal/reclaim；这里只多加一条 agent 才知道的前提：
// **有实例在忙就整轮跳过**。那个实例可能正要 load 一个 blob，而它的台账
// 还没写下这条引用。回收可以推迟，删错不可逆。
func (a *Agent) collectGarbage(ctx context.Context) {
	if a.opts.State == nil {
		return
	}
	if a.anyBusy() {
		a.opts.Log.Debug("an instance is reconciling, skipping garbage collection this round")
		return
	}
	var reg *runtime.Registry
	if a.opts.Reconciler != nil {
		reg = a.opts.Reconciler.Runtimes
	}
	reclaim.Run(ctx, reclaim.Options{
		State:    a.opts.State,
		Runtimes: reg,
		BlobDir:  a.opts.BlobDir,
		Log:      a.opts.Log,
	})
}

func (a *Agent) anyBusy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.busy) > 0
}
