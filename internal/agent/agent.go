// Package agent 是 mechlet 连上 mechd 之后的那一半。
//
// 它把两条已有的路径接起来：
//
//	protocol.Client  拿到期望状态（没有动词，只有「应该有什么」）
//	reconcile        把期望状态变成机器上的实际状态
//
// **这两条路径都不是为它新写的**：M2 的 `mechlet apply -f` 走的是同一个
// reconciler，第 6 步的协议库两侧也已经测过。agent 只负责编排——
// 收到规格 → 取载荷 → 调和 → 上报 digest。
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/mecharion/mecharion/internal/protocol"
	"github.com/mecharion/mecharion/internal/reconcile"
	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/state"
)

// Options 配置 agent。
type Options struct {
	Client     *protocol.Client
	Reconciler *reconcile.Reconciler
	State      *state.Store
	Node       string
	// BlobDir 是内容寻址的载荷目录。
	BlobDir string
	Log     *slog.Logger
	// Facts 在每次上报时被调用，返回本机事实的 JSON。
	Facts func() ([]byte, error)

	// Desired 是期望状态的本机副本。为 nil 时**不做周期调和**——
	// 那种模式只用于测试与一次性的 apply。
	Desired *DesiredStore
	// ReconcileInterval 覆盖 mechd 下发的调和周期，供测试缩短等待。
	ReconcileInterval time.Duration

	// Certs 是本机证书的位置。为空表示单机形态（unix socket，无证书）。
	Certs CertPaths
	// RenewBefore 覆盖「剩余多久就续期」的阈值，供测试触发轮换。
	RenewBefore time.Duration
}

// Agent 是 mechlet 的常驻协调循环。
type Agent struct {
	opts Options

	mu sync.Mutex
	// last 是每个实例最近一次调和的结果，键为 "<component>/<role>"。
	last map[string]*reconcile.Report
	// desired 记录本次全量里应当存在的实例，用于识别「已被移除的」。
	desired map[string]bool
	// busy 标记正在调和的实例。
	//
	// 推送与定时器会撞在一起。**后来者跳过而不是排队**：排队会在一次慢调和
	// 之后堆出一串已经过期的任务，然后连着跑好几轮——而它们的期望状态
	// 完全一样，除了让机器忙起来什么也做不了。
	busy map[string]bool
	// orphans 是最近一次算出的孤儿实例键。
	orphans []string
	// orphanRecords 是同一批孤儿，外加它们在盘上留下了什么。
	orphanRecords []protocol.OrphanRecord
	// blocked 记录哪些实例停在「上次失败、已停止自动重试」的状态。
	//
	// 只为一件事存在：**区分进入与停留**。进入要喊一声，停留要安静。
	blocked map[string]bool
	// certExpired 记录「证书已过期」这句话喊过没有——它不会自己好转。
	certExpired bool
	// refused 记录「控制面拒绝了本节点」喊过没有。同一条理由。
	refused bool
	// cordoned 是控制面下发的暂停状态；cordonAnnounced 记它喊过没有。
	cordoned        bool
	cordonAnnounced bool
}

// New 构造 agent。
func New(o Options) *Agent {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Agent{
		opts:    o,
		last:    map[string]*reconcile.Report{},
		desired: map[string]bool{},
		busy:    map[string]bool{},
		blocked: map[string]bool{},
	}
}

// acquire 尝试占住一个实例；已被占住时返回 false。
func (a *Agent) acquire(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy[key] {
		return false
	}
	a.busy[key] = true
	return true
}

func (a *Agent) release(key string) {
	a.mu.Lock()
	delete(a.busy, key)
	a.mu.Unlock()
}

