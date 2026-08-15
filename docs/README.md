# Mecharion 设计文档

**Mecharion** (*MEK-uh-RY-un*, rhymes with *Orion*) · 社区缩写 **m7n**

> From **mech**anism + **Arion**, the immortal horse of Greek myth.

本目录记录 Mecharion 的完整设计：**系统是什么样**（`design/`）、**为什么是这样**（`adr/`）、**接口契约是什么**（`spec/`）、**外部视角审阅过什么**（`review/`）、**当前正在怎么做**（`dev/`）、**运维方要做什么**（`ops/`）。

---

## 为什么有这套文档

Mecharion 的许多设计选择——为什么顶层对象叫 `Site` 而不是 `Cluster`、为什么控制面用 SQLite 而不是 BoltDB、为什么 Pack 的物化目录必须只读——都不是显然的。它们是在对比了 Ansible、Cloudera Manager、Ambari、Kubernetes、Nomad、Helm、Consul、Balena 等既有实践之后做出的取舍，每个选择都有对应的代价。

如果只留下结论，后来者会在不理解代价的情况下推翻它们，或者在不理解意图的情况下沿用它们。因此：

- **`design/`** 描述当前系统的形态，面向要理解或使用系统的人。它是可变的，跟随实现更新。
- **`adr/`** 记录每一个重要决策的背景、调研过的候选方案、选择理由和承担的代价。它是**只追加不修改**的——推翻一个决策的方式是写一份新的 ADR 并把旧的标记为「已取代」，而不是编辑历史。
- **`spec/`** 是对外契约（Pack 格式、API、协议）。破坏性变更必须升版本号。
- **`review/`** 是某个时间点对整个项目的外部视角审阅快照，按日期归档，**发布后不再修改**。
- **`dev/`** 是里程碑执行过程中的过程性记录（任务拆解、执行日志、真机验收踩的坑）。它**不是**设计结论的来源——收尾时会把值得留存的结论回写进 `design/adr`，详见 [dev/README.md](dev/README.md)。分开的原因很直接：`design/decision-log.md` 已经因为混入实施日记而变得臃肿（见 [REV-027](review/20260809/06-defect-register-and-roadmap.md)），再让过程性内容继续挤占 `design/adr` 只会让这两份文档的可信度继续下降。
- **`ops/`** 是任务导向的运维手册（备份恢复、证书、升级、故障排查）——回答"我现在要做一件具体的事，该敲什么命令"，不是"系统为什么长这样"。离线优先（[ADR-0015](adr/0015-offline-first-hermetic.md)）要求这份内容能在没有网络、没有 mecharion.dev 的情况下从本地仓库/发布制品里读到，因此它留在核心仓，不是只发布到官网。

---

## 阅读顺序

**第一次了解本项目**，按顺序读：

1. [设计总览与设计原则](design/00-overview.md) — 项目定位、目标与非目标、七条设计原则
2. [总体架构](design/01-architecture.md) — mechlet / mechd / mechctl 的职责与关系
3. [对象模型](design/02-object-model.md) — Site / Component / Role / RoleInstance
4. [Pack 概念设计](design/03-pack.md) — 组件包是什么、如何离线分发

**要开发 Pack**，读 [3](design/03-pack.md) → [4](design/04-paths-and-storage.md) → [spec/pack-v1.md](spec/pack-v1.md)

**要参与内核开发**，全部读完，重点 [5](design/05-runtime.md) [6](design/06-state-and-drift.md) [7](design/07-persistence.md)

---

## 目录

### design/ — 系统设计

