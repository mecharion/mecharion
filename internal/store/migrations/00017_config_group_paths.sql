-- +goose Up
-- ConfigGroup 上的多盘绑定（spec §8.6）。
--
-- 路径名 → 卷名列表，例如 `{"dataDirs":["data1","data2"]}` 表示
-- 这一组机器的 dataDirs 落在 /data1 与 /data2 上。
--
-- 求值代码（render/paths.go 的 resolveOnePath）**从 M2 起就写好了**，
-- 一直读的是 mechd 侧一个 `return nil` 的桩——因为这一列不存在。
-- ADR-0021 的原始动机场景（20 台 4 盘、5 台 12 盘的 DataNode）
-- 因此从来没有真的能配出来。
ALTER TABLE config_groups ADD COLUMN paths TEXT NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE config_groups DROP COLUMN paths;
