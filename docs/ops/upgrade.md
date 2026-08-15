# 升级

这份文档讲的是**控制面自身**的升级——`mechd`/`mechlet` 这两个进程换成新版本。组件升级（`mechctl component upgrade`，滚动升级你部署的 postgresql/kafka 之类）是完全不同的另一件事，见 [21-upgrade-and-rollback.md](../design/21-upgrade-and-rollback.md)。

## 升级前先手动备份

```bash
mechctl backup create --out /mnt/backup/mechd-pre-upgrade-$(date +%Y%m%d).db
```

**这一步现在是手动的，不是自动的**——`mechlet install` 不会替你先打一份快照再动手。升级前打一份的理由很直接：新版本的数据库迁移一旦跑过，`mechd serve` 的自动迁移（下面会讲）没有对应的"自动降级"命令，出问题时唯一可靠的退路是恢复到升级前的这份备份。

## 升级步骤

mechd 与 mechlet **始终同步升级**，不支持只升级其中一个（控制面要向后兼容 agent，但两者的版本纪律是绑在一起走的，见 [10-cli §11](../design/10-cli.md#11-mechd--mechlet)）。

```bash
# 1. 备份（见上）

# 2. 把新版本的四个二进制放到与当前 mechctl/mechlet 同一个目录下
#    （install 期望 mechctl/mechpack/mechd/mechlet 四个一起在场）

# 3. 重新跑一遍 install——这一步只是把新二进制装进版本目录、
#    原子切换软链，不会动正在跑的进程
./mechlet install --standalone

# 4. 真正的切换：重启两个服务
systemctl restart mecharion-mechd mecharion-mechlet

# 5. 确认新版本在跑
mechctl version
journalctl -u mecharion-mechd -n 50
```

**第 3 步和第 4 步是两件事，容易漏掉第 4 步**：`mechlet install` 只把新二进制装好、把 `current` 软链原子切换到新版本目录，这一步本身是安全的（不影响正在跑的进程——Linux 允许继续执行一个已经被 unlink 的可执行文件）。但已经在跑的 `mecharion-mechd`/`mecharion-mechlet` 进程不会因为软链变了就自动换代码，必须显式 `systemctl restart` 才会真正切到新版本。跑完第 3 步就以为升级完成、没做第 4 步，是这套流程里最容易踩的坑。

## 数据库迁移

`mechd serve` **每次启动**都会自动跑完全部未应用的 schema 迁移（`goose`，迁移文件嵌进二进制，不依赖外部工具或网络）。第 4 步重启 mechd 时，这一步会自动发生，不需要额外的迁移命令。

**没有自动降级**：如果新版本的迁移跑完之后发现需要回滚到旧版本，schema 层面没有一条命令能把迁移撤销回去——`internal/store` 内部确实有 `Down`/`DownTo` 这两个方法，但它们目前**只用于测试与内部验证，没有暴露给运维方的命令入口**。真正需要回退时的唯一可靠路径是：停 mechd → 用升级前那份备份（见上）覆盖 `mechd.db` → 换回旧版本的二进制 → 重启。

## 多节点：先控制面后 agent

多节点形态下，先升级 mechd（控制面），确认它稳定运行之后再逐个升级各节点的 mechlet——顺序反过来（先 agent 后控制面）在升级窗口期间会让新版本的 mechlet 对着旧版本的 mechd 说话，控制面向后兼容 agent 是这套系统的兼容性方向，反过来没有对应的保证。

## 目前没有的

如实说明边界：

- **没有 release acceptance 从旧版本自动升级的验收测试**——这是 D8（详见仓库 `docs/dev/M10-boundary-and-contract/plan.md`）的范围，目前每次跨版本升级前建议先在测试环境走一遍完整流程，而不是直接对生产做。
- **没有升级兼容矩阵**——"从 v0.1.0 升到 v0.3.0 是否安全"目前没有一份正式声明；0.y.z 阶段本来就不承诺兼容性（见 `CHANGELOG.md`），跨越较大版本号时更要先在测试环境验证。