// Run 跑到 ctx 结束。
//
// 两条循环彼此独立：下发流断了不影响上报，反之亦然。这正是「不用双向流」
// 换来的东西（17-protocol §1）。
func (a *Agent) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.reportLoop(ctx)
	}()

	// ad-hoc 命令通道（ADR-0038）。**与下发那条分开跑**：命令流断了
	// 不影响下发，反之亦然。
	//
	// 它只在有 Desired 时启动——restart 要从本机的期望状态里找规格，
	// 而那种模式（一次性 apply、测试）本来就没有常驻的期望状态。
	if a.opts.Desired != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = a.opts.Client.RunTasks(ctx, a)
		}()
	}

	if a.opts.Desired != nil {
		// **重启后立刻调和一次，不等第一个 tick。**
		//
		// mechlet 重启的常见原因是升级或机器重启，而那两种情况下机器上
		// 多半真的有事要做。让它先干净一轮，比先睡 60 秒有用得多。
		a.reconcileAll(ctx, "startup")

		wg.Add(1)
		go func() {
			defer wg.Done()
			a.reconcileLoop(ctx)
		}()
	}

	err := a.opts.Client.Run(ctx, a)
	wg.Wait()
	return err
}

// reconcileLoop 周期性地按本机保存的期望状态调和一遍。
//
// **这是常驻 Agent 相对 Ansible 的核心价值**（ADR-0001）：有人手工改了
// 配置、手工停了服务，只有周期性重读才发现得了。它与推送是两条独立的
// 触发源，共用同一段调和逻辑。
func (a *Agent) reconcileLoop(ctx context.Context) {
	interval := a.reconcileInterval()
	t := time.NewTicker(interval)
	defer t.Stop()

	a.opts.Log.Info("periodic reconcile started", "interval", interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.reconcileAll(ctx, "periodic")
		}
	}
}

func (a *Agent) reconcileInterval() time.Duration {
	if a.opts.ReconcileInterval > 0 {
		return a.opts.ReconcileInterval
	}
	d := time.Duration(a.opts.Client.Intervals().ReconcileSeconds) * time.Second
	if d <= 0 {
		d = 60 * time.Second
	}
	return d
}

// reconcileAll 按本机保存的期望状态调和全部实例。
func (a *Agent) reconcileAll(ctx context.Context, why string) {
	specs, err := a.opts.Desired.Load()
	if err != nil {
		// 读不出期望状态时**不静默**：这种情况下 mechlet 看起来在跑，
		// 实际什么都没在维持
		a.opts.Log.Error("failed to read local desired state, skipping this round", "trigger", why, "err", err)
		return
	}
	// 孤儿每轮重算：断连期间被移除的组件，重连时才报得出来
	desired := make(map[string]bool, len(specs))
	for _, in := range specs {
		desired[in.Spec.InstanceKey()] = true
	}
	a.refreshOrphans(desired)

	// **被 cordon 就整轮不动机器。**
	//
	// 上报照常、孤儿照常算、证书照常续——停的只有「把期望状态往机器上落」
	// 这一件事。那正是 cordon 的语义：「我要手工调试这台机，别让 mechlet
	// 把我的改动改回去」（10-cli §4.2）。
	//
	// 回收也一起停：它会删东西，而 cordon 说的是别动。
	if a.isCordoned() {
		a.noteCordoned(why)
		return
	}

	// 没有实例也要往下走：**一台被清空的机器正是最需要回收的那一台**，
	// 而在这里 return 会让它的镜像与载荷永远留在盘上。
	for _, in := range specs {
		if ctx.Err() != nil {
			return
		}
		key := in.Spec.InstanceKey()
		if !a.acquire(key) {
			// 推送正在处理这个实例，或上一轮还没跑完
			a.opts.Log.Debug("instance busy, skipping this round", "instance", key, "trigger", why)
			continue
		}
		err := a.reconcileOne(ctx, in)
		a.release(key)
		if err != nil {
			if a.isBlocked(key) {
				// 稳定地停在旧版等人处理，不是一次新的失败。
				// 每轮一条 ERROR 会让真正的新问题看不见。
				a.opts.Log.Debug("still on last known-good version", "instance", key, "trigger", why)
			} else {
				// **一个实例失败不该拖垮其余的**，下一轮照常重试
				a.opts.Log.Error("reconcile failed", "instance", key, "trigger", why, "err", err)
			}
		}
	}

	// 回收排在最后：这一轮刚 prune 掉的那几代，它们的引用已经进了清单。
	a.collectGarbage(ctx)

	// 证书续期搭在调和周期上，不另起一条定时器。
	//
	// 证书有效期一年、阈值 30 天，用什么周期检查都绰绰有余；而多一条
	// 定时器就多一个需要自己处理重启与并发的东西（13-mechd §7 的同一条
	// 理由：不引入必须运维的新组件）。
	a.renewCerts(ctx)
}

