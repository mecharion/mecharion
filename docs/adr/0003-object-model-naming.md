# ADR-0003: 对象模型分层与命名

- **状态**：已接受
- **日期**：2026-08-01
- **相关**：[ADR-0004](0004-site-as-top-scope.md)、[ADR-0006](0006-multi-role-pack.md)

## 背景

需要为「静态制品 → 部署实例 → 角色 → 落到单机」这条链路上的每一层命名。命名会贯穿 CLI、API、Web UI、文档与社区讨论，且极难在有用户之后修改。

## 候选方案与调研

### 主流方案横向对比

| 系统 | 静态制品 | 部署实例 | 角色/成分 | 落到单机 | 分组 | 变更过程 |
|---|---|---|---|---|---|---|
| **Cloudera Manager** | Parcel | **Service** | Role Type | **Role (Instance)** | Cluster / Host Template | Command |
| **Ambari** | Stack | **Service** | **Component** | Host Component | Cluster / Host Group | Request |
| **Kubernetes** | Image | Deployment / StatefulSet | — | Pod | Namespace / Node | Rollout |
| **Nomad** | — | **Job** | Task Group | **Allocation** | Datacenter / Node Class | Deployment |
| **Helm** | Chart | **Release** | — | — | Namespace | Upgrade |
| **Ansible** | Role | — | — | — | Group / Host | Play |
| **Consul** | — | Service | — | Service Instance | Datacenter | — |

### 从对比中得到的三条结论

**① 「角色」这一层只有大数据系工具有**（CM 的 Role、Ambari 的 Component）。Kubernetes 与 Nomad 都没有，因为它们假设一个部署单元只有一种进程。Mecharion 需要一 Pack 多角色（[ADR-0006](0006-multi-role-pack.md)），因此在模型上属于 CM/Ambari 这一支，命名也应向其靠拢。

**② `Deployment` 在两大生态中含义相反**：Kubernetes 中是静态对象，Nomad/Argo 中是「一次滚动变更的过程」。而 Mecharion 需要一个表示变更过程的对象，用掉 `Deployment` 会让那个对象无名可用。

**③ 没有任何一家把 artifact 与 instance 用同一个词。** `Pack` 已占据 artifact 位，中间层必须另起名。

### 三套候选方案

#### 方案 A：Cloudera 谱系 — `Cluster → Service → Role → RoleInstance`

- ✅ 大数据运维人群零学习成本
- ❌ **`Service` 与 systemd service 正面冲突**——Mecharion 把 systemd runtime 作为一等公民，资源类型就叫 `systemd_unit`，用户会持续追问「重启 service 是重启哪个」
- ❌ JDK Pack、纯 sysctl 调优 Pack 叫 Service 别扭（CM 靠 Gateway role 打补丁绕过）

#### 方案 B：Kubernetes/Nomad 谱系 — `Cluster → Job → TaskGroup → Allocation`

- ❌ `Job` 强烈暗示有限时任务，Mecharion 管的是常驻服务
- ❌ `Allocation` 是调度器语义（「调度器为它分配了资源」），Mecharion **没有调度器**——节点由用户显式指定
- 模型根本不匹配，出局

#### 方案 C：Component 谱系 — `Site → Component → Role → RoleInstance` ⭐

- ✅ 与产品的中文词汇完全对齐：`站点 → 组件 → 角色 → 角色实例`
- ✅ `Component` 对「有守护进程」与「无守护进程」一视同仁——JDK 是组件、nginx 是组件、一组内核参数也是组件
- ✅ 完全避开 systemd 冲突
- ⚠️ 与已退役的 Ambari 中的 `Component`（相当于本方案的 `Role`）冲突

### 被否决的其他候选

| 词 | 否决原因 |
|---|---|
| `Release` | Helm 包袱；且与「软件版本」在日常语言中冲突 |
| `Instance` | 与云主机实例（EC2 instance）冲突；且 `RoleInstance` 会造成 instance 嵌套 |
| `Application` / `App` | 对 java-webapp 自然，对「一组内核参数」荒谬 |
| `Stack` | 撞 CloudFormation / Pulumi / Docker Stack，尤其撞 **Ambari 的 Stack**（相当于本项目的 Pack 集合），最易误解 |

## 决策

采用**方案 C**：

```
Site → Component → Role → RoleInstance
Node（受管主机，带 labels）
Rollout（一次变更的执行过程）
Endpoint（外部受管目标，预留）
```

**刻意保留不用的词**：`Service`（让给 systemd）、`Deployment`（含义两极且需留给 Rollout 语义）、`Cluster`（见 [ADR-0004](0004-site-as-top-scope.md)）、`Instance`（与云主机实例冲突）。

配套约定：文档中提到 systemd 时一律使用它自己的正确术语 **unit**，不写 "systemd service"。

## 理由

1. **中英文双语一致性不是小事。** 项目的第一批开发者与用户使用中文，文档与社区讨论会长期双语并行。`Site/Component/Role/RoleInstance` 在两种语言里都自然，方案 A 在中文里同样别扭（「服务 postgresql 的角色 primary」尚可，「服务 jdk」不通）。
2. **`Component` 的通用性覆盖产品的实际边界。** 产品要管的不只是守护进程，还有运行时（JDK）与纯主机配置——`Service` 对后两者是错误陈述。
3. **保留 `Deployment` 给变更过程**是从 Nomad/Argo 的实践中学到的：变更过程需要一个一等对象（分批、暂停、回滚），提前把词占掉会让它无名可用。

## 后果

### 收益

- 无 systemd 术语冲突
- 覆盖有/无守护进程的全部组件类型
- `Rollout` 词位保留，变更过程可建模为一等对象
- 中英文表达一致

### 代价

- **与 Ambari 的 `Component` 语义冲突**：Ambari 中 Component 相当于本项目的 Role。Ambari 已进入 Apache Attic，影响可接受，但从 Ambari 迁移的用户需要一次概念映射。
- **`Component` 是通用词**，日常口语中「这个组件」偏模糊。CLI 需提供简写别名（如 `comp` / `co`）。
- **CM/Ambari 存量用户有学习成本**：若后续判断目标用户压倒性来自该人群，应重新评估方案 A（届时写新 ADR 取代本文）。

## 参考

- Cloudera Manager: Cluster / Service / Role Type / Role / Role Config Group
- Apache Ambari: Cluster / Service / Component / Host Component
- Kubernetes: Deployment / Pod / Namespace
- HashiCorp Nomad: Job / Task Group / Allocation / Deployment
- Helm: Chart / Release
