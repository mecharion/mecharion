# mechd —— 控制面

mechd 持有**期望状态**，并把它变成每个节点的[已解析规格](12-spec-and-state.md)。

它**不执行任何部署动作**——那是 mechlet 的事（[ADR-0002](../adr/0002-mechlet-as-sole-engine.md)）。
mechd 退场时单机能力不受影响，这条边界决定了下面的全部划分。

---

## 1. 内部分层

```
┌─ 入口 ─────────────────────────────────────────────────┐
│  HTTP API (0.0.0.0 + HTTPS)      gRPC (mechlet 拨入)   │
└───────────────┬────────────────────────┬───────────────┘
                │                        │
┌───────────────▼────────────────────────▼───────────────┐
│  应用服务层                                             │
│    deploy / upgrade / rollback / remove                │
│    ★ 每个动作 = 一次「解析 → 下发 → 编排」               │
└───────────────┬────────────────────────────────────────┘
                │
┌───────────────▼────────────────────────────────────────┐
│  解析管线（16-render-pipeline）                          │
│    放置 → 参数 → 依赖绑定 → 渲染 → 封装 ResolvedSpec     │
│    ★ 纯函数：同样的输入必然产出同样的 digest             │
└───────────────┬────────────────────────────────────────┘
                │
┌───────────────▼────────────────────────────────────────┐
│  Store（07-persistence §1.5）   SecretVault（17-secrets）│
│  BlobStore（内容寻址文件）       PackIndex（解开的 Pack） │
└────────────────────────────────────────────────────────┘
```

**解析管线是纯函数**这条性质值得单列：它让「为什么这台机器上是这份配置」
可以离线复现——`mechctl component render <name> --on <node>` 不碰任何机器
就能打印出完整的 ResolvedSpec。事故复盘时这是第一手材料。

> 唯一的例外是 `generate`（首次生成密码）与 ordinal 分配——两者都是
> **一次性副作用**，产生后即固化，见 [16-secrets](16-secrets.md) 与
> [ADR-0028](../adr/0028-stable-ordinals.md)。管线的其余部分不写任何状态。

## 2. 一次 deploy 的内部时序

```
mechctl component deploy go-webapp --nodes n1,n2
  │
  ├─ ① 校验：Pack 存在、参数合法、节点在册
  │
  ├─ ② 组件级拓扑排序           ← 消费方要等提供方解析完
  │     for 每个组件（依赖在前）:
  │        放置 → 分配 ordinal → 解析参数 → 求值 exports
  │
  ├─ ③ 渲染：逐 RoleInstance 产出 ResolvedSpec + 封 digest
  │
  ├─ ④ 落库：期望状态 + Rollout（状态机）
  │
  ├─ ⑤ 下发：按 Role 的 requires 拓扑排序、分批推给 mechlet
  │
  └─ ⑥ 收敛：等待各实例上报，按健康门禁推进 / 暂停
```

