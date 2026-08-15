-- +goose Up
-- 初始 schema。分四组：期望状态、观测状态、过程、事件与审计。
--
-- 期望状态是**用户的意图**，必须备份；观测状态随时可以从节点重新采集。
-- 两者严格分开，见 docs/design/07-persistence.md §1.6。

-- ── 期望状态 ────────────────────────────────────────────────────────────

CREATE TABLE sites (
    id          INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL UNIQUE,
    kind        TEXT    NOT NULL,               -- edge | cluster | standalone
    labels      TEXT    NOT NULL DEFAULT '{}',  -- JSON
    created_at  TEXT    NOT NULL
);

CREATE TABLE nodes (
    id          INTEGER PRIMARY KEY,
    site_id     INTEGER NOT NULL REFERENCES sites(id) ON DELETE RESTRICT,
    name        TEXT    NOT NULL,
    address     TEXT    NOT NULL,
    labels      TEXT    NOT NULL DEFAULT '{}',  -- JSON，placement 的 scope 按它分组
    roots       TEXT    NOT NULL DEFAULT '{}',  -- JSON
    volumes     TEXT    NOT NULL DEFAULT '[]',  -- JSON
    status      TEXT    NOT NULL DEFAULT 'unknown',
    created_at  TEXT    NOT NULL,
    UNIQUE (site_id, name)
);

CREATE TABLE components (
    id            INTEGER PRIMARY KEY,
    site_id       INTEGER NOT NULL REFERENCES sites(id) ON DELETE RESTRICT,
    name          TEXT    NOT NULL,
    pack_name     TEXT    NOT NULL,
    pack_version  TEXT    NOT NULL,
    pack_revision INTEGER NOT NULL DEFAULT 1,
    profile       TEXT    NOT NULL DEFAULT '',
    params        TEXT    NOT NULL DEFAULT '{}',  -- JSON，Component 级覆盖
    -- 运维可放松 Pack 声明的 driftPolicy，但不能收紧
    -- （docs/design/06-state-and-drift.md §4.2）
    drift_policy  TEXT    NOT NULL DEFAULT '',
    created_at    TEXT    NOT NULL,
    updated_at    TEXT    NOT NULL,
    UNIQUE (site_id, name)
);

CREATE TABLE config_groups (
    id           INTEGER PRIMARY KEY,
    component_id INTEGER NOT NULL REFERENCES components(id) ON DELETE CASCADE,
    role         TEXT    NOT NULL,
    name         TEXT    NOT NULL,
    members      TEXT    NOT NULL DEFAULT '[]',  -- JSON: 节点名列表
    params       TEXT    NOT NULL DEFAULT '{}',  -- JSON
    UNIQUE (component_id, role, name)
);

-- role_instances 承载 ordinal —— **一次分配后固化，永不重算**。
-- 按节点名排序会让一次扩容改掉所有已有节点的身份（ZooKeeper myid、
-- Kafka node.id），集群当场损坏。见 ADR-0028。
CREATE TABLE role_instances (
    id              INTEGER PRIMARY KEY,
    component_id    INTEGER NOT NULL REFERENCES components(id) ON DELETE CASCADE,
    role            TEXT    NOT NULL,
    node_id         INTEGER NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
    ordinal         INTEGER NOT NULL,
    config_group_id INTEGER          REFERENCES config_groups(id) ON DELETE SET NULL,
    created_at      TEXT    NOT NULL,
    UNIQUE (component_id, role, node_id),
    -- 序号在角色内唯一。移除实例后**不回收**，允许空洞——回收会让新实例
    -- 拿到刚被释放的编号，而集群里其它成员的元数据可能还引用着旧成员。
    UNIQUE (component_id, role, ordinal)
);

-- 依赖绑定同样固化：否则新装一套 ZooKeeper 就可能让已有部署静默改指向
-- （spec §5.2）。
CREATE TABLE pack_bindings (
    id                  INTEGER PRIMARY KEY,
    component_id        INTEGER NOT NULL REFERENCES components(id) ON DELETE CASCADE,
    require_name        TEXT    NOT NULL,
    bound_component_id  INTEGER NOT NULL REFERENCES components(id) ON DELETE RESTRICT,
    created_at          TEXT    NOT NULL,
    UNIQUE (component_id, require_name)
);

-- 密文值。同一 (component, param) 只有一条，version 随轮换递增。
-- version 参与 spec digest —— 轮换必须产生新 generation，否则默认
-- driftPolicy: report 会让它永远发不出去（docs/design/16-secrets.md §5）。
CREATE TABLE secrets (
    id           INTEGER PRIMARY KEY,
    component_id INTEGER NOT NULL REFERENCES components(id) ON DELETE CASCADE,
    param        TEXT    NOT NULL,
    version      INTEGER NOT NULL DEFAULT 1,
    ciphertext   BLOB    NOT NULL,
    created_at   TEXT    NOT NULL,
    updated_at   TEXT    NOT NULL,
    UNIQUE (component_id, param)
);

-- 被 KEK 包裹的数据密钥。单行表；换 KEK 只需重新包裹这几十字节，
-- 不必重加密整库（docs/design/16-secrets.md §3）。
CREATE TABLE vault_keys (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    wrapped_dek BLOB NOT NULL,
    created_at  TEXT NOT NULL
);

