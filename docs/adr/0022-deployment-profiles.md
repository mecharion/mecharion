# ADR-0022: 用 profiles 表达部署形态

- **状态**：已接受
- **日期**：2026-08-02
- **相关**：[ADR-0006](0006-multi-role-pack.md)、[ADR-0020](0020-placement-constraints.md)、[ADR-0005](0005-pack-logic-payload-split.md)

## 背景

同一个组件常有多种部署形态：

| 组件 | 形态 |
|---|---|
| Hadoop / HDFS | 单机 standalone / 分布式 distributed / 高可用 ha |
| Kafka | KRaft 模式（combined / separated）/ ZooKeeper 模式 |
| PostgreSQL | 单机 / 主备 |
| Elasticsearch | 单节点 / 多节点集群 |

以 HDFS 为例，三种形态之间的差异有六项：

| 差异项 | standalone | distributed | HA |
|---|---|---|---|
| 角色集合 | NN, DN | ＋SNN | ＋JournalNode, ZKFC；NN 变 2 个 |
| cardinality | NN=1, DN=1 | DN=1-N | NN=2, JN=3, DN=3-N |
| placement | 无 | NN ⊥ SNN | NN⊥NN, JN⊥JN, NN≡ZKFC |
| 参数默认值 | replication=1 | =3 | =3 |
| 参数集 | — | — | ＋nameservice_id |
| 外部依赖 | 无 | 无 | ＋zookeeper pack |

**关键观察：这六项 Pack 格式全都已经能表达，只是不能「有条件地」表达。** 问题不是缺能力，而是缺一条**变化轴的命名**。

## 候选方案与调研

### 方案 A：拆成三个独立 Pack

`hadoop-standalone` / `hadoop-distributed` / `hadoop-ha`

- ✅ 每个包内部简单，无条件逻辑
- ❌ 载荷与 90% 模板重复
- ❌ 三个包之间版本漂移
- ❌ **形态迁移变成卸载重装**

最后一条是决定性的。「先上非 HA，半年后加 HA」是真实的运维动作，对 HDFS 这类有状态系统，重装不可接受。

### 方案 B：enum 参数 ＋ `when`

```yaml
params:
  mode: { type: enum, values: [standalone, distributed, ha] }
roles:
  - name: journalnode
    when: '{{ eq .Params.mode "ha" }}'
```

看似不引入新概念，实则需要在**六个地方各加一次条件机制**：

1. role 的 `when`（新增）
2. 条件 cardinality（新增）
3. 条件 placement（新增）
4. 条件参数可见性（新增）
5. 条件参数默认值（新增）
6. 条件 requires（新增）

而且 lint 无法验证「HA 形态是否自洽」——它看到的只是一堆彼此独立的条件表达式。

### 方案 C：一等的 `profiles` 概念 ⭐

调研对照：

| 系统 | 机制 | 观察 |
|---|---|---|
| **Ambari** | Blueprint——把组件分配到 host group，不同 blueprint 即不同形态 | 形态是**部署侧**的产物，Pack（Stack）本身不表达形态 |
| **Cloudera Manager** | 无形态概念；HA 通过独立的「启用 HA」向导完成 | 形态迁移是**特写的一次性流程**，不可复用 |
| **Helm** | `values.yaml` 中的开关（`ha.enabled`）＋ 模板里大量 `if` | 即方案 B，Chart 复杂度随开关数量组合爆炸 |
| **Maven / Spring** | `profiles` — 具名的配置预设 | **术语与语义最贴合** |
| **Docker Compose** | `profiles:` — 给 service 打标签，按 profile 选择性启用 | 方向一致（形态 = 服务子集 + 配置） |

Helm 的经验是最有力的反面证据：`if .Values.ha.enabled` 遍布模板是 Chart 复杂度失控的主要来源，而这正是方案 B 的结局。

## 决策

**引入 `profiles`——Pack 顶层的具名部署形态。**

### 核心性质：profile 是「预设」，不是「变体」

profile 在 **mechd 放置阶段被解析掉**——解析后得到具体的 `(roles, cardinality, placement, params, requires)`，**mechlet、资源引擎与 Runtime 完全不知道 profile 存在**。

这是本决策最重要的性质：**profile 不给引擎增加任何概念，只给 Pack 作者与用户增加一个表达维度。**

