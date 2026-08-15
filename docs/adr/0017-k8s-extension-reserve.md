# ADR-0017: 为 Kubernetes 预留 Target/Executor 拆分

- **状态**：已接受
- **日期**：2026-08-01
- **相关**：[ADR-0010](0010-runtime-abstraction.md)、[ADR-0003](0003-object-model-naming.md)

## 背景

产品规划中包含 Kubernetes 支持，但 v1 不实现。问题是：**现在需要为它预留什么，才能避免将来做破坏性变更？**

预留过多是过度设计；预留不足会导致将来改数据库 schema、API、CLI 输出与 UI。

## 分析：Kubernetes 与其他 Runtime 的差异

| | systemd / docker / compose / podman | Kubernetes |
|---|---|---|
| 作用域 | **节点本地** | **集群** |
| 执行者 | 目标节点上的 mechlet | 能连到 API Server 的某个执行者 |
| 有「启停进程」动作吗 | 有 | 没有，只有「提交 manifest」 |
| 谁负责调和 | mechlet | k8s 自己的控制器 |
| 状态来源 | 本地探测 | k8s API |

**结论：Kubernetes 不适用 `Runtime` 接口**（[ADR-0010](0010-runtime-abstraction.md)），强行塞入会污染所有实现。它需要一个独立的执行路径。

## 候选方案与调研

### 方案 A：什么都不预留，将来再说

- ❌ 将来需要修改 `RoleInstance` 的核心字段，波及数据库 schema、API、CLI 输出、UI 与全部查询
- ❌ 这是所有预留项中代价最高的一项

### 方案 B：现在就设计完整的 ClusterRuntime 抽象

- ❌ 在没有实现的情况下设计抽象，几乎必然设计错
- ❌ 消耗 v1 的开发资源

### 方案 C：只预留模型层的最小接口 ⭐

调研对照：

| 系统 | 如何表达「执行者 ≠ 目标」 |
|---|---|
| **Terraform** | Provider 配置（连接信息）与 Resource（目标）分离 |
| **Ansible** | `delegate_to` — 在 A 上执行、作用于 B。**这是同一个问题的既有解法** |
| **Crossplane** | ProviderConfig（如何连）与 Managed Resource（管什么）分离 |
| **Cluster API** | 管理集群与工作负载集群分离 |

Ansible 的 `delegate_to` 最能说明问题：**「在哪执行」与「作用于什么」是两个独立的维度**，这在基础设施工具中是普遍需求，不只是 Kubernetes 的特例。

## 决策

**只做三件事，全部在模型层，代码量接近零。**

### ① `Role.scope: node | cluster`

schema 中存在该字段，v1 校验时只接受 `node`。

加字段是零成本，事后加是破坏性变更。

### ② `RoleInstance` 拆分 `Target` 与 `Executor` ⭐ 唯一真正重要的一条

```go
type RoleInstance struct {
    Component string
    Role      string
    Target    Ref   // 这个角色作用在什么之上
    Executor  Ref   // 谁去执行这个动作
    // …
}

type Ref struct {
    Kind string // "Node" | "Endpoint"
    Name string
}
```

v1 中两者永远相同（都指向那个 Node），看起来像冗余。但 Kubernetes 场景下：

- `Target` = 一个 Kubernetes 集群（Endpoint）
- `Executor` = 某个能连到该集群 API Server 的 mechlet，或 mechd 自己

### ③ `Ref.Kind` 枚举预留 `Endpoint`

代表外部受管目标（k8s 集群的 kubeconfig、云账号、网络设备）。v1 解析到该分支时返回「尚未支持」。

### 明确不做

`ClusterRuntime` 接口的形态、manifest 如何渲染、状态如何归一化——等真正实现时再设计，不提前猜测。

## 理由

**如果现在把它写成 `NodeName string`，将来要改的是数据库 schema、API、CLI 输出、UI，以及所有依赖它的查询——代价高一个量级。** 现在写成两个 `Ref` 类型，v1 中让它们指向同一个 Node，成本是零。

这是「预留」的正确形态：**只在成本为零、而事后修改成本极高的地方预留，且不预留任何需要设计判断的东西。**

### 附带收益：不只服务 Kubernetes

`Target` / `Executor` 分离对 Kubernetes 之外同样有用。将来管理交换机、云 API、外部数据库，全都是「target 不是节点、executor 是某个节点」的形态。这说明这个拆分不是为单一场景打的补丁，而是模型本身应有的维度——Ansible 的 `delegate_to` 已经证明了这一点。

## 后果

### 收益

- 将来引入 Kubernetes 或任何 cluster-scoped 目标时，无需修改核心数据模型
- 附带支持「代理执行」这一普遍模式
- 预留成本接近零，不消耗 v1 开发资源

### 代价

- **v1 中存在看起来冗余的字段**：`Target` 与 `Executor` 永远相同。需在代码注释与本 ADR 中说明原因，否则后来者会「优化」掉它
- **两个字段增加序列化体积与 API 复杂度**：微小但真实
- **预留不保证正确**：真正实现 Kubernetes 时可能发现还需要别的东西。本条预留只覆盖了「最贵的那个」，不是全部
- **`Endpoint` 概念悬空**：schema 中存在但无实现，可能造成用户困惑。文档中标注为「预留，尚未支持」

## 参考

- Ansible `delegate_to`
- Terraform Provider / Resource 分离
- Crossplane ProviderConfig / Managed Resource
- Kubernetes Cluster API 的管理集群/工作负载集群分离