// Apply 收到一次全量下发。实现 protocol.Handler。
//
// 参数是「这台机器上应该有什么」——**没有动词**。怎么达成是 mechlet
// 自己的事，这正是它在断连时仍能工作的原因。
func (a *Agent) Apply(ctx context.Context, asg protocol.Assignment) error {
	specs, fullSync := asg.Specs, asg.FullSync
	a.opts.Log.Info("dispatch received",
		"instances", len(specs), "full_sync", fullSync, "cordoned", asg.Cordoned)
	a.setCordoned(asg.Cordoned)

	// **孤儿清理排在最前面。** 一个正在被清的孤儿与一个刚被重新部署的
	// 实例可能共用同一个键——先清后装，最终状态才是「装着的那个」。
	// 反过来（先装后清）会把刚装好的东西删掉。
	//
	// 被 cordon 时不清：cordon 说的是「别动这台机器」，而清理会删东西。
	if !asg.Cordoned {
		a.purgeOrphans(ctx, asg.PurgeOrphans)
	}

	seen := map[string]bool{}
	var failed []string

	for _, in := range specs {
		key := in.Spec.InstanceKey()
		seen[key] = true

		// **先落盘再调和。** 反过来的话，一次「调和成功但进程随即被杀」
		// 会让机器上已经是新状态、而本机记的期望还是旧的——下一轮周期调和
		// 会把它改回去，表现为「刚部署完就被回滚了」。
		if a.opts.Desired != nil {
			if err := a.opts.Desired.Save(in); err != nil {
				a.opts.Log.Error("failed to save desired state", "instance", key, "err", err)
				failed = append(failed, key)
				continue
			}
		}

		if asg.Cordoned {
			// **期望状态照常落盘，只是不往机器上落。**
			//
			// 落盘是必须的：uncordon 之后要能立刻收敛，而那时 mechd 未必
			// 会再推一次。不落的话，「恢复」就变成了「等下一次变更」。
			continue
		}
		if !a.acquire(key) {
			// 周期调和正在处理它。**这不是失败**：那一轮用的是刚落盘的
			// 同一份期望状态，结果一样
			a.opts.Log.Debug("instance busy, skipping reconcile for this dispatch", "instance", key)
			continue
		}
		err := a.reconcileOne(ctx, in)
		a.release(key)
		if err != nil {
			// **一个实例失败不该拖垮其余的**：它们彼此独立，
			// 而「因为 A 装不上所以 B 也没装」是最难解释的一类现场
			a.opts.Log.Error("reconcile failed", "instance", key, "err", err)
			failed = append(failed, key)
		}
	}

	a.mu.Lock()
	a.desired = seen
	a.mu.Unlock()

	if fullSync {
		a.refreshOrphans(seen)
		// **只在全量之后清理。** 增量下发只带一部分实例，按它清理会把
		// 其余的全删掉——那是一次静默的、看起来像「组件自己消失了」的事故。
		if a.opts.Desired != nil {
			if err := a.opts.Desired.Keep(seen); err != nil {
				a.opts.Log.Warn("failed to prune local desired state", "err", err)
			}
		}
	}
	if len(failed) > 0 {
		sort.Strings(failed)
		return fmt.Errorf("%d 个实例调和失败: %v", len(failed), failed)
	}
	return nil
}

