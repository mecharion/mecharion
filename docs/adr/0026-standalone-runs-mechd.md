# ADR-0026: 单机形态同机运行 mechd，存储与 API 不分叉

- **状态**：已接受
- **日期**：2026-08-02
- **修订**：[ADR-0002](0002-mechlet-as-sole-engine.md) 中「mechd 是可选协调层」的措辞
- **相关**：[ADR-0012](0012-mechd-embedded-sqlite.md)、[ADR-0013](0013-mechlet-no-database.md)、[ADR-0025](0025-noun-first-cli.md)

## 背景

[ADR-0002](0002-mechlet-as-sole-engine.md) 确立了「边缘离线单机是基准形态，中心化是叠加的一层」，并把 mechd 描述为**可选**的协调层：单机形态下 `mechctl --local` 直连 mechlet，mechlet 从本地 JSON 文件读期望状态。

这套设计有一个**从未被论证过的隐含前提**：单机不需要 Web UI。

而这个前提站不住。一台完全离线的边缘机（门店、产线、工控现场）上，运维人员需要一个界面看组件状态、重启服务——CLI 对熟练运维够用，对现场人员不够。

一旦承认单机需要 UI，就必须回答：UI 的查询能力（筛选、排序、分页）、事件与审计历史从哪来。

## 问题

按原设计，单机只有 mechlet + JSON 文件。要支持 UI，只有两条路：

- **让 mechlet 长出 API + 查询层**，用文件存储支撑
- **让单机也跑 mechd**

第一条意味着 `Store` 接口有两个实现（SQLite 与文件）。

## 候选方案与调研

### 方案 A：维持现状，单机无 UI

否决。它让单机成为**功能缺失的特化**，与「边缘是基准形态而非降级模式」的核心原则直接矛盾。

### 方案 B：mechlet 提供 API + UI，存储用文件 + 内存索引

初期推荐过这条，理由是「单机数据规模只有几十条记录，SQL 无价值；文件存储更轻、现场 `cat` 就能看」。

**这个论证有三处错误：**

**① 「文件存储无迁移负担」是错的。** [ADR-0013](0013-mechlet-no-database.md) 自己就写了「JSON 结构中带 `schemaVersion` 字段，mechlet 启动时按需转换」。迁移负担两边都有，区别只是 SQLite 用 goose（成熟工具、有版本表、可回滚），文件存储用手写转换函数。

**② 两个 `Store` 实现，正是本项目在别处极力避免的「两套代码路径」。** [ADR-0002](0002-mechlet-as-sole-engine.md) 的核心论证是「两套代码必然产生行为差异，成本随功能数线性增长」——在执行层坚持这条、却在存储层主动引入分叉，是自相矛盾。而且分叉发生在最难测的地方：一个 bug 在 SQLite 实现里修了、文件实现里没修，只会在边缘现场暴露。

**③ 审计不是性能问题，是功能问题。** 原论证把事件历史当成性能话题（「扫文件慢但少见」）。但审计要回答的是「谁在什么时候把哪个 Pack 的哪个版本应用到了哪里」，按用户 / 时间 / 组件过滤是**基本要求**而非优化。JSONL 扫文件在合规场景下不能算实现了审计。

### 方案 C：单机同机运行 mechd + mechlet ⭐

```bash
mechlet install --standalone     # 一条命令，装两个 systemd unit
```

### 方案 D：单二进制内嵌 mechd + mechlet（k3s 式）

- ✅ 一个进程、一个 unit
- ❌ 进程内通信若走直接函数调用，又是一条与网络路径不同的代码路径——分叉换了个地方出现
- ❌ 若仍走 gRPC over socket，则「同进程」只省下一个 systemd unit，代价是二进制显著变大

k3s 的实践表明进程内直连是微妙差异的来源。收益不足以覆盖这个风险。

## 决策

**采用方案 C：单机形态 = mechd + mechlet 同机部署。**

| | 多节点 | 单机 |
|---|---|---|
| mechd | 独立机器 | **同机** |
| mechlet | 每个受管节点 | 同机 |
| 存储 | SQLite | **SQLite（同一份代码）** |
| API + WebUI | mechd 提供 | **mechd 提供（同一份代码）** |
| 审计与事件 | mechd | **mechd** |
| 唯一差异 | — | **mechd↔mechlet 的传输方式** |

代码分叉降到只剩一处：连接的传输凭据。

### 本机通信走 unix socket，不使用 mTLS

```
mechd  gRPC   listen: unix:///run/mecharion/mechd.sock   给 mechlet
mechd  HTTP   listen: 0.0.0.0:8443 (HTTPS)               给 mechctl 与浏览器
```

