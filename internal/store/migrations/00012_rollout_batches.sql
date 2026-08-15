-- +goose Up
--
-- 多节点分批 Rollout（M7 第 7 步）。
--
-- `rollout_batches` 从 00001_init 起就建着，**一次都没被写过**——M6 的
-- 单机 Rollout 直接用 `rollouts`，因为单机只有一批。这里第一次真的用它。
--
-- 加两列：
--   stage  阶段序号。一次 Rollout 是两层结构（22-multi-node §2.4）：
--          阶段按角色的 requires 拓扑序，阶段内再按 maxUnavailable 切批。
--   role   这一批属于哪个角色。冗余存一份是为了**历史记录自洽**：
--          实例可能在 Rollout 之后被删掉，那时仅凭 id 说不出它是谁。
ALTER TABLE rollout_batches ADD COLUMN stage INTEGER NOT NULL DEFAULT 0;
ALTER TABLE rollout_batches ADD COLUMN role TEXT NOT NULL DEFAULT '';

-- 两个旋钮都在 Component 上（22-multi-node §2.4）。
--
-- **没有 maxSurge**（ADR-0035）：裸机实例有固化 ordinal、固定数据目录与
-- 端口，「多起一个」不成立。可用性靠「同时只动 maxUnavailable 个」维持。
ALTER TABLE components ADD COLUMN rollout_max_unavailable INTEGER NOT NULL DEFAULT 1;
-- canary 是首批大小；0 表示不做金丝雀。
ALTER TABLE components ADD COLUMN rollout_canary INTEGER NOT NULL DEFAULT 1;

-- +goose Down
ALTER TABLE components DROP COLUMN rollout_canary;
ALTER TABLE components DROP COLUMN rollout_max_unavailable;
ALTER TABLE rollout_batches DROP COLUMN role;
ALTER TABLE rollout_batches DROP COLUMN stage;
