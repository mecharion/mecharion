# 路线图

## 设计意图

里程碑的切分不是按功能重要性排序，而是按**风险与不可逆性**排序：

- **M1–M6 全程只需要一台机器**——没有网络、没有分布式、没有一致性问题，但产品最难且最不可逆的部分（Pack 格式、资源引擎、路径调和、升级回滚）全部完成。这正是[单机形态同机运行 mechd](../adr/0026-standalone-runs-mechd.md) 带来的好处：**单机可验证的范围一直延伸到完整的用户流程**
- **M7 之后加入的是协调**——那是相对标准的工程问题
- **M4（docker）刻意早于 M5/M6**——见下方说明

## 里程碑

| 阶段 | 内容 | 完成判据 |
|---|---|---|
| **M0** ✅ | 仓库骨架、四个二进制、CI（linux amd64/arm64 + darwin + windows）、`.gitignore`、README 首屏、[CLI 动词表](10-cli.md) | `mechlet version` 可跑，CI 全绿 |
| **M1** ✅ | **pack/v1 格式定型** + 解析校验 + `mechpack init` / `assemble` / `lint` / `inspect` | `mechpack lint --hermetic --strict examples/packs` 全部通过 ✅（示例数量随规范验证需要增减，此处不写死具体个数） |
| **M2** ✅ | 资源引擎 + **systemd runtime** + generation 物化 + paths / linkInto 解析 + 健康检查 | `mechlet apply -f <已解析规格>` 在 systemd 容器里把 go-webapp 跑起来 ✅ |
| **M3** ✅ | **mechd 最小实现** + 单机完整链路：SQLite Store、gRPC(unix socket)、放置/参数/拓扑解析、HTTP API、`mechlet install --standalone` | `mechctl component deploy go-webapp` 端到端跑通 ✅ |
| **M4** ✅ | **docker + compose runtime**（Probe / capability 上报 / 标签隔离 / bind mount） | 跑通一个容器化 webapp，与 M2 共用全部上层逻辑 ✅ |
| **M5** ✅ | 持续调和 + 漂移检测 + `status` / `diff` + driftPolicy | 手改配置，一个调和周期内被检出并按策略处理（**三个 runtime 一起过**）✅ |
| **M6** ✅ | generation 并存 + 升级 + 自动回滚 + Rollout（单机） | 升级失败时自动切回，服务不丢（**三个 runtime 一起过**）✅ |
| **M7** ✅ | 多节点：`node bootstrap`、mTLS、多节点 Rollout（分批 / 健康门禁 / 暂停） | 3 节点滚动升级，一个失败则整体暂停 ✅ |
| **M8** ✅ | Web UI（由 params schema 自动生成表单）+ 最简用户体系 | 不写 YAML 的人能从浏览器部署与改配置 ✅ |
| **M9** ✅ | **补齐生命周期的另一半**：`component remove`、`orphans`、`apply -f`、`restart` 与 ad-hoc 通道 | 部署出来的东西删得掉；文档里写的命令都真的存在（有守卫钉着）✅ 收尾审查见 [24-lifecycle §5](24-lifecycle-completion.md) |
| **M10** | 打磨、文档、v0.1.0 公开发布 | |

### M2 为什么不需要 mechd

[ADR-0026](../adr/0026-standalone-runs-mechd.md) 确定单机也要跑 mechd，看起来 M2 的验收就得先有 mechd。但不是——**M2 只做资源引擎**，用 `mechlet apply -f <已解析规格>` 这个调试入口驱动。

它读的规格与 mechd 下发的是**同一个结构**，走的是**同一个 reconciler**。这不是分叉，是同一条路径的另一种输入来源。M3 把输入换成 mechd 的 gRPC 下发，reconciler 一行不改。

### M2 交付了什么

