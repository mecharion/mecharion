# 备份与恢复

> mechd 的数据库是期望状态的一部分，不是缓存。丢了它不是"重新扫描一遍就好"——`role_instances.ordinal` 与 `pack_bindings` 都是分配出来的、无法重算的值（[ADR-0028](../adr/0028-stable-ordinals.md)）。见 [07-persistence.md §1.7](../design/07-persistence.md#17-什么必须备份) 的完整背景。

## 备份清单

一次完整备份要分开备份四样东西，**不要打包成一份**：

| | 位置 | 怎么备份 |
|---|---|---|
| mechd 数据库 | `<数据目录>/mechd.db`（缺省 `/var/lib/mecharion/mechd/mechd.db`） | `mechctl backup create --out <路径>` |
| 主密钥 | `/etc/mecharion/secret.key` | 直接拷贝文件，**必须与数据库分开存放** |
| PKI（自签 CA） | `/etc/mecharion/pki/` | 直接拷贝整个目录 |
| blobs（Pack 内容） | `<数据目录>/blobs/` | 可选：可以从 Pack 重新组装，但离线环境下重建成本高，建议一并备份 |

**为什么主密钥必须单独存放**：数据库里的 `secrets` 表是信封加密——密文在数据库里，解密它的主密钥在 `secret.key`。把两者备份进同一份归档，等于把锁和钥匙锁在同一个箱子里，信封加密拆开存放这件事本身就失去了意义（见 [16-secrets §3](../design/16-secrets.md#3-存储信封加密)）。

## 备份数据库

```bash
mechctl backup create --out /mnt/backup/mechd-$(date +%Y%m%d-%H%M%S).db
```

这是一次一致性快照（SQLite `VACUUM INTO`），**不需要停 mechd**——期间数据库照常读写，快照本身仍然是某一个时间点的一致状态，不会拿到写到一半的数据。

`--out` 必须写在**别的盘或别的机器上**。不给这个参数时会落在数据目录下的 `backups/` 子目录（缺省 `/var/lib/mecharion/mechd/backups/`），这只是一个凑手的默认值，不是一个真正的备份策略——同一块盘上的"备份"在磁盘故障时会和原件一起丢，这个默认值只适合"先跑一次看看格式对不对"这类场景，不能拿来当生产备份计划。

## 恢复

前提：这台机器上的 `mechd` 已经停掉（`systemctl stop mecharion-mechd`）。

```bash
systemctl stop mecharion-mechd
cp /mnt/backup/mechd-20260101-000000.db /var/lib/mecharion/mechd/mechd.db
systemctl start mecharion-mechd
```

`mechd serve` 每次启动都会自动跑完未应用的 schema 迁移（`goose`，见 [10-cli §11](../design/10-cli.md#11-mechd--mechlet)）——如果恢复用的备份是旧版本 mechd 打的，启动时会自动把它迁移到当前版本认识的 schema，这一步不需要额外操作。

**反过来不行**：用**更新版本**的 mechd 打出的备份，不能拿一个**更旧版本**的 mechd 去读——旧版本的代码不认识新版本引入的 schema。跨版本恢复时，先把 mechd 本身升级到不早于打备份那个版本，再做恢复。

**恢复数据库不等于恢复完整状态**——如果同时也在恢复主密钥/PKI，要按同一个时间点的三份一起恢复；数据库里的 `secrets` 密文只有配对的那份 `secret.key` 才解得开，PKI 对不上会导致所有节点的 mTLS 全部失败（见 [certificates.md](certificates.md)）。

## 目前没有的：自动化与演练

这里如实说明当前的边界，而不是假装没有的东西已经有了：

- **没有自动定期备份**——上面的命令需要人或外部 cron/systemd timer 来调度，mechd 自己不会主动做。
- **没有升级前自动备份**——`mechlet install`/控制面升级不会替你先打一份快照，见 [upgrade.md](upgrade.md) 里"升级前先手动备份"这一步为什么是手动的。
- **没有正式的恢复演练机制或 RTO/RPO 基线**——这些留给了 D6/D8（详见仓库 `docs/dev/M10-boundary-and-contract/plan.md`），本文档只保证"照着步骤做一遍，能验证是真的可行的"，不代表已经有工具替你定期演练。
