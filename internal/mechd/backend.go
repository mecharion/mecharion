package mechd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mecharion/mecharion/internal/protocol"
	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/store"
)

// Backend 把服务层接到 gRPC 协议上。
//
// 它实现 protocol.Backend。分成两个类型而不是让 Service 直接实现，
// 是因为两者的**读者不同**：Service 面向人（HTTP / CLI），Backend 面向
// mechlet。混在一起会让「这个方法是给谁用的」需要每次去查。
type Backend struct {
	S *Service
	// ConfDir 是 pki 所在的配置目录，证书续期要用它。
	ConfDir string
}

var _ protocol.Backend = (*Backend)(nil)

// Register 记录一次节点上线。
//
// **不凭空创建节点**：注册的是一台已经在册的机器。允许任意拨上来的
// agent 自己建节点，等于让「这个 Site 里有哪些机器」由网络决定。
func (b *Backend) Register(ctx context.Context, reg protocol.NodeRegistration) error {
	site, err := b.S.resolveSite(ctx, "")
	if err != nil {
		return err
	}
	node, err := b.S.Repos.Nodes().GetByName(ctx, site.ID, reg.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf(
				"node %s is not registered -- run mechctl node add %s first, "+
					"or use mechlet install --standalone to initialize", reg.Name, reg.Name)
		}
		return err
	}

	if err := b.S.Repos.Nodes().SetStatus(ctx, node.ID, store.NodeSeen); err != nil {
		return err
	}
	// capabilities 是事实的一部分：一套采集机制、一个命名空间
	// （spec §9.4.2）。requires.capability 是针对它的匹配器。
	caps := map[string]any{}
	for _, c := range reg.Capabilities {
		caps[c.Name] = map[string]any{
			"version": c.Version, "available": c.Available, "detail": c.Detail,
		}
	}
	cur, err := b.S.Repos.Status().GetFacts(ctx, node.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return b.S.Repos.Status().PutFacts(ctx, store.NodeFacts{
		NodeID: node.ID, Facts: cur.Facts, Capabilities: caps,
		CollectedAt: b.S.now(),
	})
}

