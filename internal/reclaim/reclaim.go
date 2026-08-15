// Package reclaim 回收已经没有任何 generation 引用的镜像与载荷。
//
// 它是**节点级**的：一个载荷可以被多个实例、多个组件共用（内容寻址的
// 直接后果），镜像更是如此，因此「谁还在引用它」这个问题单个实例的
// 调和器回答不了（22-upgrade §2.5 ③）。
//
// 单独成包而不是留在 agent 里，是因为它有**两个使用者**：常驻 agent 的
// 每轮调和，与一次性的 `mechlet apply`。后者同样会 prune 掉 generation，
// 若不回收，一台只用 apply 的机器上垃圾清单只增不减。
package reclaim

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/state"
)

// Options 是一次回收所需的一切。
type Options struct {
	State    *state.Store
	Runtimes *runtime.Registry
	// BlobDir 是内容寻址的载荷根目录。为空时只回收镜像。
	BlobDir string
	Log     *slog.Logger
}

// Result 是一次回收的结果，供日志与测试断言。
type Result struct {
	Images []string
	Blobs  []string
}

// Run 处理一遍回收清单。
//
// 候选来自节点级清单，而不是「上一次 prune 返回了什么」：进程在 prune 与
// 删除之间重启一次，后者就永远丢了（22-upgrade §2.5 ②）。
//
// 只删清单里的东西，因此别人拉的镜像永远不会被碰到：它从没被我们物化过，
// 也就从没进过任何一代台账（§2.5 ④）。
func Run(ctx context.Context, o Options) Result {
	var done Result
	if o.State == nil {
		return done
	}
	log := o.Log
	if log == nil {
		log = slog.Default()
	}

	g, err := o.State.LoadGarbage()
	if err != nil {
		log.Warn("failed to read garbage manifest, skipping this round", "err", err)
		return done
	}
	if len(g.Images) == 0 && len(g.Blobs) == 0 {
		return done
	}

	liveImages, liveBlobs, err := o.State.LiveRefs()
	if err != nil {
		// 引用集算不全时**什么都不删**：漏删一次只是没省下空间，
		// 误删一次要靠重新分发几百 MB 来补。
		log.Warn("failed to aggregate references, skipping this round", "err", err)
		return done
	}

	var handled Result
	for _, it := range g.Images {
		if liveImages[it.ID] {
			// 还有别的 generation 在用——从清单里划掉，它不是垃圾
			handled.Images = append(handled.Images, it.ID)
			continue
		}
		if err := removeImage(ctx, o.Runtimes, it.ID); err != nil {
			log.Warn("failed to delete image, leaving it for next round", "image", it.ID, "err", err)
			continue
		}
		log.Info("image reclaimed", "image", it.ID)
		handled.Images = append(handled.Images, it.ID)
		done.Images = append(done.Images, it.ID)
	}
	for _, it := range g.Blobs {
		if liveBlobs[it.ID] {
			handled.Blobs = append(handled.Blobs, it.ID)
			continue
		}
		if err := removeBlob(o.BlobDir, it.ID); err != nil {
			log.Warn("failed to delete blob, leaving it for next round", "blob", shortSum(it.ID), "err", err)
			continue
		}
		log.Info("blob reclaimed", "blob", shortSum(it.ID))
		handled.Blobs = append(handled.Blobs, it.ID)
		done.Blobs = append(done.Blobs, it.ID)
	}

	if len(handled.Images) == 0 && len(handled.Blobs) == 0 {
		return done
	}
	g.Drop(handled.Images, handled.Blobs)
	if err := o.State.SaveGarbage(g); err != nil {
		// 已经删掉的东西划不掉，下一轮会再删一次——两个删除都容忍
		// 「已经没了」，因此这只是多一次无用功，不会出错。
		log.Warn("failed to write garbage manifest", "err", err)
	}
	return done
}

// removeImage 逐个问过管镜像的 Runtime。
//
// 没有任何 Runtime 管镜像时**当作已删**：一台只跑 systemd 的机器上不该有
// 镜像清单，硬留着它只会让这条记录永远删不掉。
func removeImage(ctx context.Context, reg *runtime.Registry, image string) error {
	if reg == nil {
		return nil
	}
	var lastErr error
	for _, rc := range reg.Reclaimers() {
		if err := rc.RemoveImage(ctx, image); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// removeBlob 删掉一个内容寻址的载荷文件。
//
// 布局与取载荷时一致：<blobDir>/sha256/<前两位>/<完整摘要>。
func removeBlob(root, sum string) error {
	if root == "" || len(sum) < 2 {
		return nil
	}
	path := filepath.Join(root, "sha256", sum[:2], sum)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func shortSum(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
