# 设计总览与设计原则

## 1. 项目定位

Mecharion 是一套**应用生命周期管理（Application Lifecycle Management）**工具，用 Go 编写，同时提供 CLI 与可视化界面，管理 Linux 环境下组件的**开发打包、部署、配置、升级、状态监控与运维**全过程。

它的能力边界可以用两个既有产品来定位：

| 参照物 | Mecharion 吸收了什么 | Mecharion 不同在哪 |
|---|---|---|
| **Ansible** | 主机配置管理、临时命令/脚本执行、幂等资源模型 | Ansible 不持久化期望状态，无法回答「现在偏离了吗」；Mecharion 以声明式期望状态为主干，支持持续调和与漂移检测 |
| **Cloudera Manager** | 组件生命周期、多角色服务模型、滚动升级、参数化配置界面 | CM 深度绑定 Hadoop 生态且闭源；Mecharion 是通用的、开源的、面向任意组件的 |

一句话概括：**用 Cloudera Manager 的模型深度，管理 Ansible 那样广泛的目标。**

## 2. 目标场景

Mecharion 必须在光谱的两端都成立，这个跨度是许多设计决策的根源：

```
中心化数据中心                                          边缘离线单机
├─ 数百节点                                            ├─ 1 个节点
├─ mechd 独立部署                                      ├─ mechd 与 mechlet 同机
├─ 网络常通                                            ├─ 完全离线，U 盘交付
└─ 多团队、需 RBAC 与审计                              └─ 单人运维，但审计能力相同
```

要覆盖的部署形态：

- **优先**：裸机二进制 + systemd
- **v1 同时支持**：docker、docker compose
- **后续**：podman、Kubernetes

要覆盖的组件类型：主机配置（内核参数、limits、用户、挂载）、运行时（JDK）、中间件（nginx、PostgreSQL、MinIO、Kafka）、业务应用（Go / Java Web 应用）、大数据组件（多角色，如 HDFS 的 NameNode/DataNode）。

## 3. 非目标

明确不做的事，同样重要：

| 非目标 | 说明 |
|---|---|
| **不做 CI/CD** | Mecharion 从「已构建好的产物」开始工作。构建由开发者用自己的工具链完成，详见 [ADR-0015](../adr/0015-offline-first-hermetic.md) |
| **不做容器编排调度器** | 节点由用户显式指定，没有装箱、抢占、自动重调度。需要这些请用 Kubernetes |
| **不做监控告警平台** | 提供组件状态与健康信息并可导出，但不做指标存储、图表与告警规则引擎 |
| **不替代 Kubernetes** | Kubernetes 是 Mecharion 可以部署和纳管的目标之一，不是竞品 |
| **不做配置的自动修复宿主机依赖** | 缺失的 OS 层依赖会快速失败并给出明确提示，不尝试联网安装——那会破坏离线约束 |

## 4. 七条设计原则

以下原则是全部具体决策的推导依据。当后续设计出现分歧时，回到这里。

### 原则一：单机自足优先

**边缘离线单机是基准形态，中心化是在其之上叠加的一层，而不是反过来。**

单机形态是 `mechd` + `mechlet` **同机部署**，功能与多节点完全一致——同一套存储、同一套 API、同一个 WebUI、完整的事件与审计。**它不是「功能子集」，而是「同一套东西的同机部署」**；唯一的差异是两者之间走 unix socket 而非网络。

`mechd` 不在数据面上：它不可用时 `mechlet` 继续按最后已知的期望状态调和与自愈，业务不受影响。

如果反过来设计——先做中心化、边缘作为「离线降级模式」——就必然出现两套执行路径、两份行为差异和长期的一致性 bug。

> 参见 [ADR-0002](../adr/0002-mechlet-as-sole-engine.md)、[ADR-0026](../adr/0026-standalone-runs-mechd.md)

### 原则二：离线是约束，不是特性

部署阶段**不允许任何外部服务依赖**：不访问 apt/yum 源、不访问 npm/PyPI/Maven、不访问容器 registry、不做编译。

所有依赖在**组件开发阶段**由开发者用自己的构建工具解决，产物随 Pack 一起分发。这条约束通过 `mechpack lint --hermetic` 静态检查强制执行，官方 Pack 仓库的 CI 会拦截违规。

> 参见 [ADR-0015](../adr/0015-offline-first-hermetic.md)

### 原则三：不可变物化 + 原子切换

