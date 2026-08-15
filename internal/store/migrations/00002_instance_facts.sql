-- +goose Up
-- 把节点事实的**放置时快照**固化到 RoleInstance 上。
--
-- 事实有两种用途，规则完全不同（spec §9.4.1）：
--
--   判定条件（requires.resources.memory）→ 用**实时**值，不满足则快速失败
--   配置取值（defaultFrom 算出的 heap）  → 用**放置时快照**，固化
--
-- 第二条是硬要求。解析管线是纯函数、按需重算，若它读的是 node_facts
-- 那张随心跳刷新的表，一次加内存就会让 heap 从 8G 变成 16G、digest 变化、
-- 服务在业务时间被重启。更糟的是某次采集报了 0 字节 → heap=0 → 起不来。
--
-- 事实漂移改由 `mechctl node facts diff/refresh --apply` 显式应用，
-- 那时才更新这一列。

ALTER TABLE role_instances ADD COLUMN facts TEXT NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE role_instances DROP COLUMN facts;
