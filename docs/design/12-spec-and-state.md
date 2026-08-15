# 已解析规格与本地状态

本文定义两件事：

- **`ResolvedSpec`** —— mechd 下发给 mechlet 的东西，也是资源引擎的输入
- **mechlet 本地状态** —— 它自己记住的东西

这两者是同一件事的两面：**下发什么、本地记什么**。它们共同构成 mechd 与 mechlet 之间的契约，M2 的 `mechlet apply -f <spec>` 与 M3 的 gRPC 下发读的是同一个结构。

---

## 1. `ResolvedSpec`

### 1.1 它为什么存在

[ADR-0006](../adr/0006-multi-role-pack.md) 确立了「**渲染必须发生在放置之后**」——跨角色拓扑引用（`topology.role('primary').nodes[0]`）只有在角色分布确定后才能解析。

由此得到一条关键性质：

> **mechlet 收到的是一份已解析的快照，它不需要（也无法）查询其他节点。**

这是整个系统能够简单的关键——mechlet 之间零通信。

### 1.2 一份 spec 对应一个 RoleInstance

不是一个 Component，不是一个节点。`hdfs-prod` 在 6 个节点上有 12 个 RoleInstance，就有 12 份 spec。

一个节点上可能有多份 spec（同一 Component 的多个角色、或多个 Component）。

### 1.3 结构

```go
type ResolvedSpec struct {
    // ── 元信息 ──────────────────────────────────────────
    SchemaVersion int    `json:"schemaVersion"` // 本结构自身的版本
    Digest        string `json:"digest"`        // 规格内容的 sha256，见 §1.5

    // ── 身份 ────────────────────────────────────────────
    Site        SiteRef `json:"site"`
    Component   string  `json:"component"`
    Role        string  `json:"role"`
    ConfigGroup string  `json:"configGroup"`   // 未分组时为 "default"
    Node        NodeRef `json:"node"`
    Ordinal     int     `json:"ordinal"`       // 同角色内的稳定序号

    // ── 来源 ────────────────────────────────────────────
    Pack    PackRef `json:"pack"`              // name / version / revision / digest
    Profile string  `json:"profile"`           // 未声明 profiles 时为 ""

    // ── 已解析的取值 ────────────────────────────────────
    Params map[string]ParamValue `json:"params"`
    Paths  map[string]PathValue  `json:"paths"`

    // ── 载荷 ────────────────────────────────────────────
    Blobs []BlobRef `json:"blobs"`

    // ── 已渲染的资源 ────────────────────────────────────
    Resources []ResolvedResource `json:"resources"` // shared 在前，role 在后

    // ── 工作负载 ────────────────────────────────────────
    Workload *ResolvedWorkload `json:"workload,omitempty"`
    Health   *ResolvedHealth   `json:"health,omitempty"`
    Hooks    []ResolvedHook    `json:"hooks,omitempty"`

    // ── 期望运行态 ──────────────────────────────────────
    RunState string   `json:"runState,omitempty"`  // running（默认）| stopped | removed
    Removal  *Removal `json:"removal,omitempty"`   // 仅 runState=removed 时有意义

    // ── 上下文 ──────────────────────────────────────────
    Topology TopologySnapshot           `json:"topology"`
    Requires map[string]RequireBinding  `json:"requires,omitempty"`

    // ── 调和行为 ────────────────────────────────────────
    Reconcile ReconcileOptions `json:"reconcile"`
}
```

### 1.4 关键字段

#### `Params` —— 为什么还需要它

资源已经渲染好了，为什么还要下发参数值？三个用途：

| 用途 | 说明 |
|---|---|
| **hook 环境变量** | `MECHARION_PARAM_<NAME>`；`sensitive` 的经临时文件传递 |
| **诊断** | `mechctl --local` 展示本实例的生效参数 |
| **上报** | 观测状态里带上参数摘要，便于 UI 呈现 |

