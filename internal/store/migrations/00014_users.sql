-- +goose Up
--
-- Web UI 的用户（M8 第 2 步，23-web-ui §3.1）。
--
-- **没有 role 列**，这是刻意的（ADR-0037）：现阶段登录即全权。建一个只有
-- 一个取值的角色列，等于替将来挖坑——真要分角色时表结构、接口、审计都得
-- 重来一遍，而那个空字段只会在中间这段时间制造「看起来支持其实不支持」。
--
-- `password_hash` 存的是 **argon2id 的完整编码串**，形如：
--
--   $argon2id$v=19$m=65536,t=3,p=4$<base64 盐>$<base64 摘要>
--
-- 参数写在串里而不是写在代码里，因此**将来调强参数不需要迁移**：老用户的
-- 口令用他们当初的参数验，下次改口令时自然升到新参数。这是 argon2 编码格式
-- 存在的理由，也是不要自己拼一个「盐 + 摘要」两列的理由。
CREATE TABLE users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,
    created_at    TEXT    NOT NULL,
    updated_at    TEXT    NOT NULL
);

-- +goose Down
DROP TABLE users;