**mTLS 在本机 unix socket 上是负收益**：socket 权限（`0600 root:root`）是内核强制的对端身份保证，比证书更强且无法伪造。再叠一层 TLS 只增加启动复杂度与故障面（证书生成、轮换、时钟偏移）。

这不构成代码分叉——gRPC 的 service 定义、handler、序列化全部共用，差异只是一行传输凭据：

```go
// 多节点
grpc.NewClient(server, grpc.WithTransportCredentials(mtlsCreds))
// 单机
grpc.NewClient("unix:///run/mecharion/mechd.sock", insecure.NewCredentials())
```

### mechlet 不再有 standalone / managed 之分

原设计要区分两种 mechlet 模式并据此决定本地 socket 的读写权限。现在 **mechlet 永远连着一个 mechd**（本地或远程），模型统一：

```
正常      mechctl → mechd
--local   mechctl → mechlet socket，只读诊断，仅用于 mechd 不可达时
```

少一个概念。

### mechlet 重试连接，不用 systemd Requires=

mechlet 启动时 mechd 可能未就绪。**用重试而非 systemd 依赖**——多节点形态下本机根本没有 mechd，mechlet 本来就必须处理「mechd 不可达」，用同一套逻辑更健壮，也避免了单机与多节点的 unit 文件分叉。

## 理由

**决定性理由：这个方案让代码分叉降到最低，而「避免分叉」正是 ADR-0002 的立论基础。**

方案 B 用「二进制小 20MB」换「多一套存储实现」。对一台运行 PostgreSQL 或 JVM 的机器，20MB 是噪音；而一套需要长期保持行为一致的重复实现，是持续的维护税。

**次要理由：单机从「功能子集」变成「同一套东西的同机部署」。** 这不只是措辞——它意味着单机用户拿到的是完整的审计、完整的查询、完整的 UI，而不是一个阉割版。这与「边缘是基准形态」的宣称终于一致了。

## 对 ADR-0002 的修订

ADR-0002 的**三条结论全部保留且更强**，只有措辞需要修订：

| 原诉求 | 本决策下 |
|---|---|
| 避免两套执行路径 | ✅ **更彻底**——连存储层都不分叉 |
| 控制面故障不影响业务 | ✅ 仍成立：mechd 挂了，mechlet 继续按最后已知期望状态调和自愈 |
| 边缘不是降级模式 | ✅ **更强**：边缘跑的是完整的一套，含 UI 与审计 |

修订后的表述：

> **mechd 不是「可选」，而是「可以与 mechlet 同机」。** 边缘单机是 mechd + mechlet 的同机部署，功能与多节点完全一致。控制面与数据面的分离仍然成立——mechd 不可用时 mechlet 继续自愈。

## 后果

### 收益

- 存储、API、UI、审计全部一份实现
- 单机具备完整的事件与审计查询能力
- mechlet 不再需要 standalone / managed 两种模式，连接模型统一
- SQLite 的迁移走 goose，不再需要手写 JSON 结构转换

### 代价

- **单机多一个 systemd unit**：用户视角仍是一条安装命令，但 `systemctl` 里会看到两个服务。文档需说明各自职责与排查入口
- **边缘设备资源占用上升**：mechd(Go+SQLite) + mechlet 约 **60–100MB RSS**，磁盘约 40MB。需在文档中给出下限；**极小工控设备（≤256MB 内存）不适用**
- **升级要动两个组件**：约定**始终同步升级**，`mechlet install` 一并处理。跨版本时先 mechd 后 mechlet（控制面向后兼容 agent）
- **单机的故障面变大**：多了 SQLite 与本地 gRPC 两个环节。以「mechd 不可用时 mechlet 继续自愈」缓解——业务不受影响，只是暂时不能下发变更
- **ADR-0013 需要澄清而非推翻**：mechlet 仍然不用数据库，它的本地状态（已安装实例、generation 台账、断连事件缓冲）仍是 JSON 文件；**审计与事件的权威存储在 mechd 的 SQLite**，mechlet 的 JSONL 只是断连期间的缓冲

### 对路线图的影响

原 M2 的验收判据是「`mechctl --local apply` 在离线机上跑通 go-webapp」，隐含了「无 mechd」。本决策后单机也需要 mechd，里程碑需重排——见 [25-roadmap.md](../design/25-roadmap.md)。

关键是不能因此把 mechd 提前到资源引擎之前：**M2 仍只做资源引擎与 systemd runtime**，用 `mechlet apply -f <已解析规格>` 这个调试入口驱动。它读的规格与 mechd 下发的是同一结构，走的是同一个 reconciler——**不是分叉，是同一条路径的另一种输入来源**。

## 参考

- k3s 的单二进制内嵌（方案 D 的实践参照）
- Docker 组等价于 root 的教训（socket 权限即认证的旁证）
- goose / golang-migrate 的迁移版本表机制
