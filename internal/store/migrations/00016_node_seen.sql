-- +goose Up
-- nodes.status 不再声称「在不在线」，只答「出现过没有」。
--
-- 它此前只有一条写入路径（Register 写 online），没有人写回 offline，
-- 于是一台 agent 早就没了的机器会永远显示在线（22-multi-node §6.13）。
-- 一个只进不出的状态列没法靠补一条写路径修好——mechd 重启、进程被
-- SIGKILL、机器断电各有各的漏法，每补一条就多一处要维护的对称性。
--
-- 因此把「在线」这个语义整个从存储里拿掉：它由读的那一刻的长连接算出来。
-- 留在列里的是一件永远不会过期的事实——**来过就是来过**。
UPDATE nodes SET status = 'seen' WHERE status = 'online';
UPDATE nodes SET status = 'pending' WHERE status = 'offline';

-- +goose Down
UPDATE nodes SET status = 'online' WHERE status = 'seen';
UPDATE nodes SET status = 'offline' WHERE status = 'pending';
