-- +goose Up
-- 被节点自动回滚掉的那个 digest。
--
-- mechd 看到的只是「上报的 digest 不是期望的那个」，而那与「还没来得及
-- 升级」长得一模一样。Rollout 的状态机要靠这一条区分
-- 「失败并已回滚」与「正在推进」——没有它，一次失败的升级会一直显示
-- 「进行中」直到超时。
ALTER TABLE instance_status ADD COLUMN rolled_back_from TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE instance_status DROP COLUMN rolled_back_from;
