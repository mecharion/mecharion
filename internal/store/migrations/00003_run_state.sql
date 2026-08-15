-- +goose Up
-- 期望运行态：这个实例现在**应不应该在跑**。
--
-- 它与「它应该长什么样」是两件事，因此不能塞进 spec digest：
-- generation 是物化单位，而停一个服务不改变盘上任何一个字节
-- （internal/spec/digest.go 里记了完整理由）。
--
-- 有了它，一次 `systemctl stop` 才有判据可依（20-continuous-reconcile §2.2）：
--
--   没人说过话 → 观测到停止 = 漂移 → 恢复
--   component stop 之后 → 停止是意图 → 维持，且**手工启动它也要停回去**
--
-- 存在 role_instance 上而不是 component 上：维护窗口经常只针对一台机器
-- （滚动重启、单节点排障），而 component 级的粒度表达不了那件事。
-- `mechctl component stop <name>` 写的是它全部实例，这只是默认值。

ALTER TABLE role_instances ADD COLUMN run_state TEXT NOT NULL DEFAULT 'running';

-- +goose Down
ALTER TABLE role_instances DROP COLUMN run_state;
