# 资源引擎

资源引擎是 mechlet 的核心：把「一份已解析的期望状态」变成机器上的实际状态。

设计依据见 [ADR-0027](../adr/0027-resource-engine-contract.md)。

---

## 1. 职责边界

```
┌─ 调和器 reconciler ────────────────────────────────┐
│  · 接收已解析规格（来自 mechd 的 gRPC 下发）          │
│  · generation 分配、物化、原子切换、回滚              │
│  · 编排七个阶段                                     │
│  · notify 聚合                                     │
│  ┌─ 资源引擎 ─────────────────────────┐            │
│  │  Read / Diff / Apply / Remove      │  ← 本文    │
│  │  16 种资源类型                      │            │
│  └────────────────────────────────────┘            │
│  ┌─ Runtime 接口 ──────────────────────┐           │
│  │  systemd │ docker │ compose         │           │
│  └─────────────────────────────────────┘           │
└────────────────────────────────────────────────────┘
```

**资源引擎不管 generation、不管 Runtime、不管 notify 的执行时机**——它只负责「让这一条资源变成期望的样子」。

---

## 2. 接口

```go
// Resource 是一条已渲染的期望状态 ＋ 它的行为。
//
// 实例本身承载期望值——因此各方法不需要再接收 desired 参数。
type Resource interface {
    // ID 是资源在一次调和中的稳定身份，用于状态追踪、notify 去重、
    // 以及「已不再声明但仍存在」的比对。
    // Pack 未显式声明 id 时由 type + 关键参数派生。
    ID() string

    // Type 返回资源类型名，如 "template"。
    Type() string

    // Read 探测当前实际状态。
    // 返回 error 表示「本环境不适用」等应当中止调和的情况；
    // 「本该能读但这次读不到」应返回 Observed{State: Unknown}。
    Read(ctx context.Context) (Observed, error)

    // Diff 比较期望与实际，返回字段级差异。纯描述性，不参与 Apply。
    Diff(observed Observed) []Change

    // Apply 让资源变成期望的样子。**必须幂等**——引擎不保证只在有差异时调用。
    Apply(ctx context.Context) error

    // Remove 在 component remove 时被调用。可为 no-op。
    Remove(ctx context.Context) error
}
```

### 2.1 `Observed` 与三态

```go
type State int

const (
    StateAbsent  State = iota // 资源不存在
    StatePresent              // 存在，且字段已读出
    StateUnknown              // 无法确定
)

type Observed struct {
    State  State
    Fields map[string]any // State==Present 时的实际字段值
    Reason string         // State==Unknown 时说明为什么读不到
}
```

| 状态 | 何时 | 引擎行为 |
|---|---|---|
| `Absent` | 文件不存在、用户不存在、unit 未安装 | 走创建路径 |
| `Present` | 读到了全部字段 | 与期望比对 |
| `Unknown` | NFS 挂死、systemd 未运行、`getent` 超时、权限不足 | **跳过该资源**，在 status 中单独归类，不计为「漂移」 |

> **「本环境不适用」不是 `Unknown`，是 `error`。**
> `sysctl vm.max_map_count` 在没有该键的内核上——那是用户选错了内核，应当报错让他知道。
> 没有这条判据，`Unknown` 会变成所有疑难情况的垃圾桶，从而失去信息量。

### 2.2 `Change`

```go
type ChangeKind int

const (
    KindScalar ChangeKind = iota // 短值：mode、uid、port
    KindText                     // 长文本：文件内容、unit 内容
    KindList                     // 列表：groups、addresses
)

type Change struct {
    Field string
    Want  string
    Got   string     // Observed.State==Absent 时为空
    Kind  ChangeKind // 决定呈现方式，不影响语义
}
```

**行级 diff 的计算与呈现在 CLI 层**，资源引擎不碰 diff 算法：

| Kind | CLI 呈现 |
|---|---|
| `Scalar` | `mode: 0644 → 0640` |
| `Text` | unified diff |
| `List` | 逐项增删标记 |

Web UI 拿到同一份 `[]Change` 换一种呈现。

### 2.3 错误分类

```go
type ErrorClass int

const (
    ErrTransient ErrorClass = iota // 可重试：超时、临时 IO 错误、资源忙
    ErrPermanent                   // 不可重试：权限不足、路径非法、内核不支持
)
```

