-- +goose Up
-- 孤儿实例：**机器上还在、但下发里没有**的角色实例。
--
-- 出现的原因通常是组件被移除、或某次解析失败让它没被下发。两种情形在
-- 节点侧分辨不了，因此 mechlet **只报不删**——卸载不可逆，而「mechd 少发
-- 了一条」与「用户真的删了这个组件」长得一模一样（20-continuous-reconcile §2.4）。
--
-- 记在节点上而不是组件上：mechd 里可能**已经没有那个 Component 了**，
-- 挂不到 role_instances 上去。
--
-- first_seen_at 与 last_seen_at 分开记，是因为它们回答的是不同的问题：
-- 「这东西什么时候被落下的」（多半对得上某次变更）与「它现在还在不在」。
-- 只记一个时间戳的话，一个三个月前的残留与今天刚出现的看起来一样。
CREATE TABLE node_orphans (
    node_id       INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    instance_key  TEXT    NOT NULL,
    first_seen_at TEXT    NOT NULL,
    last_seen_at  TEXT    NOT NULL,
    PRIMARY KEY (node_id, instance_key)
);

-- +goose Down
DROP TABLE node_orphans;
