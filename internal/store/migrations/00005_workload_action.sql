-- +goose Up
-- 调和器本轮**对工作负载做了什么**：restored | stopped | 空。
--
-- 没有它，一次「服务被人停了、调和把它拉起来」在中心完全不可见：
-- 没有资源被 Apply、没有切软链，结果是 ok。**一个每分钟崩一次又被拉起的
-- 服务，从中心看完全健康**——那正是最该被看见的一种故障。
--
-- **带时间戳**是必需的，不是锦上添花：不带的话它只是「上一轮做了什么」，
-- 而上报是**周期快照**——调和比上报密时，那一轮会在被上报之前就被下一轮
-- 覆盖掉，动作就丢了。规格里对 digest 写过同一句话：状态可以重复确认，
-- 事件丢一次就永远丢了。
--
-- 不做累计计数：累计值要考虑什么时候清零，而那个问题没有好答案
-- （重新部署算不算？换 generation 算不算？）。要看趋势应当查事件流。
ALTER TABLE instance_status ADD COLUMN workload_action TEXT NOT NULL DEFAULT '';
ALTER TABLE instance_status ADD COLUMN workload_action_at TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE instance_status DROP COLUMN workload_action_at;
ALTER TABLE instance_status DROP COLUMN workload_action;
