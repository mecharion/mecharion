-- +goose Up
--
-- 批次健康门禁（M7 第 8 步，22-multi-node §2.5）。
--
-- 三列都是**运行期状态**，与 `targets`（一次算好就不再变的计划）分开放。
-- 混在一起的话，每次刷新窗口都要重写整份目标清单。
--
--   released_at      这一批被放行的时刻。batchTimeout 从它起算，
--                    **不是从 Rollout 开始起算**——一次 4 批的升级合法地要
--                    花 4×（物化 + 稳定窗口），拿一个全局超时去卡它必然误判。
--   healthy_since    这一批**首次**同时满足收敛与健康的时刻。任何一个实例
--                    掉出去就清空——那正是门禁挡住崩溃循环的全部机制：
--                    机器每崩一次就把它清掉，于是永远攒不满 stableFor。
--   restart_baseline 窗口开始时各实例的工作负载重启次数（JSON: 实例 id → 次数）。
--                    窗口期间涨了就说明崩过，与观察时机无关——有限窗口必然
--                    漏掉周期比它长的崩溃，这一条不会漏。
ALTER TABLE rollout_batches ADD COLUMN released_at TEXT NOT NULL DEFAULT '';
ALTER TABLE rollout_batches ADD COLUMN healthy_since TEXT NOT NULL DEFAULT '';
ALTER TABLE rollout_batches ADD COLUMN restart_baseline TEXT NOT NULL DEFAULT '';

-- 重启次数此前只到 protocol 层就停了（`WorkloadStatus.Restarts`），没落库。
-- 门禁要用它，而它同时也让 `component status` 说得出「这个服务重启过 37 次」
-- ——那件事本来就该看得见。
ALTER TABLE instance_status ADD COLUMN restarts INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE instance_status DROP COLUMN restarts;
ALTER TABLE rollout_batches DROP COLUMN restart_baseline;
ALTER TABLE rollout_batches DROP COLUMN healthy_since;
ALTER TABLE rollout_batches DROP COLUMN released_at;
