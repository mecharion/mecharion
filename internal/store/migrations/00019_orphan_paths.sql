-- +goose Up
-- 孤儿要能被**找到**，而不只是被数出来（24-lifecycle §2.4）。
--
-- 原来的 node_orphans 只有 instance_key。一条「n2 上有个 pg-main__primary
-- 的孤儿」回答不了唯一要紧的那个问题：**那台机器上到底还剩什么、在哪儿**。
-- 10-cli §4.3 明写「保留而不提供发现机制等于把问题推给未来」——只记一个
-- 键，那个机制就只完成了一半。
ALTER TABLE node_orphans ADD COLUMN paths TEXT NOT NULL DEFAULT '[]';

-- purge 的意图。非空表示「这个孤儿该被清掉」。
--
-- **它是状态不是指令**（ADR-0029）：下发里带的是「该清掉的孤儿键」，
-- 节点每次收到都照做一遍，删一个已经不在的目录是 no-op。节点失联三天
-- 回来之后仍然会收到它，不需要 mechd 记着「我还欠它一条 purge」。
--
-- **自限**：节点清完之后本地收据就没了，孤儿也就不再出现在上报里，
-- 而 SetOrphans 每轮整体替换——这一行会跟着消失，purge 意图自然失效。
-- 因此不需要任何确认序号或一次性标记。
ALTER TABLE node_orphans ADD COLUMN purge_requested_at TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE node_orphans DROP COLUMN purge_requested_at;
ALTER TABLE node_orphans DROP COLUMN paths;
