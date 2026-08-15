-- Observed state: what mechlet reports back.
--
-- ASCII ONLY. sqlc silently corrupts identifiers when a query file contains
-- non-ASCII bytes -- and it exits 0 while doing so. See store_test.go
-- (TestQueryFilesAreASCII). Chinese commentary belongs in the Go code.

-- name: UpsertInstanceStatus :exec
INSERT INTO instance_status (
    role_instance_id, digest, generation, result,
    workload_state, workload_action, workload_action_at, rolled_back_from,
    health, detail, reported_at, restarts, removed, retained_paths
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (role_instance_id) DO UPDATE SET
    digest          = excluded.digest,
    generation      = excluded.generation,
    result          = excluded.result,
    workload_state  = excluded.workload_state,
    workload_action = excluded.workload_action,
    workload_action_at = excluded.workload_action_at,
    rolled_back_from = excluded.rolled_back_from,
    health          = excluded.health,
    detail          = excluded.detail,
    reported_at     = excluded.reported_at,
    restarts        = excluded.restarts,
    removed         = excluded.removed,
    retained_paths  = excluded.retained_paths;

-- name: GetInstanceStatus :one
SELECT * FROM instance_status WHERE role_instance_id = ?;

-- name: ListInstanceStatusByComponent :many
SELECT s.* FROM instance_status s
JOIN role_instances ri ON ri.id = s.role_instance_id
WHERE ri.component_id = ?;

-- name: UpsertNodeFacts :exec
INSERT INTO node_facts (node_id, facts, capabilities, collected_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (node_id) DO UPDATE SET
    facts        = excluded.facts,
    capabilities = excluded.capabilities,
    collected_at = excluded.collected_at;

-- name: GetNodeFacts :one
SELECT * FROM node_facts WHERE node_id = ?;

-- name: UpsertDriftReport :exec
INSERT INTO drift_reports (role_instance_id, resource_id, changes, seen_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (role_instance_id, resource_id) DO UPDATE SET
    changes = excluded.changes,
    seen_at = excluded.seen_at;

-- name: ListDriftByComponent :many
SELECT d.* FROM drift_reports d
JOIN role_instances ri ON ri.id = d.role_instance_id
WHERE ri.component_id = ?
ORDER BY d.role_instance_id, d.resource_id;

-- name: ClearDrift :exec
DELETE FROM drift_reports WHERE role_instance_id = ?;

-- name: CreateSuppression :one
INSERT INTO suppressions (
    role_instance_id, resource_id, reason, created_by, created_at, expires_at
) VALUES (?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: ListSuppressionsByComponent :many
SELECT s.* FROM suppressions s
JOIN role_instances ri ON ri.id = s.role_instance_id
WHERE ri.component_id = ? AND s.expires_at > ?
ORDER BY s.role_instance_id, s.resource_id;

-- name: PruneSuppressions :exec
DELETE FROM suppressions WHERE expires_at <= ?;

-- name: AppendEvent :exec
INSERT INTO events (at, site_id, kind, subject, payload)
VALUES (?, ?, ?, ?, ?);

-- name: ListEvents :many
SELECT * FROM events
WHERE site_id = ?
ORDER BY at DESC, id DESC
LIMIT ?;

-- name: AppendAudit :exec
INSERT INTO audit (at, actor, action, target, pack_ref, result, detail)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ListAudit :many
SELECT * FROM audit ORDER BY at DESC, id DESC LIMIT ?;

-- name: UpsertNodeOrphan :exec
-- purge_requested_at is deliberately NOT updated here: it is set by the
-- operator and must survive every subsequent report. The row disappears on
-- its own once the node stops reporting the orphan, which is what clears it.
INSERT INTO node_orphans (node_id, instance_key, first_seen_at, last_seen_at, paths)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (node_id, instance_key) DO UPDATE SET
    last_seen_at = excluded.last_seen_at,
    paths        = excluded.paths;

-- name: RequestOrphanPurge :execrows
UPDATE node_orphans SET purge_requested_at = ?
WHERE node_id = ? AND instance_key = ?;

-- name: ListOrphanPurges :many
-- length(...) > 0 rather than != '': sqlc 1.31 strips the quotes off a
-- string literal here too, leaving a syntax error. Same trap as the
-- UPDATE ... SET case in expected.sql.
SELECT instance_key FROM node_orphans
WHERE node_id = ? AND length(purge_requested_at) > 0
ORDER BY instance_key;

-- name: ListAllNodeOrphans :many
SELECT * FROM node_orphans ORDER BY node_id, instance_key;

-- name: DeleteNodeOrphansNotIn :exec
-- Rows not touched by this round are stale. Compare with <> rather than <:
-- < assumes the clock strictly advances, and it does not have to. If the
-- clock steps backwards (NTP), rows stamped in the future are never < the
-- current stamp and would survive forever. <> means "not written by this
-- round", which is the actual intent and holds whatever the clock does.
DELETE FROM node_orphans
WHERE node_id = ? AND last_seen_at <> ?;

-- name: ListNodeOrphans :many
SELECT * FROM node_orphans WHERE node_id = ? ORDER BY instance_key;

-- name: CountNodeOrphans :one
SELECT count(*) FROM node_orphans WHERE node_id = ?;

-- -- rollouts ------------------------------------------------------------

-- name: CreateRollout :one
INSERT INTO rollouts (component_id, state, reason, started_at, from_version, to_version, kind)
VALUES (?, ?, '', ?, ?, ?, ?)
RETURNING *;

-- name: GetRollout :one
SELECT * FROM rollouts WHERE id = ?;

-- name: ListRollouts :many
SELECT * FROM rollouts WHERE component_id = ? ORDER BY started_at DESC, id DESC LIMIT ?;

-- name: ActiveRollout :one
SELECT * FROM rollouts
WHERE component_id = ? AND state IN ('running', 'paused', 'halted')
ORDER BY started_at DESC, id DESC LIMIT 1;

-- name: SetRolloutState :exec
UPDATE rollouts SET state = ?, reason = ?, ended_at = ? WHERE id = ?;

-- -- rollout_batches ------------------------------------------------------

-- name: CreateRolloutBatch :one
INSERT INTO rollout_batches (rollout_id, seq, stage, role, targets, state)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: ReleaseRolloutBatch :exec
UPDATE rollout_batches SET state = ?, released_at = ? WHERE id = ?;

-- name: SetRolloutBatchGate :exec
UPDATE rollout_batches
SET healthy_since = ?, restart_baseline = ?
WHERE id = ?;

-- name: ListRolloutBatches :many
SELECT * FROM rollout_batches WHERE rollout_id = ? ORDER BY seq;

-- name: SetRolloutBatchState :exec
UPDATE rollout_batches SET state = ? WHERE id = ?;