| 包 | 职责 |
|---|---|
| `internal/spec` | `ResolvedSpec`：mechd 与 mechlet 的契约，含 digest 与唯一的 late-bound 占位符 |
| `internal/state` | mechlet 本地状态：路径固化、generation 台账、原子写 |
| `internal/resource` | 资源引擎，7 / 16 种类型（`directory` `file` `template` `symlink` `archive` `user` `group`） |
| `internal/runtime` + `runtime/systemd` | Runtime 抽象与 systemd 实现 |
| `internal/health` | `http` / `tcp` / `exec` 三种探针 |
| `internal/reconcile` | 调和器：七阶段编排、generation 分配与原子切换、notify 聚合、回收 |
| `internal/faults` / `internal/command` | 失败分类与外部命令执行，资源引擎与 Runtime 共用 |
| `test/webapp` + `test/e2e` | 验收夹具与端到端测试 |

**尚未实现、已明确推迟的：**

| 项 | 推迟到 | 现在的行为 |
|---|---|---|
| 其余 9 种资源类型（`sysctl` `limits` `hosts_entry` `mount` `timer` `systemd_unit` `command` `script` `package`） | 尚无排期 | 构造时报「尚未实现」，与「类型名拼错」区分开 |
| ~~hooks 执行~~ | ✅ M3 第 7 步（与 secret 注入通道一起做） | 十个生命周期点 + `notify` 指向 hook 名 |
| ~~持续调和循环~~ | ✅ M5 第 1 步 | 期望状态落盘 + 周期调和；重启与断连时同样有效（ADR-0033） |
| ~~健康失败自动回滚~~ | ✅ M6 | 健康检查失败即报调和失败并触发自动回滚 |

> 「明确报错而非静默跳过」是刻意的：静默会让 Pack 作者以为功能生效了，
> 而问题要到生产上才暴露。

### M3 的设计与实施顺序

设计文档：[13-mechd](13-mechd.md) · [14-placement](14-placement.md) ·
[15-render-pipeline](15-render-pipeline.md) · [16-secrets](16-secrets.md) ·
[17-protocol](17-protocol.md) · [18-hooks](18-hooks.md)

新增 ADR：[0028 ordinal 固化](../adr/0028-stable-ordinals.md) ·
[0029 协议形态](../adr/0029-push-over-server-stream.md) ·
[0030 密钥存储与下发](../adr/0030-secret-storage-and-delivery.md)

实施按**依赖顺序**，每一步都能独立测：

| # | 内容 | 可验证的成果 | 状态 |
|---|---|---|:--:|
| 1 | SQLite Store：goose 迁移、sqlc、Repo 接口 | 建库、CRUD、迁移可回滚 | ✅ |
| 2 | SecretVault：生成、信封加密、轮换 | 主密钥丢失时拒绝启动；密文换机器解不开 | ✅ |
| 3 | PackIndex：本地 Pack 集合的解析与索引 | 补上 lint 的 R43（跨 Pack 导出名校验） | ✅ |
| 4 | 放置：ordinal 分配 + 四类校验 | 扩容不改已有 ordinal；约束冲突的错误信息指名到实例 | ✅ |
| 5 | 解析管线：参数链 → 绑定 → 渲染 → 封装 | `mechctl component render` 离线产出 ResolvedSpec | ✅ |
| 6 | gRPC：Register / Subscribe / Report / FetchBlob | mechlet 连上、拿到规格、上报 digest | ✅ |
| 7 | hooks 执行 + 密钥注入 | postgresql 的 `bootstrap-roles.sh` 真跑通 | ✅ |
| 8 | HTTP API + `mechctl` 接线 | `component deploy/status/diff/ack-drift` | ✅ |
| 9 | `mechlet install --standalone` + `mechlet agent` | 一条命令装出可用的单机 | ✅ |

> **M3 的验收在容器里跑通了**（`test/e2e/standalone_linux_test.go`）：
> 起真的 mechd、跑真的 `mechlet agent`、用真的 `mechctl` 发命令，
> 最后确认 systemd 里有一个在跑的服务、且 mechd 认为它已收敛。
> 中间任何一环打桩，这条验收就换了个更容易通过的题目。

