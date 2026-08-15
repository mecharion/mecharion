package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/mecharion/mecharion/internal/store/sqlcgen"
)

// ── 领域类型 ────────────────────────────────────────────────────────────

// InstanceStatus 是一个实例最近一次上报的观测状态。
type InstanceStatus struct {
	InstanceID int64
	// Digest 是**收敛判据**：Rollout 靠「上报的 digest == 期望的 digest
	// 且健康」判定一批完成，而不是靠 mechlet 说「我成功了」。
	// 前者是状态，可以重复确认；后者是事件，丢一次就永远丢了。
	Digest        string
	Generation    int
	Result        string
	WorkloadState string
	// WorkloadAction 是调和器本轮对工作负载做的事：restored | stopped | 空。
	WorkloadAction string
	// WorkloadActionAt 是它发生的时刻（RFC3339）。带时间才让它是状态。
	WorkloadActionAt string
	// RolledBackFrom 是被节点自动回滚掉的那个 digest。
	RolledBackFrom string
	Health         string
	// Restarts 是工作负载的累计重启次数（systemd NRestarts / docker RestartCount）。
	//
	// 滚动升级的健康门禁靠它识别崩溃循环：稳定窗口是有限的，一个周期比它
	// 长的崩溃能干干净净地溜过去，而这个数涨了就是崩过，与观察时机无关。
	Restarts int
	// Removed 表示这个实例已经按 runState: removed 拆干净了。
	//
	// **Digest 在这里帮不上忙**：RunState 不参与 digest，一个拆完的实例
	// 与一个装着的实例上报的 digest 一模一样。Rollout 那套收敛判据在卸载
	// 路径上完全失效，只能另给一个信号。
	Removed bool
	// RetainedPaths 是这次卸载故意留下的目录（数据默认保留）。
	//
	// 在记录被删掉之前，它是回答「删完之后机器上会剩什么」的唯一地方。
	RetainedPaths []string
	Detail        map[string]any
	ReportedAt    time.Time
}

// NodeFacts 是节点上报的事实与能力。
type NodeFacts struct {
	NodeID       int64
	Facts        map[string]any
	Capabilities map[string]any
	CollectedAt  time.Time
}

// DriftReport 是一条已检出的漂移。
type DriftReport struct {
	InstanceID int64
	ResourceID string
	Changes    []any
	SeenAt     time.Time
}