**② 必须早于 ③**：`{{ .Requires.postgresql.Exports.app.password }}` 要求
PG 的参数已经解析完（[15-render-pipeline §5](15-render-pipeline.md#5-组件间的解析顺序)）。

**④ 必须早于 ⑤**：先把期望状态写下来，再下发。反过来的话，mechd 在下发后
崩溃会留下「机器上装了、库里没有」的孤儿——而机器上的东西没有主人是最难
清理的一类状态。

## 3. API 表面

### 3.1 HTTP（给人和 UI）

```
GET    /api/v1/sites                    /components        /nodes        /packs
POST   /api/v1/packs                    上传 .mpack（唯一一处新增的供应链入口）
GET    /api/v1/components/{name}        …/status  …/diff  …/render
GET    /api/v1/components/{name}/params ?role=&group=  参数表单（M8 §4.2）
GET/PUT/DELETE  …/components/{name}/groups[/{group}]   配置组（ADR-0021）
POST   /api/v1/components               deploy
PATCH  /api/v1/components/{name}        upgrade / set-profile / assign
POST   /api/v1/components/{name}/rollback  …/restart  …/ack-drift
DELETE /api/v1/components/{name}
GET    /api/v1/components/{name}/watch  SSE：状态+rollout 的**完整快照**（M8 §4.5）
GET    /api/v1/events                   审计与事件流
```

默认绑 `0.0.0.0` + HTTPS，自签 CA 10 年、服务端证书 1 年自动轮换
（[08-security §3.2](08-security.md#32-mechd-的-http-接口)）。

**`/render` 与 `/diff` 是只读的解析管线出口**——它们跑完整的管线但不落库、
不下发。这是「先看看会发生什么」的唯一正确实现方式：不是另写一套预演逻辑，
而是同一条管线少走最后两步。

### 3.2 gRPC（给 mechlet）

见 [17-protocol](17-protocol.md)。单机走 unix socket，多机走 mTLS。

### 3.3 实现说明

| 层 | 包 |
|---|---|
| HTTP API + 认证 | `internal/mechd`（`httpapi.go`） |
| 应用服务：deploy / status / diff / ack-drift | `internal/mechd`（`service.go`） |
| gRPC 后端 | `internal/mechd`（`backend.go`），实现 `protocol.Backend` |
| 命令 | `mechd serve` · `mechctl component <动词>` |

**期望状态落库，已渲染的规格不落库。** `Assignment` 与 `status` 都按需
重跑一遍解析管线——管线是纯函数，同样的输入必然产出同样的 digest，
因此重算是安全的。缓存则要处理失效（参数改了、依赖升级了、事实刷新了），
漏一条就是「改了不生效」。M3 的规模下重算是毫秒级。

> 这条选择有一个前提：管线读的**必须是固化的输入**。见下。

### 3.4 事实快照必须固化到 RoleInstance

`defaultFrom` 读 `Node.Facts`。若解析管线按需重算、而它读的是随心跳刷新的
那份实时事实，就会出现：

```
节点加了内存 16GB → 32GB
  ↓ 下一次 Assignment 重算
heap 从 8G 变成 16G → digest 变 → 新 generation → 服务在业务时间重启
```

更糟的是某次采集报了 0 字节 → `heap=0` → 服务起不来。

因此 `role_instances` 上有一列 `facts`：**放置时的快照**
（[spec §9.4.1](../spec/pack-v1.md#941-两种用途规则完全不同)）。
心跳写的是 `node_facts`（实时那一份），两者互不影响；事实漂移由
`mechctl node facts refresh --apply` 显式应用。

**「按需重算」与「事实固化」是同一个设计的两半。** 只做前者会得到一个
会自己变的期望状态；只做后者则要维护一份缓存。

### 3.5 diff 用真实的密钥，不用一次性值

`deploy --dry-run` 用一次性密钥——它可能在部署一个全新的组件，而
**首次生成**是真实的副作用，一次「先看看会发生什么」不该留下它。

`diff` 相反：它要拿期望 digest 与上报的比。一次性密钥会让 `secretRefs`
的 id 与版本每次都不同，于是**任何带 `generate` 参数的组件都会永远显示
「有待下发的变更」**——一个永远报有变更的 diff 比没有 diff 更糟，
它会让人学会忽略它。

读已固化的密钥没有副作用，因此 diff 走真实 Vault。

## 4. 写入并发

SQLite 是单写者。约定（[07-persistence §1.4](07-persistence.md#14-工程约定)）是
**写收敛到单 goroutine**。具体形态：

```
所有写操作 → 一个 command channel → 单 writer goroutine → 单连接
读操作     → 独立连接池，WAL 下与写并发
```

心跳是唯一的高频写（1000 节点 × 15s ≈ 66 写/秒），且**可合并**：
同一节点在一个批次窗口内的多次上报只写最后一次。这让写队列在节点数增长时
仍然平坦。

## 5. 单机形态

[ADR-0026](../adr/0026-standalone-runs-mechd.md) 定了单机也跑 mechd。落到实现上：

```bash
mechlet install --standalone
```

| 步骤 | 做什么 |
|---|---|
| ① | 四个二进制装到 `/usr/local/lib/mecharion/generations/<v>/bin`，切 `current` 软链，并在 **`/usr/bin` 挂四条软链**——零 PATH 配置（[04-paths §布局判据](04-paths-and-storage.md#三条布局判据)） |
| ② | 生成 `/etc/mecharion/mechd.yaml` 与 `mechlet.yaml`（roots、volumes 探测） |
| ③ | 初始化 SQLite（goose 迁移）与**主密钥**（16-secrets §3） |
| ④ | 生成自签 CA + 服务端证书 |
| ⑤ | 写两个 systemd unit，`mechd` 先起，`mechlet` 重试连接（不用 `Requires=`） |
| ⑥ | 注册本机为 Node，upstream 指向 `unix:///run/mecharion/mechd.sock` |
| ⑦ | **打印一次**初始 admin token 与主密钥备份提示 |

第 ⑦ 步的两条输出都**只打印这一次**。token 只存哈希，主密钥丢了则运维手填的
密钥不可恢复（引擎生成的可重新生成）。

### 5.1 已实现的部分

`mechlet install --standalone` 走五步：装二进制并挂软链 → 生成配置 →
初始化库与主密钥 → **注册本机节点** → 写两个 unit。

第 ④ 步不能省：`Backend.Register` 会拒绝未在册的机器
（**mechd 不凭空创建节点**——允许任意拨上来的 agent 自己建节点，
等于让「这个 Site 里有哪些机器」由网络决定）。单机安装因此必须先把
自己登记上，否则 agent 一拨上来就被拒。

`--link-dir` 可以改软链落点。默认 `/usr/bin`；改它多半只在打包与 chroot
里有意义——**验收测试用它把安装与容器自身的软链隔开**，否则一次安装会
改掉测试夹具的 `mechctl`。

两个 unit 之间刻意**不写 `Requires=`**：那会让 mechd 的一次重启把 mechlet
拖下水，而 mechlet 本就该在 mechd 不可达时继续自治。顺序用 `After=` 表达，
连不上则由 mechlet 自己指数退避重连（[01-architecture §4.2](01-architecture.md#42-启动依赖用重试不用-systemd-requires)）。

### 5.2 `mechlet agent`

它把两条**已有**的路径接起来，没有新写第三条：

```
protocol.Client  拿到期望状态（没有动词，只有「应该有什么」）
       ↓
reconcile        把期望状态变成机器上的实际状态
```

M2 的 `mechlet apply -f` 走的是同一个 reconciler。agent 只负责编排：
收到规格 → 取载荷 → 还原密钥 → 调和 → 上报 digest。

下发与上报是**两条独立的循环**：一条断了不影响另一条。这正是
「不用双向流」换来的东西（[17-protocol §1](17-protocol.md#1-形态服务端流下发--一元上报)）。

一个实例调和失败**不拖垮其余的**——它们彼此独立，而「因为 A 装不上
所以 B 也没装」是最难解释的一类现场。

> 单机与多机走**同一条**九步链路（[01-architecture §6](01-architecture.md#6-一次-apply-的完整链路)），
> 差别只在传输层与「放置结果平凡」。没有任何一段逻辑是单机专用的。

## 6. 断连与重连

mechlet 断连时按最后已知期望状态继续自治（[01-architecture §5](01-architecture.md#5-断连自治)）。
重连时 mechd 的处理**刻意简单**：

> **重连即全量重推。**

不做增量同步、不做确认序号、不做差异计算。理由是下发本身**幂等**——
digest 相同的规格在 mechlet 侧是无操作（[12-spec-and-state §1.5](12-spec-and-state.md#15-digest--generation-的身份)）。
一个内容寻址的期望状态让整类同步协议问题消失了。

代价是重连瞬间的一次全量传输。1000 节点 × 每节点几 KB 的规格 = 几 MB，
且只在 mechd 重启时发生。用一个协议的复杂度换这点带宽，不划算。

## 7. 不做的事

| | 为什么 |
|---|---|
| **调度器** | 节点由用户显式指定。`placement` 是**校验**不是求解（[14-placement](14-placement.md)） |
| **内部消息队列** | 单进程内的 channel 足够；引入 MQ 会带来一个必须运维的新组件，与「边缘单机」直接冲突 |
| **多副本 / 选主** | [ADR-0014](../adr/0014-no-ha-in-v1.md)。mechd 不在数据面上，宕机不影响已部署组件运行 |
| **Pack 仓库联网拉取** | [ADR-0015](../adr/0015-offline-first-hermetic.md)。只在本地 Pack 集合内解析 |

## 8. 相关决策

- [ADR-0026 单机形态同机运行 mechd](../adr/0026-standalone-runs-mechd.md)
- [ADR-0012 mechd 用嵌入式 SQLite](../adr/0012-mechd-embedded-sqlite.md)
- [ADR-0029 mechd ↔ mechlet 的协议形态](../adr/0029-push-over-server-stream.md)
- [14-placement](14-placement.md) · [15-render-pipeline](15-render-pipeline.md) · [16-secrets](16-secrets.md) · [17-protocol](17-protocol.md)