**第 5 步是分水岭**：它跑通之后，M2 的 `mechlet apply -f` 与 mechd 下发读的是
同一个结构、走同一个 reconciler——[验收](#里程碑)的最后一公里只剩传输层。

第 3 步顺带补掉 M1 遗留的 ⏳ 规则：R43 需要跨 Pack 索引，R39 需要 profile 守卫
与依赖声明的关联（[spec §19](../spec/pack-v1.md#19-校验规则汇总)）。

### M4 的设计与实施顺序

设计文档：[19-container-runtime](19-container-runtime.md)

新增 ADR：[0031 用 CLI 而非 Docker SDK](../adr/0031-docker-cli-not-sdk.md) ·
[0032 `ExecIn` 进 Runtime 接口](../adr/0032-runtime-exec-seam.md)

| # | 内容 | 可验证的成果 | 状态 |
|---|---|---|:--:|
| 1 | `ExecIn` 接缝：接口 + systemd 实现 + exec 探针改造 | systemd 上行为**不变**，测试全绿 | ✅ |
| 2 | `test/node-docker` 镜像与 testenv 支持 | 容器里 `docker version` 可用 | ✅ |
| 3 | docker runtime：Probe / Materialize / Start / Stop / Observe | 单容器 webapp 跑起来 | ✅ |
| 4 | 标签纪律 + **负向测试** | 同名无标签容器一律不被碰 | ✅ |
| 5 | `ExecIn` 的 docker 实现 + exec 探针在容器上跑通 | `pg_isready` 那类探针可用 | ✅ |
| 6 | compose runtime | 一个双服务 project 跑起来 | ✅ |
| 7 | `docker` 官方 Pack（用 systemd runtime 装 docker） | 离线装出 dockerd | ✅ |
| 8 | lint 规则 R52（docker mounts 不得引用 `.Paths.Current`） | 负向用例被拒 | ✅ |

**第 1 步先行是刻意的**：接缝的修正要在有第二个实现**之前**完成，否则会写出
一个「为了让 docker 能用」而临时加的方法，而不是一个想清楚的原语。

**第 7 步排在最后**：它需要真实的 Docker 静态二进制（几百 MB），而前六步用
发行版自带的 dockerd 就能测——验证链路不必等发布物料。

### M5 的设计与实施顺序

详见 [20-continuous-reconcile](20-continuous-reconcile.md)。

新增 ADR：[0033 mechlet 持有本地期望状态与密钥副本](../adr/0033-mechlet-local-desired-state.md)

| # | 内容 | 可验证的成果 | 状态 |
|---|---|---|:--:|
| 1 | 期望状态落盘 + 周期调和循环 | 不推送也会调和，重启后仍然调和 | ✅ |
| 2 | 期望运行态 + `component stop` / `start` | 停了就不会被拉起来 | ✅ |
| 3 | workload 漂移检测与恢复 | 手停/手删，一周期内恢复（**三个 runtime**） | ✅ |
| 4 | `driftPolicy` 的 Component 级覆盖 | 收紧被拒，放松生效 | ✅ |
| 5 | 孤儿实例进 status | 手工造一个孤儿，看得到且**没被删** | ✅ |
| 6 | `status` / `diff` 的漂移视图补齐 | 三种 driftPolicy 在 CLI 上看得出来；workloadAction 上到中心 | ✅ |
| 7 | 三个 Runtime 的漂移 e2e | 验收表整张过 | ✅ |
| 8 | 抑制到期恢复 + 审计 | ack-drift 到点重新告警 | ✅ |

**第 1 步先行**：其余七步全都要靠它才**观察得到**——没有周期触发，任何漂移
都得手工再 apply 一次才现形，那测的就不是漂移检测。

### M6 的设计与实施顺序

详见 [21-upgrade-and-rollback](21-upgrade-and-rollback.md)。

| # | 内容 | 可验证的成果 | 状态 |
|---|---|---|:--:|
| 1 | 台账顺序修正：健康检查先于标记 active | 健康失败的 generation 不会被当成 active | ✅ |
| 2 | `<key>.applied.json` + 密钥保留 | 节点上有一份可用于回滚的旧规格 | ✅ |
| 3 | 自动回滚：切回 + 起旧版 + 上报 | 新版起不来时**服务不丢**（三个 runtime） | ✅ |
| 4 | 失败 digest 锁：不反复重试 | 回滚之后稳定停在旧版 | ✅（与第 1 步同批，见下） |
| 5 | `component upgrade` / `rollback` | 端到端升级与手工回滚 | ✅ |
| 6 | Rollout 状态机 + `mechctl rollout` | 升级过程可见、可暂停、可中止 | ✅ |
| 7 | generation 与镜像/blob 回收 | 保留中的版本引用的镜像不被删 | ✅ |
| 8 | 三个 Runtime 的升级/回滚 e2e | 验收表整张过 | ✅ |

**第 1 步先行**：它是个**已经存在的错误**——健康检查跑在「标记 active」
之后，于是一个健康没过的 generation 已经被记成当前版本，下一轮直接判定
复用、不再重试。后面每一步都建立在「台账可信」之上。

**第 4 步与第 1 步一起做**，不是提前实现：`FindGeneration` 不看状态，
一个失败的 generation 会被当成回滚目标。只做第 1 步会让失败版本**每轮
重试一次**，比修之前更糟——两者是同一条性质（台账可信 + 不反复横跳）
的两半。

### M7 的设计与实施顺序

详见 [22-multi-node](22-multi-node.md)。

新增 ADR：[0034 节点加入与身份](../adr/0034-node-join-and-identity.md) ·
[0035 不做 maxSurge](../adr/0035-no-maxsurge.md)

**M7 的起点是「第二台机器现在真的加不进来」**：全仓库只有
`mechlet install --standalone` 会写 `nodes` 表，`mechctl node add` 不存在，
mechd 也只监听 unix socket。

| # | 内容 | 可验证的成果 | 状态 |
|---|---|---|:--:|
| 1 | 3 节点测试环境（testenv 多容器 + 网络） | 三台容器互通，mechd 在其中一台 | ✅ |
| 2 | gRPC TCP + mTLS 监听；证书 CN 作为身份 | 带证书连得上，不带连不上 | ✅ |
| 3 | join token（建/列/吊销）+ join 接口 + 签发 | 一台新机器用 token 加进来 | ✅ |
| 4 | `mechlet install --join` + `mechctl node bootstrap ssh://` | 两条加入路径都通 | ✅ |
| 5 | 证书轮换 + 吊销（`node remove` / `node revoke`） | <30 天自动换；吊销后 RPC 全拒 | ✅ |
| 6 | `node cordon` / `uncordon` | cordon 的节点不被调和 | ✅ |
| 7 | 批次划分与落盘（阶段/批次/目标）+ **按批次门禁下发** | `rollout status` 说得出「第 2/4 批」，且那时机器上真的只升了 2 批 | ✅ |
| 8 | 健康门禁 + `stableFor` + 批次超时 | 崩溃循环的机器不会批准下一批 | ✅ |
| 9 | 失败即暂停 + `resume` 断点续做 | 一个失败则整体暂停，已完成批次不动 | ✅ |
| 10 | 三节点滚动升级 e2e | 验收表整张过（§5.1 逐行对照） | ✅ |

**第 1 步先行**：没有三台机器，后面九步全都只能靠单元测试想象——而这个
里程碑的每一条判据都是「多台机器一起」才成立的。与 M5 把「周期调和」
排第一是同一条理由：**先让要观察的现象能发生**。

**第 2 步在第 3 步之前**：先让传输与身份成立，再谈怎么发身份。反过来会先
写出一个「用 token 换一张没人验的证书」的中间状态。

**`cordon` 纳入 M7**（第 6 步），虽然本节标题原文没列它：「批次里遇到一台
被 cordon 的机器怎么办」是分批 Rollout 必须回答的输入。`drain` 不纳入。

### 为什么 M4（docker）必须早于 M5 / M6

**在漂移检测与升级编排写出来之前，就让第二个 Runtime 存在。**

这样那两块逻辑天生就是 Runtime 无关的，而不是先写死在 systemd 上、再回头重构。这是把 docker 提前到 v1 的最大收益，比「早点支持容器」本身重要得多。

Runtime 抽象的接缝是否划对，只有第二个实现才能验证（见 [ADR-0010](../adr/0010-runtime-abstraction.md)）。等到 v1.0 之后再补，retrofit 成本会高一个量级。

## M9：补齐生命周期的另一半

M8 收尾时做了一次里程碑级审查，逐条核对「文档承诺的」与「代码里有的」。
这一节是那次审查的输出——**它们都在设计文档里写着，只是没有实现**。

**M9 就是把它们做完。** 完整设计见 [24-lifecycle-completion](24-lifecycle-completion.md)。

留着这一节而不是直接改文档删掉那些描述，是因为那些设计本身没有错，
错的是「读文档的人会以为它们已经能用」。

### 必须补（缺了就不完整）

| | 现状 | 为什么必须 |
|---|---|---|
| **`component remove`** | CLI 与 API **都没有** | 一个生命周期管理工具删不掉自己部署的东西。10-cli §4.3 有完整的参数与执行阶段设计 |
| **`mechctl apply -f`** | 不存在 | 10-cli 两处称它是「声明式主干的入口」。目前只有命令式的 deploy |

`component remove` 的落地比看上去大：节点侧只有 `Runtime.Remove`（停 unit
或容器），**没有实例级的卸载编排**，也没有对应的下发消息。而
[20-continuous-reconcile §2.4](20-continuous-reconcile.md) 明确定了孤儿
**永不自动删**、「真正的移除走 `mechctl component remove`」——因此
**不能靠「期望状态里没有了」来实现**。

> 这一段原来的结论是「它需要一条新的、显式的卸载指令」。做完之后要更正：
> **不是指令，是 `runState` 的第三个值 `removed`。** 指令是事件，丢一次
> 就永远丢了；而这个项目从 M4 起守的是「状态可以重复确认」。断连三天的
> 节点回来之后，它收到的仍然是「这个实例不该存在」，照做即可。
> 见 [24-lifecycle-completion §2.1](24-lifecycle-completion.md)。

> 顺带修一处**文档自相矛盾**：10-cli §4.3 说 `--force` 跳过的失联节点
> 「重新上线时……mechlet 据此自行清理」，这与 §2.4 的「只记不删」直接
> 冲突。以 §2.4 为准——那条纪律有明确的理由（「mechd 少发了一条」与
> 「用户真的删了」在节点侧分辨不了，而卸载不可逆）。

### 文档里有、代码里没有（M9 的处置）

**做了的**：`orphans` 名词族（remove 保留数据之后的配套）、`restart`
（它带出了整条 ad-hoc 命令通道，[ADR-0038](../adr/0038-adhoc-task-channel.md)）。

**其余全部标成「待真实需求」**，留在 10-cli 的命令表里但显式标注未实现，
各自写明为什么先不做——见 [10-cli §4](10-cli.md) 与
[24-lifecycle §3](24-lifecycle-completion.md)。

> **「划掉」不是「删掉」。** M8 收尾审查发现的问题是「读 10-cli 的人会
> 以为它们已经能用」，而悄悄删掉会让那些设计连同理由一起消失——那比
> 留着更糟。
>
> 现在有一条**自动化守卫**（`internal/cli/ctlcmd/docdrift_test.go`）钉住
> 这件事：10-cli 里每一条命令，要么真的存在，要么标着未实现；反过来，
> 已经实现的也不许还标着未实现。

### M9 收尾审查

做完之后又做了一次里程碑审查，输出见
[24-lifecycle §5](24-lifecycle-completion.md)。**主要发现是「验收表全绿
不等于代码被测过」**：15 条判据条条真机验过，而 restart 的两端、
`apply -f` 的 `configGroups:` 与 secret 路由这四处的单元覆盖率是 0.0%
——前两处只被不产出覆盖数据的真机套件走过，后两处**发布了、文档里写着、
一次都没被执行过**。补了 22 条测试。

还抓到一条**在 Windows 上绿错了的测试**（验的其实是「什么都删不动」，
一个照删不误的实现同样能过），以及一处「开发机覆盖率系统性偏低」
（reconcile 在 Windows 13.1%、Linux 79.1%）。

### 已记录在案的其它缺口

| | 出处 |
|---|---|
| `node add`（pending）之后无法 join，批量预置走不通 | 决策 216 |
| `Overrides.Role` 在 mechd 存储里从未被填过，模型比实现多一层 | 决策 223 |
| 滑块把验收表第 2、3、6 条挡在端到端之外 | [23-web-ui §6.12.2](23-web-ui.md) |
| 前端 `node_modules` 平台锁定，Windows 与 WSL 不能共用 | M8 收尾审查 |
| 多节点全量套件在忙机器上的负载敏感失败（**定位清楚、没有解决**） | 决策 330、[24-lifecycle §3](24-lifecycle-completion.md) |
| `checkSiteMatches` 的 kind 放行分支未测（拒绝分支有测） | [24-lifecycle §5.6](24-lifecycle-completion.md) |
| 真机套件不产出覆盖数据，「哪些代码只被真机走过」目前靠人看 | 同上 |

## M1 的特殊地位

**pack/v1 是全项目最重要的一次设计。** Pack 格式是唯一改不动的东西——用户写的 Pack 一旦散出去，格式就被锁死了。

因此它单独占一个里程碑、单独一轮设计评审，不夹在功能代码里顺手定了。

## 官方 Pack 顺序

顺序按**验证能力**排列，不按用户需求热度：

| 顺序 | Pack | 验证什么 |
|---|---|---|
| 1 | `go-webapp` | M2 的测试夹具。单静态二进制 + systemd unit + 配置文件 + `/healthz`，无外部依赖，可在 CI 容器中端到端跑 |
| 2 | `docker` | **离线安装 root 级运行时**。用 systemd runtime 从静态二进制 tarball 安装 dockerd + containerd + runc + compose 插件 |
| 3 | `jdk` | **无 workload 的 Pack** + 被其他 Pack 依赖 |
| 4 | `java-webapp` | **Pack 间依赖解析** + JVM 参数化 |
| 5 | `nginx` | **模板渲染 + reload**（`reloadRequired` 语义） |
| 6 | `postgresql` | ⭐ **真正的试金石**：数据目录分离、配置位于数据目录内、有状态升级、primary/replica 多角色、跨角色拓扑引用 |
| 7 | `minio` | 多磁盘绑定（`kind: multi`） |

> 第 1 个选 `go-webapp` 而非 nginx 是刻意的：nginx 会把开发拖进发行版差异、包版本可用性等与核心设计无关的泥潭；go-webapp 是自己完全掌控的最小闭环。nginx 排在验证「模板 + reload」的位置更合适。

## 已知需在实现阶段确定的议题

| 议题 | 何时必须定 |
|---|---|
| ~~CLI 动词表与输出格式~~ | ✅ M0，见 [10-cli.md](10-cli.md) |
| ~~最低 Go 版本 / 平台矩阵~~ | ✅ M0，见下 |
| ~~结构化日志库选型~~ | ✅ M0：标准库 `log/slog`，不引入第三方 |
| 是否暴露 Prometheus metrics | M5 |
| ~~Linux 发行版矩阵 / glibc 下限的正式承诺~~ | ✅ M2：**无 libc 下限**（静态二进制），实际下限由 systemd ≥ 239 决定。见下 |
| ~~健康检查类型集；「健康」与「就绪」是否区分~~ | ✅ M2：`http` / `tcp` / `exec` 三种，**不区分健康与就绪**。见下 |
| ~~mechd ↔ mechlet 协议细节~~ | ✅ M3 设计已定，见 [17-protocol](17-protocol.md) 与 [ADR-0029](../adr/0029-push-over-server-stream.md) |
| ConfigGroup 的 UI 呈现 | ✅ M8 设计已定，见 [23-web-ui §4.1](23-web-ui.md)；「合并同配置组」辅助命令**不在 M8**，待真实多组使用经验 |
| `volumeClass` 自动挑盘（`class` 声明字段 v1 已有） | 待真实异构大集群用户 |
| 增量 blob 传输 | 随时可加，延后成本为零 |
| 控制面 HA | 有真实需求时，见 [ADR-0014](../adr/0014-no-ha-in-v1.md) |
| Kubernetes runtime | 有真实需求时，接口已预留，见 [ADR-0017](../adr/0017-k8s-extension-reserve.md) |

### M2 定下的支持范围

#### libc：没有下限

`CGO_ENABLED=0` 让四个二进制都是**纯静态**的（[M0 技术选型](#m0-定下的技术选型)），
因此**不存在 glibc 版本下限**——这是那条约束最实在的一份回报。同一个二进制在
glibc 与 musl（Alpine）上都能跑，也不需要为每个发行版各出一个包。

「glibc 下限」这个议题在提出时假设了动态链接。实现之后它自己消失了，这里记下来
是为了让后来的人知道**它已经被回答过**，而不是被忘了。

#### 内核：≥ 3.10

Go 本身要求 Linux 3.2+。取 3.10（RHEL 7 那一代）作为承诺值，留出余量。
实际上这条永远不会成为瓶颈——systemd 的要求比它高得多。

#### systemd：≥ 239（真正的下限）

| 发行版 | systemd | 状态 |
|---|:---:|---|
| Debian 12 (bookworm) | 252 | ✅ **CI 实测**（`test/node`） |
| Debian 11 | 247 | ✅ 承诺支持 |
| Ubuntu 22.04 / 24.04 | 249 / 255 | ✅ 承诺支持 |
| Ubuntu 20.04 | 245 | ✅ 承诺支持 |
| RHEL / Rocky / Alma 9 | 252 | ✅ 承诺支持 |
| RHEL / Rocky / Alma 8 | 239 | ✅ 承诺支持（下限） |
| SLES 15 | 249 | ✅ 承诺支持 |
| RHEL 7 | 219 | ❌ 不支持（该版本本身已 EOL） |

**239 这条线不是技术极限，是支持承诺。** 代码只依赖 systemd v220 就有的子命令，
更低的版本多半也能跑；但没有 CI 覆盖的东西不该承诺。

`systemd.Probe` 会**主动拦截**低于下限的系统，把实际版本与下限一并报出来：

```
node-7 不支持 systemd runtime
  systemd 219 低于支持下限 239 —— 请升级发行版，或改用其它 runtime
```

明确拒绝，好过让用户在某个用不了的属性上撞一鼻子灰（[原则六 显式优于隐式](00-overview.md#原则六显式优于隐式)）。

> 为了不让下限被无谓抬高，`systemctl show` 的输出解析同时认
> `--value` 的两种形态（`no` 与 `CanReload=no`）——`--value` 是 v230 才有的 flag，
> 单为它抬两个大版本不值得。

#### 两条与发行版无关、但会咬人的环境事实

实现过程中撞到的，写进 [spec §15.1](../spec/pack-v1.md#151-runtime-systemd) 供 Pack 作者参考：

| 事实 | 后果 |
|---|---|
| **最小化镜像没有 `/bin/kill`**（由 `procps` 提供） | 照抄 systemd 手册的 `ExecReload=/bin/kill -s HUP $MAINPID` 会得到一个永远 reload 失败的 Pack。改用 shell 内建：`/bin/sh -c 'kill -HUP $MAINPID'` |
| **`/tmp` 常被挂成 `noexec`**（systemd 自带的 `tmp.mount` 就是） | 从那里启动进程得到 `203/EXEC Permission denied`，错误信息只字不提 `noexec`。载荷一律解在 `{{ .Paths.Generation }}` 之下，这条不变式已规避该问题 |

### M2 定下的健康检查

**三种探针：`http` / `tcp` / `exec`**，互斥，实现在 `internal/health`。它们跑在
Runtime 接口**之上**，跨 Runtime 行为一致（[05-runtime §3 规则①](05-runtime.md#3-核心原理接缝划在真正不同的地方)）。

**不区分「健康」与「就绪」。** Kubernetes 分开是因为它要据此摘除 Service 端点——
那是一个 Mecharion 没有的机制。单机与小集群场景下二者几乎总是同一件事，分开只会
让 Pack 作者面对一个他答不上来的问题。真有需要时再加，那时会有具体案例告诉我们该
怎么分；而合并成一个之后再拆开，比一开始就拆错要容易。

一条实现上的判据：**探针一律探 `127.0.0.1`**。探针跑在服务所在节点的 mechlet 里，
走本机既绕开了防火墙，也避免把「网络通不通」混进「服务活没活」。

### M0 定下的技术选型

| 项 | 选择 | 理由 |
|---|---|---|
| 最低 Go 版本 | **1.25.7** | 覆盖当前与上一个稳定版；`log/slog` 等所需特性均已就绪。<br>精确到补丁号是被 `pressly/goose` 顶上去的（M3 引入），不是我们自己的需要——但 1.25 用户本来就该跟到最新补丁 |
| CLI 框架 | **spf13/cobra** | 事实标准，纯 Go，子命令/补全/帮助齐备 |
| 日志 | **标准库 `log/slog`** | 无第三方依赖，Handler 接口足以支撑后续结构化需求 |
| YAML | **gopkg.in/yaml.v3** | 纯 Go |
| **CGO** | **一律 `CGO_ENABLED=0`** | 单静态二进制是产品的基础性质（离线分发、mechlet 自升级都依赖它）；这也是 [ADR-0012](../adr/0012-mechd-embedded-sqlite.md) 选 `modernc.org/sqlite` 的前提 |
| **平台专属代码** | **隔离编译**（`*_linux.go` / `*_other.go`） | mechlet 会有 systemd D-Bus、`chown`、文件模式等 Linux 专属实现。非 Linux 平台返回明确的「不支持」而非编译失败，保证 `go build ./...`、`go vet ./...` 与单元测试在三平台都能跑，CI 矩阵不为 mechlet 破例。集成测试自然只在 Linux 容器中执行 |
| CLI 结构 | **名词优先** `mechctl <名词> <动词>` | 见 [ADR-0025](../adr/0025-noun-first-cli.md) |

### 平台矩阵

| 二进制 | linux/amd64 | linux/arm64 | darwin | windows |
|---|:---:|:---:|:---:|:---:|
| `mechctl` | ✅ | ✅ | ✅ | ✅ |
| `mechpack` | ✅ | ✅ | ✅ | ✅ |
| `mechd` | ✅ | ✅ | ❌ | ❌ |
| `mechlet` | ✅ | ✅ | ❌ | ❌ |

`mechd` / `mechlet` 依赖 systemd 等 Linux 设施，只发布 Linux；`mechctl` / `mechpack` 是运维与开发者工具，需在开发机上可用，因此全平台构建并测试。