// reconcileOne 把一个实例调和到期望状态。
//
// 推送与周期调和共用它——**两条触发源，一段逻辑**。分开写的话，
// 「推送时会做而周期时不做」这类差异会悄悄长出来。
func (a *Agent) reconcileOne(ctx context.Context, in protocol.InstanceSpec) error {
	rep, err := a.reconcileInner(ctx, in)
	if a.opts.Desired == nil {
		return err
	}

	if err == nil {
		// **成功之后**才把它记成回滚点。写在前面的话，一个失败的版本会立刻
		// 变成回滚目标——回滚到一个从没跑起来过的版本毫无意义。
		if merr := a.opts.Desired.MarkApplied(in); merr != nil {
			// 记不下回滚点不该让这次成功的调和变成失败：服务已经好了。
			// 但要说出来——下次升级失败时会没有落脚点。
			a.opts.Log.Warn("failed to record rollback point, the next failed upgrade will have nowhere to fall back to",
				"instance", in.Spec.InstanceKey(), "err", merr)
		}
		return nil
	}

	a.noteBlocked(in, rep, err)

	if !needsFallback(rep) {
		return err
	}
	return a.fallback(ctx, in, rep, err)
}

// noteBlocked 只在**进入**「已停止自动重试」时喊一声。
//
// 这个状态会一直保持到有人来处理。每个调和周期喊一次，等于把一条稳定状态
// 当成反复发生的事件播报——真正发生变化的那一刻就淹在里面了。
// 「现在停在这里」由报告与 `component status` 长期可见，不靠日志刷屏。
func (a *Agent) noteBlocked(in protocol.InstanceSpec, rep *reconcile.Report, cause error) {
	key := in.Spec.InstanceKey()
	nowBlocked := rep != nil && rep.Blocked

	a.mu.Lock()
	was := a.blocked[key]
	if nowBlocked {
		a.blocked[key] = true
	} else {
		delete(a.blocked, key)
	}
	a.mu.Unlock()

	if nowBlocked && !was {
		a.opts.Log.Warn("this version failed to deploy last time, automatic retries stopped, waiting for manual handling",
			"instance", key, "digest", in.Spec.Digest, "reason", cause)
	}
}

func (a *Agent) isBlocked(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.blocked[key]
}

// needsFallback 判断这次失败要不要回落到最后一次成功应用的规格。
//
// **分界线是「软链切了没有」**（21-upgrade-and-rollback §2.2）：
//
//	资源 Apply 失败 / preUpgrade 失败 → 旧版还在跑，中止即可
//	Start 失败 / 健康检查不过        → 旧版已经被停掉，必须把世界推回去
//
// Blocked 也要回落：那意味着这个版本上次就失败过，本轮连试都没试——
// 机器上跑的可能是上次那半截状态，得确保它回到已知可用的那一版。
// 不这样做的话，一次失败的回滚会让服务永远躺着，因为后续每一轮都被锁住。
func needsFallback(rep *reconcile.Report) bool {
	return rep != nil && (rep.Switched || rep.Blocked)
}