Rollout 据此决定「重试本批」还是「直接暂停」——把网络抖动和配置错误当成同一回事，会让运维在两种完全不同的故障上采取相同的（错误的）动作。

---

## 3. 调和的七个阶段

```
① 解析并创建 paths 声明的目录        按声明的 owner / group / mode
② shared.resources                  按声明顺序 Read → Diff → Apply
③ role.resources                    按声明顺序 Read → Diff → Apply
④ Runtime.Materialize
⑤ notify 聚合执行
⑥ 健康检查
⑦ 上报观测状态
```

**② 早于 ③ 是必要的**：`shared` 里通常有建用户与解压载荷，角色资源依赖它们。

**① 早于 ② 是必要的**：`paths` 声明的目录（含 `kind: multi` 的每块盘）必须先存在，资源才能往里写。这也是 Pack 不需要为这些路径写 `directory` 资源的原因（[spec §8.3](../spec/pack-v1.md#83-字段)）。

### 3.1 阶段内的失败处理

```
某资源 Apply 失败
  → 标记失败，**同阶段后续资源不再执行**（可能有隐含依赖）
  → 已执行的资源不回滚
  → 上抛给调和器：升级场景切回上一 generation；首次安装场景保持现状并上报
```

**资源级无事务是刻意的**——文件系统操作本来就没有事务，假装有会导致「回滚到一半又失败」这种更难诊断的状态。回滚能力来自 generation 的不可变与原子软链切换（[ADR-0008](../adr/0008-immutable-generation-linkinto.md)）。

### 3.2 三个独立的周期

它们经常被混为一谈，必须分清：

| | 默认 | 作用 | 谁在跑 |
|---|---|---|---|
| **调和间隔** | **60s** | 走完整个七阶段 | mechlet 本地 |
| **健康检查间隔** | **15s** | 只做阶段 ⑥ 的探测 | mechlet 本地 |
| **状态上报（心跳）间隔** | **15s** | 把观测状态推给 mechd | mechlet → mechd |

三者独立可配。调和要重读一整套资源，因此间隔明显更长；心跳只发一个小结构，与健康检查同频即可。

> [ADR-0012](../adr/0012-mechd-embedded-sqlite.md) 中「1000 节点 × 15 秒心跳 ≈ 66 写/秒」指的是**第三项**，与调和间隔无关。

---

## 4. 资源类型清单

16 种类型，每种给出 `Read` 读什么、`Diff` 比哪些字段、`Apply` 怎么做。

> 本文描述的是**目标设计**，实现进度见 [25-roadmap](25-roadmap.md#m2-交付了什么)。
> 尚未实现的类型在构造资源时会明确报「尚未实现」，与「类型名拼错」区分开——
> 前者用户改一个字母就好，后者只能等版本。

### 4.1 文件系统

#### `file` / `template`

两者的差别只在内容来源：`file` 来自 `content` / `source` / `blob`，`template` 来自渲染。

| | |
|---|---|
| **Read** | `stat` → 不存在即 `Absent`；存在则读 mode/uid/gid，内容取 **sha256** 而非全文 |
| **Diff 字段** | `content`(Text) · `mode`(Scalar) · `owner`(Scalar) · `group`(Scalar) |
| **Apply** | 写临时文件 → `chown`/`chmod` → **`rename` 原子替换** |
| **Remove** | 删除文件 |

**内容比较用 sha256 而非全文**：一个 200MB 的文件不该被读进内存做字符串比较。但 `Diff` 需要 `Want`/`Got` 供 CLI 做行级 diff——因此**仅在检测到 sha256 不同、且文件小于阈值（默认 1MB）时才读取全文**；超过阈值只报「内容不同（sha256 a1b2… → c3d4…）」。

#### `directory`

| | |
|---|---|
| **Read** | `stat` |
| **Diff 字段** | `mode` · `owner` · `group` |
| **Apply** | `MkdirAll` + `chown` + `chmod` |
| **Remove** | **仅在为空时删除**——非空目录可能含用户数据 |

#### `symlink`

| | |
|---|---|
| **Read** | `lstat` + `readlink` |
| **Diff 字段** | `target` |
| **Apply** | 已存在且 `force` 时先删再建；建软链本身用「建临时名 + rename」保证原子 |

#### `archive`

解开归档到目标目录。**幂等判定是这里唯一的难点**——怎么知道一个归档已经解开过？

| | |
|---|---|
| **Read** | 检查 `<dest>/.mecharion-archive`，其中记录已解开的 blob sha256 |
| **Diff 字段** | `blob`（已解开的摘要 vs 期望的摘要） |
| **Apply** | 解开到 dest → 写入标记文件 |
| **Remove** | no-op —— 它通常解在 generation 目录内，随 generation 一并回收 |

用标记文件而非「dest 非空」判定，是为了能检测出 **blob 变了但路径没变** 的情况。

> archive 的目标通常是 generation 目录，而 generation 是不可变的——因此实际上一个 generation 内它只会执行一次。标记文件主要服务于中断后重试。

### 4.2 身份

#### `user` / `group`

| | |
|---|---|
| **Read** | `getent passwd` / `getent group`；命令超时或 nsswitch 异常 → **`Unknown`** |
| **Diff 字段** | `uid`/`gid` · `primaryGroup` · `groups`(List) · `home` · `shell` |
| **Apply** | 不存在则 `useradd`/`groupadd`；存在则 `usermod`/`groupmod` 收敛差异字段 |
| **Remove** | **no-op** |

`Remove` 是 no-op 的理由：系统上可能有大量文件以该 uid 为属主，删掉用户会让它们变成孤儿 uid，而下次某个新用户复用该 uid 时会**意外获得这些文件的所有权**。这是真实的安全事故模式。

由 `component remove --purge-user` 显式触发时才删除（[spec §14.3](../spec/pack-v1.md#143-身份)）。

### 4.3 主机配置

#### `sysctl`

**这类资源有两个状态**，都必须一致：

| | |
|---|---|
| **Read** | 运行期值读 `/proc/sys/<key>`；持久化值读 `/etc/sysctl.d/99-mecharion-<component>.conf`。**key 在本内核不存在 → `error` 而非 `Unknown`** |
| **Diff 字段** | `runtime`(Scalar) · `persisted`(Scalar) |
| **Apply** | `sysctl -w` 使运行期生效 **＋** 写持久化文件 |
| **Remove** | 删除本 Component 的持久化文件；**运行期值不回滚**（不知道系统原本的值） |

「改了配置但没重启，以为生效了」是运维事故的常见来源。把两个状态都建模，让 `diff` 能直接指出「持久化了但运行期未生效」。

#### `limits`

| | |
|---|---|
| **Read** | 解析 `/etc/security/limits.d/99-mecharion-<component>.conf` |
| **Diff 字段** | `soft` · `hard` |
| **Apply** | 写文件 |
| **Remove** | 删除该文件 |

**无运行期状态**——`limits` 只对新登录会话生效，已运行的进程不受影响。文档需说明这一点，否则用户会奇怪为什么改了没效果。

#### `hosts_entry`

| | |
|---|---|
| **Read** | 解析 `/etc/hosts` 中由标记注释界定的托管区块 |
| **Diff 字段** | `hostnames`(List) |
| **Apply** | 只重写托管区块，**区块外的内容一字不动** |
| **Remove** | 移除本 Component 的条目；区块为空时连标记一并移除 |

```
# BEGIN mecharion:hdfs-prod
10.0.1.10  nn1.internal
10.0.1.11  nn2.internal
# END mecharion:hdfs-prod
```

`/etc/hosts` 常有用户手工维护的内容，**必须只碰自己的区块**。

#### `mount`

| | |
|---|---|
| **Read** | `/proc/mounts`（运行期）＋ `/etc/fstab`（持久化） |
| **Diff 字段** | `mounted`(Scalar) · `fstab`(Scalar) |
| **Apply** | 未挂载则 `mount`；`persistent: true` 时写 fstab |
| **Remove** | **只移除 fstab 条目，不 `umount`** |

**不自动 `umount`** 是刻意的：卸载一个正在被使用的挂载点会直接中断服务。移除时上报「fstab 已清理，下次重启后生效；如需立即卸载请手工执行」。

#### `timer`

systemd timer，实际生成 `.timer` + `.service` 两个 unit。

| | |
|---|---|
| **Read** | 两个 unit 文件的内容 + `systemctl is-enabled/is-active <name>.timer` |
| **Diff 字段** | `serviceContent` · `timerContent` · `enabled` · `active` |
| **Apply** | 写两个 unit → `daemon-reload` → `enable --now` |
| **Remove** | `disable --now` → 删两个 unit → `daemon-reload` |

#### `systemd_unit`

| | |
|---|---|
| **Read** | unit 文件内容 + `is-enabled` + `is-active`；**systemd 未运行（容器无 init）→ `Unknown`** |
| **Diff 字段** | `content`(Text) · `enabled` · `state` |
| **Apply** | 写 unit → `daemon-reload` → 按 `enabled`/`state` 调整 |
| **Remove** | `disable --now` → 删 unit → `daemon-reload` |

> 与 `workload.runtime: systemd` 的分野见 [spec §14.4](../spec/pack-v1.md#144-主机配置)：`systemd_unit` 是「一个 unit 应当存在并处于某状态」，`workload` 是「这个角色的受监管进程」。

### 4.4 逃生舱

#### `command` / `script`

**这类资源没有可观测的状态**——它的「实际状态」只能由守卫表达。这是逃生舱的固有代价，也是 [spec 规则 31](../spec/pack-v1.md#19-校验规则汇总) 强制要求守卫的原因。

| | |
|---|---|
| **Read** | 执行守卫：`creates` 路径存在 / `unless` 退出 0 / `onlyif` 退出非 0 → `Present`（无需执行）；否则 `Absent` |
| **Diff 字段** | `satisfied`(Scalar) —— 只有布尔 |
| **Apply** | 执行命令 |
| **Remove** | **no-op** —— 引擎不知道如何撤销一条任意命令 |

守卫本身执行失败（命令不存在、超时）→ `Unknown`，不是 `Absent`。否则一个写错的守卫会导致命令被反复执行。

### 4.5 非 hermetic

#### `package`

| | |
|---|---|
| **Read** | 调用发行版包管理器查询 |
| **Diff 字段** | `installed` · `version` |
| **Apply** | 安装/升级 |
| **Remove** | **no-op** —— 卸载可能影响其他组件 |

**被 `mechpack lint --hermetic` 拦截**（[spec §17](../spec/pack-v1.md#17-hermetic-规则)），官方 Pack 不得使用。提供它只是为了有 repo 的环境。

---

## 5. 「已不再声明但仍存在」

Pack 升级后某个资源从声明中消失（比如去掉了一条 `sysctl`），引擎**不做自动清理**，而是记录并上报。

理由：**多数资源没有可靠的「反向」**——`sysctl` 该恢复成什么？引擎不知道系统原本的值。猜错比留下更糟。

```
$ mechctl component status hdfs-prod
…
已不再声明但仍存在 (2):
  sysctl   net.core.somaxconn = 32768     于 0006 移除声明
  limits   hdfs nofile 65536              于 0006 移除声明
  → 这些资源不再被管理。如需清除，手工处理或用 mechctl component remove 重建
```

这与 `driftPolicy: report` 的哲学一致：**先让人知道，由人决定**。Puppet 与 Chef 同样采取「不管理的资源不触碰」，我们在此基础上补上上报，让残留可见。

**真正的清理路径是 `component remove`**——那时按逆序调用各资源的 `Remove()`，语义明确、由用户显式发起。

---

## 6. 对实现者的要求

每种资源类型的测试**必须包含**：

| 用例 | 断言 |
|---|---|
| 从零 Apply | 资源被正确创建 |
| **连续 Apply 两次** | **第二次无副作用**（幂等契约由测试保证，编译器不检查） |
| Apply 后 Read → Diff | 返回空 |
| 手工破坏某个字段后 Diff | 只报该字段，不误报其他字段 |
| 读不到时 | 返回 `Unknown` 而非 `Absent` |
| Remove 后 Read | 返回 `Absent`（no-op 的类型除外） |

第二条是重中之重：`Apply` 的幂等是**实现者的责任**，接口签名不强制它。一个非幂等的资源类型会在持续调和下反复产生副作用，而这类 bug 在单次 apply 的测试里发现不了。

---

## 7. 相关决策

- [ADR-0027 资源引擎的接口契约](../adr/0027-resource-engine-contract.md)
- [ADR-0008 generation 不可变，用 linkInto 调和应用原生布局](../adr/0008-immutable-generation-linkinto.md)
- [ADR-0010 Runtime 抽象及其接缝位置](../adr/0010-runtime-abstraction.md)
- [06-state-and-drift 状态管理、漂移与升级](06-state-and-drift.md)
