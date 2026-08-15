-- +goose Up
--
-- 节点吊销。
--
-- **与 `node remove` 是两件事**：remove 把节点从册子上抹掉（换硬件、退役），
-- revoke 保留那一行但切断它——「这台机器被偷了/被攻破了，先断掉，
-- 但我还要看它上面装过什么」。
--
-- 吊销走**应用层检查**而不是 CRL（ADR-0034）：被吊销的证书握手仍会成功，
-- 但任何 RPC 都会被拒。代价写在那个 ADR 里。
ALTER TABLE nodes ADD COLUMN revoked_at TEXT;

-- +goose Down
ALTER TABLE nodes DROP COLUMN revoked_at;
