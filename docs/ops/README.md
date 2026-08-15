# 运维手册

## 这是什么

任务导向的操作文档：**我现在要做一件具体的事，该敲什么命令**。这与 [`design/`](../design/) 不同——`design/` 回答"系统为什么长这样"，面向要理解或修改系统的人；这里回答"怎么运行它"，面向要运行这套系统的运维方。

## 目录

| 文档 | 内容 |
|---|---|
| [backup-and-restore.md](backup-and-restore.md) | 备份哪些东西、`mechctl backup create`、恢复步骤 |
| [certificates.md](certificates.md) | 自签 CA、mTLS 节点证书、轮换、吊销、CA 丢失的灾难恢复 |
| [upgrade.md](upgrade.md) | 控制面（mechd/mechlet 自身）升级步骤，区别于组件升级 |
| [troubleshooting.md](troubleshooting.md) | `/healthz` vs `/readyz`、日志、`--local` 诊断、常见故障信号 |

## 为什么留在核心仓，不是只发到官网

`mecharion.dev` 建好之后会展示这份内容（见 [ADR-0019](../adr/0019-namespace-domain.md)），但源头在这里，不在官网仓库。离线优先（[ADR-0015](../adr/0015-offline-first-hermetic.md)）是产品的核心原则——运维方在完全没有网络的现场，应当能从本地的 git 仓库或发布制品里读到这些文档，不依赖能不能访问 `mecharion.dev`。

## 覆盖范围与已知边界

这几份文档只覆盖 [`docs/dev/M10-boundary-and-contract/plan.md`](../dev/M10-boundary-and-contract/plan.md) C6 这一行明确圈定的四个主题（备份恢复、证书、升级、故障排查）。审阅原文（REV-034/06-defect-register-and-roadmap.md §1.4）列过一份更长的运维文档清单——安装/卸载、拓扑、配置参考、稳定性政策等——其中一部分已经在别处覆盖（README 的 quickstart、[10-cli.md](../design/10-cli.md) 的命令参考、[08-security.md](../design/08-security.md) 的信任模型、`CONTRIBUTING.md` 的版本政策），其余留给后续单独立项，不是这次漏做。每份文档末尾也各自列了"目前没有的"，如实标出还没做到的部分，不假装已经覆盖。
