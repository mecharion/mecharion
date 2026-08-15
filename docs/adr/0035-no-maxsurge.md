# ADR-0035: 滚动升级只有 maxUnavailable 与 canary，不做 maxSurge

- **状态**：已接受
- **日期**：2026-08-06
- **相关**：[ADR-0028](0028-stable-ordinals.md)、[ADR-0008](0008-immutable-generation-linkinto.md)

## 背景

M7 要做多节点分批 Rollout。从 Kubernetes 来的用户会预期两个旋钮：

```yaml
maxUnavailable: 1    # 最多同时不可用几个
maxSurge: 1          # 最多临时多起几个
```

`maxSurge` 是 Kubernetes 滚动升级实现「零中断」的关键：先多起一个新版本
副本，就绪之后再摘掉一个旧的，任一时刻可用副本数不低于期望值。

## 候选

| 项目 | 参数 | 前提 |
|---|---|---|
| Kubernetes Deployment | `maxUnavailable` + `maxSurge` | 副本**无状态且可漂移**，多起一个只是多占一份 CPU/内存 |
| Nomad `update` | `max_parallel` + `canary` + `healthy_deadline` + `auto_revert` | canary 是**额外**分配，同样依赖可漂移 |
| Cloudera Manager 滚动重启 | 批大小 + 失败阈值 | 有状态服务，**没有** surge 概念 |
| Ansible | `serial:` | 只有批大小 |

## 决策

**只提供 `maxUnavailable` 与 `canary`（首批大小），不做 `maxSurge`。**

```yaml
rollout:
  maxUnavailable: 1     # 默认 1；quorum 角色强制 ≤ (N-1)/2
  canary: 1             # 首批大小，默认 1；设 0 关闭
```

这里的 `canary` 与 Nomad 的**不是一回事**：Nomad 的 canary 是额外起一个
实例，这里的 canary 只是「第一批只动一台」——是分批策略，不是额外副本。

## 后果

**理由（也是代价的根源）**

`maxSurge` 的前提是「同一个角色可以临时多出一个实例」。Mecharion 管的是
裸机上的实例，它们有：

- **固化的 ordinal**（[ADR-0028](0028-stable-ordinals.md)）——序号是身份的一部分，
  不是可替换的编号
- **固定的数据目录**——升级过程中一个字节都不动，是 M6 的验收判据
- **固定的端口**——同一台机器上起第二份必然撞端口

因此「多起一个」在这里不是配置问题，是**那台机器上没有第二份数据目录**。
硬要支持就得引入实例漂移，而那需要一个调度器——[10-cli §4.2](../design/10-cli.md)
已经明确写了 Mecharion 没有调度器，`drain` 不迁移实例。

**代价**

- **做不到严格零中断。** 每个实例升级时都有一次真实的停机窗口（停旧版 →
  切软链 → 起新版 → 健康检查）。可用性靠「同时只动 `maxUnavailable` 个」
  维持，而不是靠「总副本数不下降」。对单实例组件，滚动升级必然有中断。
- **来自 Kubernetes 的用户会有错误预期。** 文档必须正面写清这一点，
  而不是让他们在找不到 `maxSurge` 时自己猜。
- 将来若要支持无状态、可漂移的部署形态（k8s runtime 是[已预留的扩展点](0017-k8s-extension-reserve.md)），
  `maxSurge` 要重新讨论——但那时它属于那个 runtime，不属于裸机形态。