// fallback 回落到最后一次成功应用的规格。
//
// **上报的是回落之后的状态**：digest 必须是旧的那个，因为 digest 是收敛
// 判据——报新的会让 mechd 以为升级成功了，而机器上跑的是旧版。
//
// 它有**两种**调用情形，动作相同而含义完全不同：
//
//	回滚   新版刚刚失败，软链已经切过去了 → 把世界推回旧版，这是一次事件
//	维持   这个版本上次就失败过、本轮被锁住 → 机器早就在旧版上，这是稳定状态
//
// 分不开的话，一台稳定停在旧版等人处理的机器会**每个周期都报一次回滚**——
// 而那正好让真正发生的那一次回滚淹没在噪声里（这与「状态 vs 事件」是同
// 一条：事件说一次，状态可以反复确认，但不能把状态说成事件）。
func (a *Agent) fallback(
	ctx context.Context, in protocol.InstanceSpec, failed *reconcile.Report, cause error,
) error {
	key := in.Spec.InstanceKey()
	prev, perr := a.opts.Desired.Applied(key)
	if perr != nil {
		a.opts.Log.Error("failed to read rollback point, cannot fall back", "instance", key, "err", perr)
		return cause
	}
	if prev == nil || prev.Spec.Digest == in.Spec.Digest {
		// 没有可落脚的旧版本（首装失败），或者失败的就是当前这一版。
		// 首装失败时机器上本来就没有服务，没什么可救的。
		return cause
	}

	// **判据是「软链切了没有」**：被锁住的那一轮连试都没试，机器一步没动。
	maintaining := failed != nil && failed.Blocked && !failed.Switched
	if maintaining {
		a.opts.Log.Debug("this version is locked, maintaining the last successful spec",
			"instance", key, "locked_digest", in.Spec.Digest, "maintaining_at", prev.Spec.Digest)
	} else {
		a.opts.Log.Warn("new version failed to become ready, rolling back to the last successful version",
			"instance", key, "failed_digest", in.Spec.Digest, "fallback_digest", prev.Spec.Digest,
			"reason", cause)
	}

	rep, rerr := a.reconcileInner(ctx, *prev)
	if rerr != nil {
		// **回滚也失败了**：这是最坏的情形，必须说得清清楚楚——
		// 机器现在处于一个谁也没验证过的状态，要人来看。
		a.opts.Log.Error("rollback failed, machine is in an unverified state, manual intervention required",
			"instance", key, "rollback_target", prev.Spec.Digest, "err", rerr)
		return fmt.Errorf("%w（回滚到 %s 也失败了: %v）", cause, prev.Spec.Digest, rerr)
	}

	// 报告描述回落之后的状态，但要带上「为什么不是期望的那个」。
	//
	// **RolledBackFrom 两种情形都要带**：它回答的是「为什么这台机器不在
	// 期望的版本上」，那是一个持续为真的事实，不是一次性的事件。mechd 的
	// Rollout 判定读的正是它——只在回滚那一瞬间带上，重启一次就丢了。
	if rep != nil {
		rep.RolledBackFrom = in.Spec.Digest
		// 消息要**先说结论再说原因**：现场的人第一眼要知道「现在跑的是
		// 哪一版」，而不是先读一段探针失败的堆栈。
		verb := "已回滚到"
		if maintaining {
			verb = "已停在"
		}
		rep.Error = fmt.Sprintf("%s %s：%v", verb, prev.Spec.Pack.Version, cause)
		a.mu.Lock()
		a.last[key] = rep
		a.mu.Unlock()
	}
	if maintaining {
		a.opts.Log.Debug("maintaining last known-good version, waiting for manual handling",
			"instance", key, "version", prev.Spec.Pack.Version)
	} else {
		a.opts.Log.Warn("rolled back, service is running on the last known-good version",
			"instance", key, "version", prev.Spec.Pack.Version)
	}
	return cause
}

func (a *Agent) reconcileInner(
	ctx context.Context, in protocol.InstanceSpec,
) (*reconcile.Report, error) {
	// 载荷先到位：资源引擎解压时要它已经在内容寻址目录里
	if err := a.fetchBlobs(ctx, in.Spec); err != nil {
		return nil, err
	}

	// **写盘的最后一刻**才把哨兵串换成明文（16-secrets §4）。
	// 换出来的规格只在内存里，不落归档。
	resolved, err := spec.ResolveSecrets(in.Spec, in.Secrets)
	if err != nil {
		return nil, err
	}

	rep, rerr := a.opts.Reconciler.Reconcile(ctx, resolved)
	a.mu.Lock()
	a.last[in.Spec.InstanceKey()] = rep
	a.mu.Unlock()
	return rep, rerr
}

// fetchBlobs 把规格引用到、本地又没有的载荷取下来。
//
// 内容寻址让这件事幂等：已有且校验通过就直接返回，不重传。
func (a *Agent) fetchBlobs(ctx context.Context, s *spec.ResolvedSpec) error {
	for _, b := range s.Blobs {
		if b.SHA256 == "" {
			continue
		}
		dir := blobDirFor(a.opts.BlobDir, b.SHA256)
		if _, err := a.opts.Client.FetchBlob(ctx, b.SHA256, dir); err != nil {
			return fmt.Errorf("取载荷 %s (%s): %w", b.Name, short(b.SHA256), err)
		}
	}
	return nil
}

// blobDirFor 返回某个摘要的落点目录。
//
// 按前两位分片：一个目录里堆几万个文件会让 ls 与 readdir 都变慢。
func blobDirFor(root, sum string) string {
	return root + "/sha256/" + sum[:2]
}

