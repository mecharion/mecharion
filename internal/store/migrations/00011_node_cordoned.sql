-- +goose Up
--
-- cordon：暂停一台机器上的调和，运行中的进程不动。
--
-- **与 revoke / remove 都不同**：那两个切断的是「能不能连」，cordon 保留
-- 连接、保留上报，只让调和停下来——「我要手工调试这台机，别让 mechlet
-- 把我的改动改回去」（10-cli §4.2）。
--
-- 它也是 Rollout 分批必须回答的一个输入：被 cordon 的节点不进任何批次
-- （22-multi-node §2.7）。
ALTER TABLE nodes ADD COLUMN cordoned_at TEXT;

-- +goose Down
ALTER TABLE nodes DROP COLUMN cordoned_at;