组件的每一次物化产生一个不可变的 **generation**。升级不是原地修改，而是「物化新 generation → 原子切换 → 健康校验 → 失败则切回」。

由此得到两个性质：**回滚是一次指针切换（秒级）**，且**旧版本始终可用**。数据目录独立于 generation，升级永不触碰。

> 参见 [ADR-0008](../adr/0008-immutable-generation-linkinto.md)

### 原则四：声明式为主干，命令式为逃生舱

一级命令是 `apply` / `status` / `diff`，不是 `install` / `repair`。系统持有期望状态，所有操作都是「让实际状态向期望状态收敛」。

命令式能力（`mechctl node exec`、Pack 里的 `command` 资源与 hooks）作为逃生舱保留，但 `command` 资源必须带 `unless` / `onlyif` / `creates` 守卫以保持幂等。

原因：「状态管理」和「可靠升级」在结构上依赖持久化的期望状态。放弃这一点，产品就退化成一个更弱的 Ansible。

### 原则五：抽象只在真正不同处划界

引入抽象层时，接缝必须划在**技术之间确实存在差异**的位置，而不是划在概念边界上。

以 `Runtime` 抽象为例：文件/用户/目录资源、配置渲染、健康检查、generation 切换、升级编排在 systemd 与 docker 之间**完全相同**，因此它们留在接口之上，只写一次；只有「进程如何被监管」在接口之下。接口划大了，每个实现都要重复相同逻辑，抽象反而制造重复。

> 参见 [ADR-0010](../adr/0010-runtime-abstraction.md)

### 原则六：显式优于隐式

- 不隐式安装 root 级运行时（缺 docker 就报错并告知如何安装，绝不偷偷装）
- 不隐式迁移数据目录（路径变更时拒绝启动并要求人工处理）
- 不隐式接管非本工具创建的资源（只操作带 `dev.mecharion.*` 标签的容器）
- Pack 不硬编码绝对路径（路径由 Node 声明的 roots/volumes 解析）

自动化工具在基础设施领域造成的最大伤害来自「善意的猜测」。宁可多一次报错，不要一次误删。

### 原则七：现场可诊断

产品会下沉到没有网络、没有工程师的边缘环境。因此：

- 状态尽量存为**普通文件**，`cat` 就能看（mechlet 不用数据库）
- 控制面用 **SQLite**，现场可以用 `sqlite3` 直接查（而非需要专用工具的 KV 存储）
- `Observe()` 返回的状态必须携带 `RuntimeRef`，让人知道该去 `journalctl -u xxx` 还是 `docker logs yyy`
- 每个 generation 保留 `config.dist/`，随时可对比新旧版本的默认配置差异

> 参见 [ADR-0012](../adr/0012-mechd-embedded-sqlite.md)、[ADR-0013](../adr/0013-mechlet-no-database.md)

## 5. 组件一览

| 二进制 | 角色 | 说明 |
|---|---|---|
| `mechctl` | CLI | 用户入口。连 `mechd`；`--local` 是 mechd 不可达时的本机只读诊断入口 |
| `mechd` | 控制面 | 期望状态存储（SQLite）、blob 存储、Pack 注册、Rollout 编排、HTTP API、审计、Web UI。**单机形态下与 mechlet 同机运行** |
| `mechlet` | 节点代理 | **唯一的执行引擎**。资源调和、Runtime 驱动、状态上报 |
| `mechpack` | 打包工具 | 组装、校验、签名、分发 Pack。**不构建你的软件** |

Web UI 源码位于核心仓 `webui/`，通过 `go:embed` 打进 `mechd`——因此单机形态同样具备完整的可视化界面。

## 6. 术语速查

| 术语 | 含义 |
|---|---|
| **Pack** | 可分发的组件包。包含元数据、参数定义、资源清单与载荷 blob |
| **Site** | 一组被统一管理的节点。可以是一个集群、一个机房，也可以是一台边缘单机 |
| **Component** | 某个 Pack 在某个 Site 中的一份部署实例 |
| **Role** | Pack 内定义的角色类型，如 `primary` / `replica` / `NameNode` |
| **RoleInstance** | 某个 Role 落在某个具体目标上的实例 |
| **Node** | 受管主机，运行 `mechlet` |
| **generation** | 一次完整物化，是原子切换与回滚的单位 |
| **blob** | 按 sha256 内容寻址的二进制载荷 |
| **Rollout** | 一次变更的执行过程（分批、健康门禁、暂停、回滚） |