**mechlet 不用参数做任何调和决策。** `restartRequired` / `reloadRequired` 的效果由 mechd 在渲染时算进 `Resources[i].Notify`（§1.4 `Notify` 一节），mechlet 只看资源。

```go
type ParamValue struct {
    Value     any    `json:"value"`
    Type      string `json:"type"`
    Sensitive bool   `json:"sensitive"` // true 时 Value 被替换为 "" ，实际值走 SecretRefs
}
```

`sensitive` 参数的值**不放在 spec 里**——spec 会被写进 mechlet 的本地状态文件。实际值走单独的 `SecretRefs`，由 mechlet 落到 `0600` 的临时文件后注入 hook，用完即删。

#### `Paths` —— 已解析的绝对路径

```go
type PathValue struct {
    Name     string   `json:"name"`     // "config" / "dataDirs"
    Values   []string `json:"values"`   // kind:single 时长度为 1
    Kind     string   `json:"kind"`     // single | multi
    Layout   string   `json:"layout"`   // separate | inline
    LinkInto string   `json:"linkInto,omitempty"`
    DistDir  string   `json:"distDir,omitempty"`
    Mode     string   `json:"mode"`
    Owner    string   `json:"owner"`
    Group    string   `json:"group"`
}
```

引擎按此**自动创建目录**（调和阶段 ①），Pack 不需要写 `directory` 资源（[spec §8.3](../spec/pack-v1.md#83-字段)）。

#### `Resources` —— 完全渲染，除一个例外

```go
type ResolvedResource struct {
    ID          string          `json:"id"`
    Type        string          `json:"type"`
    Args        json.RawMessage `json:"args"`        // 已渲染的参数
    When        bool            `json:"when"`        // 已求值；false 的资源不下发
    DriftPolicy string          `json:"driftPolicy"` // report | reconcile | ignore
    Notify      string          `json:"notify,omitempty"`
    Origin      string          `json:"origin"`      // "shared" | "role" —— 供诊断
}
```

**`When` 已经被 mechd 求值**——求值为 `false` 的资源根本不出现在列表里。mechlet 不做条件判断。

#### 唯一的 late-bound 占位符

`{{ .Paths.Generation }}` 是**唯一允许残留在 spec 中的模板 token**。

原因：generation 目录名含一个 mechlet 本地分配的序号（`generations/0007-16.4-1`），mechd 无从得知。

处理方式是 **字面量替换，不是第二次模板渲染**：

```
mechlet 分配 generation 后，对 spec 中所有字符串执行：
    strings.ReplaceAll(s, "{{ .Paths.Generation }}", "<实际 generation 目录>")
```

不引入第二个渲染阶段——那会带来「哪些变量在哪一阶段可用」的持续困惑。单一 token 的字面量替换语义清晰、无歧义。

> `.Paths.Current`（`<home>/current` 软链）是**稳定的**，由 mechd 直接解析。Pack 在配置文件里应当引用 `Current` 而非 `Generation`——引用后者会把配置钉死在某个 generation 上。

#### `Notify` —— 由 mechd 计算最终动作

Pack 里 `template` 声明 `notify: reload`，但若这次变化的参数中有 `restartRequired: true` 的，最终动作应当是 `restart`。

**这个判断由 mechd 做**（它持有上一版 spec，知道哪些参数变了），结果直接写进 `Resources[i].Notify`。

mechlet 只做聚合去重与 `restart` 吸收 `reload`（[06-state-and-drift §6.1](06-state-and-drift.md#61-notify-的聚合)），不判断"该 restart 还是 reload"。

#### `Hooks` —— `scope: once` 由 mechd 裁决

```go
type ResolvedHook struct {
    Point   string   `json:"point"`   // preInstall / postUpgrade / …
    Script  string   `json:"script"`  // Pack 内相对路径
    Args    []string `json:"args"`
    Timeout string   `json:"timeout"`
    User    string   `json:"user"`
}
```

**`scope` 与 `when` 都不在这里**——它们已被 mechd 求值：

- `when: false` 的 hook 不下发
- `scope: once` 的 hook，mechd 查 Component 状态判断是否已执行过；已执行则不下发

因此 **mechlet 完全不需要理解 `once` 语义**，它只管「收到就执行」。执行结果上报后由 mechd 记录。

这个划分让「整个 Component 只执行一次」这个跨节点语义留在唯一有全局视角的地方。

#### `Topology` —— 拓扑快照

```go
type TopologySnapshot struct {
    Roles map[string][]InstanceRef `json:"roles"` // 角色名 → 该角色的全部实例
}

type InstanceRef struct {
    Node    string            `json:"node"`
    Address string            `json:"address"`
    Ordinal int               `json:"ordinal"`
    Labels  map[string]string `json:"labels"`
    Paths   map[string][]string `json:"paths"`  // 该实例已解析的路径（MinIO 需要）
}
```

`Paths` 在这里是必需的——MinIO 的端点列表要枚举**每个对等节点各自的盘路径**，而节点间挂载点可以不同（[spec §9.3](../spec/pack-v1.md#93-topology-引用)）。

> 拓扑快照是 spec 的一部分，因此**拓扑变化会改变 spec digest**，进而产生新 generation。加一个 DataNode 会让全体 DataNode 重新渲染——这是正确的（`dfs.datanode` 列表确实变了），但 Pack 作者应当意识到它的代价。

#### `Requires` —— 依赖解析结果

```go
type RequireBinding struct {
    Pack      string              `json:"pack"`
    Component string              `json:"component"` // 绑定到的 Component
    Version   string              `json:"version"`
    Scope     string              `json:"scope"`     // node | site
    Paths     map[string]string   `json:"paths,omitempty"`    // 仅 scope: node
    Exports   map[string]string   `json:"exports,omitempty"`  // 已拼装的连接串
}
```

`scope: site` 的绑定**不含 `Paths`**——那是别的机器上的路径（[spec §5.1](../spec/pack-v1.md#51-requirespacks--依赖的两种-scope)）。

#### `Reconcile`

```go
type ReconcileOptions struct {
    Interval          string `json:"interval"`          // 默认 60s
    HealthInterval    string `json:"healthInterval"`    // 默认 15s
    ReportInterval    string `json:"reportInterval"`    // 默认 15s
    RetainGenerations int    `json:"retainGenerations"` // 默认 3
}
```

### 1.5 `Digest` —— generation 的身份

`Digest` 是 spec 的规范化 sha256：

```
① 把 ResolvedSpec 序列化为 JSON，**键按字典序排列**
② 排除 Digest 字段自身
③ 排除敏感参数的 Value（它恒为空；见下）
④ **包含** secretRefs 的 id 与 version
⑤ sha256
```

它的作用：

| 场景 | 行为 |
|---|---|
| 收到 digest 与当前 generation 相同的 spec | **无操作**——幂等，不产生新 generation |
| 收到新 digest | 分配新 generation seq，物化，切换 |
| 收到历史 digest（回滚） | **复用已保留的 generation 目录**，直接切软链 |

第三条是回滚能秒级完成的原因：目录还在，不需要重新物化。

### 轮换必须产生新 generation

这里原先写的是「排除 SecretRefs，密钥轮换不应产生新 generation」。**那句话是错的**，
在设计跨组件凭据传递时暴露出来：

一旦口令被渲染进配置文件（而这**无法避免**——应用要从文件里读它），
文件内容就变了。此时若 digest 不变：

- 不产生新 generation
- 资源层检出差异，但 generation 没换 ⇒ 按[漂移](06-state-and-drift.md#driftpolicy-只管漂移不管期望变更)处理
- 默认 `driftPolicy: report` ⇒ **只上报不改** ⇒ **轮换永远发不出去**

因此正确的做法是：**值不进 digest（它根本不在 spec 里），但 `secretRefs` 的
version 进 digest**。轮换 → version+1 → digest 变 → 新 generation → 重渲染 → 重启。

配置文件确实变了，重启是应当的。**「轮换不该产生新 generation」这个直觉本身
就是错的**——它把「值没进 spec」和「什么都没发生」混为一谈了。

---

## 2. mechlet 本地状态

### 2.1 布局

```
/var/lib/mecharion/mechlet/
├── node.json                              节点身份与固化设置
├── instances/
│   ├── pg-main__primary.json              每个 RoleInstance 一份
│   └── hdfs-prod__datanode.json
├── specs/
│   └── pg-main__primary/
│       ├── 0007.json                      各 generation 对应的 spec（供回滚与诊断）
│       └── 0006.json
├── facts.json                             最近一次采集的事实（缓存）
└── events/2026-08-03.jsonl                断连缓冲，按天轮转、有上限
```

全部是 JSON 文件，写入方式为**写临时文件 + `rename`**（POSIX 原子重命名），任何时刻崩溃都不会留下半个文件（[ADR-0013](../adr/0013-mechlet-no-database.md)）。

### 2.2 `node.json`

```json
{
  "schemaVersion": 1,
  "nodeName": "node-7",
  "nodeID": "9f2c…",
  "dataDir": "/var/lib/mecharion",
  "roots": { "opt": "/opt/mecharion", "etc": "/etc/mecharion", "…": "…" },
  "volumes": [ { "name": "data1", "path": "/data1", "class": "bulk" } ],
  "upstream": "unix:///run/mecharion/mechd.sock",
  "installedAt": "2026-08-03T10:00:00Z"
}
```

**`dataDir` 是固化值**：mechlet 启动时校验它与 `--data-dir` 一致，不一致则**拒绝启动并给出明确错误**，不做自动迁移（[04-paths-and-storage](04-paths-and-storage.md#第一部分mecharion-自身)）。

### 2.3 `instances/<component>__<role>.json`

这是最重要的一份，它承载了三样在别处不可替代的东西。

```json
{
  "schemaVersion": 1,
  "component": "pg-main",
  "role": "primary",
  "configGroup": "default",

  "paths": {
    "home":   ["/opt/mecharion/apps/pg-main"],
    "config": ["/var/lib/mecharion/apps/pg-main/pgdata"],
    "data":   ["/var/lib/mecharion/apps/pg-main"]
  },

  "currentGeneration": 7,
  "generations": [
    { "seq": 7, "digest": "a1b2…", "version": "16.4", "revision": 1,
      "dir": "/opt/mecharion/apps/pg-main/generations/0007-16.4-1",
      "materializedAt": "2026-08-03T10:05:00Z", "state": "active" },
    { "seq": 6, "digest": "c3d4…", "version": "16.4", "revision": 1,
      "dir": "…/0006-16.4-1", "state": "retained" }
  ],

  "appliedResources": [
    { "id": "template:/var/lib/…/postgresql.conf", "type": "template" },
    { "id": "sysctl:vm.swappiness", "type": "sysctl" }
  ],

  "lastReconcile": {
    "at": "2026-08-03T10:06:00Z",
    "result": "ok",
    "durationMs": 84
  }
}
```

#### `RunState` / `Removal` —— 期望运行态

三个值：`running`（默认）、`stopped`、`removed`。前两个见
[20-continuous-reconcile §2.2](20-continuous-reconcile.md)，`removed` 是
「这个实例不该存在」（[24-lifecycle-completion §2.1](24-lifecycle-completion.md)）。

**未知值一律按 running 处理**，方向是刻意的：一个旧 mechlet 收到它不认识
的运行态时，宁可让服务继续跑着（人看得见，可以升级 mechlet），也不能猜成
`removed` 把它卸掉——那不可逆。

`Removal` 带的是三个处置开关，对应 10-cli §4.3 那张表：

```go
type Removal struct {
    KeepConfig bool  // 保留配置目录（默认删）
    PurgeData  bool  // 连数据一起删（默认保留并登记为孤儿）
    PurgeUser  bool  // 删掉 identity 资源建过的系统用户与组（默认保留）
}
```

**三个开关都是「偏离默认」的形式**，因此零值就是安全默认：配置删、
数据留、用户留。一份漏传 `Removal` 的规格不会多删任何东西。

它们**随规格下发而不由 mechlet 判断**（[ADR-0002](../adr/0002-mechlet-as-sole-engine.md)）
——`--purge-data` 是在中心敲的，让节点侧自己决定要不要删数据，等于把一个
不可逆的决定放在唯一没有上下文的那一端。

`RunState` 与 `Removal` 都**不参与 digest**：它们回答的是「该不该跑」与
「拆的时候怎么拆」，不是「盘上应该是什么」。理由与 `Suppressions` 同，
见 `spec.ComputeDigest`。

#### ① `paths` —— 固化，不可变

每次调和都用 spec 中的路径与这里比对。不一致时**拒绝调和并报错**，不自动迁移：

```
✗ pg-main@node-7 路径变更被拒绝
  paths.data  已固化: /var/lib/mecharion/apps/pg-main
              本次解析: /data1/apps/pg-main
  迁移数据目录是运维动作，请手工迁移后使用 mechctl component adopt-path 更新记录
```

若不固化，用户改了 `Node.Roots.data` 或 Pack 改了默认路径，已装组件会**静默搬家**，旧数据变成孤儿（[spec §8.7](../spec/pack-v1.md#87-路径的解析与固化)）。

#### ② `generations` —— 台账

`digest → seq` 的映射让 generation 具备**内容寻址的身份**：

- 同一份 spec 重复下发 → digest 相同 → 复用现有 generation，不产生 churn
- 回滚到历史版本 → digest 命中已保留的 generation → **直接切软链，秒级完成**

目录名仍是人类可读的 `0007-16.4-1`——**身份靠 digest，可读性靠序号**，两者不冲突。

| `state` | 含义 |
|---|---|
| `active` | `current` 软链指向它 |
| `retained` | 保留供回滚 |
| `failed` | 物化或健康检查失败，保留供诊断，不参与回滚 |

**保留策略**：默认保留 3 个（`retainGenerations`）。GC 时机是**新 generation 通过健康检查之后**——在那之前不能删任何东西，否则回滚就没有落脚点了。

#### ③ `appliedResources` —— 「已不再声明但仍存在」的依据

记录上次实际应用过的资源身份。本次 spec 中不再出现的，引擎**不自动清理**，而是上报（[ADR-0027 决策六](../adr/0027-resource-engine-contract.md)）：

```
$ mechctl component status hdfs-prod
已不再声明但仍存在 (2):
  sysctl   net.core.somaxconn     于 generation 0006 移除声明
  limits   hdfs nofile            于 generation 0006 移除声明
```

没有这份记录，残留就完全不可见。

#### ④ `removed` —— 卸载收据

非 nil 时，这条记录已经不是一个「装着的实例」，而是一张收据：
`generations` / `appliedResources` / `digests` 全部清空，机器上属于它的
只剩 `retainedPaths` 里那些目录。

```json
{
  "component": "pg-main", "role": "primary",
  "removed": {
    "at": "2026-08-08T12:10:29Z",
    "pack": "postgresql", "version": "16.4",
    "retainedPaths": ["/var/lib/mecharion/apps/pg-main"]
  }
}
```

**卸载完不能直接把状态文件删掉。** 数据目录默认保留（10-cli §4.3），而
保留却不留任何记录，等于在机器上制造一堆没人知道来历的目录——
「保留而不提供发现机制等于把问题推给未来」。`orphans` 就是靠这条记录
才列得出节点、路径与来历。

记下 `pack` / `version` 是必须的：一个只有路径的孤儿回答不了「这堆数据
是谁的、哪个版本写的」，而那正是决定「能不能删」时唯一要紧的问题。

**什么都没保留时不写这条记录**，整个状态文件直接删掉：一次干净的卸载
不该在机器上留下任何痕迹，包括这张收据——留着它，`orphans list` 里会
永远挂着一条指向空地址的记录。

### 2.4 `specs/<component>__<role>/<seq>.json`

每个保留中的 generation 对应的完整 spec。用途：

- **回滚**：切回旧 generation 时需要它的资源清单来重新应用配置
- **诊断**：`mechctl --local` 能展示「这个 generation 当时是什么样」
- **`config diff --from --to`**：对比两个 generation 的渲染结果

随 generation 一起 GC。

### 2.5 事件缓冲

```
events/2026-08-03.jsonl
```

**断连期间的缓冲**，重连上报给 mechd 后即可删除。它不承担长期查询职责——审计与事件的权威存储在 mechd 的 SQLite（[ADR-0026](../adr/0026-standalone-runs-mechd.md)）。

有上限（默认 64MB / 7 天），超限丢弃最旧记录并**记录丢弃计数**——静默丢事件比丢事件更糟。

---

## 3. 调和一次的完整流程

把前两节串起来：

```
① 收到 ResolvedSpec（gRPC 下发 / mechlet apply -f）
② 校验 schemaVersion 兼容性
③ 读 instances/<c>__<r>.json
④ 路径固化校验 —— 不一致则拒绝
⑤ digest 比对
     · 与 currentGeneration 相同 → 跳到 ⑪（仅做漂移检测）
     · 命中已保留的 generation   → 跳到 ⑨（回滚路径）
     · 全新                     → 继续
⑥ 分配新 seq，替换 {{ .Paths.Generation }} 占位符
⑦ 拉取缺失 blob（按 sha256，已有则跳过）
⑧ 物化：创建 paths 目录 → shared 资源 → role 资源 → Runtime.Materialize
⑨ preStop hook → Runtime.Stop → 原子切换 current 软链 → Runtime.Start
⑩ 健康检查（startupGrace 内）
     · 失败 → 切回上一 generation → 上报失败
     · 成功 → 标记新 generation 为 active，GC 超出保留数的旧 generation
⑪ 逐资源 Read → Diff → 按 driftPolicy 处理
⑫ notify 聚合执行
⑬ 写回 instances/<c>__<r>.json
⑭ 上报观测状态
```

**⑤ 是幂等的支点**：spec 没变就不物化、不切换、不重启，只做漂移检测。持续调和每 60 秒跑一次而服务岿然不动，靠的就是这一步。

**⑬ 在 ⑭ 之前**：先落盘再上报。反过来会出现「mechd 认为成功了，但 mechlet 崩溃后本地无记录」的不一致。

---

## 4. 版本兼容

`SchemaVersion` 独立于 Mecharion 的版本号。

| 情形 | 行为 |
|---|---|
| mechlet 收到**更高**的 schemaVersion | **拒绝并上报**，附带自己支持的版本。mechd 据此提示先升级 agent |
| mechlet 收到**更低**的 schemaVersion | 按旧结构解析（保留至少两个大版本的兼容） |
| 本地状态文件 schemaVersion 低于当前 | 启动时按需转换，转换前**备份原文件** |

方向是明确的：**控制面向后兼容 agent**（新 mechd 能服务旧 mechlet），反之不保证。这与「升级时先 mechd 后 mechlet」的顺序一致（[01-architecture §4.1](01-architecture.md#41-单机形态下的升级顺序)）。

---

## 5. 尚未定稿

| 议题 | 何时定 |
|---|---|
| gRPC service 定义（`.proto`）——下发、上报、心跳的消息与流控 | M3 |
| `SecretRefs` 的具体传递机制（mechd 侧存储 + 下发路径） | M3 |
| blob 拉取的并发与断点续传策略 | M3 |
| `mechlet apply -f` 是否作为长期命令保留，还是 M3 后隐藏为调试入口 | M3 |
