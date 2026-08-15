# 对象模型

## 1. 层级

```
Site  站点  ────────────────────── 一组被统一管理的目标
 ├─ Node          受管主机（运行 mechlet）
 ├─ Endpoint      外部受管目标（预留，v1 不实现）
 └─ Component  组件 ─────────────── 某个 Pack 在本 Site 的一份部署
      └─ Role  角色 ─────────────── Pack 内定义的角色类型
           └─ ConfigGroup  配置组 ── 共享同一份参数覆盖的具名子集
                └─ RoleInstance  角色实例 ── 角色落在某个目标上的实例

Rollout  变更过程 ────────────────── 一次分批变更的执行状态机
Pack     组件包 ──────────────────── 静态可分发制品（不属于任何 Site）
```

## 2. 各对象定义

### Site（站点）

一组被统一管理的目标。承担五项职责：

1. **归属**：一个 Node 属于且仅属于一个 Site
2. **命名空间**：Component 名在 Site 内唯一（`pg-main` 可在多个 Site 中重名）
3. **状态边界**：`apply` / `diff` / `rollout` 的作用域
4. **权限边界**：RBAC 授权到 Site 级
5. **规模与关系无关**：1 个节点与 300 个节点都自然；节点之间协作（Hadoop 集群）或完全不协作（5000 台边缘盒子）都自然

```yaml
site:
  name: store-shanghai-0871
  kind: edge                 # edge | cluster | standalone
  labels: { region: east, env: prod }
```

`kind` 只驱动呈现与默认行为，不改变语义：

| kind | 含义 | UI 呈现 |
|---|---|---|
| `cluster` | 节点协作构成一个系统 | 服务拓扑视图 |
| `edge` | 独立的边缘站点，通常少量节点 | 网格 / 地图视图，适合上千个 |
| `standalone` | 单机，`mechlet install --standalone` 时由同机 mechd 隐式创建 | 单节点详情页 |

> 为什么叫 Site 而不是 Cluster —— 见 [ADR-0004](../adr/0004-site-as-top-scope.md)，其中包含 9 个候选词的横向对比。

### Component（组件）

某个 Pack 在某个 Site 中的一份部署实例，携带该次部署的参数取值。

```yaml
component:
  name: pg-main
  site: dc-beijing
  pack: { name: postgresql, version: "16.4", revision: 1 }
  params:
    port: 5432
  roles:
    primary: { nodes: [node-1] }
    replica:  { nodes: [node-2, node-3], params: { hot_standby: on } }
```

一个 Site 内可以有同一个 Pack 的多个 Component（`pg-main`、`pg-report`），互不干扰。

### Role（角色）

Pack 内定义的角色类型。**一个 Pack 可承载多个 Role**——这是支撑 PostgreSQL 主备、HDFS 多角色、Kafka broker/controller 等场景的基础。

Role 的三个关键属性：

| 属性 | 作用 |
|---|---|
| `cardinality` | `1` / `0-1` / `1-N` / `0-N`，约束实例数量 |
| `requires` | 同 Component 内的角色依赖，决定启停顺序与滚动升级顺序 |
| `scope` | `node`（v1 仅支持）/ `cluster`（预留给 Kubernetes 等） |

Role 可以**没有 workload**——只落文件不起进程，用于分发客户端配置（对应 Cloudera 的 Gateway、Ambari 的 CLIENT 角色）。

`requires` 只约束**时序**。约束**位置**（哪些角色不能同处一台机器）由 Pack 顶层的 `placement` 表达：

```yaml
placement:
  - antiAffinity: [namenode, secondarynamenode]
    scope: node                 # node，或任意 Node label key（rack / zone）
    enforcement: required       # required 拒绝放置 | preferred 仅告警
    reason: "SNN 与 NN 同节点时无法承担元数据恢复职责"
```

因为 Mecharion **没有调度器**（节点由用户显式指定），这些约束是 mechd 放置阶段的**校验规则**而非调度输入——比 Kubernetes 的 `podAntiAffinity` 简单一个量级。单元素 `antiAffinity` 表示同角色的多个实例互斥（如 ZK 三实例分散）。

> 见 [ADR-0006](../adr/0006-multi-role-pack.md)、[ADR-0020](../adr/0020-placement-constraints.md)

### ConfigGroup（配置组）

**共享同一份参数覆盖的、具名的 RoleInstance 子集。** 每个 RoleInstance 属于且仅属于一个组。

```yaml
component: hdfs-prod
role: datanode
configGroups:
  - name: default          # 隐式存在，未被具名组包含的实例落在这里
  - name: 12-disk-nodes
    nodes: [n21, n22, n23, n24, n25]
    params: { max_xcievers: 8192 }
    paths:  { dataDirs: [data1, data2, …, data12] }
```

命令面：

```bash
mechctl config group list   -c hdfs-prod -r datanode
mechctl config group create 12-disk-nodes -c hdfs-prod -r datanode --nodes n21,n22
mechctl config group set    12-disk-nodes max_xcievers=8192
mechctl config group move   n23 --to 12-disk-nodes
mechctl config group diff   12-disk-nodes default
mechctl config explain      max_xcievers --node n21    # 输出完整取值来源链
```

