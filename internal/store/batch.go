package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mecharion/mecharion/internal/store/sqlcgen"
)

type rolloutBatchRepo struct{ s *Store }

func (r *repos) RolloutBatches() RolloutBatchRepo { return &rolloutBatchRepo{s: r.s} }

func (r *rolloutBatchRepo) Create(ctx context.Context, b RolloutBatch) (RolloutBatch, error) {
	blob, err := json.Marshal(b.Targets)
	if err != nil {
		return RolloutBatch{}, fmt.Errorf("store: encoding batch targets: %w", err)
	}
	row, err := r.s.wq(ctx).CreateRolloutBatch(ctx, sqlcgen.CreateRolloutBatchParams{
		RolloutID: b.RolloutID, Seq: int64(b.Seq), Stage: int64(b.Stage),
		Role: b.Role, Targets: string(blob), State: b.State,
	})
	if err != nil {
		return RolloutBatch{}, fmt.Errorf("store: creating batch: %w", err)
	}
	return batchFrom(row)
}

func (r *rolloutBatchRepo) List(ctx context.Context, rolloutID int64) ([]RolloutBatch, error) {
	rows, err := r.s.rq(ctx).ListRolloutBatches(ctx, rolloutID)
	if err != nil {
		return nil, fmt.Errorf("store: listing batches: %w", err)
	}
	out := make([]RolloutBatch, 0, len(rows))
	for _, row := range rows {
		b, err := batchFrom(row)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

func (r *rolloutBatchRepo) SetState(ctx context.Context, id int64, state string) error {
	err := r.s.wq(ctx).SetRolloutBatchState(ctx, sqlcgen.SetRolloutBatchStateParams{
		State: state, ID: id,
	})
	if err != nil {
		return fmt.Errorf("store: updating batch status: %w", err)
	}
	return nil
}

// Release 放行一批：状态与放行时刻一起写。
func (r *rolloutBatchRepo) Release(ctx context.Context, id int64, at time.Time) error {
	err := r.s.wq(ctx).ReleaseRolloutBatch(ctx, sqlcgen.ReleaseRolloutBatchParams{
		State: BatchReleased, ReleasedAt: FormatTime(at), ID: id,
	})
	if err != nil {
		return fmt.Errorf("store: releasing batch: %w", err)
	}
	return nil
}

// SetGate 刷新稳定窗口；since 为零值表示清掉窗口。
func (r *rolloutBatchRepo) SetGate(
	ctx context.Context, id int64, since time.Time, baseline map[int64]int,
) error {
	at := ""
	blob := ""
	if !since.IsZero() {
		at = FormatTime(since)
		b, err := json.Marshal(baseline)
		if err != nil {
			return fmt.Errorf("store: encoding restart baseline: %w", err)
		}
		blob = string(b)
	}
	err := r.s.wq(ctx).SetRolloutBatchGate(ctx, sqlcgen.SetRolloutBatchGateParams{
		HealthySince: at, RestartBaseline: blob, ID: id,
	})
	if err != nil {
		return fmt.Errorf("store: refreshing batch gate: %w", err)
	}
	return nil
}

func batchFrom(r sqlcgen.RolloutBatch) (RolloutBatch, error) {
	var targets []BatchTarget
	if err := decodeJSON(r.Targets, &targets, "rollout_batches.targets"); err != nil {
		return RolloutBatch{}, err
	}
	b := RolloutBatch{
		ID: r.ID, RolloutID: r.RolloutID, Seq: int(r.Seq), Stage: int(r.Stage),
		Role: r.Role, Targets: targets, State: r.State,
	}
	// 三个都可能是空串（默认值、或窗口被清掉）——空不是错误。
	if r.ReleasedAt != "" {
		at, err := ParseTime(r.ReleasedAt)
		if err != nil {
			return RolloutBatch{}, err
		}
		b.ReleasedAt = at
	}
	if r.HealthySince != "" {
		at, err := ParseTime(r.HealthySince)
		if err != nil {
			return RolloutBatch{}, err
		}
		b.HealthySince = at
	}
	if r.RestartBaseline != "" {
		if err := decodeJSON(r.RestartBaseline, &b.RestartBaseline,
			"rollout_batches.restart_baseline"); err != nil {
			return RolloutBatch{}, err
		}
	}
	return b, nil
}