| 文档 | 内容 |
|---|---|
| [00-overview.md](design/00-overview.md) | 项目定位、目标与非目标、七条设计原则 |
| [01-architecture.md](design/01-architecture.md) | 组件职责、连接模型、引导与自升级 |
| [02-object-model.md](design/02-object-model.md) | Site / Component / Role / RoleInstance / Node / Rollout |
| [03-pack.md](design/03-pack.md) | Pack 概念、blob 内容寻址、thin/thick 分发、hermetic 约束 |
| [04-paths-and-storage.md](design/04-paths-and-storage.md) | generation 不变式、linkInto、Node volumes 与多磁盘 |
| [05-runtime.md](design/05-runtime.md) | Runtime 抽象、systemd / docker / compose、k8s 扩展预留 |
| [06-state-and-drift.md](design/06-state-and-drift.md) | 期望状态、持续调和、漂移策略、升级与回滚 |
| [07-persistence.md](design/07-persistence.md) | mechd 与 mechlet 的持久化方案 |
| [08-security.md](design/08-security.md) | 信任模型、Pack 完整性校验、传输安全、审计 |
| [09-naming-conventions.md](design/09-naming-conventions.md) | 命名体系、词根、命名空间与域名约定 |
| [10-cli.md](design/10-cli.md) | CLI 名词与动词、输出格式、退出码、连接解析 |
| [11-resource-engine.md](design/11-resource-engine.md) | 资源引擎接口、调和的七个阶段、16 种资源类型清单 |
| [12-spec-and-state.md](design/12-spec-and-state.md) | 已解析规格（mechd↔mechlet 的契约）与 mechlet 本地状态 |
| [13-mechd.md](design/13-mechd.md) | 控制面的内部分层、API 表面、单机形态、断连重连 |
| [14-placement.md](design/14-placement.md) | 放置：从用户指定的节点到 RoleInstance 列表；约束校验 |
| [15-render-pipeline.md](design/15-render-pipeline.md) | 解析管线：参数链、依赖绑定、渲染、封装 |
| [16-secrets.md](design/16-secrets.md) | 密钥的生成、信封加密存储、下发与注入、轮换 |
| [17-protocol.md](design/17-protocol.md) | mechd ↔ mechlet 的 gRPC 协议与版本协商 |
| [18-hooks.md](design/18-hooks.md) | hook 的执行语义、密钥注入、失败处理 |
| [19-container-runtime.md](design/19-container-runtime.md) | docker / compose runtime：标签纪律、镜像来源、compose 的落地差异 |
| [20-continuous-reconcile.md](design/20-continuous-reconcile.md) | 持续调和、漂移检测与处理、期望运行态、孤儿 |
| [21-upgrade-and-rollback.md](design/21-upgrade-and-rollback.md) | 升级、自动回滚、失败 digest 锁、generation 与镜像回收、Rollout |
| [22-multi-node.md](design/22-multi-node.md) | 多节点：加入与身份、mTLS、分批 Rollout 与健康门禁 |
| [23-web-ui.md](design/23-web-ui.md) | Web UI：前端栈、登录与会话、验证码、由 params schema 生成表单 |
| [24-lifecycle-completion.md](design/24-lifecycle-completion.md) | 补齐生命周期的另一半：remove、orphans、apply -f、restart；§5 是 M9 收尾审查 |
| [25-roadmap.md](design/25-roadmap.md) | 里程碑、技术选型与平台矩阵 |
| [decision-log.md](design/decision-log.md) | 全部决策项索引，可追溯到对应 ADR |

### adr/ — 架构决策记录

见 [adr/README.md](adr/README.md) 获取完整列表与状态。

### spec/ — 对外契约

| 文档 | 状态 |
|---|---|
| [pack-v1.md](spec/pack-v1.md) | **draft-stable**（2026-08-02 定型骨架），v0.1.0 发布后严格冻结 |

### review/ — 阶段性全面审阅

按日期归档，一经发布不再修改。见各日期目录下的 `README.md`。

| 目录 | 内容 |
|---|---|
| [20260809/](review/20260809/) | 首次全面审阅：产品概念、架构、CLI/API/UI、代码安全、文档与开源成熟度、缺陷台账与路线图 |

### dev/ — 开发迭代记录

过程性内容，不是设计结论的来源，见 [dev/README.md](dev/README.md)。

| 目录 | 对应里程碑 |
|---|---|
| [M10-boundary-and-contract/](dev/M10-boundary-and-contract/) | M10：契约与边界收口，见 [plan.md](dev/M10-boundary-and-contract/plan.md) |

### ops/ — 运维手册

任务导向，面向要运行这套系统的人，不是要理解或修改它的人。

| 文档 | 内容 |
|---|---|
| [backup-and-restore.md](ops/backup-and-restore.md) | 备份哪些东西、`mechctl backup create`、恢复步骤 |
| [certificates.md](ops/certificates.md) | 自签 CA、mTLS 节点证书、轮换、吊销、CA 丢失的灾难恢复 |
| [upgrade.md](ops/upgrade.md) | 控制面（mechd/mechlet 自身）升级步骤，区别于组件升级 |
| [troubleshooting.md](ops/troubleshooting.md) | `/healthz` vs `/readyz`、日志、`--local` 诊断、常见故障信号 |

### brand/ — Logo

品牌视觉资产，不是设计文档。见 [brand/README.md](brand/README.md)。

---

## 文档约定

- **语言**：当前为中文。面向国际社区的英文版本应在首次公开发布前补齐，届时中文版移至 `docs/zh/`，英文版占据默认路径。
- **术语**：首次出现的核心名词使用「英文（中文）」形式，如 `Site`（站点）。CLI、API、配置文件中一律使用英文。
- **systemd 相关**：一律使用 systemd 自己的术语 **unit**，不写 "systemd service"——`Service` 一词在 Mecharion 中被刻意保留不用，详见 [ADR-0003](adr/0003-object-model-naming.md)。