// Assignment 返回某节点上应有的全部实例。
//
// **永远是全量**：调用方不需要知道上次给过什么。下发本身幂等
// （digest 相同即无操作），一个内容寻址的期望状态让整类同步协议问题消失。
func (b *Backend) Assignment(ctx context.Context, nodeName string) ([]protocol.InstanceSpec, error) {
	site, err := b.S.resolveSite(ctx, "")
	if err != nil {
		return nil, err
	}
	node, err := b.S.Repos.Nodes().GetByName(ctx, site.ID, nodeName)
	if err != nil {
		return nil, err
	}

	comps, err := b.S.Repos.Components().List(ctx, site.ID)
	if err != nil {
		return nil, err
	}

	var out []protocol.InstanceSpec
	for _, comp := range comps {
		insts, byID, err := b.S.existingInstances(ctx, comp.ID)
		if err != nil {
			return nil, err
		}
		if !hasNode(insts, node.ID) {
			continue
		}
		// 分批下发：还没轮到的实例停在旧版本上（22-multi-node §4）。
		g, err := b.S.renderGated(ctx, site, comp, inputsForExisting(insts, byID))
		if err != nil {
			// 一个组件解析不出来不该让整个节点拿不到规格——其余组件仍应下发。
			// 静默跳过是不行的，但把这一个组件的问题扩散成全节点失联更糟。
			b.S.log().Error("failed to render component, skipping this round",
				"component", comp.Name, "pack", comp.Pack.Name, "err", err)
			continue
		}
		for _, ri := range insts {
			if ri.NodeID != node.ID {
				continue
			}
			key := ri.Role + "@" + node.Name
			sp := g.specFor(ri.ID, key)
			if sp == nil {
				continue
			}
			// 期望运行态**不经过渲染管线**：它不是「这个组件长什么样」的一
			// 部分，而是「它现在该不该跑」，逐实例可不同（一台机器进维护
			// 窗口，其余照常）。渲染是纯函数，把一个随运维操作变化的值塞进
			// 去只会让它不纯。
			//
			// 盖在这里而不是渲染里也不影响 digest：ComputeDigest 显式排除
			// 了 RunState，因此盖之前盖之后算出来一样。
			sp.RunState = ri.RunState
			// 正在移除的组件盖掉逐实例的运行态：remove 是**组件级**的决定，
			// 一个先前被 `component stop` 停掉的实例同样要被拆掉。
			//
			// 开关随规格一起下发，每一轮都带——mechlet 不做判断（ADR-0006），
			// 而下发是持续行为，不是 remove 那一刻的一次性动作。
			if comp.Removing() {
				sp.RunState = spec.RunStateRemoved
				sp.Removal = &spec.Removal{
					KeepConfig: comp.Removal.KeepConfig,
					PurgeData:  comp.Removal.PurgeData,
					PurgeUser:  comp.Removal.PurgeUser,
				}
			}
			// 抑制同样不经过渲染管线，理由与运行态相同：它是运维在中心
			// 敲的一条命令，逐实例可不同，而渲染是纯函数。
			//
			// **不下发就等于没有这个功能**：调和器判抑制看的就是规格，
			// 而 ack-drift 只写了 mechd 的库。
			sups, serr := b.S.Repos.Suppressions().ListActive(ctx, comp.ID, b.S.now())
			if serr != nil {
				return nil, serr
			}
			for _, su := range sups {
				if su.InstanceID != ri.ID {
					continue
				}
				sp.Suppressions = append(sp.Suppressions, spec.Suppression{
					Resource: su.ResourceID, Reason: su.Reason,
					Until: su.ExpiresAt.UTC().Format(time.RFC3339),
				})
			}
			// **密钥取自同一次解析**：混版期间两侧的密钥表可能不同
			// （新版加了一个参数），拿错一侧会让实例缺一个值。
			out = append(out, protocol.InstanceSpec{
				Spec: sp, Secrets: g.secretsFor(ri.ID)})
		}
	}
	return out, nil
}

func hasNode(insts []store.RoleInstance, nodeID int64) bool {
	for _, ri := range insts {
		if ri.NodeID == nodeID {
			return true
		}
	}
	return false
}

