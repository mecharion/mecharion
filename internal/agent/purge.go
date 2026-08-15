package agent

import (
	"context"
	"os"
)

// purgeOrphans 清掉中心指定的那些孤儿留下的目录（24-lifecycle §2.4）。
//
// **删的是本地收据里记的路径，不是中心下发的路径。** 这条区别是整个
// purge 设计的安全性所在。考虑这个时序：
//
//	t0  中心下发 purge pg-main__primary，这台机器正好失联
//	t1  运维用同一个名字重新部署了 pg-main，新数据写进了同一个目录
//	t2  这台机器回来了，收到那条 purge
//
// 若下发的是绝对路径，t2 会把**新数据**删掉。下发实例键则不会：t1 那次
// 部署已经把本地状态写成了一个真实实例（Removed 为 nil），purge 找不到
// 收据，直接空跑。
//
// 幂等：清过的、不存在的、从没听说过的键，都走到同一个终态。
func (a *Agent) purgeOrphans(ctx context.Context, keys []string) {
	for _, key := range keys {
		if ctx.Err() != nil {
			return
		}
		in, err := a.opts.State.LoadInstance(key)
		if err != nil {
			a.opts.Log.Warn("failed to read instance pending cleanup, skipping this round", "instance", key, "err", err)
			continue
		}
		if in == nil || in.Removed == nil {
			// 没有收据 = 已经清过了，或者它压根不是一个被卸载后留下的
			// 残留（比如同名重新部署过）。**两种情况都该什么也不做。**
			continue
		}

		var failed bool
		for _, dir := range in.Removed.RetainedPaths {
			if dir == "" || !isAbs(dir) {
				continue
			}
			if err := os.RemoveAll(dir); err != nil {
				// 一个目录删不掉不该让其余的也留着——但收据要留着，
				// 下一轮再试，而它仍然会出现在孤儿列表里。
				a.opts.Log.Error("failed to clean up orphan directory", "instance", key, "path", dir, "err", err)
				failed = true
				continue
			}
			a.opts.Log.Info("orphan directory cleaned up", "instance", key, "path", dir)
		}
		if failed {
			continue
		}

		// 全清干净了才删收据。**收据是这个孤儿存在的唯一证据**——
		// 删早了，剩下的目录就再也没人说得清来历了。
		if err := a.opts.State.DeleteInstance(key); err != nil {
			a.opts.Log.Error("failed to delete removal receipt", "instance", key, "err", err)
			continue
		}
		a.opts.Log.Info("orphan cleaned up", "instance", key,
			"directories", len(in.Removed.RetainedPaths))
	}
}

// isAbs 判断是不是绝对路径。
//
// 不用 filepath.IsAbs：mechlet 只跑在 Linux 上，而这里的判据要与
// 「节点上真实的路径」一致，不随构建平台变化——在 Windows 上跑测试时
// filepath.IsAbs("/var/lib/x") 是 false，那会让这段逻辑在测试里静默失效。
//
// **reconcile/disposePaths 里用的是 filepath.IsAbs，那不是笔误。** 两处
// 问的不是同一个问题：
//
//	这里          「这条路径绝对到可以直接 RemoveAll 吗」——安全判据
//	disposePaths  「这是物化过的路径，还是没展开的 generation 占位符」
//
// 后者在两个平台上对占位符的答案都是「否」，因此那边用 filepath.IsAbs
// 反而让测试在 Windows 上也验的是真东西。代价是这里的测试只能在 Linux
// 跑（见 purge_linux_test.go 开头）。
func isAbs(p string) bool { return len(p) > 0 && p[0] == '/' }
