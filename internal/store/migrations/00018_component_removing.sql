-- +goose Up
-- Component 的生命周期状态与卸载开关（24-lifecycle §2.2）。
--
-- `removed` 只是**期望**，记录不能在下发那一刻就删掉：删了之后那个实例
-- 就不在下发里了，节点再也收不到「这个实例不该存在」——它会变成孤儿，
-- 而孤儿永不自动删（20-continuous-reconcile §2.4）。因此 Component 要
-- 在「已下发卸载意图」与「全部实例都拆完了」之间停留一段时间。
--
--   active ──remove──> removing ──全部实例报告已卸载──> （记录删除）
--                         └────── --force ──────────> （记录删除）
--
-- **默认 'active' 而不是空串**：一个没有状态的 Component 在每个使用点
-- 都要写一遍「空串算什么」，而那种判断迟早会有一处写反——写反的方向是
-- 「把一个正常组件当成正在删除的」。
ALTER TABLE components ADD COLUMN state TEXT NOT NULL DEFAULT 'active';

-- 卸载开关随规格下发，因此必须落库：mechlet 不做判断（ADR-0006），
-- 而下发是每 60 秒一次的持续行为，不是 remove 那一刻的一次性动作。
--
-- 三列都默认 0，与 spec.Removal 的零值语义一致：**配置删、数据留、
-- 用户留**。一个漏写的迁移或漏传的字段不会多删任何东西。
ALTER TABLE components ADD COLUMN removal_keep_config INTEGER NOT NULL DEFAULT 0;
ALTER TABLE components ADD COLUMN removal_purge_data  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE components ADD COLUMN removal_purge_user  INTEGER NOT NULL DEFAULT 0;

-- removing_at 让「卡了多久」可见。
--
-- 一个停在 removing 的组件是这条路上最常见的现场（某台机器失联），
-- 而「它从什么时候开始卡的」决定了运维要不要动 --force。没有这一列，
-- 那个问题只能靠翻日志回答。
ALTER TABLE components ADD COLUMN removing_at TEXT NOT NULL DEFAULT '';

-- 卸载回执：节点报「这个实例已经拆干净了」。
--
-- **digest 在这里帮不上忙**：RunState 不参与 digest（spec.ComputeDigest），
-- 因此一个拆完的实例与一个装着的实例上报的 digest 一模一样。Rollout 那套
-- 「上报的 digest == 期望的 digest」的收敛判据在卸载路径上完全失效，
-- 只能另给一个信号。
--
-- 落库而不是只记在内存里：mechd 重启之后那个组件仍要能推进到删除，
-- 否则一次重启就把它永久卡在 removing。
ALTER TABLE instance_status ADD COLUMN removed INTEGER NOT NULL DEFAULT 0;

-- 这次卸载**故意留下**的目录。
--
-- 在记录被删掉之前，它是回答「删完之后机器上会剩什么」的唯一地方——
-- 而那正是人在按下确认之前想知道的。记录删掉之后，同样这批目录会由
-- 节点侧的孤儿上报重新浮出来。
ALTER TABLE instance_status ADD COLUMN retained_paths TEXT NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE instance_status DROP COLUMN retained_paths;
ALTER TABLE instance_status DROP COLUMN removed;
ALTER TABLE components DROP COLUMN removing_at;
ALTER TABLE components DROP COLUMN removal_purge_user;
ALTER TABLE components DROP COLUMN removal_purge_data;
ALTER TABLE components DROP COLUMN removal_keep_config;
ALTER TABLE components DROP COLUMN state;
