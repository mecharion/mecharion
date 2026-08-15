# 架构决策记录（ADR）

## 这是什么

每一份 ADR 记录**一个**重要决策：当时的背景、调研过的候选方案、最终选择、理由，以及**承担的代价**。

ADR 是**只追加不修改**的。推翻一个决策的方式是写一份新 ADR 并把旧的状态改为「已被 ADR-XXXX 取代」，而不是编辑历史内容。这样后来者能看到判断是如何演进的，而不只是当前结论。

## 为什么坚持这个格式

Mecharion 的许多选择——顶层对象叫 `Site` 而不是 `Cluster`、控制面用 SQLite 而不是 BoltDB、Pack 的物化目录必须只读——都不是显然的，都是在对比既有实践后做出的取舍。

如果只留结论，后来者会在不理解代价的情况下推翻它们，或在不理解意图的情况下沿用它们。**每份 ADR 的「后果 · 代价」一节是强制的**，没有代价的决策通常意味着没想清楚。

## 模板

```markdown
# ADR-XXXX: 标题

- **状态**：已接受 | 已被 ADR-YYYY 取代 | 已废弃
- **日期**：YYYY-MM-DD
- **相关**：ADR-…

## 背景
## 候选方案与调研
## 决策
## 理由
## 后果
### 收益
### 代价
## 参考
```

## 索引

### 架构

| ID | 标题 | 状态 |
|---|---|---|
| [0001](0001-agent-based.md) | 采用常驻 Agent 架构 | 已接受 |
| [0002](0002-mechlet-as-sole-engine.md) | mechlet 为唯一执行引擎，mechd 为协调层 | 已接受；「mechd 可选」的措辞被 0026 修订 |
| [0026](0026-standalone-runs-mechd.md) | 单机形态同机运行 mechd，存储与 API 不分叉 | 已接受 |

### 对象模型

| ID | 标题 | 状态 |
|---|---|---|
| [0003](0003-object-model-naming.md) | 对象模型分层与命名 | 已接受 |
| [0004](0004-site-as-top-scope.md) | 顶层作用域采用 Site | 已接受 |
| [0020](0020-placement-constraints.md) | 角色间放置约束（affinity / antiAffinity） | 已接受 |
| [0021](0021-config-group.md) | 具名 ConfigGroup 取代无名的 Node 级覆盖 | 已接受 |

### Pack

| ID | 标题 | 状态 |
|---|---|---|
| [0005](0005-pack-logic-payload-split.md) | Pack 逻辑与载荷分离，blob 内容寻址 | 已接受 |
| [0006](0006-multi-role-pack.md) | 一个 Pack 承载多个 Role | 已接受 |
| [0007](0007-params-custom-subset.md) | params 采用自定义类型子集而非 JSON Schema | 已接受 |
| [0015](0015-offline-first-hermetic.md) | 离线优先与 hermetic 约束 | 已接受 |
| [0016](0016-mandatory-pack-signing.md) | Pack 签名为必需项 | 已被 ADR-0040 取代 |
| [0022](0022-deployment-profiles.md) | 用 profiles 表达部署形态 | 已接受 |
| [0023](0023-node-facts.md) | 节点事实：校验用实时值，渲染用固化快照 | 已接受 |
| [0024](0024-cross-pack-reference.md) | 跨 Pack 引用与 Pack 粒度判据 | 已接受 |
| [0040](0040-pack-trust-is-operator-responsibility.md) | Pack 信任是运维方自己的责任，不做签名校验 | 已接受 |

### 路径与存储布局

| ID | 标题 | 状态 |
|---|---|---|
| [0008](0008-immutable-generation-linkinto.md) | generation 不可变，用 linkInto 调和应用原生布局 | 已接受 |
| [0009](0009-node-volumes-multidisk.md) | Node volumes 与多磁盘绑定 | 已接受 |

### Runtime

| ID | 标题 | 状态 |
|---|---|---|
| [0010](0010-runtime-abstraction.md) | Runtime 抽象及其接缝位置 | 已接受 |
| [0027](0027-resource-engine-contract.md) | 资源引擎的接口契约 | 已接受 |
| [0011](0011-docker-compose-in-v1.md) | v1 纳入 docker 与 compose runtime | 已接受 |
| [0017](0017-k8s-extension-reserve.md) | 为 Kubernetes 预留 Target/Executor 拆分 | 已接受 |

### 控制面（M3）

| ID | 标题 | 状态 |
|---|---|---|
| [0028](0028-stable-ordinals.md) | ordinal 一次分配后固化，不由节点名排序推导 | 已接受；**修订 spec §9.3 的原措辞** |
| [0029](0029-push-over-server-stream.md) | 下发用服务端流，上报用一元 RPC | 已接受 |
| [0030](0030-secret-storage-and-delivery.md) | 密钥用信封加密存储，用不透明引用下发 | 已接受 |

### 容器 Runtime（M4）

| ID | 标题 | 状态 |
|---|---|---|
| [0031](0031-docker-cli-not-sdk.md) | docker / compose runtime 走 CLI，不用 Docker SDK | 已接受 |
| [0032](0032-runtime-exec-seam.md) | `ExecIn` 进 Runtime 接口 | 已接受；**修订 ADR-0010 的划界规则①** |

### 持续调和与升级（M5 / M6）

| ID | 标题 | 状态 |
|---|---|---|
| [0033](0033-mechlet-local-desired-state.md) | mechlet 持有本地期望状态与密钥副本 | 已接受 |

### 多节点（M7）

| ID | 标题 | 状态 |
|---|---|---|
| [0034](0034-node-join-and-identity.md) | 节点加入以 token 为授权，身份以证书 CN 为准 | 已接受 |
| [0035](0035-no-maxsurge.md) | 滚动升级只有 maxUnavailable 与 canary，不做 maxSurge | 已接受 |
| [0036](0036-webui-vue-and-generated-dist.md) | Web UI 用 Vue + Vite，产物由 go generate 构建而不进仓库 | 已接受 |
| [0037](0037-login-is-full-privilege.md) | M8 只做「登录即全权」，角色与 Site 授权延后 | 已接受 |
| [0038](0038-adhoc-task-channel.md) | ad-hoc 命令走独立的 `Tasks` 流，不塞进期望状态 | 已接受 |
| [0039](0039-bootstrap-token-gate.md) | 首次初始化用一次性 admin token 门禁，不用 PoW/滑块 | 已接受 |

### 持久化

| ID | 标题 | 状态 |
|---|---|---|
| [0012](0012-mechd-embedded-sqlite.md) | mechd 采用嵌入式 SQLite | 已接受 |
| [0013](0013-mechlet-no-database.md) | mechlet 不使用数据库 | 已接受 |
| [0014](0014-no-ha-in-v1.md) | v1 不实现控制面高可用 | 已接受 |

### 命名与 CLI

| ID | 标题 | 状态 |
|---|---|---|
| [0018](0018-project-naming.md) | 项目命名体系：Mecharion / m7n / mech 词根 | 已接受 |
| [0019](0019-namespace-domain.md) | 命名空间与域名约定 | 已接受 |
| [0025](0025-noun-first-cli.md) | CLI 采用名词优先结构 | 已接受 |
