-- +goose Up
--
-- join token：一次性（或有限次）凭据，换一张节点客户端证书。
--
-- **只存哈希**，与 admin token 同一条纪律：库被拖走时 token 不能直接拿去用。
--
-- node_name 为空表示**不绑名**——批量预置（cloud-init / 镜像）时名字先到
-- 先得。绑名的 token 只能签出该 CN 的证书（ADR-0034）。
CREATE TABLE join_tokens (
    id         INTEGER PRIMARY KEY,
    site_id    INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    token_hash TEXT    NOT NULL UNIQUE,
    node_name  TEXT    NOT NULL DEFAULT '',
    expires_at TEXT    NOT NULL,
    max_uses   INTEGER NOT NULL DEFAULT 1,
    used       INTEGER NOT NULL DEFAULT 0,
    revoked_at TEXT,
    created_at TEXT    NOT NULL,
    created_by TEXT    NOT NULL DEFAULT ''
);

-- 校验 token 时按哈希查，那是唯一的查询路径。
CREATE INDEX idx_join_tokens_site ON join_tokens(site_id);

-- +goose Down
DROP TABLE IF EXISTS join_tokens;
