# ADR-0021: 具名 ConfigGroup 取代无名的 Node 级覆盖

- **状态**：已接受
- **日期**：2026-08-02
- **相关**：[ADR-0003](0003-object-model-naming.md)、[ADR-0007](0007-params-custom-subset.md)、[ADR-0009](0009-node-volumes-multidisk.md)

## 背景

同一个 Role 的不同实例经常需要不同配置：

- 20 台 DataNode 是 4 盘机器、5 台是 12 盘机器，`dfs.data.dir` 与堆大小都不同
- 3 台 Kafka broker 部署在 SSD 上，其余在 HDD 上
- 某台机器内存更大，JVM 堆需要单独调

初版设计用「Node 级覆盖」表达：

```yaml
roles:
  datanode:
    nodes: [n1, …, n20]
    params: { max_xcievers: 4096 }
    overrides:
      - { nodes: [n21, …, n25], params: { max_xcievers: 8192 } }
```

## 问题

这套表达能用，但有三个结构性缺陷：

**① 差异没有身份。** 那 5 台机器构成一个有意义的集合（「12 盘机器」），但模型里它只是一个匿名的节点列表。UI 无法呈现，文档无法引用，`mechctl` 无法定位。

**② 差异无法枚举与审计。** 「这个 Component 里一共有几种配置形态、分别差在哪」需要人工比对覆盖块。当覆盖块有十几个、且互相之间有交集时，这件事实际做不到。

**③ 允许无名差异 = 允许配置雪花化。** 一年之后，200 台机器各自略有不同、且没有任何记录说明为什么——这是配置管理领域最经典的失控形态。

## 候选方案与调研

| 系统 | 模型 | 是否允许无名的单实例覆盖 |
|---|---|---|
| **Cloudera Manager** | Role Config Group：每个 Role Instance **必须**属于某个具名组 | ❌ 不允许，想要不同就得建组 |
| **Ambari** | Config Group：同样是具名组 | ❌ |
| **Kubernetes** | 无此概念——不同配置就是不同的 Deployment/StatefulSet | N/A |
| **Ansible** | inventory group_vars / host_vars | ✅ host_vars 就是无名的单主机覆盖 |
| **Puppet** | node 定义 + hiera 层级 | ✅ |

**关键观察**：两个专门做「组件生命周期管理」的产品（CM、Ambari）都选择了强制具名组；两个做「通用配置管理」的产品（Ansible、Puppet）都允许单主机覆盖。

这个分野不是偶然。通用配置管理面对的是异构的、一次性的任务；组件生命周期管理面对的是一个需要长期演进的集群，**其中每一处差异都应当是可解释的**。Mecharion 属于后者。

## 决策

**采用具名 ConfigGroup，取消无名的 Node 级覆盖。**

### 模型

```
Component
└─ Role（来自 Pack 定义）
   ├─ params            对该 Role 全部实例生效（隐式的 default 组）
   └─ ConfigGroup[]     具名子集，覆盖 Role 级取值
        └─ RoleInstance   每个实例属于且仅属于一个组
```

- 每个 Role 有一个隐式的 `default` 组，未被任何具名组包含的实例属于它
- 具名组持有：`params` 覆盖、`paths` 覆盖、成员节点列表
- 组名在 (Component, Role) 内唯一

### 参数解析链

```
Pack 默认值  →  Component 级  →  Role 级  →  ConfigGroup 级
```

**不存在第五层。** 单节点的差异化配置表现为一个只含该节点的组。

### 消除 UX 摩擦：CLI 自动建组

强制建组的代价是「为了改一台机器的一个参数要先建组」。用一条便捷命令抵消：

```bash
mechctl config set -c hdfs-prod -r datanode --node n7 max_xcievers=8192
# → 自动创建 ConfigGroup "node-n7"（成员 [n7]）
#   提示：已创建配置组 node-n7，可用 `mechctl config group rename` 改名
```

用户的操作步数与「无名覆盖」完全相同，但结果是**具名、可枚举、可 diff、可在 UI 中呈现**的。

### 完整命令面

```bash
mechctl config group list    -c hdfs-prod -r datanode
mechctl config group create  ssd-nodes -c hdfs-prod -r datanode --nodes n21,n22
mechctl config group set     ssd-nodes max_xcievers=8192
mechctl config group move    n23 --to ssd-nodes
mechctl config group diff    ssd-nodes default
mechctl config group remove  ssd-nodes           # 成员回落到 default
```

## 理由

**核心理由：允许无名差异，就是允许配置雪花化。**

Cloudera Manager 的强制建组常被用户抱怨麻烦，但它换来的是：任何时候都能回答「这个集群里有几种配置形态、各自为什么不同」。对一个要管理数百节点、生命周期以年计的系统，这个性质的价值高于建组的那点摩擦——而摩擦已经被 CLI 自动建组消除了。

**次要理由：现在做 2–3 天，后期补要迁移用户存量配置。**

| 项 | v1 现在做 | v1 不做、后期补 |
|---|---|---|
| 数据模型 | +1 张表、+1 层解析链 · 1–2 天 | schema 迁移 + **存量配置迁移** |
| CLI | 组 CRUD + 自动建组 · 1 天 | API 形状变更，破坏兼容 |
| UI | 0（M6 的配置页本来就要做，多一层分组不增加工作量） | 同左 |
| 用户侧 | 无 | **存量部署需迁移，有出错风险** |

参数解析链属于「一旦有真实配置在跑就极难改」的东西，早做便宜得多。

**不影响 Pack 格式。** `params` 的声明方式一个字不变，ConfigGroup 只影响部署侧「取值从哪来」。因此这个决定不阻塞 pack/v1 的定稿。

## 后果

### 收益

- 每一处配置差异都有名字、可枚举、可 diff、可在 UI 中呈现为一等对象
- 「这个集群有几种配置形态」是一次查询而非一次人工比对
- 加机器 = 加入某个组，不需要编辑覆盖块的节点列表
- 组可以被 `mechctl config group diff` 直接对比
- 审计事件可以精确到组

### 代价

- **强制建组是真实的心智负担**：即使有自动建组，用户仍需理解「我的实例属于哪个组」这个额外概念。文档需要在最开始就讲清楚，而不是作为高级话题
- **自动建组可能产生大量单成员组**：一个 200 节点集群若被逐台微调，会出现 200 个 `node-*` 组。这在模型上是正确的（差异确实存在），但 UI 需要能折叠展示，且应提供「合并同配置的组」的辅助命令
- **组成员变更是一次变更操作**：把节点从 A 组移到 B 组会触发配置重新渲染与可能的重启。需要在 CLI 中明确提示影响面
- **多出一层解析**：调试「这个参数最终为什么是这个值」时链路更长。`mechctl config explain <param>` 应输出完整的取值来源链

## 参考

- Cloudera Manager Role Config Group
- Apache Ambari Config Group
- Ansible `host_vars`（允许无名覆盖的反面参照）
