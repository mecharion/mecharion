-- +goose Up
-- Rollout 记录一次版本变更的**过程**，而不只是结果。
--
-- 没有它，「升级到一半」在系统里没有名字：status 只说收敛没收敛，
-- 而运维想问的是「这次升级怎么样了、能不能停下、要不要退回去」。
--
-- 状态词表定为：
--
--	running    正在推进（单机=一批）
--	succeeded  全部实例收敛到目标版本且健康
--	failed     观测到自动回滚，或超时仍未收敛
--	paused     人工冻结判定——**不是暂停推送**（单机只有一批，没什么可停）
--	aborted    人工中止，已回退到起始版本
--
-- 与 00001 里那句注释（pending|running|paused|done|failed）的差别：
-- `done` 换成 `succeeded`，并新增 `aborted`。前者是因为「完成」不区分
-- 成功与失败；后者是因为「人主动退回」与「系统判定失败」在复盘时是
-- 两件完全不同的事，混在一个词里会让事故报告写不清楚。
ALTER TABLE rollouts ADD COLUMN from_version TEXT NOT NULL DEFAULT '';
ALTER TABLE rollouts ADD COLUMN to_version   TEXT NOT NULL DEFAULT '';
-- kind 取 upgrade | rollback。
ALTER TABLE rollouts ADD COLUMN kind         TEXT NOT NULL DEFAULT 'upgrade';

CREATE INDEX idx_rollouts_component ON rollouts (component_id, started_at DESC);

-- +goose Down
DROP INDEX idx_rollouts_component;
ALTER TABLE rollouts DROP COLUMN kind;
ALTER TABLE rollouts DROP COLUMN to_version;
ALTER TABLE rollouts DROP COLUMN from_version;
