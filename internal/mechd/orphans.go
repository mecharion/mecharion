package mechd

import (
	"context"
	"fmt"
	"time"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/store"
)

// OrphanView 是 `mechctl orphans list` 的一行。
//
// **它要回答的是「这堆东西还要不要」**，因此节点、路径、来历缺一不可：
// 一条只有实例键的记录，运维既找不到它在哪，也说不清是谁留下的。
type OrphanEntry struct {
	Node     string `json:"node"`
	Instance string `json:"instance"`
	// Paths 是这个孤儿在盘上留下的目录。
	//
	// 空表示它不是 remove 留下的残留，而是「下发里没有它了」——那时
	// 机器上留着的是一整个还装着的实例（进程可能还在跑）。
	Paths []string `json:"paths,omitempty"`
	// FirstSeen 是它第一次被报上来的时刻；`since` 列显示的就是它。
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
	// PurgeRequested 表示已经有人要求清掉它，正在等节点执行。
	PurgeRequested bool `json:"purgeRequested,omitempty"`
}

// Installed 报告这个孤儿是不是「一整个还装着的实例」。
//
// 两类孤儿的处置完全不同：
//
//	有 Paths   remove 留下的数据残留，purge 就是删几个目录
//	无 Paths   下发里没有它了，但机器上**还装着、可能还在跑**
//
// 后者不能靠 purge 解决——那要先把它重新纳管，或者去那台机器上手工处理。
// 混为一谈会让人以为 purge 能停掉一个还在跑的服务。
func (e OrphanEntry) Installed() bool { return len(e.Paths) == 0 }

// ListOrphans 列出全站的孤儿（10-cli §4.7）。
func (s *Service) ListOrphans(ctx context.Context, siteName, node string) ([]OrphanEntry, error) {
	site, err := s.resolveSite(ctx, siteName)
	if err != nil {
		return nil, err
	}
	nodes, err := s.Repos.Nodes().List(ctx, site.ID)
	if err != nil {
		return nil, err
	}

	var out []OrphanEntry
	for _, n := range nodes {
		if node != "" && n.Name != node {
			continue
		}
		orphans, err := s.Repos.Status().ListOrphans(ctx, n.ID)
		if err != nil {
			return nil, err
		}
		for _, o := range orphans {
			out = append(out, OrphanEntry{
				Node: n.Name, Instance: o.InstanceKey, Paths: o.Paths,
				FirstSeen: o.FirstSeen, LastSeen: o.LastSeen,
				PurgeRequested: o.PurgeRequested,
			})
		}
	}
	return out, nil
}

// PurgeOrphanRequest 是一次 `orphans purge`。
type PurgeOrphanRequest struct {
	Site string
	// Node 与 Instance 一起定位一条孤儿记录。
	//
	// **必须同时给。** 同一个实例键会出现在多台机器上（一个三副本组件
	// 被 remove 之后留下三份数据），只给键会一次清掉三台——而人往往
	// 只想清其中一台。
	Node     string
	Instance string
	Actor    string
}

// PurgeOrphan 记下「这个孤儿该被清掉」，由下发带给节点。
//
// **它不当场删任何东西。** 中心不执行部署动作（ADR-0002），删目录是
// 节点的事；而且那台机器很可能此刻联系不上——一个「只能在线时执行」的
// 清理动作，会把最需要清理的那台机器排除在外。
//
// 因此这里只落一个意图。节点下次连上来就会收到它，清完之后孤儿从上报里
// 消失，这条意图跟着失效（SetOrphanRecords 每轮整体替换）。
func (s *Service) PurgeOrphan(ctx context.Context, req PurgeOrphanRequest) error {
	site, err := s.resolveSite(ctx, req.Site)
	if err != nil {
		return err
	}
	n, err := s.Repos.Nodes().GetByName(ctx, site.ID, req.Node)
	if err != nil {
		return faults.Permanentf("", "node %s: %w", req.Node, err)
	}

	// 先看清楚要清的是什么——一个「还装着的实例」不能靠 purge 解决
	orphans, err := s.Repos.Status().ListOrphans(ctx, n.ID)
	if err != nil {
		return err
	}
	var found *store.NodeOrphan
	for i := range orphans {
		if orphans[i].InstanceKey == req.Instance {
			found = &orphans[i]
			break
		}
	}
	if found == nil {
		return faults.Permanentf("", "%s has no orphan named %s\n"+
			"  use mechctl orphans list to see what's currently there", req.Node, req.Instance)
	}
	if len(found.Paths) == 0 {
		return faults.Permanentf("",
			"%s@%s is not leftover data from remove, it is a **still-installed instance**\n"+
				"  it is gone from the desired state, but it's still installed on the machine and may still be running.\n"+
				"  purge only deletes directories, it can't stop it. Bring it back under management first, "+
				"or handle it manually on that machine",
			req.Instance, req.Node)
	}

	ok, err := s.Repos.Status().RequestPurge(ctx, n.ID, req.Instance, s.now())
	if err != nil {
		return err
	}
	if !ok {
		return faults.Permanentf("", "%s has no orphan named %s", req.Node, req.Instance)
	}

	s.audit(ctx, req.Actor, "orphans-purge", req.Instance, nil,
		fmt.Sprintf("node=%s paths=%d", req.Node, len(found.Paths)))
	// 立刻推一次：不推的话要等下一个调和周期，而运维刚敲完清理就去看
	// 目录还在，会以为命令没生效
	if s.Notify != nil {
		s.Notify.Notify(req.Node)
	}
	return nil
}

// PurgeOrphans 实现 protocol.Purger：返回该节点上待清理的孤儿键。
func (b *Backend) PurgeOrphans(ctx context.Context, node string) ([]string, error) {
	site, err := b.S.resolveSite(ctx, "")
	if err != nil {
		return nil, err
	}
	n, err := b.S.Repos.Nodes().GetByName(ctx, site.ID, node)
	if err != nil {
		return nil, err
	}
	return b.S.Repos.Status().ListPurges(ctx, n.ID)
}
