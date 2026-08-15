# ADR-0006: 一个 Pack 承载多个 Role

- **状态**：已接受
- **日期**：2026-08-01
- **相关**：[ADR-0003](0003-object-model-naming.md)

## 背景

许多组件由多个**运行角色**构成：

- PostgreSQL：primary / replica
- HDFS：NameNode / DataNode / JournalNode / ZKFC
- Kafka：broker / controller
- 几乎所有组件：客户端配置分发（只落文件不起进程）

问题：这些角色应该是一个 Pack 内的多个 Role，还是多个独立的 Pack？

## 候选方案与调研

### 方案 A：一角色一 Pack

- ✅ Pack 格式简单，无需 role 概念
- ❌ **版本漂移**：primary 与 replica 必须版本一致，拆开后没有任何机制保证
- ❌ **重复载荷**：同一份二进制被多个 Pack 各打包一份
- ❌ **依赖地狱**：角色间启停顺序（ZK → JournalNode → NameNode → DataNode）退化为 Pack 间依赖，粒度错位
- ❌ **升级不可原子**：一次 Hadoop 升级要协调 4 个 Pack 的版本

### 方案 B：一 Pack 多 Role ⭐

调研对照：

| 系统 | 模型 | 结论 |
|---|---|---|
| **Cloudera Manager** | Service 含多个 Role Type（HDFS → NameNode/DataNode/…） | 大数据场景的事实标准，被验证可行 |
| **Ambari** | Service 含多个 Component | 同上 |
| **Helm** | 一个 Chart 可含多个 Deployment/StatefulSet | 主流包管理器同样选择多工作负载 |
| **Nomad** | 一个 Job 含多个 Task Group | 同上 |
| **Kubernetes 原语** | Deployment 只有一种 Pod 模板 | 反面：因此 Helm 必须在其之上再包一层 |

**没有任何成熟的组件管理系统采用「一角色一包」。** 这不是巧合——多角色是被管理对象的固有属性。

## 决策

**一个 Pack 承载多个 Role。**

```yaml
shared:                        # 所有角色共有（装一次即可）
  resources:
    - user:    { name: postgres, system: true }
    - archive: { blob: main, dest: "{{ .Paths.Generation }}", strip: 1 }

roles:
  - name: primary
    cardinality: "1"
    workload: { runtime: systemd, … }

  - name: replica
    cardinality: "0-N"
    requires: [primary]
    params:
      primary_host:
        type: string
        from: "topology.role('primary').nodes[0].address"

  - name: client
    cardinality: "0-N"
    # 无 workload：只分发客户端配置
```

四个必备机制：

| 机制 | 作用 |
|---|---|
| `cardinality` | `1` / `0-1` / `1-N` / `0-N`。NameNode 是 `1`（HA 下 `2`），DataNode 是 `1-N` |
| `requires` | 角色间依赖，决定启停顺序与滚动升级顺序 |
| 无 `workload` 的角色 | 客户端配置分发，对应 CM 的 Gateway / Ambari 的 CLIENT |
| `from: topology.…` | 跨角色拓扑引用——replica 需知道 primary 在哪台机 |

## 理由

**角色是被管理对象的固有属性，不是打包方式的选择。** PostgreSQL 的 primary 和 replica 共享同一份二进制、同一套配置模板、同一个升级节奏——把它们拆开就是在制造本不存在的协调问题。

`shared` 段的存在同样重要：二进制解压、系统用户创建这些动作在所有角色间只做一次，而非每个角色重复声明。

## 后果

### 收益

- 版本一致性由格式保证，不依赖用户自律
- 载荷天然复用
- 启停与滚动升级顺序可在 Pack 内声明
- 支持无 workload 的客户端角色

### 代价

- **Pack 格式复杂一档**：多出 `roles` / `shared` / `cardinality` / `requires` 四个概念，简单 Pack（如 nginx 单角色）也要写 `roles:` 一层。缓解：单角色 Pack 允许省略角色名，使用默认角色。

- **渲染时机被强约束**：`topology.…` 引用要求**模板渲染必须发生在角色放置确定之后**。这是一条不可回退的架构约束——渲染必须在 mechd（或单机形态的 mechlet）完成，mechlet 收到已解析的拓扑快照。

  这条约束同时带来一个重要收益：**mechlet 之间不需要互相查询**，每个节点只需自己那份规格。系统复杂度因此大幅降低。

- **cardinality 校验需要在放置阶段完成**：不能等到节点侧才发现「NameNode 配了两个」。

## 参考

- Cloudera Manager: Service / Role Type / Role Config Group
- Apache Ambari: Service / Component
- Helm Chart 多工作负载模型
- HashiCorp Nomad: Job / Task Group