// Suppression 是一条被显式确认过的漂移抑制。
//
// 它**有期限**（到点自动恢复告警，不会悄悄变永久）、**有理由**（进审计）、
// **仍然检测**（只是不告警）——给「凌晨救火改了一个值」一个名分，
// 而不是让它要么被永远报成异常、要么走一次正式变更（06-state-and-drift §4.1）。
type Suppression struct {
	ID         int64
	InstanceID int64
	// ResourceID 为空表示抑制整个实例，用于整机维护窗口。
	ResourceID string
	Reason     string
	CreatedBy  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

// Event 是一条事件流记录。
type Event struct {
	ID      int64
	At      time.Time
	SiteID  int64
	Kind    string
	Subject string
	Payload map[string]any
}

// AuditEntry 是一条审计记录。
type AuditEntry struct {
	ID      int64
	At      time.Time
	Actor   string
	Action  string
	Target  string
	PackRef string
	Result  string
	Detail  map[string]any
}

// ── 仓储接口 ────────────────────────────────────────────────────────────

// StatusRepo 管理观测状态。
type StatusRepo interface {
	Put(ctx context.Context, s InstanceStatus) error
	Get(ctx context.Context, instanceID int64) (InstanceStatus, error)
	ListByComponent(ctx context.Context, componentID int64) ([]InstanceStatus, error)

	PutFacts(ctx context.Context, f NodeFacts) error

	// SetOrphans 用一次上报里的孤儿列表**整体替换**某个节点的记录。
	//
	// 整体替换而非增量：孤儿消失（组件被重新下发、或被 remove 掉）时
	// 必须跟着消失，否则列表只增不减，很快就没人看了。
	SetOrphans(ctx context.Context, nodeID int64, keys []string, now time.Time) error
	// SetOrphanRecords 同 SetOrphans，但带上每个孤儿留下的目录。
	SetOrphanRecords(ctx context.Context, nodeID int64, recs []OrphanRecord, now time.Time) error
	ListOrphans(ctx context.Context, nodeID int64) ([]NodeOrphan, error)
	// RequestPurge 记下「这个孤儿该被清掉」，返回是否命中。
	RequestPurge(ctx context.Context, nodeID int64, key string, at time.Time) (bool, error)
	// ListPurges 返回该节点上待清理的孤儿键。
	ListPurges(ctx context.Context, nodeID int64) ([]string, error)
	GetFacts(ctx context.Context, nodeID int64) (NodeFacts, error)

	PutDrift(ctx context.Context, d DriftReport) error
	ListDrift(ctx context.Context, componentID int64) ([]DriftReport, error)
	ClearDrift(ctx context.Context, instanceID int64) error
}

// SuppressionRepo 管理漂移抑制。
type SuppressionRepo interface {
	Create(ctx context.Context, s Suppression) (Suppression, error)
	// ListActive 只返回**未过期**的：过期即自动恢复告警，
	// 不需要任何人记得去清理。
	ListActive(ctx context.Context, componentID int64, now time.Time) ([]Suppression, error)
	Prune(ctx context.Context, now time.Time) error
}

// EventRepo 管理事件流与审计。
type EventRepo interface {
	Append(ctx context.Context, e Event) error
	List(ctx context.Context, siteID int64, limit int) ([]Event, error)
	Audit(ctx context.Context, a AuditEntry) error
	ListAudit(ctx context.Context, limit int) ([]AuditEntry, error)
}

// ── 实现 ────────────────────────────────────────────────────────────────

func (r *repos) Status() StatusRepo            { return &statusRepo{s: r.s} }
func (r *repos) Suppressions() SuppressionRepo { return &suppressionRepo{s: r.s} }
func (r *repos) Events() EventRepo             { return &eventRepo{s: r.s} }

type statusRepo struct{ s *Store }

func (r *statusRepo) Put(ctx context.Context, in InstanceStatus) error {
	detail, err := encodeJSON(in.Detail, "{}")
	if err != nil {
		return err
	}
	retained, err := encodeJSON(in.RetainedPaths, "[]")
	if err != nil {
		return err
	}
	return r.s.wq(ctx).UpsertInstanceStatus(ctx, sqlcgen.UpsertInstanceStatusParams{
		RoleInstanceID:   in.InstanceID,
		Digest:           in.Digest,
		Generation:       int64(in.Generation),
		Result:           in.Result,
		WorkloadState:    in.WorkloadState,
		WorkloadAction:   in.WorkloadAction,
		WorkloadActionAt: in.WorkloadActionAt,
		RolledBackFrom:   in.RolledBackFrom,
		Health:           in.Health,
		Detail:           detail,
		ReportedAt:       FormatTime(in.ReportedAt),
		Restarts:         int64(in.Restarts),
		Removed:          boolToInt(in.Removed),
		RetainedPaths:    retained,
	})
}

func (r *statusRepo) Get(ctx context.Context, id int64) (InstanceStatus, error) {
	row, err := r.s.rq(ctx).GetInstanceStatus(ctx, id)
	if err != nil {
		return InstanceStatus{}, notFound(err, "实例状态")
	}
	return statusFrom(row)
}

func (r *statusRepo) ListByComponent(ctx context.Context, componentID int64) ([]InstanceStatus, error) {
	rows, err := r.s.rq(ctx).ListInstanceStatusByComponent(ctx, componentID)
	if err != nil {
		return nil, err
	}
	out := make([]InstanceStatus, 0, len(rows))
	for _, row := range rows {
		s, err := statusFrom(row)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func statusFrom(row sqlcgen.InstanceStatus) (InstanceStatus, error) {
	at, err := ParseTime(row.ReportedAt)
	if err != nil {
		return InstanceStatus{}, err
	}
	detail, err := decodeMap[any](row.Detail, "detail")
	if err != nil {
		return InstanceStatus{}, err
	}
	var retained []string
	if err := decodeJSON(row.RetainedPaths, &retained, "retained_paths"); err != nil {
		return InstanceStatus{}, err
	}
	return InstanceStatus{
		InstanceID: row.RoleInstanceID, Digest: row.Digest,
		Generation: int(row.Generation), Result: row.Result,
		WorkloadState: row.WorkloadState, WorkloadAction: row.WorkloadAction,
		WorkloadActionAt: row.WorkloadActionAt, RolledBackFrom: row.RolledBackFrom,
		Health: row.Health, Detail: detail, ReportedAt: at,
		Restarts: int(row.Restarts),
		Removed:  row.Removed != 0, RetainedPaths: retained,
	}, nil
}

func (r *statusRepo) PutFacts(ctx context.Context, f NodeFacts) error {
	facts, err := encodeJSON(f.Facts, "{}")
	if err != nil {
		return err
	}
	caps, err := encodeJSON(f.Capabilities, "{}")
	if err != nil {
		return err
	}
	return r.s.wq(ctx).UpsertNodeFacts(ctx, sqlcgen.UpsertNodeFactsParams{
		NodeID: f.NodeID, Facts: facts, Capabilities: caps,
		CollectedAt: FormatTime(f.CollectedAt),
	})
}

func (r *statusRepo) GetFacts(ctx context.Context, nodeID int64) (NodeFacts, error) {
	row, err := r.s.rq(ctx).GetNodeFacts(ctx, nodeID)
	if err != nil {
		return NodeFacts{}, notFound(err, "节点事实")
	}
	at, err := ParseTime(row.CollectedAt)
	if err != nil {
		return NodeFacts{}, err
	}
	facts, err := decodeMap[any](row.Facts, "facts")
	if err != nil {
		return NodeFacts{}, err
	}
	caps, err := decodeMap[any](row.Capabilities, "capabilities")
	if err != nil {
		return NodeFacts{}, err
	}
	return NodeFacts{NodeID: row.NodeID, Facts: facts, Capabilities: caps, CollectedAt: at}, nil
}

func (r *statusRepo) PutDrift(ctx context.Context, d DriftReport) error {
	changes, err := encodeJSON(d.Changes, "[]")
	if err != nil {
		return err
	}
	return r.s.wq(ctx).UpsertDriftReport(ctx, sqlcgen.UpsertDriftReportParams{
		RoleInstanceID: d.InstanceID, ResourceID: d.ResourceID,
		Changes: changes, SeenAt: FormatTime(d.SeenAt),
	})
}

func (r *statusRepo) ListDrift(ctx context.Context, componentID int64) ([]DriftReport, error) {
	rows, err := r.s.rq(ctx).ListDriftByComponent(ctx, componentID)
	if err != nil {
		return nil, err
	}
	out := make([]DriftReport, 0, len(rows))
	for _, row := range rows {
		at, err := ParseTime(row.SeenAt)
		if err != nil {
			return nil, err
		}
		var changes []any
		if err := decodeJSON(row.Changes, &changes, "changes"); err != nil {
			return nil, err
		}
		out = append(out, DriftReport{
			InstanceID: row.RoleInstanceID, ResourceID: row.ResourceID,
			Changes: changes, SeenAt: at,
		})
	}
	return out, nil
}

func (r *statusRepo) ClearDrift(ctx context.Context, instanceID int64) error {
	return r.s.wq(ctx).ClearDrift(ctx, instanceID)
}

type suppressionRepo struct{ s *Store }

func (r *suppressionRepo) Create(ctx context.Context, in Suppression) (Suppression, error) {
	row, err := r.s.wq(ctx).CreateSuppression(ctx, sqlcgen.CreateSuppressionParams{
		RoleInstanceID: in.InstanceID, ResourceID: in.ResourceID,
		Reason: in.Reason, CreatedBy: in.CreatedBy,
		CreatedAt: FormatTime(in.CreatedAt), ExpiresAt: FormatTime(in.ExpiresAt),
	})
	if err != nil {
		return Suppression{}, err
	}
	return suppressionFrom(row)
}

func (r *suppressionRepo) ListActive(
	ctx context.Context, componentID int64, now time.Time,
) ([]Suppression, error) {
	rows, err := r.s.rq(ctx).ListSuppressionsByComponent(ctx,
		sqlcgen.ListSuppressionsByComponentParams{
			ComponentID: componentID, ExpiresAt: FormatTime(now),
		})
	if err != nil {
		return nil, err
	}
	out := make([]Suppression, 0, len(rows))
	for _, row := range rows {
		s, err := suppressionFrom(row)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (r *suppressionRepo) Prune(ctx context.Context, now time.Time) error {
	return r.s.wq(ctx).PruneSuppressions(ctx, FormatTime(now))
}

func suppressionFrom(row sqlcgen.Suppression) (Suppression, error) {
	created, err := ParseTime(row.CreatedAt)
	if err != nil {
		return Suppression{}, err
	}
	expires, err := ParseTime(row.ExpiresAt)
	if err != nil {
		return Suppression{}, err
	}
	return Suppression{
		ID: row.ID, InstanceID: row.RoleInstanceID, ResourceID: row.ResourceID,
		Reason: row.Reason, CreatedBy: row.CreatedBy,
		CreatedAt: created, ExpiresAt: expires,
	}, nil
}

type eventRepo struct{ s *Store }

func (r *eventRepo) Append(ctx context.Context, e Event) error {
	payload, err := encodeJSON(e.Payload, "{}")
	if err != nil {
		return err
	}
	var site sql.NullInt64
	if e.SiteID != 0 {
		site = sql.NullInt64{Int64: e.SiteID, Valid: true}
	}
	return r.s.wq(ctx).AppendEvent(ctx, sqlcgen.AppendEventParams{
		At: FormatTime(e.At), SiteID: site,
		Kind: e.Kind, Subject: e.Subject, Payload: payload,
	})
}

func (r *eventRepo) List(ctx context.Context, siteID int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.s.rq(ctx).ListEvents(ctx, sqlcgen.ListEventsParams{
		SiteID: sql.NullInt64{Int64: siteID, Valid: true}, Limit: int64(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(rows))
	for _, row := range rows {
		at, err := ParseTime(row.At)
		if err != nil {
			return nil, err
		}
		payload, err := decodeMap[any](row.Payload, "payload")
		if err != nil {
			return nil, err
		}
		out = append(out, Event{
			ID: row.ID, At: at, SiteID: row.SiteID.Int64,
			Kind: row.Kind, Subject: row.Subject, Payload: payload,
		})
	}
	return out, nil
}

func (r *eventRepo) Audit(ctx context.Context, a AuditEntry) error {
	detail, err := encodeJSON(a.Detail, "{}")
	if err != nil {
		return err
	}
	return r.s.wq(ctx).AppendAudit(ctx, sqlcgen.AppendAuditParams{
		At: FormatTime(a.At), Actor: a.Actor, Action: a.Action,
		Target: a.Target, PackRef: a.PackRef, Result: a.Result, Detail: detail,
	})
}

func (r *eventRepo) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.s.rq(ctx).ListAudit(ctx, int64(limit))
	if err != nil {
		return nil, err
	}
	out := make([]AuditEntry, 0, len(rows))
	for _, row := range rows {
		at, err := ParseTime(row.At)
		if err != nil {
			return nil, err
		}
		detail, err := decodeMap[any](row.Detail, "detail")
		if err != nil {
			return nil, err
		}
		out = append(out, AuditEntry{
			ID: row.ID, At: at, Actor: row.Actor, Action: row.Action,
			Target: row.Target, PackRef: row.PackRef, Result: row.Result,
			Detail: detail,
		})
	}
	return out, nil
}

// ErrNoRows 让调用方不必导入 database/sql 就能判断「没有这一条」。
var ErrNoRows = sql.ErrNoRows

// NodeOrphan 是一条孤儿实例记录。
//
// FirstSeen 与 LastSeen 分开记，回答的是不同的问题：「这东西什么时候被
// 落下的」（多半对得上某次变更）与「它现在还在不在」。只记一个时间戳的话，
// 一个三个月前的残留与今天刚出现的看起来一样。
type NodeOrphan struct {
	NodeID      int64
	InstanceKey string
	FirstSeen   time.Time
	LastSeen    time.Time
	// Paths 是这个孤儿在盘上留下的目录。
	//
	// 空表示它不是 remove 留下的残留，而是「下发里没有它了」——那时
	// 机器上留着的是一整个还装着的实例，不只是几个目录。
	Paths []string
	// PurgeRequested 表示已经有人要求清掉它。
	PurgeRequested bool
}

func (r *statusRepo) SetOrphans(
	ctx context.Context, nodeID int64, keys []string, now time.Time,
) error {
	return r.SetOrphanRecords(ctx, nodeID, keysOnly(keys), now)
}

// keysOnly 把一组键转成没有路径细节的记录。
func keysOnly(keys []string) []OrphanRecord {
	out := make([]OrphanRecord, 0, len(keys))
	for _, k := range keys {
		out = append(out, OrphanRecord{Key: k})
	}
	return out
}

// OrphanRecord 是一个孤儿及其在盘上留下的东西。
type OrphanRecord struct {
	Key   string
	Paths []string
}

// SetOrphanRecords 用一次上报里的孤儿**整体替换**某个节点的记录。
//
// `purge_requested_at` 刻意不在 upsert 里更新：它由人设置，必须活过
// 之后的每一次上报。清干净之后节点不再上报这个孤儿，这一行会被下面那条
// 「本轮没报到的删掉」带走——purge 意图因此自动失效，不需要谁去清理。
func (r *statusRepo) SetOrphanRecords(
	ctx context.Context, nodeID int64, recs []OrphanRecord, now time.Time,
) error {
	stamp := FormatTime(now)
	for _, rec := range recs {
		paths, err := encodeJSON(rec.Paths, "[]")
		if err != nil {
			return err
		}
		if err := r.s.wq(ctx).UpsertNodeOrphan(ctx, sqlcgen.UpsertNodeOrphanParams{
			NodeID: nodeID, InstanceKey: rec.Key,
			FirstSeenAt: stamp, LastSeenAt: stamp, Paths: paths,
		}); err != nil {
			return fmt.Errorf("store: recording orphan instance %s: %w", rec.Key, err)
		}
	}
	// 本轮没报到的就是已经消失的。用时间戳而不是 NOT IN (...)：
	// 后者要拼变长 SQL，而 sqlc 不生成那种查询。
	return r.s.wq(ctx).DeleteNodeOrphansNotIn(ctx, sqlcgen.DeleteNodeOrphansNotInParams{
		NodeID: nodeID, LastSeenAt: stamp,
	})
}

func (r *statusRepo) ListOrphans(ctx context.Context, nodeID int64) ([]NodeOrphan, error) {
	rows, err := r.s.rq(ctx).ListNodeOrphans(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("store: listing orphan instances: %w", err)
	}
	out := make([]NodeOrphan, 0, len(rows))
	for _, row := range rows {
		first, err := ParseTime(row.FirstSeenAt)
		if err != nil {
			return nil, fmt.Errorf("store: parsing node_orphans.first_seen_at: %w", err)
		}
		last, err := ParseTime(row.LastSeenAt)
		if err != nil {
			return nil, fmt.Errorf("store: parsing node_orphans.last_seen_at: %w", err)
		}
		var paths []string
		if err := decodeJSON(row.Paths, &paths, "node_orphans.paths"); err != nil {
			return nil, err
		}
		out = append(out, NodeOrphan{
			NodeID: row.NodeID, InstanceKey: row.InstanceKey,
			FirstSeen: first, LastSeen: last, Paths: paths,
			PurgeRequested: row.PurgeRequestedAt != "",
		})
	}
	return out, nil
}

// ── Rollout ─────────────────────────────────────────────────────────────

// Rollout 的状态词表。
const (
	// RolloutRunning 正在推进（单机 = 一批）。
	RolloutRunning = "running"
	// RolloutSucceeded 全部实例收敛到目标版本且健康。
	RolloutSucceeded = "succeeded"
	// RolloutFailed 观测到自动回滚，或超时仍未收敛，**且没有前路**。
	//
	// 留给真正走不下去的两种：节点自动回滚（那个 digest 在节点侧已被
	// blocked，再推一遍也不会动），以及没有批次记录的退化路径超时
	// （没有「断点」可续）。它是终态，不能 resume。
	RolloutFailed = "failed"
	// RolloutHalted 某一批没过门禁，**停下来等人**（22-multi-node §2.6）。
	//
	// 与 paused 分开：脚本判 `state == "paused"` 时分不出「我按的」和
	// 「出事了」，而这两件事在复盘里差得远。与 failed 分开：它有前路
	// ——修好机器之后 `rollout resume` 从这一批续做，已完成的批次不重做。
	RolloutHalted = "halted"
	// RolloutPaused 人工冻结判定，并停在当前这批。
	//
	// 已经放行的那批照常跑完，但下一批不会被放行。它表达的是
	// 「我正在查，别急着往前走」。
	RolloutPaused = "paused"
	// RolloutAborted 人工中止，已回退到起始版本。
	//
	// 与 failed 分开，是因为「人主动退回」与「系统判定失败」在复盘时是
	// 两件完全不同的事。
	RolloutAborted = "aborted"
)

// Rollout 是一次版本变更的过程记录。
type Rollout struct {
	ID          int64
	ComponentID int64
	State       string
	Reason      string
	Kind        string // upgrade | rollback
	FromVersion string
	ToVersion   string
	StartedAt   time.Time
	EndedAt     *time.Time
}

// Active 报告这次 Rollout 是否仍在进行（含 paused）。
func (r Rollout) Active() bool {
	return r.State == RolloutRunning || r.State == RolloutPaused
}

// RolloutRepo 读写 Rollout。
type RolloutRepo interface {
	Create(ctx context.Context, r Rollout) (Rollout, error)
	// Active 返回该 Component 上仍在进行的 Rollout；没有则 ErrNotFound。
	Active(ctx context.Context, componentID int64) (Rollout, error)
	List(ctx context.Context, componentID int64, limit int) ([]Rollout, error)
	SetState(ctx context.Context, id int64, state, reason string, endedAt *time.Time) error
}

type rolloutRepo struct{ s *Store }

func (r *repos) Rollouts() RolloutRepo { return &rolloutRepo{s: r.s} }

func (r *rolloutRepo) Create(ctx context.Context, in Rollout) (Rollout, error) {
	row, err := r.s.wq(ctx).CreateRollout(ctx, sqlcgen.CreateRolloutParams{
		ComponentID: in.ComponentID, State: in.State,
		StartedAt:   FormatTime(in.StartedAt),
		FromVersion: in.FromVersion, ToVersion: in.ToVersion, Kind: in.Kind,
	})
	if err != nil {
		return Rollout{}, fmt.Errorf("store: creating Rollout: %w", err)
	}
	return rolloutFrom(row)
}

func (r *rolloutRepo) Active(ctx context.Context, componentID int64) (Rollout, error) {
	row, err := r.s.rq(ctx).ActiveRollout(ctx, componentID)
	if err != nil {
		return Rollout{}, notFound(err, "进行中的 Rollout")
	}
	return rolloutFrom(row)
}

func (r *rolloutRepo) List(ctx context.Context, componentID int64, limit int) ([]Rollout, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := r.s.rq(ctx).ListRollouts(ctx, sqlcgen.ListRolloutsParams{
		ComponentID: componentID, Limit: int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("store: listing Rollouts: %w", err)
	}
	out := make([]Rollout, 0, len(rows))
	for _, row := range rows {
		v, err := rolloutFrom(row)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (r *rolloutRepo) SetState(
	ctx context.Context, id int64, state, reason string, endedAt *time.Time,
) error {
	var ended sql.NullString
	if endedAt != nil {
		ended = sql.NullString{String: FormatTime(*endedAt), Valid: true}
	}
	if err := r.s.wq(ctx).SetRolloutState(ctx, sqlcgen.SetRolloutStateParams{
		State: state, Reason: reason, EndedAt: ended, ID: id,
	}); err != nil {
		return fmt.Errorf("store: updating Rollout status: %w", err)
	}
	return nil
}

func rolloutFrom(r sqlcgen.Rollout) (Rollout, error) {
	started, err := ParseTime(r.StartedAt)
	if err != nil {
		return Rollout{}, fmt.Errorf("store: parsing rollouts.started_at: %w", err)
	}
	out := Rollout{
		ID: r.ID, ComponentID: r.ComponentID, State: r.State, Reason: r.Reason,
		Kind: r.Kind, FromVersion: r.FromVersion, ToVersion: r.ToVersion,
		StartedAt: started,
	}
	if r.EndedAt.Valid {
		t, err := ParseTime(r.EndedAt.String)
		if err != nil {
			return Rollout{}, fmt.Errorf("store: parsing rollouts.ended_at: %w", err)
		}
		out.EndedAt = &t
	}
	return out, nil
}

// RequestPurge 记下「这个孤儿该被清掉」。
//
// 返回 false 表示没有这一条孤儿记录——那通常意味着它已经被清掉了，
// 或者节点还没报上来。**不当成错误**由调用方决定：对 `orphans purge`
// 来说那是要说清楚的信息，而不是一次失败。
func (r *statusRepo) RequestPurge(
	ctx context.Context, nodeID int64, key string, at time.Time,
) (bool, error) {
	n, err := r.s.wq(ctx).RequestOrphanPurge(ctx, sqlcgen.RequestOrphanPurgeParams{
		PurgeRequestedAt: FormatTime(at), NodeID: nodeID, InstanceKey: key,
	})
	if err != nil {
		return false, fmt.Errorf("store: marking orphan for cleanup: %w", err)
	}
	return n > 0, nil
}

// ListPurges 返回该节点上待清理的孤儿键。
func (r *statusRepo) ListPurges(ctx context.Context, nodeID int64) ([]string, error) {
	keys, err := r.s.rq(ctx).ListOrphanPurges(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("store: listing orphans pending cleanup: %w", err)
	}
	return keys, nil
}