> 见 [ADR-0021](../adr/0021-config-group.md)

### RoleInstance（角色实例）

Role 落在某个具体目标上的实例，是状态、日志、健康、generation 的最小管理单元。

```go
type RoleInstance struct {
    Component   string
    Role        string
    ConfigGroup string // 所属配置组，未指定时为 "default"
    Target      Ref    // 这个角色作用在什么之上
    Executor    Ref    // 谁去执行这个动作
    Generation  int
    Status      WorkloadStatus
}

type Ref struct {
    Kind string // "Node" | "Endpoint"
    Name string
}
```

**`Target` 与 `Executor` 必须是两个字段**，即使 v1 中它们永远相同。这是为 Kubernetes 等 cluster-scoped 目标预留的唯一必要接口——届时 Target 是一个 k8s 集群（Endpoint），Executor 是某个能连到其 API Server 的 mechlet。

> 见 [ADR-0017](../adr/0017-k8s-extension-reserve.md)

### Node（节点）

受管主机，运行 mechlet。携带 labels 用于选择，并**上报 capability**：

```yaml
node:
  name: node-7
  site: dc-beijing
  labels: { role: db, rack: r12 }
  capabilities:                      # 由 mechlet Probe() 上报
    systemd: { version: "252" }
    docker:  { version: "24.0.7", socket: /var/run/docker.sock, rootless: false }
    compose: { version: "2.24.0" }
  roots:                             # 路径根，见 04-paths-and-storage.md
    opt: /opt/mecharion
    data: /var/lib/mecharion
  volumes:
    - { name: data1, path: /data1 }
    - { name: data2, path: /data2 }
```

capability 用于放置校验：Pack 声明 `requires.capability.docker: ">=20.10"`，mechd 在放置时拒绝不满足的节点并给出可执行的错误提示。**不会隐式安装缺失的运行时**（[原则六](00-overview.md#原则六显式优于隐式)）。

### Rollout（变更过程）

一次变更的执行状态机：分批、健康门禁、暂停/恢复、回滚。

`Rollout` 这个词是刻意选择的——`Deployment` 在 Kubernetes 中指静态对象、在 Nomad/Argo 中指变更过程，含义相反，用它会造成持久的歧义。

### Endpoint（预留）

外部受管目标（Kubernetes 集群的 kubeconfig、云账号、网络设备）。v1 中 `Ref.Kind` 枚举包含它但解析时返回「尚未支持」。

## 3. 刻意保留不用的词

| 词 | 为什么不用 |
|---|---|
| `Service` | 与 systemd service 正面冲突。Mecharion 把 systemd runtime 作为一等公民，这个歧义无法承受。文档中提到 systemd 时一律用它自己的术语 **unit** |
| `Cluster` | 断言「节点之间互相协作」，对 5000 个独立边缘盒子是错误陈述；且 Site 内部会真的部署 Hadoop/Kafka 集群，造成套娃 |
| `Deployment` | 在两大生态中含义相反（K8s = 静态对象，Nomad/Argo = 变更过程），且会占用本应属于 Rollout 的语义 |
| `Instance` | 与云主机实例（EC2 instance）冲突。主机一律称 `Node` |

## 4. 参数解析优先级

```
Pack 默认值  →  Component 级  →  Role 级  →  ConfigGroup 级
```

**不存在第五层。** 单节点的差异化配置表现为一个只含该节点的组——**模型中不允许无名的 per-node 覆盖**，因为允许无名差异就是允许配置雪花化（一年后 200 台机器各自略有不同且无人知道为什么）。

CLI 用自动建组消除这条纪律带来的摩擦，操作步数与无名覆盖完全相同：

```bash
mechctl config set -c hdfs-prod -r datanode --node n7 max_xcievers=8192
# → 自动创建 ConfigGroup "node-n7"（成员 [n7]）
```

> 见 [ADR-0021](../adr/0021-config-group.md)

## 5. 三个版本概念

容易混淆，明确区分：

| 概念 | 含义 | 归属 | 例 |
|---|---|---|---|
| `version` | 上游软件版本 | Pack | `16.4` |
| `revision` | Pack 自身迭代（载荷不变、仅改模板时递增） | Pack | `1` |
| `generation` | 某 RoleInstance 在某目标上的第 N 次物化 | RoleInstance | `0007` |

**配置变更同样产生新 generation**，因此「回滚」是一个统一动作，不区分「回滚版本」与「回滚配置」。

## 6. 相关决策

- [ADR-0003 对象模型分层与命名](../adr/0003-object-model-naming.md)
- [ADR-0004 顶层作用域采用 Site](../adr/0004-site-as-top-scope.md)
- [ADR-0006 一个 Pack 承载多个 Role](../adr/0006-multi-role-pack.md)
- [ADR-0017 为 Kubernetes 预留 Target/Executor 拆分](../adr/0017-k8s-extension-reserve.md)