-- ── 观测状态（可重建，不必备份） ──────────────────────────────────────

CREATE TABLE instance_status (
    role_instance_id INTEGER PRIMARY KEY REFERENCES role_instances(id) ON DELETE CASCADE,
    digest           TEXT    NOT NULL DEFAULT '',
    generation       INTEGER NOT NULL DEFAULT 0,
    result           TEXT    NOT NULL DEFAULT '',
    workload_state   TEXT    NOT NULL DEFAULT '',
    health           TEXT    NOT NULL DEFAULT '',
    detail           TEXT    NOT NULL DEFAULT '{}',  -- JSON: 资源级明细
    reported_at      TEXT    NOT NULL
);

CREATE TABLE node_facts (
    node_id      INTEGER PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    facts        TEXT NOT NULL DEFAULT '{}',
    capabilities TEXT NOT NULL DEFAULT '{}',
    collected_at TEXT NOT NULL
);

CREATE TABLE drift_reports (
    id               INTEGER PRIMARY KEY,
    role_instance_id INTEGER NOT NULL REFERENCES role_instances(id) ON DELETE CASCADE,
    resource_id      TEXT    NOT NULL,
    changes          TEXT    NOT NULL DEFAULT '[]',
    seen_at          TEXT    NOT NULL,
    UNIQUE (role_instance_id, resource_id)
);

-- 被显式确认过的漂移。有期限、有理由、进审计
-- （docs/design/06-state-and-drift.md §4.1）。
CREATE TABLE suppressions (
    id               INTEGER PRIMARY KEY,
    role_instance_id INTEGER NOT NULL REFERENCES role_instances(id) ON DELETE CASCADE,
    resource_id      TEXT    NOT NULL DEFAULT '',   -- 空 = 整个实例
    reason           TEXT    NOT NULL,
    created_by       TEXT    NOT NULL DEFAULT '',
    created_at       TEXT    NOT NULL,
    expires_at       TEXT    NOT NULL
);

-- ── 过程 ────────────────────────────────────────────────────────────────

CREATE TABLE rollouts (
    id           INTEGER PRIMARY KEY,
    component_id INTEGER NOT NULL REFERENCES components(id) ON DELETE CASCADE,
    state        TEXT    NOT NULL,               -- pending|running|paused|done|failed
    reason       TEXT    NOT NULL DEFAULT '',
    started_at   TEXT    NOT NULL,
    ended_at     TEXT
);

CREATE TABLE rollout_batches (
    id         INTEGER PRIMARY KEY,
    rollout_id INTEGER NOT NULL REFERENCES rollouts(id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    targets    TEXT    NOT NULL DEFAULT '[]',
    state      TEXT    NOT NULL,
    UNIQUE (rollout_id, seq)
);

-- ── 事件与审计 ──────────────────────────────────────────────────────────
--
-- 分开是因为**保留策略不同**：事件是运维诊断素材，可按天清理；
-- 审计是合规要求，保留期以年计且不允许被普通清理任务碰到。

CREATE TABLE events (
    id       INTEGER PRIMARY KEY,
    at       TEXT NOT NULL,
    site_id  INTEGER REFERENCES sites(id) ON DELETE CASCADE,
    kind     TEXT NOT NULL,
    subject  TEXT NOT NULL DEFAULT '',
    payload  TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE audit (
    id       INTEGER PRIMARY KEY,
    at       TEXT NOT NULL,
    actor    TEXT NOT NULL,
    action   TEXT NOT NULL,
    target   TEXT NOT NULL DEFAULT '',
    pack_ref TEXT NOT NULL DEFAULT '',
    result   TEXT NOT NULL,
    detail   TEXT NOT NULL DEFAULT '{}'
);

-- ── 索引 ────────────────────────────────────────────────────────────────
-- 只建有明确查询场景的：Web UI 的列表页与 Rollout 的收敛判定。

CREATE INDEX idx_nodes_site           ON nodes(site_id);
CREATE INDEX idx_components_site      ON components(site_id);
CREATE INDEX idx_instances_component  ON role_instances(component_id);
CREATE INDEX idx_instances_node       ON role_instances(node_id);
CREATE INDEX idx_status_reported      ON instance_status(reported_at);
CREATE INDEX idx_suppressions_expiry  ON suppressions(expires_at);
CREATE INDEX idx_events_at            ON events(at);
CREATE INDEX idx_audit_at             ON audit(at);

-- +goose Down
DROP TABLE IF EXISTS audit;
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS rollout_batches;
DROP TABLE IF EXISTS rollouts;
DROP TABLE IF EXISTS suppressions;
DROP TABLE IF EXISTS drift_reports;
DROP TABLE IF EXISTS node_facts;
DROP TABLE IF EXISTS instance_status;
DROP TABLE IF EXISTS vault_keys;
DROP TABLE IF EXISTS secrets;
DROP TABLE IF EXISTS pack_bindings;
DROP TABLE IF EXISTS role_instances;
DROP TABLE IF EXISTS config_groups;
DROP TABLE IF EXISTS components;
DROP TABLE IF EXISTS nodes;
DROP TABLE IF EXISTS sites;