// Report 接收一次状态上报。
func (b *Backend) Report(ctx context.Context, r protocol.Report) error {
	site, err := b.S.resolveSite(ctx, "")
	if err != nil {
		return err
	}
	node, err := b.S.Repos.Nodes().GetByName(ctx, site.ID, r.Node)
	if err != nil {
		return err
	}
	now := b.S.now()

	// 事实：写进 node_facts（**实时**那一份）。
	// 它不会自动改变已固化的配置取值——那要走 node facts refresh --apply。
	if len(r.Facts) > 0 {
		var facts map[string]any
		if err := json.Unmarshal(r.Facts, &facts); err != nil {
			return fmt.Errorf("parsing facts for %s: %w", r.Node, err)
		}
		cur, err := b.S.Repos.Status().GetFacts(ctx, node.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := b.S.Repos.Status().PutFacts(ctx, store.NodeFacts{
			NodeID: node.ID, Facts: facts, Capabilities: cur.Capabilities,
			CollectedAt: now,
		}); err != nil {
			return err
		}
	}

	// 孤儿：机器上还在、但下发里没有的实例。**只记不删**——卸载不可逆，
	// 而「mechd 少发了一条」与「用户真的删了这个组件」在节点侧分辨不了
	// （20-continuous-reconcile §2.4）。
	//
	// 每次上报都整体替换：孤儿消失时记录要跟着消失，否则列表只增不减，
	// 很快就没人看了。
	// 带上每个孤儿留下的目录：只记一个键，`orphans list` 就回答不了
	// 「那台机器上到底还剩什么、在哪儿」。
	//
	// 旧 mechlet 只报 Orphans 不报 OrphanRecords，那时退回按键记录——
	// 少了细节，但「有个东西在那儿」仍然看得见。
	recs := make([]store.OrphanRecord, 0, len(r.Orphans))
	if len(r.OrphanRecords) > 0 {
		for _, o := range r.OrphanRecords {
			recs = append(recs, store.OrphanRecord{Key: o.Key, Paths: o.RetainedPaths})
		}
	} else {
		for _, k := range r.Orphans {
			recs = append(recs, store.OrphanRecord{Key: k})
		}
	}
	if err := b.S.Repos.Status().SetOrphanRecords(ctx, node.ID, recs, now); err != nil {
		return err
	}

	for _, in := range r.Instances {
		comp, err := b.S.Repos.Components().GetByName(ctx, site.ID, in.Component)
		if err != nil {
			// 报了一个 mechd 不认识的组件：多半是刚被删掉。
			// 记一笔就够了——mechlet 下一次全量下发时会把它清掉。
			b.S.log().Warn("reported an unknown component", "node", r.Node, "component", in.Component)
			continue
		}
		ri, err := b.findInstance(ctx, comp.ID, in.Role, node.ID)
		if err != nil {
			continue
		}

		if err := b.S.Repos.Status().Put(ctx, store.InstanceStatus{
			InstanceID: ri.ID, Digest: in.Digest, Generation: in.Generation,
			Result: in.Result, WorkloadState: stateOf(in.Workload),
			WorkloadAction: in.WorkloadAction, WorkloadActionAt: in.WorkloadActionAt,
			RolledBackFrom: in.RolledBackFrom,
			Health:         healthOf(in.Health), Detail: detailOf(in), ReportedAt: now,
			Restarts: restartsOf(in.Workload),
			Removed:  in.Removed, RetainedPaths: in.RetainedPaths,
		}); err != nil {
			return err
		}

		// 卸载收尾：全部实例都报了「拆干净了」，记录才消失。
		//
		// 排在写状态之后：判据要把**这一条刚落库的**上报算进去，否则
		// 最后一个节点报完的那一轮永远差一票，组件要等到下一轮才收尾。
		if comp.Removing() {
			if err := b.S.finishRemovalIfDone(ctx, comp); err != nil {
				// 收尾失败不该让整条上报失败：状态已经落库了，
				// 下一轮上报会再判一次。
				b.S.log().Error("failed to finish removal, retrying next round",
					"component", comp.Name, "err", err)
			}
			// 记录可能已经没了，后面那些针对它的写入没有意义
			continue
		}

		// 漂移：先清空再写入本轮报上来的。
		// 增量更新会让一条已经消失的漂移永远留在库里。
		if err := b.S.Repos.Status().ClearDrift(ctx, ri.ID); err != nil {
			return err
		}
		// 每次上报都是推进 Rollout 的天然节拍——不需要一条后台循环
		b.S.AdvanceRollout(ctx, comp)

		// 催一下正在看这个组件的浏览器。**只是提示，不带内容**——
		// 丢了顶多晚一点（SSE 那边有定时兜底），因此这里不做任何
		// 错误处理，也绝不阻塞上报这条热路径。
		b.S.bump(comp.Name)

		for _, rs := range in.Resources {
			if rs.State != "drift" {
				continue
			}
			if err := b.S.Repos.Status().PutDrift(ctx, store.DriftReport{
				InstanceID: ri.ID, ResourceID: rs.ID,
				Changes: []any{rs.Detail}, SeenAt: now,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Backend) findInstance(
	ctx context.Context, componentID int64, role string, nodeID int64,
) (store.RoleInstance, error) {
	insts, err := b.S.Repos.Instances().ListByRole(ctx, componentID, role)
	if err != nil {
		return store.RoleInstance{}, err
	}
	for _, ri := range insts {
		if ri.NodeID == nodeID {
			return ri, nil
		}
	}
	return store.RoleInstance{}, store.ErrNotFound
}

func stateOf(w *protocol.WorkloadStatus) string {
	if w == nil {
		return ""
	}
	return w.State
}

// restartsOf 取工作负载的累计重启次数。
//
// 滚动升级的健康门禁靠它识别崩溃循环（22-multi-node §2.5 第 3 条）：
// 稳定窗口是有限的，一个周期比它长的崩溃能干干净净地溜过去，而这个数
// 涨了就是崩过，与观察时机无关。
func restartsOf(w *protocol.WorkloadStatus) int {
	if w == nil {
		return 0
	}
	return w.Restarts
}

func healthOf(h *protocol.HealthStatus) string {
	if h == nil {
		return ""
	}
	return h.State
}

func detailOf(in protocol.InstanceStatus) map[string]any {
	d := map[string]any{}
	if in.Message != "" {
		d["message"] = in.Message
	}
	if len(in.Resources) > 0 {
		res := make([]any, 0, len(in.Resources))
		for _, r := range in.Resources {
			res = append(res, map[string]any{
				"id": r.ID, "type": r.Type, "state": r.State, "detail": r.Detail,
			})
		}
		d["resources"] = res
	}
	return d
}

// Events 接收断连期间缓冲的事件。
func (b *Backend) Events(ctx context.Context, nodeName string, events []protocol.Event) (int, error) {
	site, err := b.S.resolveSite(ctx, "")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range events {
		at := b.S.now()
		if t, err := store.ParseTime(e.At); err == nil {
			at = t
		}
		if err := b.S.Repos.Events().Append(ctx, store.Event{
			At: at, SiteID: site.ID, Kind: e.Kind,
			Subject: strings.TrimSuffix(e.Component+"/"+e.Role, "/"),
			Payload: map[string]any{
				"node": nodeName, "level": e.Level, "message": e.Message,
			},
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// OpenBlob 按 sha256 打开一个载荷。
func (b *Backend) OpenBlob(_ context.Context, sum string) (io.ReadSeekCloser, int64, error) {
	// 内容寻址：目录按前两位分片，避免一个目录里堆几万个文件
	p := filepath.Join(b.S.BlobDir, "sha256", sum[:2], sum)
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

// Allowed 实现 protocol.Authorizer：这个节点现在还能不能连。
//
// **被吊销的证书握手仍会成功**——那是应用层吊销的代价（ADR-0034）。
// 因此这道门在业务层，且协议层会在每个 RPC 上问一次。
//
// 两种「不能连」分开报：不在册（可能是被 remove 了，或者从没加入过）
// 与已吊销。现场第一个要知道的正是「是哪一种」。
func (b *Backend) Allowed(ctx context.Context, name string) error {
	site, err := b.S.resolveSite(ctx, "")
	if err != nil {
		return err
	}
	node, err := b.S.Repos.Nodes().GetByName(ctx, site.ID, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf(
				"node %s is not registered -- it may have been removed with mechctl node remove.\n"+
					"  to let this machine rejoin, use a new join token", name)
		}
		return err
	}
	if node.Revoked() {
		return fmt.Errorf(
			"node %s's certificate was revoked on %s.\n"+
				"  once you've confirmed this machine is trusted, restore it with mechctl node unrevoke %s,\n"+
				"  or have it rejoin with a new token",
			name, store.FormatTime(*node.RevokedAt), name)
	}
	return nil
}

// RenewCert 实现 protocol.CertRenewer。
func (b *Backend) RenewCert(
	ctx context.Context, node string, csr []byte,
) (cert, ca []byte, err error) {
	return b.S.RenewCert(ctx, node, csr, b.ConfDir)
}

// Cordoned 实现 protocol.Cordoner。
func (b *Backend) Cordoned(ctx context.Context, name string) (bool, error) {
	site, err := b.S.resolveSite(ctx, "")
	if err != nil {
		return false, err
	}
	node, err := b.S.Repos.Nodes().GetByName(ctx, site.ID, name)
	if err != nil {
		return false, err
	}
	return node.Cordoned(), nil
}
