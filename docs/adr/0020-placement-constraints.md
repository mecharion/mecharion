# ADR-0020: 角色间放置约束（affinity / antiAffinity）

- **状态**：已接受
- **日期**：2026-08-02
- **相关**：[ADR-0006](0006-multi-role-pack.md)、[ADR-0003](0003-object-model-naming.md)

## 背景

一 Pack 多角色（[ADR-0006](0006-multi-role-pack.md)）确立后，`cardinality` 约束了**数量**、`requires` 约束了**时序**，但没有任何机制约束**位置**。

真实需求普遍存在：

| 场景 | 约束 |
|---|---|
| HDFS NameNode / SecondaryNameNode | 必须不同节点——同节点时 SNN 无法承担元数据恢复职责 |
| PostgreSQL primary / replica | 必须不同节点——同节点时不提供任何故障保护，且端口冲突 |
| ZooKeeper 三实例 | 必须互不同节点，最好跨机架 |
| HDFS JournalNode | 建议跨机架分散 |
| DataNode / NodeManager | **建议同节点**（计算贴近数据） |

前四条是反亲和，最后一条是亲和。当前 spec 全部无法表达。

## 关键前提：Mecharion 没有调度器

节点由用户在 Component 中**显式指定**。因此这些约束不是**调度输入**（不需要打分函数、偏好排序、装箱算法），而是 mechd 放置阶段的**校验规则**。

这个区别让设计比 Kubernetes 的 `podAntiAffinity` 简单一个量级。

## 候选方案与调研

| 系统 | 机制 | 是否驱动调度 | 复杂度 |
|---|---|---|---|
| **Kubernetes** | `podAffinity` / `podAntiAffinity` + `topologyKey`，`requiredDuringScheduling` / `preferredDuringScheduling…` | ✅ 驱动调度器 | 高（含权重打分） |
| **Nomad** | `constraint`（硬）/ `affinity`（软，带 weight）/ `spread`（均匀分布） | ✅ | 中 |
| **Cloudera Manager** | **无声明式表达**，向导阶段人工提示 | ❌ | 低（但能力缺失） |
| **Ambari** | Blueprint host group——把组件分配到主机组，同组即同置 | ❌ | 低（表达力弱，无法表达「必须不同」） |

### 从调研得到的结论

**① Kubernetes 的表达形态可以借鉴，机制不必借鉴。** `topologyKey` 的抽象非常好——「不同节点」与「不同机架」是同一个概念在不同粒度上的实例。但其权重打分体系是为调度器服务的，Mecharion 不需要。

**② Cloudera 的缺失是真实痛点。** CM 在部署 HDFS 时只在向导里提示「建议 SNN 与 NN 分开」，无法阻止，也无法在后续变更时重新校验。这正是要避免的。

**③ Ambari 的 host group 表达力不足。** 它能表达「同置」（放进同一个组），无法表达「必须分开」。

## 决策

Pack 顶层新增 `placement`，声明**角色之间的位置关系**：

```yaml
placement:
  - antiAffinity: [namenode, secondarynamenode]
    scope: node
    enforcement: required
    reason: "SNN 与 NN 同节点时无法承担元数据恢复职责"

  - antiAffinity: [zookeeper]        # 单元素 = 同角色的多个实例之间互斥
    scope: node

  - antiAffinity: [journalnode]
    scope: rack                       # 任意 Node label key
    enforcement: preferred

  - affinity: [datanode, nodemanager]
    scope: node
    enforcement: preferred
```

| 字段 | 默认 | 说明 |
|---|---|---|
| `antiAffinity` / `affinity` | — | 角色名列表，二选一 |
| `scope` | `node` | `node`，或任意 Node label key（`rack` / `zone` / `az`） |
| `enforcement` | `required` | `required`：违反则**拒绝放置**；`preferred`：**仅告警** |
| `reason` | — | 强烈建议——会出现在校验失败的错误信息中 |

**单元素 `antiAffinity` 表示同角色的多个实例之间互斥**，这样 ZK 三实例分散不需要额外语法。

`scope` 为 label key 且节点缺该 label 时：`required` 报错（无法验证不等于通过），`preferred` 告警。

### 放在顶层而非角色内

约束天然是**双边关系**，放在某个角色下会产生「谁拥有这条约束」的歧义，也会导致同一条约束被两边重复声明。顶层声明是唯一无歧义的位置。

### 边界

`placement` 只表达**角色之间**的关系：

- 「某角色必须落在带 X 标签的节点上」→ 部署时的节点选择，由用户在 Component 中指定
- 「节点必须具备某种能力」→ `requires.capability`

三者不重叠，各有明确归属。

## 理由

**因为没有调度器，这个功能的成本极低而价值很高。** 实现是 mechd 放置阶段的一个校验 pass（约 150 行），无数据模型变更、无新对象、无调度算法。

而它防止的是一类**在部署时完全无声、在故障时才暴露**的错误：SNN 和 NN 装在同一台机器上，系统一切正常，直到那台机器挂掉时才发现元数据无法恢复。这类错误正是 ALM 工具应当在源头拦截的。

`reason` 字段进入错误信息是刻意设计——一条「违反了 antiAffinity 约束」的报错对运维毫无帮助，「SNN 与 NN 同节点时无法承担元数据恢复职责」才是可行动的。

## 后果

### 收益

- 一类高危配置错误在放置阶段被拦截
- `scope` 复用 Node labels，支持机架/可用区级约束，零额外机制
- Pack 作者可以把领域知识（哪些角色不能同处）固化进 Pack，而非依赖用户知晓
- 每次变更放置时自动重新校验，不只是首次部署

### 代价

- **`preferred` 在无调度器的前提下只能是告警**，无法像 Kubernetes 那样「尽量满足」。这是诚实的限制，必须在文档中说清楚，否则用户会误以为引擎会帮忙优化放置
- **Pack 作者可能过度约束**：写死 `required` 会让某些合法的小规模/测试部署（如单机跑全套 HDFS）无法进行。需提供部署时的显式豁免开关（`--ignore-placement <约束>`），并记入审计
- **多约束叠加时的诊断复杂度**：约束一多，「为什么这个放置不合法」可能不直观。校验失败输出必须列出**每条被违反的约束及其涉及的具体实例**，不能只报第一条
- **`scope` 依赖 Node label 的正确维护**：`rack` 标签打错会让约束形同虚设或误报

## 参考

- Kubernetes `podAntiAffinity` 与 `topologyKey`
- HashiCorp Nomad `constraint` / `affinity` / `spread`
- Cloudera Manager 的向导式提示（能力缺失的反面案例）
- Apache Ambari Blueprint host group（表达力不足的反面案例）
