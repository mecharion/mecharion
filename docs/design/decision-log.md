# 决策清单

设计阶段确认的全部决策项，编号沿用初始评审时的顺序，便于追溯。**"依据" 列指向承载完整调研与理由的 ADR。**

- 状态：**已确认** = 设计阶段拍板；**待实现确认** = 方向已定，细节在对应里程碑落实
- 无 ADR 的条目表示该决策无实质候选对比（属推论或惯例），理由记在对应设计文档中

---

## A. 命名与品牌

| # | 决策 | 依据 |
|---|---|---|
| 1 | 项目名 **Mecharion**，读 *MEK-uh-RY-un*（rhymes with Orion），社区缩写 **m7n**；`mech` 只作拼接词根，不单独指代项目 | [ADR-0018](../adr/0018-project-naming.md) |
| 2 | 二进制：`mechctl`（CLI）· `mechd`（控制面）· `mechlet`（节点代理）· `mechpack`（打包工具） | [ADR-0018](../adr/0018-project-naming.md) |
| 3 | 仓库：`mecharion/mecharion`（核心 monorepo）· `mecharion/packs`（官方包）· `mecharion/.github`（组织首页） | [09-naming-conventions](09-naming-conventions.md#5-github-组织与仓库) |
| 4 | License **Apache-2.0**；Go module path `github.com/mecharion/mecharion`（不用 vanity import） | [ADR-0019](../adr/0019-namespace-domain.md) |
| 5 | Web UI 源码在核心仓 **`webui/`**，`go:embed` 打进 `mechd`，不独立建仓 | [09-naming-conventions](09-naming-conventions.md#5-github-组织与仓库) |

## B. 总体架构

| # | 决策 | 依据 |
|---|---|---|
| 6 | 采用**常驻 Agent** 架构 | [ADR-0001](../adr/0001-agent-based.md) |
| 7 | **边缘离线单机是基准形态**，中心化是其上叠加的一层；两种形态共用同一份 reconciler / Pack 解析器 / 资源引擎 | [ADR-0002](../adr/0002-mechlet-as-sole-engine.md) |
| 7b | **单机 = mechd + mechlet 同机部署**——`mechd` 不是「可选」而是「可同机」。存储、API、WebUI、审计全部一份实现，单机功能与多节点完全一致 | [ADR-0026](../adr/0026-standalone-runs-mechd.md) |
| 7c | 本机 mechd↔mechlet 走 **unix socket 且不用 mTLS**——socket 权限是内核强制的对端身份，比证书更强 | [ADR-0026](../adr/0026-standalone-runs-mechd.md) |
| 7d | mechlet **不再有 standalone/managed 之分**；`mechctl` 永远连 mechd，`--local` 退化为 mechd 不可达时的本机**只读**诊断入口 | [10-cli §1.4](10-cli.md#14-连接目标由环境解析不靠-flag) |
| 7e | mechlet 启动用**连接重试**而非 systemd `Requires=`——多节点下本机没有 mechd，同一套逻辑覆盖两种形态，避免 unit 文件分叉 | [01-architecture §4.2](01-architecture.md#42-启动依赖用重试不用-systemd-requires) |
| 7f | mechd 与 mechlet **始终同步升级**；跨版本时先 mechd 后 mechlet | [01-architecture §4.1](01-architecture.md#41-单机形态下的升级顺序) |
| 8 | 连接方向 **mechlet 主动拨出到 mechd**（长连 gRPC stream），mechd 永不反向连节点 | [ADR-0001](../adr/0001-agent-based.md) |
| 9 | 引导：`mechctl node bootstrap ssh://…` + `mechlet install --token`（离线）。**SSH 只用于 bootstrap** | [ADR-0001](../adr/0001-agent-based.md) |
| 10 | `mechlet` 支持 **one-shot 模式**，零额外代码换取 agentless 逃生舱 | [ADR-0001](../adr/0001-agent-based.md) |
| 11 | mechlet 自升级走 generation + 软链 + **watchdog 超时自动回退**，必须在节点规模上量前完成 | [01-architecture §4](01-architecture.md#4-mechlet-自升级) |

## C. 对象模型

| # | 决策 | 依据 |
|---|---|---|
| 12 | `Site → Component → Role → RoleInstance`，加 `Node`（带 labels）与 `Rollout` | [ADR-0003](../adr/0003-object-model-naming.md) |
| 13 | `Site.kind: edge | cluster | standalone`；`standalone` 在 `mechlet install --standalone` 时由同机 mechd 隐式创建 | [ADR-0004](../adr/0004-site-as-top-scope.md) |
| 14 | **保留不用的词**：`Service`（让给 systemd）、`Deployment`（含义两极且需留给 Rollout）、`Cluster`、`Instance` | [ADR-0003](../adr/0003-object-model-naming.md)、[ADR-0004](../adr/0004-site-as-top-scope.md) |
| 15 | 提到 systemd 时一律用其自有术语 **unit**，文档中不出现 "systemd service" | [ADR-0003](../adr/0003-object-model-naming.md) |

## D. Pack 格式

| # | 决策 | 依据 |
|---|---|---|
| 16 | `schema: pack/v1`，**不用域名做 apiVersion group**（域名注册后重新论证，结论不变） | [ADR-0019](../adr/0019-namespace-domain.md) |
| 17 | **逻辑与载荷分离**，blob 按 sha256 内容寻址；**thin pack** / **thick pack（`.mpack`）** 双形态 | [ADR-0005](../adr/0005-pack-logic-payload-split.md) |
| 18 | `version`（上游软件版本）+ `revision`（Pack 自身迭代）双版本号 | [02-object-model §5](02-object-model.md#5-三个版本概念) |
| 19 | **一 Pack 多角色**：`cardinality` / `requires` / 无 workload 角色 / `shared` 资源段 | [ADR-0006](../adr/0006-multi-role-pack.md) |
| 20 | `from: topology.…` 跨角色引用 → **渲染必须在 placement 之后**，mechlet 收到已解析拓扑快照 | [ADR-0006](../adr/0006-multi-role-pack.md) |
| 21 | params 用**自定义类型子集**（12 类型 + 运维语义字段），**类型集封闭、不提供 JSON Schema 逃生舱**；不够用时增补类型 | [ADR-0007](../adr/0007-params-custom-subset.md) |
| 22 | params 优先级：**Pack 默认 → Component → Role → ConfigGroup**；**模型中不存在无名的 per-node 覆盖**，CLI 自动建组消除摩擦 | [ADR-0021](../adr/0021-config-group.md) |
| 25b | **`placement` 放置约束**（affinity / antiAffinity + scope + enforcement），mechd 放置阶段校验 | [ADR-0020](../adr/0020-placement-constraints.md) |
| 25c | **`profiles` 部署形态**——在 mechd 放置阶段解析掉，引擎侧零新增概念；只管结构不管内容 | [ADR-0022](../adr/0022-deployment-profiles.md) |
| 25d | **`.Node.Facts`**——`requires` 校验用实时值，`defaultFrom` 渲染用**放置时快照并固化**；事实变化只上报漂移，不自动改配置 | [ADR-0023](../adr/0023-node-facts.md) |
| 25e | **`.Requires.<pack>.*`** 跨 Pack 引用；依赖分 `scope: node`（暴露 `.Paths`，要求同节点）与 `scope: site`（只暴露 `.Topology`） | [ADR-0024](../adr/0024-cross-pack-reference.md) |
| 25f | **Pack 粒度两条独立判据**：需共存 → 拆包（`jdk11`/`jdk17`）；仅不能原地升级 → `upgradePolicy.compatible`（`postgresql` 声明 `~16`）。因此不需要 `alias` | [ADR-0024](../adr/0024-cross-pack-reference.md) |
| 25g | **CLI 名词优先**：`mechctl <名词> <动词>`；无缩写/复数别名（补全解决输入长度）；顶层只留 `apply` / `deploy` / `version` / `completion`；**`delete` 取消，只保留 `remove`**——歧义由结构消解而非约定 | [ADR-0025](../adr/0025-noun-first-cli.md) |
| 25h | `component remove` 默认**保留数据目录**并登记为孤儿，`--purge-data` 才删；`orphans list/purge` 配套发现与清理 | [10-cli §4.3](10-cli.md#remove) |
| 25i | `deploy` 遇同名 Component **默认拒绝**，`--update` 才收敛；遇残留数据目录拒绝并要求 `--adopt-data`/`--purge-data`——**静默接管是真实的运维事故来源** | [10-cli §4.3](10-cli.md#deploy) |
| 25j | `secret` 类型参数**禁止 `--set` 明文**（会进 shell history 与 `ps aux`）；`--set-file` / `--set-stdin` 覆盖无人值守，TTY 交互仅供人工且**非 TTY 下不阻塞** | [10-cli §4.3](10-cli.md#secret-参数的三种输入) |
| 25k | mechlet 的 Linux 专属实现采用**平台隔离编译**（`*_linux.go` / `*_other.go`），非 Linux 平台返回「不支持」而非编译失败——保证 `go build ./...` 与三平台 CI 矩阵不为 mechlet 破例 | [25-roadmap](25-roadmap.md) |
| 29c | **路径在首次物化时固化**，之后不可变；解析结果与固化值不一致时拒绝调和而非自动迁移。路径中的名字是 **Component 名**（Component 名默认等于 Pack 名） | [spec §8.7–8.8](../spec/pack-v1.md#87-路径的解析与固化) |
| 23 | `mechpack lint --hermetic` 静态检查外部依赖调用，官方 packs CI 强制通过 | [ADR-0015](../adr/0015-offline-first-hermetic.md) |
| 24 | **Pack 签名为必需项**，默认 `enforce` | [ADR-0016](../adr/0016-mandatory-pack-signing.md) |
| 24b | **R46**：引用 `sensitive` 参数的模板，渲染它的资源 `mode` 的 others 位必须为 0。选择限制文件权限而非脱敏 diff 输出——引擎无法可靠识别渲染产物中的敏感片段 | [spec §19](../spec/pack-v1.md#19-校验规则汇总) |
| 25 | 打包命令叫 `mechpack assemble` 而非 `build`——只组装 Pack，不构建软件 | [ADR-0015](../adr/0015-offline-first-hermetic.md) |

## E. 资源模型与磁盘布局

| # | 决策 | 依据 |
|---|---|---|
| 26 | 每个资源类型必须实现 `Read / Diff / Apply` 三方法 | [06-state-and-drift §3](06-state-and-drift.md#3-资源模型) |
| 27 | v1 资源集见 [06-state-and-drift](06-state-and-drift.md#v1-资源集16-种)；`package` 提供但标记 non-hermetic | [ADR-0015](../adr/0015-offline-first-hermetic.md) |
| 28 | **主机配置与组件部署是同一个引擎**——无 `workload` 的 Pack 即纯主机配置 | [06-state-and-drift §3](06-state-and-drift.md#主机配置与组件部署是同一个引擎) |
| 29 | **路径设计**：Mecharion 自身遵循 FHS，data-dir 安装时指定后不可变；组件路径由 Pack 用 `paths` 声明，`linkInto` 调和原生布局，`distDir` 保留默认配置基线 | [ADR-0008](../adr/0008-immutable-generation-linkinto.md) |
| 29b | Node 声明 `roots` + `volumes`，Pack 用 `kind: multi` 支持多盘，绑定在 Component/Role/Node 级解析 | [ADR-0009](../adr/0009-node-volumes-multidisk.md) |
| 30 | **术语：generation**（原「版本目录」）。配置变更同样产生新 generation，回滚是统一动作 | [ADR-0008](../adr/0008-immutable-generation-linkinto.md) |

## F. Runtime

| # | 决策 | 依据 |
|---|---|---|
| 31 | `Runtime` 八方法接口；健康检查、升级编排、配置渲染**留在接口之上**；`Observe()` 返回归一化状态且必须带 `RuntimeRef` | [ADR-0010](../adr/0010-runtime-abstraction.md) |
| 32 | **v1 实现 systemd + docker + compose 三个 Runtime**；compose project 作为不透明整体；**不隐式安装 docker**；`managed: false` + `dev.mecharion.*` 标签隔离用户自有 docker；官方 `docker` Pack 用 systemd runtime 离线安装；**数据用 bind mount 不用 named volume** | [ADR-0011](../adr/0011-docker-compose-in-v1.md) |
| 33 | 为 Kubernetes 预留三件事：`Role.scope` 字段、**`RoleInstance` 拆分 `Target`/`Executor`**、`Ref.Kind` 预留 `Endpoint` | [ADR-0017](../adr/0017-k8s-extension-reserve.md) |

## G. 状态管理

| # | 决策 | 依据 |
|---|---|---|
| 34 | mechlet 持续调和（探测实际状态 ↔ 期望状态） | [ADR-0001](../adr/0001-agent-based.md) |
| 34b | **调和间隔默认 60s、健康检查间隔默认 15s，两者独立可配**——调和要重读整套资源，间隔应明显更长 | [06-state-and-drift §3.1](06-state-and-drift.md#31-调和循环) |
| 34c | `notify` **聚合去重、调和结束后统一执行**；`restart` 吸收 `reload`；**Diff 为空不触发**（否则每个调和周期都会重启服务）；notify 失败算调和失败 | [06-state-and-drift §6.1](06-state-and-drift.md#61-notify-的聚合) |
| 34d | 资源引擎接口：`Read` 三态（`Absent`/`Present`/`Unknown`，「本环境不适用」归为错误）；`Diff` 字段级、**行级 diff 在 CLI 层**；`Apply` **不接收 Changes、自身幂等**；资源级无事务，回滚能力来自 generation 原子切换 | 待写入 11-resource-engine.md |
| 35 | 每资源 `driftPolicy: report \| reconcile \| ignore`，**默认 `report`** | [06-state-and-drift §4](06-state-and-drift.md#4-漂移策略) |
| 36 | 断连时按最后已知期望状态继续自愈，重连后补报事件 | [ADR-0002](../adr/0002-mechlet-as-sole-engine.md) |
| 37 | ad-hoc 执行 `mechctl node exec -l role=db -- <script>` 走 agent 通道，不用 SSH | [ADR-0001](../adr/0001-agent-based.md) |

## H. 存储

| # | 决策 | 依据 |
|---|---|---|
| 38 | **mechd 用 `modernc.org/sqlite`** + `sqlc` + `goose`；WAL 模式，写收敛到单 goroutine | [ADR-0012](../adr/0012-mechd-embedded-sqlite.md) |
| 39 | **blob 绝不进 DB**，走文件系统内容寻址，DB 只存元数据与引用计数 | [ADR-0005](../adr/0005-pack-logic-payload-split.md) |
| 40 | **mechlet 不用数据库**：原子写 JSON + JSONL 事件缓冲 | [ADR-0013](../adr/0013-mechlet-no-database.md) |
| 41 | **HA v1 不做**（mechd 不在数据面上）；主备文件复制兜底；**存储收进 repository 接口** | [ADR-0014](../adr/0014-no-ha-in-v1.md) |
| 42 | 事件/审计单独表 + 保留策略；指标历史另开存储 | [07-persistence §1.4](07-persistence.md#14-工程约定) |

## I. 安全

| # | 决策 | 依据 |
|---|---|---|
| 43 | 多节点 mechlet ↔ mechd **mTLS**，bootstrap token 换每节点证书，支持轮换；**单机走 unix socket，不用 mTLS** | [08-security §3.1](08-security.md#31-mechlet--mechd) |
| 43b | mechd HTTP **默认绑 `0.0.0.0` + HTTPS**，首次启动自动生成 **10 年 CA + 1 年服务端证书**，剩余 <30 天自动轮换热重载；本机 `mechctl` 直接读 CA 文件，零配置 | [08-security §3.2](08-security.md#32-mechd-的-http-接口) |
| 43c | 首次启动生成初始 admin token，**只打印一次**——对外监听意味着认证不能是可选的 | [08-security §3.3](08-security.md#33-初始凭据) |
| 43d | **不创建 `mecharion` 用户或组**；socket `0600 root:root`，mechctl 需 root。理由：Pack 部署 ≡ 执行 root 代码，建组只会营造「受限权限」的假象（Docker 组的教训） | [08-security §3.5](08-security.md#35-不创建专用用户或组) |
| 44 | 明确边界：装 Pack ≡ 执行 root 代码，**签名 + 可信发布者列表是唯一信任锚**；不做执行沙箱 | [ADR-0016](../adr/0016-mandatory-pack-signing.md) |
| 45 | 全量审计：谁、何时、哪个 Pack 的哪个 version+revision、应用到哪些目标、结果如何 | [08-security §6](08-security.md#6-操作日志不是合规级审计)（这条决定当时的措辞后来更正为"操作日志"，见该节） |

## J. 路线

| # | 决策 | 依据 |
|---|---|---|
| 46–53 | 里程碑 M0 → M9，其中 **M4（docker/compose）刻意早于 M5/M6（漂移与升级）**——只有第二个 Runtime 存在时，那两块逻辑才天生 Runtime 无关 | [25-roadmap](25-roadmap.md) |
| 54 | 官方 Pack 顺序：`go-webapp` → `docker` → `jdk` → `java-webapp` → `nginx` → `postgresql` → `minio` | [25-roadmap](25-roadmap.md#官方-pack-顺序) |

## K. M2 实现阶段定下的

| # | 决策 | 依据 |
|---|---|---|
| 55 | **`driftPolicy` 只管漂移，不管期望变更**。判据是 generation 有没有换：digest 变了说明期望变了，无条件收敛。否则一个默认 `report` 的 template 会让所有配置变更永远发不出去 | [06-state-and-drift §4](06-state-and-drift.md#driftpolicy-只管漂移不管期望变更) |
| 56 | 健康检查三种探针 `http` / `tcp` / `exec`，**不区分「健康」与「就绪」**；一律探 `127.0.0.1` | [25-roadmap](25-roadmap.md#m2-定下的健康检查) |
| 57 | **无 libc 下限**（`CGO_ENABLED=0` 纯静态，glibc 与 musl 通用）；实际下限是 **systemd ≥ 239**，`Probe` 主动拦截并报出版本 | [25-roadmap](25-roadmap.md#m2-定下的支持范围) |
| 58 | 换 generation **不走 notify**——停/切/起已经涵盖；再叠一次就是在刚起来的进程上又重启一遍 | [06-state-and-drift §6.1](06-state-and-drift.md#61-notify-的聚合) |
| 59 | 失败的 generation 记为 `failed` 而非丢弃：目录留一个供诊断，状态保证它不会成为回滚落脚点 | [12-spec-and-state §2.3](12-spec-and-state.md) |
| 60 | 失败分类（`transient` / `permanent`）与外部命令执行**提到共享包**（`internal/faults`、`internal/command`），资源引擎与 Runtime 共用一套；否则 Rollout 要面对两套互不兼容的分类 | [11-resource-engine §2.3](11-resource-engine.md#23-错误分类) |
| 61 | 尚未实现的能力**明确报错而非静默跳过**（未实现的资源类型、指向 hook 的 notify）——静默会让 Pack 作者以为功能生效了 | [25-roadmap](25-roadmap.md#m2-交付了什么) |

## L. 跨组件凭据传递（M3 前置）

起因：**「go-webapp 要用 PG 的口令，依赖机制怎么把它送过去？」**
调研了 CM / Ansible Vault / Puppet hiera-eyaml / Chef Vault / k8s Operator /
Vault+Nomad / systemd credentials 七种做法。

| # | 决策 | 依据 |
|---|---|---|
| 62 | **`exports.fields`**：提供方导出具名字段，消费方自行组装。`format` 保留给形状确定的地址列表。理由：PostgreSQL 不可能知道下游要 libpq DSN、JDBC URL 还是 Spring 的三个分开字段 | [spec §5.4](../spec/pack-v1.md#54-exportsfields--具名字段与凭据) |
| 63 | **消费方不得读提供方的参数**（规则 49）。被消费的凭据须是专为该依赖关系存在的账号，不是 superuser——给下游应用 superuser 口令是提权 | [spec §5.4](../spec/pack-v1.md#54-exportsfields--具名字段与凭据) |
| 64 | **`params.generate`** 由引擎生成口令。**无人值守与边缘离线由此同时成立**：运维不输入任何东西，也不需要联系外部密钥服务（Vault 类方案做不到后者） | [spec §7.6](../spec/pack-v1.md#76-generate--引擎生成的密码) |
| 65 | **不变式收窄**：「密钥从不出现在渲染产物中」做不到（应用要从文件读）。改为「**只出现在最终消费点的文件里**，不进 spec / 归档 / 审计 / 日志 / UI / diff」 | [08-security §5.1](08-security.md#51-一条能守住的不变式) |
| 66 | **spec 里放不透明引用，不放明文**，落盘时字面量替换。净复杂度是**降低**的：否则七处持久化与展示点各要写一次脱敏，漏一处要等出事才发现 | [08-security §5.2](08-security.md#52-实现方式spec-里放引用不放值) |
| 67 | **轮换必须产生新 generation**——推翻了原先「排除 SecretRefs 使轮换不产生新 generation」的写法。值不进 digest，但 `secretRefs.version` 进；否则默认 `driftPolicy: report` 会让轮换永远发不出去 | [12-spec-and-state §1.5](12-spec-and-state.md#轮换必须产生新-generation) |
| 68 | 敏感传播由 **mechd 绑定时自动完成**，不是 lint 规则——lint 只看得见一个 Pack，依赖方可能来自别处单独发布。自动传播还使它**不可能被遗忘** | [spec §5.4](../spec/pack-v1.md#54-exportsfields--具名字段与凭据) |
| 69 | 导出字段的敏感标记由「引用的参数是不是 secret」**推导**，不由 Pack 声明——推导不可能与实际不一致 | `mechpack inspect` |
| 70 | mechd 侧**默认信封加密**（可关，关掉即明文 + `0600`）。两层密钥是为了轮换主密钥只需重包裹几十字节、接 KMS 不需迁移数据。明确其边界：挡 DB 副本外流，**不挡 root** | [08-security §5.5](08-security.md#55-mechd-侧的静态存储) |
| 71 | **不做 systemd credentials**（`LoadCredentialEncrypted`）：需 systemd ≥ 251，而支持下限是 239；且仍要求应用能从文件读 | — |
| 72 | **口令默认走 `envFile` + `${VAR}`，不内联进主配置**。这就是 Jasypt / `ENC(...)` 真正想解决的问题，而代价是零。应用支持 `password_file` 时用它更好 | [spec §7.7](../spec/pack-v1.md#77-口令怎么交给应用) |
| 73 | **禁止 `workload.systemd.env` 引用敏感参数**（规则 50）：内联 `Environment=` 会写进 0644 的 unit 文件，且被 `systemctl show` 原样打印（已实测确认；`EnvironmentFile` 只暴露路径） | [spec §19](../spec/pack-v1.md#19-校验规则汇总) |

## M. 调和的克制（M2 后置讨论）

起因是两个担心：**检测的开销**，与**引擎自作主张**。

| # | 决策 | 依据 |
|---|---|---|
| 74 | **摘要缓存** `(size, mtime) → sha256`，随实例状态持久化。否则十个组件各带 50MB 二进制的节点每分钟要读并哈希 500MB——持续调和一旦昂贵就会被关掉，那样产品最核心的能力就没了 | [06-state-and-drift §3.1](06-state-and-drift.md#检测的开销) |
| 75 | 缓存必须堵住 **racily clean 窗口**：mtime 与记录时刻挨得太近的条目一律重算。否则「同一秒内把 `port: 8080` 改成 `port: 1234`」这类同长度改写会**永久**检测不到。判据与 git 索引一致 | 同上 |
| 76 | **`ack-drift`**：临时修改要有名分。有期限（不会悄悄变永久）、有理由（进审计）、仍然检测（只是不告警） | [06-state-and-drift §4.1](06-state-and-drift.md#41-临时修改需要有名分) |
| 77 | `driftPolicy` 允许 **Site/Component 级覆盖，且只能放松不能收紧**。写在 Pack 里等于 Pack 作者决定了运维现场的临时修改能不能活下来——权责反了 | [06-state-and-drift §4.2](06-state-and-drift.md#42-谁说了算pack-作者还是现场运维) |
| 78 | **自动改回不得顺带重启**（默认降级为只上报，`allowDriftRestart` 显式打开）。运维只是想试个参数，服务却在他手底下重启了——这是最不该自作主张的动作 | [06-state-and-drift §4.3](06-state-and-drift.md#43-自动改回不得顺带重启) |
| 79 | **不采用「部署周期外一律不碰配置」的全局模式**：它会连**检测**一起关掉，而持续漂移检测正是选常驻 Agent 的理由（ADR-0001）。用户真正的诉求由 76–78 三条精确满足，不必牺牲检测 | [ADR-0001](../adr/0001-agent-based.md) |

## N. M3 控制面设计

| # | 决策 | 依据 |
|---|---|---|
| 80 | **ordinal 一次分配后固化，不由节点名排序推导**。按名字排序会让一次扩容改掉所有已有节点的身份（ZooKeeper `myid`、Kafka `node.id`），集群当场损坏。序号不回收，允许空洞 | [ADR-0028](../adr/0028-stable-ordinals.md) |
| 81 | **下发用服务端流，上报用一元 RPC**。关键是失败模式隔离：心跳丢包不该撕掉正在进行的部署 | [ADR-0029](../adr/0029-push-over-server-stream.md) |
| 82 | **下发一律全量，不做增量同步**。幂等下发（digest 相同即无操作）让整类同步协议问题消失，代价只是重连瞬间几 MB | 同上 |
| 83 | **协议里没有动词**——下发的永远是期望状态而非指令。指令式接口会让断连自治不可能 | 同上 |
| 84 | **密钥用信封加密存储、用不透明引用下发**。引用方案的净复杂度**更低**：否则七处持久化与展示点各要写一次脱敏 | [ADR-0030](../adr/0030-secret-storage-and-delivery.md) |
| 85 | **解析管线是纯函数**（除 generate 首次生成与 ordinal 首次分配）。`render` / `diff` / `--dry-run` 是同一条管线少走两步，**不另写预演逻辑**——两套实现迟早不一致，而不一致的预演比没有预演更糟 | [15-render-pipeline §9](15-render-pipeline.md#9-可复现性) |
| 86 | **组件间按 requires 拓扑排序解析**，且解析顺序与 Rollout 下发顺序是同一个顺序 | [15-render-pipeline §5](15-render-pipeline.md#5-组件间的解析顺序) |
| 87 | **`scope: once` 的仲裁在 mechd**，mechlet 完全不理解 once 语义。那是跨节点语义，放在唯一有全局视角的地方，mechlet 就永远不需要相互查询 | [18-hooks §1](18-hooks.md#1-职责切分) |
| 88 | **hook 不做重试**。不幂等的 hook 重试一次就可能把事情做坏两遍，而引擎无从判断它是否幂等 | [18-hooks §4](18-hooks.md#4-失败处理) |
| 89 | **mechd 的数据库是期望状态的一部分，不是缓存**。`ordinal` 与 `pack_bindings` 是分配出来、无法重算的值；主密钥必须与 DB **分开备份**，放一起就抵消了信封加密 | [07-persistence §1.7](07-persistence.md#17-什么必须备份) |
| 90 | **不引入内部消息队列**：单进程内 channel 足够，MQ 会带来一个必须运维的新组件，与边缘单机直接冲突 | [13-mechd §7](13-mechd.md#7-不做的事) |

## O. M5 / M6 实现阶段定下的

| # | 决策 | 依据 |
|---|---|---|
| 91 | **mechlet 落盘期望状态与密钥副本**（本地信封加密库）。代价：节点被攻破即暴露该节点全部组件凭据的明文，KEK 与密文同机 | [ADR-0033](../adr/0033-mechlet-local-desired-state.md) |
| 92 | **digest 排除 `runState` / `driftPolicy` / `suppressions`**——它们是「这台机器该被怎么对待」，而 generation 是「盘上有什么」 | [22-upgrade §2](21-upgrade-and-rollback.md) |
| 93 | **失败的 digest 被锁住，不自动重试**；且只在**有旧版本可以停留**时才锁——首装失败没有服务可丢，重试是对的 | [22-upgrade §2.4](21-upgrade-and-rollback.md) |
| 94 | Rollout **由上报推进，不设后台循环**；无 `pending` 态；`failed`（系统判定）与 `aborted`（人叫停）分开 | [22-upgrade §2.6](21-upgrade-and-rollback.md) |
| 95 | `rollout pause` **冻的是判定不是部署**；`rollout abort` **真的回退**，走与 `component rollback` 相同的路径 | 同上 |
| 96 | 回收的引用**落成状态**（节点级清单）而非 prune 返回的事件；判据**全局**（跨全部实例）；`docker image rm` **不加 --force**，留住 dockerd 自己那道防线 | [22-upgrade §2.5](21-upgrade-and-rollback.md) |
| 97 | 「回滚」与「维持」必须分开：前者是事件、后者是状态。混在一起会让稳定停在旧版的机器每周期报一次回滚，真正那一次因此被淹没 | [22-upgrade §2.4](21-upgrade-and-rollback.md) |

## P. M7 多节点设计

| # | 决策 | 依据 |
|---|---|---|
| 98 | **节点加入 token 即授权**，不设人工批准环节；token 可绑定节点名、有 TTL 与次数上限、可吊销、每次使用进审计。代价：拿到未绑名 token 的人可以往集群里塞一台机器 | [ADR-0034](../adr/0034-node-join-and-identity.md) |
| 99 | **身份以客户端证书 CN 为准**，多节点下忽略请求里的 `node_name`，不一致时**拒绝**而非静默采信证书 | 同上 |
| 100 | **吊销走 mechd 应用层状态检查，不做 CRL**。前提是 [ADR-0014](../adr/0014-no-ha-in-v1.md) 的单 mechd。代价：被吊销的证书**仍能完成 TLS 握手** | 同上 |
| 101 | **滚动升级不做 `maxSurge`**，只有 `maxUnavailable` + `canary`。裸机实例有固化 ordinal、固定数据目录与端口，「多起一个」不成立。代价：**做不到严格零中断** | [ADR-0035](../adr/0035-no-maxsurge.md) |
| 102 | 批次**创建时一次算好并落盘**，中途节点上下线不改已算好的批次——否则「一共几批」这个答案会在执行过程中变化 | [22-multi-node §4](22-multi-node.md) |
| 103 | 健康门禁要求状态**持续 `stableFor`（默认 30s）**。没有稳定窗口，一台正在崩溃循环里的机器会批准下一批，故障被逐批放大 | [22-multi-node §2.5](22-multi-node.md) |
| 104 | 一批失败即**整体暂停**，**不自动回滚已完成批次**——那是一次更大的变更，判断依据只有 Pack 作者知道。代价：集群停在混合版本，status 必须把它显式显示出来 | [22-multi-node §2.6](22-multi-node.md) |
| 105 | 批内节点**离线不跳过**（等超时判失败），**cordon 跳过**（人明确说过别动）；两者都要在 status 里指名 | [22-multi-node §2.7](22-multi-node.md) |
| 106 | 身份判定收口在 `protocol.nodeOf` **一处**——五个 RPC 各读一次 `node_name` 的话，「某个 RPC 忘了忽略它」就是一个能绕过 mTLS 的洞 | [22-multi-node §2.2](22-multi-node.md) |
| 107 | mTLS gRPC 端口 `--grpc` **默认开**（`0.0.0.0:8444`）。暴露面是一次要求客户端证书的 TLS 握手；默认关掉则每套多节点部署都要先改配置，而**那一步出错时的症状是节点连不上** | [22-multi-node §6.2](22-multi-node.md) |
| 108 | **传输形态由地址决定**（`unix://` 明文，其它 mTLS），不另设 `--mtls` 开关——布尔量迟早与地址对不上，而错配的症状看不出是配置问题 | 同上 |
| 109 | `internal/pki` 独立成包：mechd 启动、`mechd ca issue`、Join RPC 三处**共用同一套签发逻辑**。两套 x509 模板意味着两套边界条件，而证书出错时症状都一样 | 同上 |
| 110 | `node add`（登记）与「加入」分开：前者是控制面动作、状态为 `offline`，后者需要那台机器上有人 | [10-cli §4.2](10-cli.md#42-node) |
| 111 | join 走 **HTTP API** 而不是 gRPC：它是一问一答，且 gRPC 那个监听是 `RequireAndVerifyClientCert`——放松成 `VerifyClientCertIfGiven` 会把「必须有证书」从传输保证降级成应用层检查 | [22-multi-node §3.1](22-multi-node.md) |
| 112 | **私钥不过网**：节点本机生成密钥对，只发 CSR。`mechd ca issue` 做不到这件事，因此它是次选而非等价路径 | [22-multi-node §3](22-multi-node.md) |
| 113 | 信任锚是 **CA 公钥指纹**（随 token 带外交付），不是 TOFU，也不用搬 ca.crt。取 SPKI 而非整张证书：CA 重签时公钥可不变，否则所有未用 token 会集体失效 | [22-multi-node §3.2](22-multi-node.md) |
| 114 | 签发的 **CN 由授权决定，不信 CSR 里写的**；CSR 的自签名必须验，否则可拿别人的公钥换证书 | [22-multi-node §6.4](22-multi-node.md) |
| 115 | join 校验**先 token 后名字**：反过来的话，拿废 token 的人能通过「名字被占了没」的报错探测集群里有哪些节点 | `internal/mechd/join.go` |
| 116 | 集群测试镜像**带 sshd**，`test/node` 保持贫瘠：bootstrap 的前提就是目标机器有 sshd，而真实用户的机器本来也不是贫瘠的。hermetic 由单节点套件强制 | [22-multi-node §6.5](22-multi-node.md) |
| 117 | bootstrap 暂存目录**不放 /tmp**：加固机器普遍挂 noexec（CIS 基线），症状是一个 126 退出码。放安装根下，失败发生在正确的地方 | 同上 |
| 118 | bootstrap **不校验 SSH 主机密钥**，理由与代价都写明：信任锚在下一步的 CA 指纹校验，中间人拿不到能通过指纹的 mechd 身份；但他能让「这台机器」变成他的机器 | 同上 |
| 119 | `node remove` 仍有实例时**默认拒绝**：删掉一个还跑着组件的节点，会让那些组件从中心视图消失而仍在机器上跑 | [10-cli §4.2](10-cli.md#42-node) |
| 120 | 吊销判据挂在**身份收口处**，每个 RPC 都查：被吊销的证书握手仍会成功，「握手过了」不等于「获准了」 | [22-multi-node §6.6](22-multi-node.md) |
| 121 | `revoke`（保留在册但切断）与 `remove`（抹掉）分开；两者都在**已连接**的 agent 上立刻生效，不必等重连 | 同上 |
| 122 | 换证书**不重启**：拨号用 `GetClientCertificate` 每次握手读盘上的当前证书 | 同上 |
| 123 | 证书已过期时**不装作能自愈**：续期本身要走 mTLS，这是人必须介入的状态，喊一声并给出路 | 同上 |
| 124 | agent 侧把「被控制面拒绝」与「连不上」分开：前者不会自己好转，沉默会让站在机器前的人看不出为什么不再同步 | `internal/agent/certs.go` |
| 125 | `cordoned` 进下发字段，**是状态不是动词**：随全量重推，断连期间的语义不需要节点自己猜。协议层的守卫测试真的拦了一次 | [22-multi-node §6.7](22-multi-node.md) |
| 126 | cordon 停的只有「往机器上落」：连接、上报、孤儿、证书续期照常，**期望状态照常落盘**——那决定了 uncordon 之后能否立刻收敛 | 同上 |
| 127 | cordon 的进入与解除**都要有声音**：解除时调和循环不再走那条分支，没人会说话 | 同上 |
| 128 | 批次**必须管住下发**，不只是落盘：只记不管的实现能说出漂亮的「第 1/3 批」而三批的机器早已升完，比没有分批更糟 | [22-multi-node §6.8](22-multi-node.md) |
| 129 | 混版下发靠**渲染两次**（新版一次、起始版一次）再按实例挑，不在一次渲染的结果上改版本号——那样的 digest 与两边都对不上 | 同上 |
| 130 | 代价：混版期间每个实例的 `topology.…` 视图来自它自己那一版的 Pack。让两边看到同一份混合视图会使规格随别人的进度反复重算 | 同上 |
| 131 | **先开 Rollout 再下发**：下发会唤醒节点，节点醒来就来拉规格，此时没有批次记录就等于一次性下发。宿主侧单测抓不到，容器验收抓到了 | 同上 |
| 132 | 收敛的定义加一条「跑着目标版本」：否则排队中的实例与自己的旧版期望对得上，`status` 会在第一批没放行时就说「已收敛」 | 同上 |
| 133 | `AdvanceRollout` 宣布成功要**同时**满足「整体收敛」与「所有批次已完成」——少了后者，第一批做完就归档，剩下的批次再不会放行 | 同上 |
| 134 | `pendingVersion` 区分「在排队」与「出了问题」：两者在收敛列里都是「否」，而一个只要等，一个要人去看 | 同上 |
| 135 | 被 cordon 的名单存成 `seq=0` 的批次记录：只写日志的话，事后问「为什么这台还是旧版」时日志已经翻过去了；`seq=0` 让它不进批次分母 | 同上 |
| 136 | `maxUnavailable` / `canary` **只影响下一次变更**：半路重算会让「一共几批」在执行过程中变化，而运维正是靠它判断还要多久 | 同上 |
| 137 | quorum 角色的并发上限按**参与分批的**实例数算，不按声明总数：被 cordon 的本来就不会动，计进分母只会让并发度虚高 | `internal/mechd/batch.go` |
| 138 | `rollout pause` 停在当前批：已放行的照常跑完，下一批不放行。只冻结判定却继续推批次等于没有 pause | `internal/mechd/rollout.go` |
| 139 | 成功的判据**只有**「每一批都做完」，不叠加「整体已收敛」：叠加会让被 cordon 的机器把每一次「先隔离一台再升级」都拖成超时失败 | [22-multi-node §6.8](22-multi-node.md) |
| 140 | 「abort 真的把机器退回来」挪到多节点验收：单机只有一批且 pause 在放行之前，机器根本没动过，那条断言在单机上无从检验 | `test/multinode/rollout_linux_test.go` |
| 141 | 健康门禁加第 4 条判据「窗口内重启计数没涨」：稳定窗口是有限的，一个每 40 秒崩一次的进程能从 30 秒窗口里溜过去 | [22-multi-node §2.5](22-multi-node.md) |
| 142 | 门禁**自己判**，不搭 `InstanceView.Converged` 的便车：那个字段为显示需要会被调整，跟着它走会悄悄改掉滚动升级的判据 | `internal/mechd/gate.go` |
| 143 | 失败之后门禁**继续关着**：查「最近一条」Rollout 而不是「进行中」那条，否则失败的瞬间剩下的批次会一起升上去 | [22-multi-node §6.9](22-multi-node.md) |
| 144 | 失败那一批的机器算「已放行」，不把规格抽回旧版：那等于替人做了整体回滚的决定，而被拦下的机器多半正在崩 | 同上 |
| 145 | `BatchTimeout` 从**批次放行**起算；有批次时全局 `RolloutTimeout` 不生效——否则「10 分钟内未收敛」会盖掉指名道姓的那条 | `internal/mechd/gate.go` |
| 146 | `StableFor` / `BatchTimeout` 都不是旋钮：慢启动靠 Pack 的 `health.startupGrace` 表达，加旋钮等于把同一件事表达两遍 | [22-multi-node §2.5](22-multi-node.md) |
| 147 | 窗口起点与重启基线**落盘**（`rollout_batches.healthy_since` / `restart_baseline`）：放内存的话 mechd 一重启窗口就重置，重启前后答案不一致 | `internal/store/migrations/00013_batch_gate.sql` |
| 148 | 「没声明健康探针」放行，只有明确 `unhealthy` 才拦：要求 healthy 会让每个没探针的 Pack 永远卡在门口 | `internal/mechd/gate.go` |
| 149 | mechd 关闭时先 `Drain()` 让订阅流收摊，再 `GracefulStop()`：Subscribe 是长连流永不返回，少了这一步 mechd 一定挂到 `TimeoutStopSec` 再吃 SIGKILL | `internal/protocol/server.go` |
| 150 | 代价不只是「重启慢半分钟」：SIGKILL 意味着 `defer st.Close()` 走不到，SQLite 的 WAL 没有干净关闭 | 同上 |
| 151 | 关闭路径**双层**：Drain 是机制，超时 + `Stop()` 是兜底。关闭永远不该无限等——今天卡 Subscribe，明天可能是别的 handler | `internal/cli/mechdcmd/serve.go` |
| 152 | 兜底会掩盖机制失效：摘掉 Drain 做变异时，「没被 SIGKILL」那几条断言**一条都没红**。因此验收要额外断言**没走兜底路径** | `test/multinode/wiring_linux_test.go` |
| 153 | 读 journal 判断行为时要用 `--since <停机前的时刻>`，不能用 `-n N`：后者跨多次启停，会拿上一轮的日志判这一轮 | 同上 |
| 154 | 批次没过门禁 → `halted`（停下等人）而不是 `failed`（终态）：它有前路，修好之后 resume 从这一批续做 | [22-multi-node §2.6](22-multi-node.md) |
| 155 | 不复用 `paused`：脚本判 `state == "paused"` 分不出「我按的」和「出事了」，而这两件事在复盘里差得远 | 同上 |
| 156 | halted 不写 `ended_at`：这次变更还没收场，写了的话 history 里它看起来就像已经完了 | `internal/mechd/rollout.go` |
| 157 | `resume` 只做三件事：失败那批退回 released、门禁窗口清零、放行时刻重置。**已完成的批次一个都不碰** | 同上 |
| 158 | 放行时刻必须重置，否则刚 resume 就会立刻再次超时——运维会看到「刚续做就又停了」而机器明明是好的 | 同上 |
| 159 | halted 时 `pause` 被拒：它已经停了，pause 什么都不改变却会把「为什么停的」冲掉 | 同上 |
| 160 | halted 时 `AdvanceRollout` 早返回，挡的不是推进（那本来就会停），而是**自动回滚判定把 halted 冲成 failed** | [22-multi-node §6.10](22-multi-node.md) |
| 161 | `current()` 要认 failed 批次：停下之后运维问的第一句是「停在第几批」，只认 released 的话 status 会说「第 0/3 批」 | `internal/mechd/release.go` |
| 162 | `halted` 与 `failed` 的分界线是**节点侧回没回滚**：Start/健康失败 → 自动回滚 → failed（那个 digest 已被 blocked，resume 推不动）；Apply 失败 / 节点离线 → halted | [22-multi-node §6.10](22-multi-node.md) |
| 163 | 验收不能拿「新版起不来」去造 halted——那条路走自动回滚。§2.7 的「节点离线」才是 batchTimeout 的用武之地 | `test/multinode/rollout_linux_test.go` |
| 164 | §5 第 13 行由「设 2 被拒」改为「压到 1 并且说出来」：旋钮在 Component 上而 quorum 在角色上，为前者拒掉整条设置会让同组件的无状态角色也用不上 | [22-multi-node §5](22-multi-node.md) |
| 165 | 「无证书直连」的验收要用夹具二进制发起**真的握手**：原来那条测的是 agent 自己拒绝启动，一个没启用 mTLS 的 mechd 照样能通过 | `test/tlsprobe/` |
| 166 | 安全断言的判据必须是**因为什么失败**，不能是「反正失败了」：TLS 告警（`tls: certificate required`）与应用层 EOF 要分开，否则 mTLS 降级验不出来 | [22-multi-node §5.1](22-multi-node.md) |
| 167 | `resume` 能救的是「还没做」，不是「做坏了」：节点已回落的 digest 在节点侧被 Blocked，推同一个 digest 不会再试。好在它落在 `failed` 而非 `halted`，状态机自己把边界表达对了 | [22-multi-node §2.6](22-multi-node.md) |
| 168 | 造「节点离线」要在**升级发起之前**停 agent：放行之后再停，可能杀在物化途中，那走的是 Blocked 回落而不是「没上报」——同一条测试因此单独跑绿、进套件红 | `test/multinode/rollout_linux_test.go` |
| 169 | 里程碑收尾要**逐份文档核对**，不只看代码与测试：M7 收尾时查出 8 处缺口，其中 4 处是**已经错了**的（`pause` 语义、吊销列表、README 说 mechd 尚未开始、ADR 链接名） | 本次审计 |
| 170 | 陈旧文档比缺失文档更糟：`10-cli.md` 里 `pause` 还写着 M6 的「不是暂停推送」，而第 9 步已经把它改成也停批次队列——照着它写脚本的人会得到相反的行为 | `docs/design/10-cli.md` |
| 171 | M8 前端选 Vue 3 + Element Plus + UnoCSS；产物由 `go generate` 构建，**不进仓库**（`.gitignore` 只留 `.gitkeep`） | [ADR-0036](../adr/0036-webui-vue-and-generated-dist.md) |
| 172 | 因此「拿到源码就能 build 出完整产物」到此为止——这是对 sqlcgen/agentpb 那条纪律的**刻意例外**：前端产物的体量与变更频率不是一个量级 | 同上 |
| 172b | `//go:embed all:dist` 的 `all:` 前缀不能省：它让只含 `.gitkeep` 的空目录也编译得过，否则干净 clone 直接编译失败 | 同上 |
| 172c | 没构建 UI 时要给说明页而不是 404——404 看起来像路由坏了 | 同上 |
| 173 | 用户体系只做最简：用户名口令 + 图形验证码，**登录即全权**。不预留 `role` 空字段——那只会制造「看起来支持其实不支持」 | [ADR-0037](../adr/0037-login-is-full-privilege.md) |
| 174 | **传统图形验证码已挡不住机器**：2026 年商用求解亚秒级、成功率约 99%。改用 PoW（Argon2 内存硬）做防护，滑块只做可见的人机信号 | 同上 |
| 175 | 分工要写清楚：安全性**只**来自 PoW 与限流；滑块 ML 可破，把它当防护会高估整体强度 | 同上 |
| 175b | PoW 难题必须**一次性核销**：不核销的话算一次就能重放无数次登录尝试，成本压制归零 | [23-web-ui §3](23-web-ui.md) |
| 176 | 会话用 HttpOnly cookie 而不是 localStorage 存 token：一次 XSS 就偷不走会话。代价是要防 CSRF（SameSite + 自定义头） | [23-web-ui §3](23-web-ui.md) |
| 177 | 会话落 SQLite 不放内存：放内存的话 mechd 一重启所有人被登出，与「状态可以重复确认」相悖 | 同上 |
| 178 | `node bootstrap` 在 UI 里做**引导**不做代执行：让 mechd 持有能登录所有节点的 SSH 私钥，等于把 M7「零入站端口」的收益还回去 | [23-web-ui §2.4](23-web-ui.md) |
| 179 | Pack 上传**先校验后落盘**：先落盘再校验的话，一次失败上传会留下半截 Pack，而放置阶段会看见它 | [23-web-ui §2.7](23-web-ui.md) |
| 180 | 表单要标出每个值**来自哪一层**（Pack 默认 / Role 级 / 本组覆盖）：不标的话，「以为在改一台机器实际改了整个角色」这类误操作没法避免 | [23-web-ui §4.1](23-web-ui.md) |
| 181 | `//go:embed all:dist` 的 `all:` 不能省，且要有测试盯着：少了它干净 clone 编译失败，而报错看不出与前端有关 | `internal/webui/embed.go` |
| 182 | 按需引入要**同时**去掉全量 CSS 与 `.use(ElementPlus)`：只配 resolver 不去掉那两行，等于没配（JS 1032→127 KB，CSS 360→17.6 KB） | `webui/src/main.ts` |
| 183 | 前端产物的压缩用 Go 做不用 npm 插件：前端依赖越少，五年后重写它时越轻，而 gzip 在标准库里 | `internal/tools/webuibuild` |
| 184 | SPA 回退**只对没有扩展名的路径**：一个找不到的 .js 回退成 HTML，浏览器只报一句语法错误，真实原因（资源 404）被完全掩盖 | `internal/webui/webui.go` |
| 185 | 产物不入库因此 CI 没有「一致性检查」可做，能做的是**确认它还构建得出来**——前端坏了要在 CI 红，不是等发布时才发现 mechd 没界面 | `.github/workflows/ci.yml` |
| 186 | 前端依赖全部升到最新，**只有 TypeScript 停在 6.x**：TS 7 不再导出 `lib/tsc`，vue-tsc 起不来。不为一个版本号放弃整条类型检查 | `webui/package.json` |
| 187 | 跨大版本升级要核对**静默失效**，不能只看「构建成功」：UnoCSS 的原子类、Element Plus 的按需样式，失效时构建照样过、界面变裸 HTML | 同上 |
| 188 | 口令**没有 `--password` 选项**，非交互且无 `--password-file` 时直接拒绝：不从 stdin 裸读，那会把人推向 `echo pw \| mechctl` | `internal/cli/ctlcmd/user.go` |
| 189 | argon2 代价参数写进编码串：将来调强不需要迁移，老口令用当初参数验，验证成功那一刻顺手重算 | `internal/password/password.go` |
| 190 | 口令只管长度（≥12），不管字符类别：「必须含大小写数字符号」会把人推向 `Passw0rd!` 这类可预测模式 | `internal/mechd/user.go` |
| 191 | 「用户不存在」与「口令错」必须**同一句话、同样耗时**：区分了等于白送一个用户名枚举接口 | 同上 |
| 192 | 摘要比较的判据是「**任何一位被改动都会被发现**」，不是「错口令被拒」：后者放过了「只比第一个字节」这种把强度砍到 2^8 的改动 | `internal/password/password_test.go` |
| 193 | 用户改为**单一固定 `admin`**、首访 UI 初始化：目标是无人值守部署，要求先 SSH 敲命令等于把自动化链断在最后一步 | [ADR-0037](../adr/0037-login-is-full-privilege.md) |
| 194 | 代价照实认：**初始化前存在抢注窗口**（默认监听 0.0.0.0）。缓解=一次性 409 + 启动即 WARN + 审计记来源 IP + 限流 | 同上 |
| 195 | **刻意不做超时锁定**（Portainer 那种）：部署到访问可能隔几小时，超时会把正常流程锁死，而唯一出路是回服务器敲命令——正是本方案要避免的 | 同上 |
| 196 | 不做多用户：没有角色的多用户在**权限隔离**上等于零，它能提供的问责与凭据生命周期要等角色做出来才成立，现在做是先付复杂度后拿收益 | 同上 |
| 197 | 只有一个账号也要**校验用户名**：「名字随便填都行」的实现在将来加账号时会变成洞，而那时没人会想起来这里从没校验过 | `internal/mechd/user.go` |
| 198 | 审计不信任 `X-Forwarded-For`：它是客户端可以随便写的，写进审计的必须是我们确实看到的地址 | `internal/mechd/userapi.go` |
| 199 | 挑战放内存、会话落库：丢失的代价差着量级——挑战丢了重来一次，会话丢了所有人被登出，而重启是常规操作 | [23-web-ui §6.4](23-web-ui.md) |
| 200 | PoW 的目标摘要必须下发（它就是题面）；服务端仍自己重算核对，不削弱任何东西 | `internal/authn/challenge.go` |
| 201 | 挑战**无论对错都先核销**：只在成功时核销的话，一次 PoW 的成本会被摊到几十次滑块尝试上 | 同上 |
| 202 | CSRF 做两道（SameSite=Strict + 自定义头），不把防护押在浏览器实现上；Bearer token 那条不受此约束 | `internal/mechd/authapi.go` |
| 203 | 明文 HTTP 下不能置 `Secure`：置了浏览器根本不存 cookie，登录会表现成「密码错」——一个极难查的现场 | 同上 |
| 204 | 改口令要清掉全部会话：不清的话被偷走的会话改完口令仍有效，而改口令的动机通常正是怀疑被偷 | `internal/mechd/user.go` |
| 205 | `API.Challenges` 用接口不用具体类型：HTTP 测试塞桩，避免在 authn 上开「能解出答案」的导出方法——那种方法迟早被接到别处 | `internal/mechd/httpapi.go` |
| 206 | 限流按 **IP** 不按用户：只有一个账号，按用户等于任何人都能靠反复输错把管理员锁死。代价是 NAT 后面的人互相连累 | `internal/authn/ratelimit.go` |
| 207 | `nodes.status` 退回只答「**出现过没有**」，在线与否在读的那一刻由长连接算出：一个只进不出的状态列没法靠补写路径修好——mechd 重启、进程被杀、机器断电各有各的漏法 | [22-multi-node §6.13](22-multi-node.md) |
| 208 | 在线判据用**长连接**不用「最后上报时间」：后者会把一台没有实例、因而从不上报的空闲机器判成离线，而那恰是运维最需要确认它活着的时刻 | 同上 |
| 209 | 节点状态**三个值**（pending / online / offline）：「还没装」与「装了但死了」要人做的事完全不同，合并之后唯一线索是「最后上报时间是不是空的」 | 同上 |
| 210 | gRPC 两侧都配 keepalive：拔电源不产生 TCP FIN，Linux 默认要 2 小时才探测，在此之前「在线」这个判据一直是错的 | `internal/protocol/keepalive.go` |
| 211 | 服务端 `EnforcementPolicy.MinTime` 必须跟着客户端一起调：gRPC 默认 5 分钟，配错的症状是连接每几分钟断一次而日志里只有 `too_many_pings` | 同上 |
| 212 | `Presence` 没接上时一律答离线，不沿用库里的值：沿用会让一次接线遗漏毫无症状，而那正是本条要修的缺陷的形态 | `internal/mechd/service.go` |
| 213 | UI 里收敛分**三态**（已收敛 / 排队中 / 未收敛）：二值化之后「在等」与「出事了」长得一样，而 `pendingVersion` 存在的理由正是要免掉这次比对 | [23-web-ui §6.5](23-web-ui.md) |
| 214 | 「换个前端」不产生新缺陷，只是换了个照明角度：同一个谎话在 CLI 的文本行里混了很久，做成一列带颜色的标签才被看见 | 同上 |
| 215 | keepalive 的验收必须同时卡**下限**：早于一次探测超时就变 offline 的，发现它的一定是别的东西。`docker network disconnect` 测的是内核，不是这几行代码 | [22-multi-node §6.14](22-multi-node.md) |
| 216 | 已知缺口：`node add`（pending）之后无法 join——守卫拒绝任何已在册的名字。不在 M8 顺手改，因为放松它要先定「离线签发过证书但没连上来」怎么算 | [22-multi-node §6.15](22-multi-node.md) |
| 217 | 表单的声明合并与来源层由 `render` 导出，mechd 不重写一遍：两处各自决定优先级，只会在某个取值不对时才被发现，而那时没人会想到去比对两份实现 | [23-web-ui §4.2](23-web-ui.md) |
| 218 | 表单跑一次**真解析**：`from` 是只读展示，而展示需要值，值依赖拓扑与放置结果 | `internal/mechd/form.go` |
| 219 | 算不出来的 `from` 标 `pending`，不回空值：空值看起来像「没配」，而用户对「没配」的反应是去填它——可它根本没有输入框 | 同上 |
| 220 | secret 只回一个布尔，**不回长度**：长度把爆破空间从「所有口令」缩到「12 位的口令」 | [23-web-ui §4.2](23-web-ui.md) |
| 221 | 字段按 `(group, advanced, name)` 排序。代价：Pack 作者写的顺序保不住（`params` 是 map）。不排更糟——map 遍历随机，表单每次轮询都重新洗牌 | `internal/render/form.go` |
| 222 | 表单的判据是「值**从哪来**」不是「值是多少」：优先级写反的实现在两层设了同一个值时每个值都对 | `internal/render/form_test.go` |
| 223 | 已知缺口：`Overrides.Role` 在 mechd 存储里从未被填过，模型比实现多一层。界面只承诺**影响面**，不承诺它落在哪张表 | [23-web-ui §6.6](23-web-ui.md) |
| 224 | 照着规范的「推荐写法」造夹具，会系统性避开规范允许的其它写法：`go-webapp` 省略角色名的缺陷单元测试全绿、真集群一跑就露 | 同上 |
| 225 | 改配置不走 `deploy --update`：Deploy 重算放置因而要求指定节点，一次「改日志级别」被迫重述整个拓扑，少写一个节点名就是缩容 | [23-web-ui §4.3](23-web-ui.md) |
| 226 | 同一步补上 `mechctl config get/set/explain`：只加 PATCH 给 UI 用，等于在 UI 里做出 CLI 做不到的事——那正是 ConfigGroup 被推迟的理由 | `internal/cli/ctlcmd/config.go` |
| 227 | `secret` 在浏览器里就是密码框：CLI 禁明文是因为 ps 输出与 shell 历史，这两条路径在 POST body 上都不存在。前提是请求体绝不进日志、传输必须 HTTPS | [23-web-ui §4.3](23-web-ui.md) |
| 228 | 保存前先 dryRun 再确认：一个只说「确定吗」的确认没有价值，要问的是「会发生什么」，而前端算不出这一次改动合起来会动几台机器 | 同上 |
| 229 | 只有会重启/reload 才二次确认：每次都拦会训练用户无脑点确定，而下一次是真的 | 同上 |
| 230 | `SetParams` 先算后写：先落库再校验会留下「库里非法、机器上是旧值」的中间态，之后每次调和都失败且原因指向一份没人审过的数据 | `internal/mechd/setparams.go` |
| 231 | 预览用 `previewSecrets`（读真的写假的）：真 Vault 会把用户没确认的口令落库，一次性随机密钥则让 From 报出一个集群上从不存在的 digest | `internal/mechd/previewsecrets.go` |
| 232 | **「digest 变了没有」不是判据，「它是不是那台机器上真正的那个」才是**：为 previewSecrets 写的前两版判据都是假的 | `internal/mechd/setparams_test.go` |
| 233 | 有未保存改动时停掉轮询；提交只送动过的参数。前者错了表现为「输入框自己清空」，后者错了会悄悄冲掉别人刚改的参数 | [23-web-ui §6.7](23-web-ui.md) |
| 234 | 已知缺口：前端只有 `vue-tsc` 把关，没有测试框架。三处「错了会毁掉用户输入」的逻辑目前只靠手动走一遍 | 同上 |
| 235 | 组成员用**显式节点名单**不用标签选择器：成员变更会触发重渲染与可能的重启，标签选择器让「打个标签」这个动作悄悄重启一批服务 | [23-web-ui §4.4.1](23-web-ui.md) |
| 236 | 成员重叠**在写入时拒绝**，不在读取时按优先级挑：读取时挑选让「这台机器用哪个组」取决于一条没人记得的规则，症状是「我改了 A 组那台机器没变」 | 同 §4.4.2 |
| 237 | 建组 / 改成员 / 删组都**不重算 ordinal**（ADR-0028）：组只影响取值不影响身份，而「顺手按组重排让它们连号」是个看起来很合理的改动 | 同 §4.4.3 |
| 238 | 删组是一次真实变更不是清理：成员回落会改配置文件、可能重启服务，因此与 `config set` 走同一套预览与确认 | 同 §4.4.4 |
| 239 | `config set --node` 自动建组**必须提示**：模型里没有无名 per-node 覆盖，静默创建会让 `group list` 冒出一堆没人记得来历的 node-* 组 | `internal/cli/ctlcmd/config.go` |
| 240 | `paths` 绑定的卷在**建组时**就校验：留到渲染时才报的话，症状是「组建好了，下一次调和整个组失败」，而那时用户已离开建组的上下文 | [23-web-ui §4.4.6](23-web-ui.md) |
| 241 | **「只改这一台」在路径上不成立**：topology 里带着同角色全部实例的路径（模板要 range 它），因此改一台的多盘绑定会让同角色其余实例重新物化。参数覆盖没有这个问题 | [23-web-ui §6.8](23-web-ui.md) |
| 242 | 前端引入 vitest，并把三条「错了会毁掉用户输入」的规则抽成 `useParamEdits`——**可测性驱动的重构** | `webui/src/lib/useParamEdits.ts` |
| 243 | SSE 推**完整状态快照**不推增量：漏一条就永远错的协议要配重放机制（Last-Event-ID + 服务端历史 + 客户端游标），而快照让「重连本身就是修复」 | [23-web-ui §4.5.1](23-web-ui.md) |
| 244 | bump 只是「催一下」，不带内容：带内容会让它变成不能丢的东西，于是要缓冲、重试、背压——把一条纪律换成一堆机制 | `internal/mechd/watchhub.go` |
| 245 | 重算有一个**与集群规模无关的上限**（1s）：Report 里每实例 bump 一次且 reportedAt 每次都变，50 实例就是每秒 3 次完整解析管线重算 | [23-web-ui §6.9](23-web-ui.md) |
| 246 | 流的节拍用真实时钟不用 `s.now()`：可替换时钟是给 rollout 门禁测试用的，混用会让冻住时钟的测试把节流变成每秒一次 | `internal/mechd/watchapi.go` |
| 247 | **刻意不设 `http.Server.WriteTimeout`**：它对整个响应计时，而 SSE 没有尽头。设上的症状是「页面开着开着就不更新了」，且恰好在整数秒之后 | `internal/cli/mechdcmd/serve.go` |
| 248 | SSE 认证只能靠 cookie（EventSource 不能设请求头）。把 CSRF 检查从「写操作」放宽成「所有请求」会让这条流当场断掉 | [23-web-ui §4.5.4](23-web-ui.md) |
| 249 | 轮询代码**不删**：实时性是一个优化不是依赖。SSE 被反向代理吃掉时页面仍然对，只是慢一点，界面上用最不显眼的样式标注 | `webui/src/lib/useLive.ts` |
| 250 | `onerror` 里**不 close()**：那等于扔掉浏览器自带的重连与退避，然后自己写一个更差的 | 同上 |
| 251 | 加入命令里的主机名由**浏览器**填不由服务端填：mechd 绑 0.0.0.0 不知道自己对外是什么地址，而 Host 头是客户端可以随便写的——把它拼进一条要在别的机器上执行的命令里，等于让请求方决定新节点去连谁 | [23-web-ui §4.6.1](23-web-ui.md) |
| 252 | UI 里**没有**「帮我装到那台机器上」的按钮：让 mechd 去 SSH 就要给它能登录所有节点的私钥，而 M7 的主要收益正是把长期暴露面从 SSH 常开降为零入站端口 | 同 §4.6.3 |
| 253 | 不提供「重新查看 token」：要么是假的，要么意味着明文落了库。列表只显示元数据，丢了就重新生成一张 | 同 §4.6.2 |
| 254 | 验收判据是「生成的命令真的能把节点加进来」，不是「页面渲染出来了」——一条粘过去不工作的命令看起来完全正常，而失败发生在另一台机器上 | [23-web-ui §6.10](23-web-ui.md) |
| 255 | `fillJoinHost` 有一条判据是「token 与 CA 指纹一个字节都不动」：它们是凭据与信任锚，任何规范化处理都可能毁掉它们，而症状指向别处 | `webui/src/lib/joinCommand.ts` |
| 256 | `.mpack` 此前**一行实现都没有**（规范定了、代码里只有 .gitignore 提过）。第 10 步补齐格式读写 + `mechpack bundle` + 上传，是三件事不是一件 | [23-web-ui §4.7](23-web-ui.md) |
| 257 | 压缩用 zstd（`klauspost/compress`，纯 Go 无 cgo）。`.mpack` 的存在理由是离线交付，为省一个依赖去换二进制载荷的压缩率，方向反了 | 同 §4.7.1 |
| 258 | 归档的四条可复现要求各挡一件事：排序与时间戳归零挡「摘要对不上」，无符号链接与 `..` 挡解包逃逸。后两条**解包侧也要再查**——打包侧的校验保证不了别人打的包 | `internal/pack/mpack.go` |
| 259 | 上传的校验在**临时目录**里做，通过了才原子改名进 Pack 集合：解到集合里再查，那一瞬间放置阶段就可能看见半个 Pack | `internal/mechd/upload.go` |
| 260 | **上传必须做「载荷入库」**（thick → thin）。少了它包能部署却永远不收敛，而 deploy 那步是成功的、哪里都不报错 | [23-web-ui §6.11](23-web-ui.md) |
| 261 | 载荷入库要**重算摘要核对**：文件名里的 sha256 是归档自己声称的，名不副实的载荷进库之后每次按摘要取都拿到错的东西 | `internal/mechd/upload.go` |
| 262 | 「文件不完整」与「不是这个格式」分开报且顺序要对——前者重传，后者换文件。两条读取路径（条目头 / 条目内容）都要包 | `internal/pack/mpack.go` |
| 263 | `isUserError` 加类型判断（显式 `faults.Permanent` 优先于关键词匹配）。只认显式打过标的：ClassOf 把未分类错误也算 Permanent，拿它当判据会掩盖真正的服务端故障 | `internal/mechd/httpapi.go` |
| 264 | 验收要一个**真包**：`hack/realpack.sh` 走 go build → assemble（真 sha256）→ bundle。examples 里那些的摘要是占位符，能过 lint 但装不了 | 同 §6.11 |
| 265 | 验收表做成**可重复的套件**（`test/webui`）而不是一次性脚本：一份贴出来的输出记录的是「那一次跑过」，不是「现在还成立」 | [23-web-ui §4.8](23-web-ui.md) |
| 266 | 判据分三类并各自说清楚：A 真集群验、B 只有浏览器能验（指明由谁覆盖）、C 构建期。**一条写着 ✓ 而实际没验的判据，比一条写着「靠 X 与 Y 推出来」的更糟** | 同上 |
| 267 | **滑块把三条判据挡在端到端之外**（2、3、6 都要有效会话或正确位移）。要解开只有真浏览器或服务端后门两条路，后者不做 | [23-web-ui §6.12.2](23-web-ui.md) |
| 268 | 第 6 条的第一版 e2e 判据是**假的**：无效 cookie 的 401 来自身份校验，没走到 CSRF——删掉那行检查它照样通过 | 同 §6.12.1 |
| 269 | 「不带头被拒」不足以验 CSRF，必须同时验「**带上头就放行**」——否则一个「所有写请求都拒」的实现同样通过 | `internal/mechd/authapi_test.go` |
| 270 | 验第 22 条必须 `DisableCompression`：Go 的 Transport 自动解压**并抹掉 Content-Encoding**，用默认客户端永远看不到那个头 | [23-web-ui §6.12.4](23-web-ui.md) |
| 271 | 限流那条测试必须放文件最后（名字带 ZZ）：它锁 IP 30 秒，而 Go 按源码顺序跑，那个顺序是隐式的 | `test/webui` |
| 272 | 收尾这一步**先查现有覆盖再写新的**：19–22 条在第 1 步就有测试，第一版重复写了一份 | [23-web-ui §6.12.3](23-web-ui.md) |
| 273 | secret 的 `--set` 拦截**只能在客户端做**：服务端分辨不出值是 `--set` 还是 `--set-file` 传来的，而风险（shell history、ps）本来就全在客户端 | `internal/cli/ctlcmd/secretinput.go` |
| 274 | 拿不到参数类型时**放行并警告**，不阻断：mechd 不可达 / Pack 名打错都会让类型查不到，把它们变成部署失败是把卫生规则升级成可用性问题 | 同上 |
| 275 | `--set-stdin` 在非 TTY 且 stdin 为空时**报错**，不静默用空值：脚本少接一个管道，空口令会让组件带着空密码起来，而那要很久以后才被发现 | 同上 |
| 276 | 验收套件的前提不成立时 **Fatal 不是 Skip**。「没有集群」（Skip）与「集群没装好」（Fatal）是两件事——后者静默跳过就是假绿 | `test/webui` |
| 277 | M7/M8 的验收套件进 CI，单列一个 job：它们是里程碑的判据本身，只在手动跑时执行等于没有 | `.github/workflows/ci.yml` |
| 278 | **在文档现场标注「尚未实现」**，而不只在 roadmap 里列一行：读 10-cli §4.3 的人不会先去翻 roadmap | `docs/design/10-cli.md` |
| 279 | 修掉一处文档自相矛盾：10-cli 曾说失联节点重新上线时 mechlet「自行清理」，与 20-continuous-reconcile §2.4 的「孤儿永不自动删」冲突。以后者为准——「自行清理」会让一次下发故障变成一次静默卸载 | 同上 |
| 280 | WSL 里有 gcc，因此 `-race` 可用；但仓库在 /mnt/d（9p），构建慢 30 倍。分工：Windows 迭代、WSL 跑 -race 与平台敏感测试。前端 `node_modules` 平台锁定，不能两边共用 | 本次审查 |
| 281 | 卸载是 `runState` 的第三个值 `removed`，**不是新 RPC**：指令是事件，丢一次就永远丢了；状态可以重复确认，断连三天的节点回来仍然收到「这个实例不该存在」 | [24-lifecycle §2.1](24-lifecycle-completion.md) |
| 282 | 叫 `removed` 而非 Ansible 那一系的 `absent`：已有的两个值是 `running` / `stopped`，同一种构词。代价是它有点过去时的味道，靠字段名（**期望**运行态）与注释消解 | 同上 |
| 283 | **未知 runState 一律按 running 处理**：旧 mechlet 收到不认识的值时宁可让服务继续跑（人看得见），也不能猜成 removed 把它卸掉——那不可逆 | `internal/spec/spec.go` |
| 284 | 「哪个路径算数据」**写死在引擎里，Pack 不能覆盖**：未归类的（含 HDFS 的 `dataDirs` 这类自定义名）一律**保留**。删错不可逆，留错可以 purge。代价：Pack 无法声明「这个自定义目录是可丢的」，真需要时再加字段 | `internal/reconcile/remove.go` |
| 285 | `Removal` 的三个开关都是「偏离默认」的形式，零值即安全默认（配置删、数据留、用户留）——一份漏传它的规格不会多删任何东西 | `internal/spec/spec.go` |
| 286 | `runtime.Runtime` 加纯函数 `RefFor`：`Remove` 需要 `Ref`，而此前只有 `Materialize` 产得出来——走它等于拆之前先装一遍（写 unit、`docker load` 几百 MB），且中途失败会把「删除」变成「装了一半」 | `internal/runtime/runtime.go` |
| 287 | 卸载读**固化路径**而非规格路径，且绕开 `CheckPaths`：那条校验是防「静默搬家」的，让它挡在卸载前，一个路径漂移过的实例就再也删不掉了 | `internal/reconcile/remove.go` |
| 288 | 留了东西才写卸载收据，什么都没留就把状态文件整个删掉：留着它，`orphans list` 会永远挂一条指向空地址的记录 | `internal/state/state.go` |
| 289 | `--purge-user` 失败**只警告不失败**：userdel 在用户还有进程或还是别处文件属主时会拒绝，那是常态；为一个可选清理赌上整条删除路径不划算。但警告必须回到敲命令的人面前，不能只写节点日志 | `internal/reconcile/remove.go` |
| 290 | 卸载顺序补上 `postStop`（10-cli 原来的顺序没有它）：别处的「停」一律是三件套，少一个就会出现「升级时跑、卸载时不跑」这类只在特定路径上存在的差异 | [10-cli §4.3](10-cli.md) |
| 291 | `postRemove` 的 cwd 必须为空：generation 目录刚被删掉，指着它时 fork/exec 报的是「脚本不存在」——指向脚本，而脚本明明在 Pack 根下 | `internal/reconcile/remove.go` |
| 292 | 卸载时**不存在的目录一条都不记**（保留与删除两侧都是）：一个部署失败之后才收到 removed 的实例，会把一堆从没被建出来的目录登记成孤儿，而运维会真的跑去那台机器上找它 | `internal/reconcile/remove.go` |
| 293 | 协议一个字节没改：ResolvedSpec 在 gRPC 里是 opaque 的 `spec_json`，加字段天然向后兼容。代价是兼容得很「静默」——旧 mechlet 收到卸载意图什么也不做也不报错，组件卡在 removing，因此排查清单上要有「mechlet 版本太老」这一条 | [17-protocol](17-protocol.md) |
| 294 | `removing` 期间再敲 `remove` **只能加 `--force`，不能改开关**：三个开关逐节点生效，改到一半会得到「一半节点删了数据、一半留着」的集群，而那种不一致事后几乎排查不出来。代价是「忘了加 --purge-data」没有捷径，只能等删完再 orphans purge | [24-lifecycle §2.2](24-lifecycle-completion.md) |
| 295 | `--force` 不需要单独的孤儿登记逻辑：记录一删，实例就不在下发里了，节点侧现成的 `refreshOrphans` 会自己把残留报上来 | 同上 |
| 296 | 卸载完成靠一个**独立信号**（`InstanceStatus.removed`），不复用 digest：RunState 不参与 digest，拆完的实例与装着的实例上报的 digest 一模一样，Rollout 那套收敛判据在这条路上完全失效 | `proto/.../agent.proto` |
| 297 | 「没上报过」**不等于**「拆完了」：一台从没连上来的机器与一台报告拆完的机器，库里的区别只有那条状态记录。当成完成会让失联节点上的服务被静默遗忘——记录删了，机器上还跑着 | `internal/mechd/removal.go` |
| 298 | 写闸门加在**「取记录」这个动作**上，不是十一个写动词各写一遍 if：漏掉的那一处不会有任何症状，直到有人真的用它。读路径走另一个函数——读一个正在被删的组件完全正当 | 同上 |
| 299 | 组件级 `removing` **盖过**逐实例 runState：一个先前被 stop 的实例同样要拆掉，否则它永远停在 stopped，整个组件因此永远卡在 removing | `internal/mechd/backend.go` |
| 300 | **sqlc 1.31 会吃掉 `SET x = '字面量'` 的引号**，变成列引用，且退出码为 0，症状要到运行期才现形。状态一律走绑定参数。与「queries 只能是 ASCII」同源：这个生成器在这个文件上会静默出错 | `internal/store/queries/expected.sql` |
| 301 | `make proto` 从第一个 commit 起就跑不起来（go.mod 没有 `tool` 段），CI 的 `make proto-check` 一直是失败的。已补上，版本对齐已提交的生成物（protoc-gen-go-grpc v1.6.1） | `go.mod` |
| 302 | **`-y` 不能跳过输名字这一档**（10-cli §7 明写）。两者挡的不是同一种错误：`-y` 挡「手滑敲了回车」，输名字挡「删错了对象」。原实现是 `if yes { return nil }`，与 §7 直接冲突，M9 第 3 步发现并修掉；脚本改用 `echo <name> \| …` | `internal/cli/ctlcmd/confirm.go` |
| 303 | `--purge-data` 要**第三档**确认（§7 的表：组件名 ＋ 单独确认删数据）：删进程与配置可以重新部署回来，删数据不能，两者不该由同一个动作买断 | `internal/cli/ctlcmd/remove.go` |
| 304 | 二档确认**服务端也验一遍**（`confirm` 必须等于组件名）：CLI 那道提示挡得住手滑，挡不住一条直接打过来的 HTTP 请求，而 UI 与脚本走的都是这条路 | `internal/mechd/httpapi.go` |
| 305 | remove 用 **HTTP DELETE**，不是 POST …/remove：它是整个 API 里唯一真正销毁东西的动词，用 HTTP 自己的销毁方法能让「这条请求会删东西」在网关日志与审计里一眼可见 | 同上 |
| 306 | 影响面的目录清单**由渲染管线算出来**，用与节点侧同一张归类表（`spec.DispositionOf`）：两处各写一份的话，预览迟早会与真正发生的事不一致——而那正是二档确认唯一的价值 | `internal/mechd/removecomponent.go` |
| 307 | 归类**只认精确的名字**：`conf` / `etc` / `configs` 都不算 `config`（mechd 的 paramkit 夹具正是叫 `conf`，因此它的配置目录被当成数据保留）。模糊匹配没有采纳——归类会变得不可预测，而这里唯一不能接受的是猜错方向去删东西 | `internal/spec/removal.go` |
| 308 | 没有任何实例的 Component **当场删掉**，不进 removing：它永远等不到上报，进了就永久卡住——既不能改（写闸门），也不会消失 | `internal/mechd/removecomponent.go` |
| 309 | CLI **总是先干跑一次**算影响面，哪怕给了 -y：这条命令的后果无法从命令行本身看出来（几台机器？哪些目录？），而没看清后果的删除是这条路上最贵的错误 | `internal/cli/ctlcmd/remove.go` |
| 310 | `node remove` 的确认按 §7 重排：**`--force` 才要求输名字**（它会连着实例一起抹掉），不带 force 时 y/N。原实现一视同仁地要名字，比表更严——而更严的那一版在 -y 失效之后会让脚本里的 `node remove -y` 静默失败 | `internal/cli/ctlcmd/node.go` |
| 311 | **夹具里被「忽略」吞掉的失败最难发现**：多节点夹具的清理步骤 `node remove -y` 在 confirmName 改动后失效，错误被一句「（忽略）」吃掉，测试照样绿，只是前提悄悄不成立了。真机验收的输出里才看见 | `test/multinode/join_linux_test.go` |
| 312 | 一条命令里的多档确认**必须共用同一个 bufio.Reader**：它会预读，第一档会把后面几行一起吞进自己的缓冲区，第二档拿到一个空 stdin。症状是「明明喂了两行，第二问却说读不到回答」——`remove --purge-data` 在脚本里因此**永远走不完** | `internal/cli/ctlcmd/confirm.go` |
| 313 | CLI 流程的测试不能靠「指向一个连不上的地址」：remove 会先干跑一次算影响面，请求在确认之前就失败了，测试看着绿却一次都没走到确认。改用 httptest 桩，判据是**桩有没有收到真删的请求**，不是命令有没有报错——一个先删后问的实现同样会报错，但东西已经没了 | `internal/cli/ctlcmd/removeflow_test.go` |
| 314 | `orphans purge` 做成**状态**而不是命令型 RPC：`Assignment.purge_orphans` 每轮重复下发，节点每次照做，清完之后孤儿从上报消失、意图自动失效——不需要确认序号，也不需要节点在线 | `proto/.../agent.proto` |
| 315 | purge 下发的是**实例键不是绝对路径**。时序：purge 下发时节点失联 → 运维同名重新部署 → 节点回来收到 purge。带路径会删掉**新数据**；带键则空跑（那时本地是真实实例而非收据） | `internal/agent/purge.go` |
| 316 | 目录删失败时**收据必须留着**：它是这个孤儿存在的唯一证据，删早了剩下的目录就再也不会出现在 `orphans list` 里——变成一个谁也不知道的残留 | 同上 |
| 317 | 孤儿分两类且列表上要分得开：有路径=数据残留（purge 可解），无路径=**还装着、可能还在跑**（purge 只删目录，停不掉它）。服务端直接拒绝后者 | `internal/mechd/orphans.go` |
| 318 | 不算孤儿目录大小（§2.4 原本承诺了）：算它要 walk 几 TB、几百万文件，而孤儿会在那儿放几个月，上报 15 秒一次。代价是「这堆数据值不值得留」要人自己登机 du——明确的取舍，不是遗漏 | 同上 |
| 319 | 「本轮没报到的删掉」用 `<>` 不用 `<`：`<` 假设时钟严格前进，而 NTP 回拨会让未来时间戳的行永远删不掉。`<>` 表达的正是「不是本轮写的」 | `internal/store/queries/observed.sql` |
| 320 | 造「删不掉的目录」用带 NUL 的路径，不用「去掉父目录写权限」：后者对 root 无效，而本项目测试常以 root 跑（容器、WSL）——那条测试会被静默 skip，而 skip 掉的测试看起来和通过一模一样 | `internal/agent/purge_test.go` |
| 321 | 删掉 Component 记录之后**必须唤醒那些节点**：不唤醒时没有任何东西报错，症状是沉默的——节点还攥着「runState: removed」的期望，每 60 秒重新卸载一遍一个早就拆干净的实例，而 `orphans list` 永远是空的，保留的数据因此永远无人认领 | `internal/mechd/removal.go` |
| 322 | 上一条是**三台真机的验收发现的，不是单元测试**。当时那一层全绿：它们验的是「记录没了」，没有一条问过「节点知不知道」。补了两条唤醒测试，同类遗漏下次不必再靠真机发现 | `internal/mechd/removal_test.go` |
| 323 | ad-hoc 命令走**独立的 `Tasks` 流**，不塞进 `Assignment`（ADR-0038）。分界是「丢一次会怎样」：期望状态丢了下一轮重新确认，命令丢了就是没执行、必须告诉人、断连回来**不该补做** | [ADR-0038](../adr/0038-adhoc-task-channel.md) |
| 324 | 命令**不落库、不重试、不排队**：离线就是离线，如实报告。一个「等它上线再执行」的队列会让人以为命令一定会生效，而那是期望状态的语义 | `internal/protocol/tasks.go` |
| 325 | `unreachable` 与 `failed` 必须分开：前者是「那台没连着，命令根本没发出去」，后者是「发出去了但没成功」——运维一个去查网络，一个去看日志 | 同上 |
| 326 | 命令结果**按入参下标对齐，不按节点名**：一台机器上可以有同一组件的两个角色，按节点名会让后一条覆盖前一条，而少掉的那行看起来只是「没返回」 | 同上 |
| 327 | 节点侧也要判命令过期：一条已超时的命令中心早已报成失败并返回给用户，此时再动机器是一次**没人预期的重启** | `internal/protocol/taskclient.go` |
| 328 | 任务类型常量放 `protocol` 而非 `agent`：两端共用，而让控制面 import 节点侧的包是反的，那条依赖迟早带来一个真正的环 | 同上 |
| 329 | 测试要用**夹具自己的手法**操纵夹具起的东西。restart 验收里我用 `systemctl stop` 停一个由 `startAgent` 起的裸进程：unit 显示 inactive、中心仍 online、命令真执行了、journal 却 0 次——四个现象拼起来像「中心凭空报成功」，而每一环都没错 | `test/multinode/restart_linux_test.go` |
| 330 | **多节点全量套件在长时间连续运行的机器上会出现负载敏感的失败**（rollout 与 resume 卡在 systemd `starting`，而三台都报「已收敛、健康」）。判据：单独跑 fresh 集群通过，且失败集合在两次全量之间会变。**没有当成已解决**——它是一个真实存在的脆弱性，只是不属于任何一次代码改动 | `test/multinode` |
| 331 | 「文档有代码无」做成**自动化守卫**而不是又一次人工核对：10-cli 里每条命令要么存在、要么标着未实现；反向也查（已实现的不许还标未实现）。M8 那次核对是人工的，而人工核对只做一次，下一次漂移会在没人看时发生 | `internal/cli/ctlcmd/docdrift_test.go` |
| 332 | 那条守卫的**第一版自己是坏的**：`real[key] \|\| real[noun]` 让任何已存在名词下的任意动词都算通过，只抓得到整个名词缺失——往文档加一条假命令一测便知。修好后立刻又冒出两条真实缺口——**通过 ≠ 有效**的第三次应验 | 同上 |
| 333 | 未实现的命令**标注而不是删除**：删掉会让设计连同理由一起消失，而 M8 审查的结论恰恰是「读文档的人会以为它们能用」——要修的是说实话，不是让文档变短 | [10-cli §4](10-cli.md) |

### M9 里程碑审查

| # | 决策 | 出处 |
|---|---|---|
| 334 | **验收表全绿不等于代码被测过。** M9 收尾时 15 条判据条条有覆盖，但 `Service.Restart`、`agent.RunTask`、`applyGroup`、`splitSecretKey` 四处的单元覆盖率是 **0.0%**——它们只被真机套件走过，而真机套件不产出覆盖数据，于是「验过了」与「有测试」被悄悄划了等号 | 本次审查 |
| 335 | 补的测试要挑**沉默的分支**，不是好走的分支。restart 的「已停止但启动失败」会让服务停着而人以为它在跑；apply 的 secret 路由错了会把 A 的口令写进 B——两处都不报错。四组变异（含「口令发给所有组件」）全部变红 | `internal/agent/tasks_test.go`、`internal/mechd/apply_test.go` |
| 336 | **一条在 Windows 上「绿」的测试可能绿错了。** `purge.go` 的 `isAbs` 只认 `/` 开头（刻意的），于是 `TestPurgeSkipsWhenTheInstanceWasRedeployed` 在 Windows 上验的其实是「什么都删不动」——一个照删不误的实现同样能过。改文件名加平台约束，不去放宽产品代码 | `internal/agent/purge_linux_test.go` |
| 337 | 同一个「是不是绝对路径」在两处**故意不同**，不是笔误：purge 问的是「绝对到可以 RemoveAll 吗」（安全判据，POSIX 硬编码），disposePaths 问的是「这是物化路径还是没展开的 generation 占位符」（用 `filepath.IsAbs`，因而 Windows 上也验得真）。就近写清楚，防下一个人「顺手统一」 | `internal/agent/purge.go` |
| 338 | 开发机上 `go test ./...` 的覆盖率会**系统性偏低**：reconcile 在 Windows 是 13.1%，在 Linux 是 79.1%。拿 Windows 的数字判断「哪里没测」会指向完全错误的地方 | 本次审查 |
| 339 | **一条偶尔变红的测试比没有测试更贵**。审查中我自己刚写的 `res.Duration == 0` 在整包跑时随机失败（单跑必过），守的东西又只是「字段填上了」——删掉。留着它，最后换来的是所有人都不看红色 | `internal/agent/tasks_test.go` |
| 340 | CI 步骤名是排障时第一眼看的东西，指错地方比没名字更费时间：「M7 多节点验收」在 M9 把新测试加进同一个包之后成了谎话，面板上看是绿的 M7，实际跑的东西多了一倍 | `.github/workflows/ci.yml` |
| 341 | 决策号 **207 被用了两次**（nodes.status 与登录锁定时长），而后者还漂到了 M9 段的末尾——`同上` 于是指向 11-cli，与内容毫无关系。号不复用：重发为 342，并把出处写成具体文件而不是 `同上`（`同上` 一旦被移动就会指错，这次正是这么坏的） | 本条 |
| 342 | 锁定时长要有**上限**：这个系统没有自助找回，无上限的指数退避会让一次误操作把人永久关在门外（原编号 207，重号，见 341） | `internal/authn/ratelimit.go` |

---

## 待实现阶段确定的议题

> 结论以 [25-roadmap.md](25-roadmap.md#已知需在实现阶段确定的议题) 为准，
> 此处只保留索引，避免两处漂移。

| 议题 | 何时必须定 |
|---|---|
| ~~CLI 动词表定稿与输出格式（`-o json,yaml`）~~ | ✅ M0，见 [10-cli.md](10-cli.md)；结构改为名词优先，见 [ADR-0025](../adr/0025-noun-first-cli.md) |
| ~~最低 Go 版本~~ | ✅ M0：Go 1.25 |
| ~~Linux 发行版矩阵 / glibc 下限~~ | ✅ M2：**无 libc 下限**（`CGO_ENABLED=0` 纯静态），实际下限是 systemd ≥ 239 |
| ~~结构化日志库选型~~ | ✅ M0：标准库 `log/slog` |
| 是否暴露 Prometheus metrics | M5 |
| ~~健康检查类型集；「健康」与「就绪」是否区分~~ | ✅ M2：`http` / `tcp` / `exec`，**不区分健康与就绪** |
| ~~mechd ↔ mechlet 协议细节~~ | ✅ M3 设计已定，见 [17-protocol](17-protocol.md) |
| `volumeClass` **自动挑盘**（`class` 声明字段 v1 已有） | 待真实异构大集群用户，见 [pack-v1 §22](../spec/pack-v1.md#22-未决问题) |
| 增量 blob 传输 | 随时可加，**延后成本为零**（纯传输层优化，格式无关） |
| 控制面 HA | 见 [ADR-0014 重新评估条件](../adr/0014-no-ha-in-v1.md#重新评估的条件) |
| Kubernetes runtime | 见 [ADR-0017](../adr/0017-k8s-extension-reserve.md) |
