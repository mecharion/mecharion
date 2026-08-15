-- +goose Up
-- 上一个版本：`component rollback` 不带参数时的目标。
--
-- 记在 components 行上而不是单开一张历史表：**回滚只关心「上一个」**，
-- 而完整的版本历史在 events / audit 里已经有了（那里每条都带时间与操作者）。
-- 一张只被一个查询用到的历史表，维护成本高于它的价值。
--
-- 只在**版本真的变了**时更新。同一版本重复 deploy 不该把 previous 冲掉——
-- 否则一次无意义的重复操作会让回滚目标变成它自己。
ALTER TABLE components ADD COLUMN previous_version  TEXT    NOT NULL DEFAULT '';
ALTER TABLE components ADD COLUMN previous_revision INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE components DROP COLUMN previous_revision;
ALTER TABLE components DROP COLUMN previous_version;