// refreshOrphans 重算「机器上还在、但期望状态里没有」的实例。
//
// **只记不删。** 卸载不可逆，而「mechd 少发了一条」与「用户真的删了这个
// 组件」在节点侧分辨不出来（20-continuous-reconcile §2.4）。真正的移除
// 走 `mechctl component remove`。
//
// 每轮都重算，而不是只在全量下发时算一次：一个断连了三天的节点，
// 期间被移除的组件仍然应当在它重新连上时被报出来，而不是等下一次全量。
func (a *Agent) refreshOrphans(desired map[string]bool) {
	keys, err := a.opts.State.ListInstances()
	if err != nil {
		a.opts.Log.Warn("failed to list local instances", "err", err)
		return
	}
	var orphans []string
	var records []protocol.OrphanRecord
	for _, k := range keys {
		if desired[k] {
			continue
		}
		orphans = append(orphans, k)
		// **把收据一起报上去。** 只报一个键，中心回答不了唯一要紧的问题：
		// 那台机器上到底还剩什么、在哪儿、是谁哪一版留下的。
		//
		// 读不出来的实例仍然算孤儿，只是少了细节——「有个东西在那儿」比
		// 「什么都不知道」有用得多。
		rec := protocol.OrphanRecord{Key: k}
		if in, err := a.opts.State.LoadInstance(k); err == nil && in != nil {
			if rm := in.Removed; rm != nil {
				rec.RetainedPaths = rm.RetainedPaths
				rec.Pack, rec.Version = rm.Pack, rm.Version
				rec.RemovedAt = rm.At.UTC().Format(time.RFC3339)
			}
		}
		records = append(records, rec)
	}
	sort.Strings(orphans)

	a.mu.Lock()
	prev := a.orphans
	a.orphans = orphans
	a.orphanRecords = records
	a.mu.Unlock()

	// 只在集合变化时写日志：每 60 秒重复同一条 warn 会把日志淹掉，
	// 而运维会开始无视它
	if !sameKeys(prev, orphans) {
		for _, k := range orphans {
			a.opts.Log.Warn("this machine has an instance not in the desired state",
				"instance", k,
				"hint", "it may have been removed; uninstalling requires an explicit component remove")
		}
	}
}

func sameKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ── 上报 ────────────────────────────────────────────────────────────────

// reportLoop 周期性上报观测状态。
//
// 与调和周期**独立可配**：上报是高频小包（默认 15s），调和是低频重活
// （默认 60s）。绑在一起会让二者互相牵制（11-resource-engine §3.2）。
func (a *Agent) reportLoop(ctx context.Context) {
	interval := time.Duration(a.opts.Client.Intervals().ReportSeconds) * time.Second
	if interval <= 0 {
		interval = 15 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			err := a.report(ctx)
			if err == nil {
				// 复位，让「恢复之后再次被拒」仍然喊得出来
				a.clearRefused()
				continue
			}
			if a.noteRefused(err) {
				continue
			}
			// 上报失败不是错误状态：mechd 可能正在重启，
			// 而 mechlet 本就该继续自治
			a.opts.Log.Debug("report failed, retrying later", "err", err)
		}
	}
}

func (a *Agent) report(ctx context.Context) error {
	if a.opts.Client.Session() == "" {
		return nil // 还没握手
	}

	a.mu.Lock()
	orphans := append([]string(nil), a.orphans...)
	records := append([]protocol.OrphanRecord(nil), a.orphanRecords...)
	a.mu.Unlock()

	r := protocol.Report{
		Node: a.opts.Node, Orphans: orphans, OrphanRecords: records,
	}
	if a.opts.Facts != nil {
		if b, err := a.opts.Facts(); err == nil && len(b) > 0 {
			r.Facts = b
			r.FactsAt = time.Now().UTC().Format(time.RFC3339)
		}
	}

	r.Instances = a.LocalStatus()

	resync, err := a.opts.Client.Report(ctx, r)
	if err != nil {
		return err
	}
	if resync {
		// mechd 说会话对不上了。**不在这里重连**——那会和主循环抢连接。
		// 记一笔就够了：主循环断开重连时自然会重新握手。
		a.opts.Log.Info("mechd requested a rehandshake, waiting for next reconnect")
	}
	return nil
}

