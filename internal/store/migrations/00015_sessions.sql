-- +goose Up
--
-- Web UI 的登录会话（M8 第 3 步）。
--
-- **落库而不是放内存**，与挑战（authn.Store）相反，两者的取舍不同：
--
--   挑战  丢了重来一次，用户多点一下。TTL 三分钟，落库纯属浪费
--   会话  丢了**所有人被登出**——mechd 一次重启就把人踢光，
--         而重启在这个项目里是常规操作（换二进制、改配置）
--
-- 存的是 token 的 **sha256**，不是 token 本身：库被拖走时不能直接拿去冒充。
-- 与 admin token 的处理是同一条（httpapi.go 的 TokenAuth）。
CREATE TABLE sessions (
    token_hash TEXT    PRIMARY KEY,
    user_name  TEXT    NOT NULL,
    created_at TEXT    NOT NULL,
    expires_at TEXT    NOT NULL
);

CREATE INDEX idx_sessions_expires ON sessions(expires_at);

-- +goose Down
DROP INDEX idx_sessions_expires;
DROP TABLE sessions;