### 可覆盖的五项（穷尽）

| 项 | 覆盖语义 |
|---|---|
| `roles[<name>].enabled` | `false` 时该角色在本形态中不存在 |
| `roles[<name>].cardinality` | 覆盖角色声明的值 |
| `placement` | **整体替换**顶层 placement |
| `params` | 覆盖 `default`，或追加本形态专有参数 |
| `requires` | **合并**进顶层 requires |

### 不可覆盖的部分

`blobs` / `resources` / `workload` / `paths` / `shared` 不可被 profile 覆盖。这些位置的差异用**既有的** `.Profile` 变量 ＋ `when:` 表达。

**这条边界是刻意的**：profile 只管**结构**（哪些角色、多少个、能否同处一台机器），不管**内容**。否则 profile 会退化成一个什么都能覆盖的万能层，失去可分析性。

### 形态迁移

```yaml
upgradeFrom: [distributed]
```

未列出的迁移路径被拒绝。数据面迁移动作（如 `-initializeSharedEdits`）由 Pack 作者用 hooks 完成，配合 `scope: once`。

## 理由

**决定性理由：变化轴只有一条，就只该命名一次。**

方案 B 需要六个独立的条件机制去表达同一件事，这违反[原则五（抽象只在真正不同处划界）](../design/00-overview.md#原则五抽象只在真正不同处划界)——真正不同的是「部署形态」这一个维度，而不是六个互不相关的条件。

**次要理由：形态成为可分析的对象。**

有了 profile，`mechpack lint` 可以对**每个形态独立验证**：

- cardinality 与 placement 是否**存在一个可满足的放置方案**（「3 个 journalnode 互斥但 minNodes=2」在打包阶段就报错）
- 模板引用的参数是否在该形态的参数集内（对每个 profile 用 `missingkey=error` 渲染一遍）

方案 B 下这两项验证都做不到——lint 面对的只是一堆彼此独立的布尔表达式。

**第三个理由：用户侧的结构正确。**

形态选择是部署向导的**第一步**（决定需要几台机器、装哪些角色），而不是 200 个参数表单里的一个下拉框。把它建模成一等概念，UI 与 CLI 才能给出正确的信息架构：

```
$ mechctl pack show hadoop
部署形态：
  standalone   (默认)  ≥1 节点   单机，无冗余，仅供开发测试
  distributed          ≥2 节点   分布式，无 HA，NameNode 单点
  ha                   ≥4 节点   高可用，双 NameNode + JournalNode 仲裁
```

## 后果

### 收益

- 一个 Pack 覆盖多形态，载荷与模板不重复，无版本漂移
- 形态迁移可表达（`upgradeFrom`），不必卸载重装
- 每个形态的自洽性在**打包阶段**被独立验证
- 引擎侧零新增概念——profile 在放置阶段解析掉
- L1/L2 的 Pack 完全不接触本机制

### 代价

- **Pack 作者多一个概念**：需要判断「这是形态差异还是参数差异」。判据：**改变角色集合或数量的是形态，只改配置值的是参数**
- **模板中仍有 `.Profile` 条件**：profile 消除了结构性条件，但内容差异（hdfs-site.xml）仍需分支。以模板片段组合（`_` 前缀片段 + `{{ template }}`）缓解，未根除
- **`placement` 整体替换而非合并**：profile 覆盖 placement 时必须重写全部约束，顶层的通用约束需要复制。选择替换是因为合并约束列表的语义（同一对角色出现两次怎么办）难以直觉理解，但代价是重复
- **形态迁移的数据面动作仍是 Pack 作者的责任**：Mecharion 只保证结构层面的重新解析与 Rollout，不保证任意形态之间可切换。`upgradeFrom` 是作者的声明，引擎无法验证其真实性
- **lint 规则 19（可满足性检查）实现有复杂度**：本质是一个小规模约束满足问题。规模很小（角色数 <20），暴力搜索足够，但需要正确处理 `scope` 为 label key 的情况

## 参考

- Maven / Spring `profiles`
- Docker Compose `profiles`
- Helm `values` 开关 ＋ 模板条件（复杂度失控的反面案例）
- Apache Ambari Blueprint
- Cloudera Manager 的「启用 HA」独立向导（形态迁移不可复用的反面案例）
