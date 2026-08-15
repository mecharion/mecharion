# ADR-0009: Node volumes 与多磁盘绑定

- **状态**：已接受
- **日期**：2026-08-01
- **相关**：[ADR-0008](0008-immutable-generation-linkinto.md)

## 背景

两个真实需求：

1. **按服务分盘**：把 PostgreSQL 的数据放 SSD、日志归档放大容量 HDD
2. **单服务多盘**：HDFS DataNode 的 `dfs.data.dir` 通常配置为 `/data1/dfs,/data2/dfs,/data3/dfs`；MinIO、Kafka 同理

而节点的磁盘配置**因机器而异**——同一个 Component 部署到 20 台机器，其中 5 台只有 2 块盘。

## 候选方案与调研

### 方案 A：Pack 中写绝对路径

- ❌ 同一个 Pack 无法部署到磁盘配置不同的节点
- ❌ 用户要为每种磁盘配置维护一份 Pack 副本

### 方案 B：把路径完全交给用户参数

Pack 声明一个 `data_dirs` 参数，用户填绝对路径列表。

- ✅ 简单
- ❌ 用户需为每个节点手写绝对路径，20 台机器就是 20 份覆盖
- ❌ Pack 无法表达「我需要几块盘」「这些盘的用途是什么」

### 方案 C：节点声明卷，Pack 声明需求，部署时绑定 ⭐

调研对照：

| 系统 | 做法 |
|---|---|
| **Cloudera Manager** | `dfs.data.dir` 是 Role Config Group 级参数，可按主机组差异化。**证实这个能力是必需的** |
| **Kubernetes** | PV / PVC / StorageClass — 节点提供存储、工作负载声明需求、调度时绑定。**分层思路直接可借鉴** |
| **Nomad** | host volumes：客户端配置中声明命名卷，Job 中按名引用。**最接近本项目所需的形态** |
| **Ansible** | 无此抽象，靠 inventory 变量硬编码路径 |

Nomad 的 host volume 模型最贴合：轻量、无动态供应、命名引用。

## 决策

**采用方案 C，参照 Nomad host volume 的轻量形态。**

### 节点声明它有什么

```yaml
# /etc/mecharion/mechlet.yaml
roots:
  opt:  /opt/mecharion
  etc:  /etc/mecharion
  data: /var/lib/mecharion       # 安装时 --data-dir 指定
  logs: /var/log/mecharion
  run:  /run/mecharion

volumes:
  - { name: data1, path: /data1 }
  - { name: data2, path: /data2 }
  - { name: data3, path: /data3 }
```

### Pack 声明它要什么

```yaml
paths:
  data:
    default: "{{ .Node.Roots.data }}/apps/{{ .Component }}"

  dataDirs:                                    # HDFS DataNode
    kind: multi                                # 渲染进模板时是列表
    default: ["{{ .Node.Roots.data }}/apps/{{ .Component }}/dfs"]
    subpath: "dfs/dn"                          # 用户只给盘名，引擎补子路径
```

### 用户在部署时绑定

按 [参数优先级](../design/02-object-model.md#4-参数解析优先级)（Pack 默认 → Component → Role → Node）覆盖：

```yaml
overrides:
  node: node-7
  paths:
    dataDirs: [data1, data2, data3]    # → /data1/dfs/dn, /data2/dfs/dn, /data3/dfs/dn
```

**「不同服务放不同盘」就是一次普通的 Component 级覆盖**（`paths.data: bulk1`），不需要额外机制。

### 明确不做（v1）

`volumeClass`（Pack 声明 `prefer: fast|bulk`，引擎自动挑盘）是自然延伸，但 v1 不实现——先让用户显式绑定，等有真实抱怨再加。

## 理由

**核心原则：Pack 绝不硬编码绝对路径。** 一旦硬编码，多盘、按节点差异化、按服务分盘三个需求同时死掉。

三层职责划分清晰且各自稳定：

| 层 | 职责 | 变化频率 |
|---|---|---|
| Node | 我有哪些盘 | 随硬件，低频 |
| Pack | 我需要几块盘、用途是什么 | 随组件版本，低频 |
| 部署时覆盖 | 这次用哪些盘 | 随部署，高频 |

Kubernetes 的 PV/PVC 证明了这个分层是对的；Nomad 的 host volume 证明了不需要动态供应也能用得很好。Mecharion 面对的是物理机与固定磁盘，动态供应无意义，因此取 Nomad 的轻量形态。

## 后果

### 收益

- 同一个 Pack 可部署到磁盘配置各异的节点
- 多盘需求（HDFS/MinIO/Kafka）原生支持
- 按服务分盘无需额外机制
- 用户引用盘名而非路径，节点迁移时只改节点侧声明

### 代价

- **多一层间接**：Pack 作者与用户都要理解「卷名 → 路径」的映射。缓解：默认值覆盖单盘场景，只有多盘用户才需接触
- **节点侧需要维护 volumes 声明**：新增磁盘要改 `mechlet.yaml`。缓解：可由 `mechlet` 探测候选挂载点并提示，但**不自动写入**（[原则六](../design/00-overview.md#原则六显式优于隐式)）
- **`kind: multi` 的校验较复杂**：需检查卷存在、可写、空间充足，且部分组件（HDFS）在盘数变化时有特殊语义（减盘需先下线数据）。v1 只做存在性与可写性校验，语义层面交给 Pack 的 hook
- **未做 volumeClass 意味着大规模场景下手工绑定量大**：50 台机器各有不同盘数时，覆盖配置会很啰嗦。这是有意接受的 v1 简化

## 参考

- HashiCorp Nomad host volumes（最贴近的形态）
- Kubernetes PersistentVolume / StorageClass（分层思路）
- Cloudera Manager Role Config Group 中的 `dfs.data.dir` 差异化配置