// LocalStatus 返回本机全部实例的当前状态快照。
//
// **与 `report()` 共用同一份数据、同一个转换函数**：这正是 ADR-0026
// 「`--local` 只读诊断」要给出的东西——它不是一份为诊断新算出来的视图，
// 是 mechlet 上报给 mechd 的那份数据，只是这次直接在本机读出来，不用
// 等一轮上报周期，也不需要 mechd 可达。**没有对照期望状态算收敛**：
// 那份对照只有 mechd（持有期望状态）算得出来，本机视图能给出的只是
// 「这台机器上现在跑着什么、健康与否」。
func (a *Agent) LocalStatus() []protocol.InstanceStatus {
	a.mu.Lock()
	defer a.mu.Unlock()

	keys := make([]string, 0, len(a.last))
	for k := range a.last {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]protocol.InstanceStatus, 0, len(keys))
	for _, k := range keys {
		if rep := a.last[k]; rep != nil {
			out = append(out, statusOf(rep))
		}
	}
	return out
}

// statusOf 把一次调和报告变成上报结构。
func statusOf(rep *reconcile.Report) protocol.InstanceStatus {
	st := protocol.InstanceStatus{
		WorkloadAction:   rep.LastWorkloadAction,
		WorkloadActionAt: rep.LastWorkloadAt,
		RolledBackFrom:   rep.RolledBackFrom,
		Component:        rep.Component, Role: rep.Role,
		// **digest 是收敛判据**：Rollout 靠「上报的 digest == 期望的 digest
		// 且健康」判定一批完成，而不是靠这里说「我成功了」
		Digest:     rep.Digest,
		Generation: rep.Generation,
		Result:     string(rep.Result),
		Message:    rep.Error,
	}
	// 卸载回执：中心靠它把 Component 从 removing 推到「记录删除」。
	//
	// **每一轮都报，不是只报一次。** 只要那份规格还说 removed，调和就会
	// 重新确认一次并再报一次 true——丢掉一次上报没有后果。若做成事件，
	// 一次网络抖动就会让那个组件永远卡在 removing。
	if rm := rep.Removed; rm != nil {
		st.Removed = true
		st.RetainedPaths = rm.RetainedPaths
	}
	if w := rep.Workload; w != nil {
		// **用 String() 而不是 string()**：runtime.State 的底层类型是 int，
		// `string(w.State)` 是**转成对应码点的字符**，不是取名字——
		// 结果是一个控制字符，上报上去就成了空的 workload 状态。
		// 这条 go vet 的 stringintconv 对带 String() 方法的具名类型不报。
		st.Workload = &protocol.WorkloadStatus{
			State: w.State.String(), Restarts: w.Restarts,
		}
		if !w.Since.IsZero() {
			st.Workload.Since = w.Since.UTC().Format(time.RFC3339)
		}
	}
	if h := rep.Health; h != nil {
		state := "healthy"
		if !h.Healthy {
			state = "unhealthy"
		}
		st.Health = &protocol.HealthStatus{
			State: state, LastError: h.Error,
			CheckedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}
	for _, rr := range rep.Resources {
		s := "ok"
		switch rr.Action {
		case reconcile.ActionFailed:
			s = "failed"
		// ActionReported 与 ActionSuppressed 都是「检出了差异」：
		// 后者只是不告警，**仍然要报上去**——status 里要显示「已抑制」，
		// 而那份数据只能来自这里
		case reconcile.ActionReported, reconcile.ActionSuppressed:
			s = "drift"
		}
		if rr.Error != "" {
			s = "failed"
		}
		st.Resources = append(st.Resources, protocol.ResourceStatus{
			ID: rr.ID, Type: rr.Type, State: s, Detail: rr.Reason,
		})
	}
	return st
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// ErrNotConnected 表示还没连上 mechd。
var ErrNotConnected = errors.New("尚未连上 mechd")
