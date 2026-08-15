# 25. 补齐生命周期的另一半（M9）

> 目标：**部署出来的东西删得掉**，以及「文档里写的命令都真的存在」。

## 0. 现状：只有一半的生命周期

M8 收尾审查逐条核对了「文档承诺的」与「代码里有的」。结论集中在一件事上：

| 能做 | 不能做 |
|---|---|
| 部署、升级、回滚、改配置、停止、启动 | **删除** |

`mechctl component remove` 在 [10-cli §4.3](10-cli.md) 有完整的参数与执行
阶段设计——`--purge-data` / `--keep-config` / `--purge-user` / `--force` /
`--ignore-not-found`，还有一整节讲「默认删什么、默认留什么」，并称它是
「**整个工具里最危险的操作**」。

**CLI 与 HTTP API 都没有它。**

一个应用生命周期管理工具删不掉自己部署的东西，意味着：部署错了只能手工
登录每台机器清理——而那正是这个工具存在的理由。

其余缺口见 [10-roadmap「M9：补齐生命周期的另一半」](25-roadmap.md)。

## 1. 已经定过的，不重做

| 决定 | 出处 |
|---|---|
| 孤儿实例**永不自动删** | [20-continuous-reconcile §2.4](20-continuous-reconcile.md) |
| 数据目录默认保留，与「升级永不触碰数据目录」同源 | [10-cli §4.3](10-cli.md) |
| remove 走二档确认（输入 Component 名），`-y` 不能跳过 | 同上 |
| mechlet 不做判断，只按下发的规格调和 | [ADR-0002](../adr/0002-mechlet-as-sole-engine.md) |
| 期望状态驱动，不发命令 | [ADR-0001](../adr/0001-agent-based.md) · [ADR-0029](../adr/0029-push-over-server-stream.md) |

最后一条决定了 remove 的形态，见 §2.1。

## 2. 要定的问题

### 2.1 「卸载」怎么告诉节点

这是 M9 最核心的一个决定。

**调研过的做法：**

| 系统 | 机制 | 代价 |
|---|---|---|
| Kubernetes | 对象打 `deletionTimestamp`，控制器看到后走 finalizer，全部完成才真删对象 | 两阶段；但「删除是一种状态」这个形态被反复验证过 |
| Ansible | `state: absent` —— 与 `state: present` 同一个字段 | 无状态机，每次都是一次性执行 |
| Puppet | `ensure => absent` | 同上 |
| Chef | `action :remove` | **命令式**，丢了就丢了 |
| Salt | `pkg.removed` 状态 | 同 Ansible |

**共同点**：几乎所有做「期望状态」的系统都把删除表达成**状态的一个值**，
而不是一条命令。只有命令式的那些用动作。

**决定：`runState` 加第三个值 `removed`。**

`spec.ResolvedSpec.RunState` 已经存在，取值 `running` | `stopped`，
mechlet 已经在按它调和。加一个值是最小的改动，而且它落在正确的概念上
——**「这个实例不该存在」与「这个实例不该在跑」是同一类陈述**。

```
running  → 应该装着，且在跑
stopped  → 应该装着，但不该在跑
removed  → 不该装着          ← 新增
```

**叫 `removed` 而不是 `absent`**：Ansible / Puppet 那一系用的是
`absent` / `present`，但这里已有的两个值是 `running` / `stopped`——
同一种构词（过去分词）。混进一个 `absent` 会让这组值读起来不像一套。

> 代价照实说：`removed` 有一点过去时的味道，容易被读成「已经删掉了」
> 而不是「应该删掉」。这个字段的名字（`RunState`，**期望**运行态）
> 与它的三个兄弟值一起消解了这个歧义，但注释里要写明白。

**为什么不是新开一个 RPC**：一条「请卸载」的指令是**事件**，丢一次就永远
丢了；而这个项目从 M4 起就在守「状态可以重复确认」这条纪律
（[20-continuous-reconcile](20-continuous-reconcile.md)）。节点断连三天
回来之后，它收到的仍然是「这个实例不该存在」，照做即可——不需要 mechd
记着「我还欠它一条卸载指令」。

**顺带的好处：协议一个字节都不用改。** ResolvedSpec 在 gRPC 里是一段
opaque 的 `spec_json`（[17-protocol](17-protocol.md)），往里加字段天然
向后兼容——旧 mechlet 忽略 `removal`，并把不认识的 `runState` 按
running 处理（见 §2.1 的兜底方向）。**不需要版本协商，也不需要升版本号。**

代价是这种兼容是「静默」的：一个没升级的旧 mechlet 收到卸载意图之后
**什么也不会做，也不会报错**——它只是让服务继续跑着。组件因此卡在
`removing`，需要人去看为什么。这比反过来（旧 mechlet 误删）好得多，
但它意味着 `removing` 迟迟不结束时，「节点上的 mechlet 版本太老」
必须是排查清单上的一条。

### 2.2 组件记录什么时候真删

`removed` 只是**期望**。记录不能在下发的那一刻就删掉——删了之后节点就
再也收不到那条期望了（它不在下发里 = 孤儿，而孤儿永不自动删）。

因此 Component 多一个生命周期状态：

```
active ──remove──> removing ──全部实例报告已卸载──> （记录删除）
                      │
                      └─ --force ──> （记录删除，未完成的登记为孤儿）
```

`removing` 期间：

- 下发照常，但那些实例的 `runState` 是 `removed`
- `component list` 里仍然看得到它，标注「正在移除」
- **不接受任何其它写操作**（改配置、升级都拒绝）——一个正在被删的东西
  不该还能改

**再敲一次 `remove` 只能用来推进，不能改开关。**

`--force` 放行（那是失联节点唯一的出路），`--purge-data` /
`--keep-config` / `--purge-user` 一律忽略并提示。理由是这三个开关
**是逐节点生效的**：改到一半会得到一个「一半节点删了数据、一半留着」
的集群，而那种不一致事后几乎无法排查——运维看到的是同一个组件在不同
机器上留下了不同的东西，却没有任何记录说明为什么。

```
$ mechctl component remove pg-main --purge-data
⚠ pg-main 正在移除中（3/5 已完成）
  --purge-data 已忽略：开关在第一次 remove 时就定下了
  已完成的 3 个节点保留了数据，现在改会造成不一致

  · 推进：      --force（跳过失联节点）
  · 清掉数据：  等删完后 mechctl orphans purge
```

代价照实说：**「我刚才忘了加 --purge-data」没有捷径**，只能等它删完再
用 `orphans purge` 清一遍。多一步，但那一步是可见、可逐条确认的，
比一个状态不一致的集群好。

**孤儿不需要额外的登记机制。** 记录一删，那些实例就不在下发里了，
节点侧现成的 `refreshOrphans` 会把留下的收据报上来。因此 `--force`
的实现就是「现在就删记录」，不必为它单写一条登记逻辑——失联的那台机器
重新上线时会自己把残留报出来。

### 2.3 节点侧卸载的顺序

10-cli 已经定了：

```
preStop → Stop → postStop → preRemove → Runtime.Remove
        → 删 generation / config → 数据按开关处理 → postRemove
```

**postStop 是实施时加进来的**，10-cli 原来的顺序里没有它。理由是别处的
「停」一律是 preStop → Stop → postStop 三件套；卸载少一个，就会出现
「升级时 postStop 跑、卸载时不跑」这类只在特定路径上存在的差异——那正是
最难被测出来的一类。一个在 postStop 里释放共享锁的 Pack，会在卸载时把锁
漏在那儿。

节点侧现有的零件：`Runtime.Remove`（systemd / docker / compose 三个都有，
停 unit 或容器并清掉痕迹）、hook 执行器、generation 目录管理。
**缺的是把它们串起来的那一段**，以及「删哪些目录」的判断。

两个实施时才浮现的约束：

- **不能用 `Materialize` 拿 `Ref`。** `Runtime.Remove` 要一个 `Ref`，而
  此前只有 `Materialize` 产得出来——走它意味着拆之前先装一遍：写 unit
  文件、`docker load` 一个几百 MB 的镜像，然后立刻删掉。不只是浪费，
  中间任何一步失败都会把「删除」变成「装了一半」。因此
  `runtime.Runtime` 加了一个纯函数 `RefFor`（只推名字，不碰机器）。
- **postRemove 的 cwd 不能是 generation 目录。** 那个目录在上一步刚被
  删掉，而 hook 的 cwd 约定就是它（18-hooks §3）。cwd 不存在时 Go 的
  fork/exec 报的是「脚本不存在」——指向脚本，而脚本明明在 Pack 根下。
  因此 postRemove 单独用一个 cwd 为空的执行器。

**数据目录的开关必须随规格下发**，不能让 mechlet 自己决定——
mechlet 不做判断（ADR-0006）。因此 `removed` 之外还要带上「数据怎么办」。

**哪个路径算「数据」：按预定义名约定，不给 Pack 覆盖的余地。**

`paths` 的名字是 Pack 起的，预定义名只有五个，而 HDFS 那样的包会用
`dataDirs` 这类自定义名。归类判据写死在引擎里：

| paths 名 | remove 时 |
|---|---|
| `home`（含 generations） | 删 |
| `runtime` | 删 |
| `config` | 删，`--keep-config` 保留 |
| `data` / `logs` | 保留，`--purge-data` 才删 |
| **其它一切（自定义名）** | **保留**，`--purge-data` 才删 |

**未归类的默认是保留**，方向是刻意选的：删错不可逆，留错可以用
`orphans purge` 补救。HDFS 的 `dataDirs` 正好落在这一行上。

> 调研过的替代方案：给 `paths` 加一个 `onRemove: purge|config|data`
> 字段，让 Pack 自己声明。**没有采纳**——pack-v1 现在是 draft-stable，
> 不为目前两个示例包都用不上的字段扩张格式。
>
> 代价照实说：Pack 因此**无法声明「这个自定义目录是可丢的」**。一个
> 自定义 `cache` 目录每次卸载都会留一个孤儿，只能手工 `orphans purge`。
> 真出现这个需求时再加字段——加一个带安全默认值的可选字段是兼容的，
> 反过来（先加了再想改语义）才不是。

**归类只认精确的名字**，`conf` / `etc` / `configs` 都不算 `config`。
第 3 步做影响面时才发现，mechd 的 `paramkit` 夹具正是把配置目录起名叫
`conf` 的——于是它的配置目录被当成数据保留了下来。

模糊匹配（`conf` ≈ `config`）**没有采纳**：归类会因此变得不可预测，
而这里唯一不能接受的是「猜错了方向去删东西」。

> 由此产生一条 Pack 作者必须知道的约束：**想让配置目录随卸载一起消失，
> 就必须把它命名为 `config`**。目前这条只写在文档里，没有任何工具会
> 提醒他——`mechpack lint` 加一条提示是自然的去处，留给第 7 步。

### 第 3 步的交付物

| 位置 | 内容 |
|---|---|
| `internal/spec/removal.go` | 归类表移到 spec，两端共用 |
| `internal/mechd/removecomponent.go` | `RemoveComponent` + 影响面 + 依赖检查 |
| `internal/mechd/httpapi.go` | `DELETE /components/{name}`，服务端也验确认串 |
| `internal/cli/ctlcmd/remove.go` | CLI，先干跑再确认 |
| `internal/store` | `ListDependents`（数字答不了「是哪几个」） |
| `test/multinode/remove_linux_test.go` | 三台真机的整条链路验收 |

**两处安全上的更正**，都是照 10-cli §7 那张表做的：

1. **`-y` 不再能跳过「输名字」这一档。** 原实现是 `if yes { return nil }`，
   与 §7 明写的「-y 不能全部跳过」直接冲突。两者挡的根本不是同一种错误：
   `-y` 挡「手滑敲了回车」，输名字挡「删错了对象」。
2. **`--purge-data` 要第三档确认。** §7 的表写的是「组件名 ＋ 单独确认
   删除数据」——删进程与配置可以重新部署回来，删数据不能。

> 改完之后 `node remove` 的确认也跟着重排了一次：**`--force` 才要求
> 输名字**（它会连着实例一起抹掉），不带 `--force` 时仍是 y/N。
> 原来的实现对两种情况一视同仁地要名字，比 §7 的表更严——而更严的那一版
> 一旦 `-y` 失效，就会让脚本里的 `node remove -y` 静默失败。
>
> 这个连锁反应是在真机验收里发现的：多节点夹具的清理步骤正是
> `node remove -y`，它失败之后被一句「（忽略）」吞掉了。**夹具的失败
> 被吞掉最难发现**——测试照样绿，只是前提悄悄不成立了。

### 2.4 保留的数据要能被发现

数据目录默认保留。10-cli 明写：

> 保留而不提供发现机制等于把问题推给未来。

因此 `orphans` 与 remove 是**同一步**的两半，不能只做前一半：

```bash
mechctl orphans list [--node N]
mechctl orphans purge <id> [-y]
```

节点侧的孤儿上报**已经有了**（`SetOrphans`，每次上报整体替换），
`node list` 里也显示了。缺的是列举与清理的动词。

**purge 怎么送达节点：下发「该清掉的孤儿键」，节点删自己收据里的路径。**

协议里没有任何命令型 RPC——`Assignment` 是纯状态，`cordoned` 那一段
明写「状态不是动词」（ADR-0029）。purge 因此也做成状态：

```
Assignment { purgeOrphans: ["pg-main__primary"] }

节点侧：收据在 → 删 RetainedPaths → 删收据
        收据不在 → 什么也不做
```

**关键的安全性在于删的是节点自己记的路径，不是中心指定的路径。** 考虑
这个时序：purge 下发时节点失联，其间运维用同名重新部署了组件，节点回来
之后收到那条 purge。若下发的是绝对路径，它会把**新数据**删掉——而那正是
本项目反复在防的那类事故。下发实例键则不会：那时本地状态已经是一个真实
实例而不是收据，purge 直接空跑。

它还是**自限的**：收据一删，孤儿上报里就没有它了，mechd 跟着丢掉 purge 项。
不需要任何确认序号或一次性标记。

**不算目录大小，只列路径。**

§2.4 原来写的是「含节点、路径、大小」。大小**没有做**：算它要 walk 整个
目录树，而孤儿恰恰常常是几 TB、几百万文件的数据目录，且会在那儿放好
几个月。上报默认 15 秒一次，为一个几乎不变的数字反复全盘扫描不划算。

> 代价照实说：**「这堆数据值不值得留」是人在决定要不要 purge 时第一个
> 想知道的**，而现在他得自己登上那台机器 `du` 一遍。这是一次明确的
> 取舍，不是遗漏。真需要时的去处是「按需算一次」而不是「随上报算」——
> 那要一条新的请求路径，届时再说。

### 第 4 步的交付物

| 位置 | 内容 |
|---|---|
| 迁移 `00019` | `node_orphans` 加 `paths` 与 `purge_requested_at` |
| `agent.proto` | `OrphanRecord`（上报侧）与 `Assignment.purge_orphans`（下发侧） |
| `internal/agent/purge.go` | 节点侧执行：删自己收据里的路径 |
| `internal/mechd/orphans.go` | 列举、记录 purge 意图、实现 `protocol.Purger` |
| `internal/cli/ctlcmd/orphans.go` | `orphans list` / `purge` |

**孤儿分两类，列表上就要分得开**：

```
有路径   remove 留下的数据残留 —— purge 就是删那几个目录
无路径   下发里没有它了，但机器上还装着、可能还在跑
```

后者 `purge` 解决不了（它只删目录，停不掉进程），因此服务端直接拒绝并
说明原因。混为一谈会让人以为问题已经解决了。

> `Assignment` 加字段时，仓库里那条 `TestAssignmentHasNoVerbs` 当场
> 拦了下来，要求说明新字段不是动词。`purge_orphans` 通过这条判据：
> 它说的是「这些孤儿不该存在」，每轮重复下发、节点每次照做一遍、
> 清完之后自动失效。**这条守卫值回票价**——它逼着把理由写在了测试里。

> **真机验收逮到一条单元测试看不见的缺陷。** 组件记录删掉之后，mechd
> 没有唤醒那些节点——于是它们还攥着「runState: removed」的期望，每
> 60 秒重新卸载一遍一个早就拆干净的实例，而 `orphans list` 永远是空的。
>
> 症状完全**沉默**：没有任何错误，中心看起来一切正常，只是保留下来的
> 数据永远无人认领。这一层的单元测试当时全绿，因为它们验的是
> 「记录没了」，而没有一条问过「节点知不知道」。
>
> 这正是「容器里跑通才算数」那条纪律要挡的东西：第 1–3 步各自的测试
> 都绿，接缝却是坏的。补了两条唤醒测试之后，同类遗漏下次不必再靠
> 三台真机来发现。

### 2.5 `apply -f` 的输入是什么

10-cli 称它是「声明式主干的入口」，接受一份可能同时含 Site、Component、
ConfigGroup 的文件。

**不能复用 `render -f plan.yaml` 的格式**：那份是解析管线的**入参**
（实例已放置、ordinal 已分配、依赖已绑定），是低一层的东西。`apply` 要的
是「我想要什么」，放置由 mechd 算。

这需要定一个新 schema。**它是 M9 里唯一的新格式**，因此值得单独想清楚：
写错的格式会变成一批存量文件。

**决定：单文件、按名词分段。** 顶层三个键 `site` / `components` /
`configGroups`，字段名与 `component deploy` 的参数一一对应。

```yaml
site:
  name: prod
  kind: cluster

components:
  - name: pg-main
    pack: postgresql
    version: "16.4"          # 可省 = 本地最新
    profile: ha
    roles:
      primary: [n1]
      replica: [n2, n3]
    set:
      max_connections: 200
    setFile:                 # secret 类型只能走这里
      admin_password: /run/secrets/pg
    require:
      zk: zk-prod

configGroups:
  - component: pg-main
    role: replica
    name: ssd
    members: [n2]
    params: { shared_buffers: 8GB }
```

**字段名与 CLI 参数对齐**是这个形状最要紧的地方：`roles` / `set` /
`setFile` / `require` / `profile` 都是 `deploy` 已有的概念，学一遍就够。

> 调研过的另外两种：
>
> - **K8s 风格的多文档**（`---` 分隔、kind/metadata/spec）。**没有采纳**：
>   多一层 spec 嵌套，而且会招来「那 apiVersion 呢」「那 status 呢」
>   一整套 K8s 期待——而这个项目刻意不是 K8s（ADR-0017）。
> - **复用 `render -f plan.yaml`**。**不行**：那份是解析管线的入参，
>   实例已放置、ordinal 已分配、依赖已绑定。用它等于让人手写本该自动
>   算出来的东西。
>
> 代价照实说：**一个大集群的文件会很长，且不能拆开管**——想分文件就得
> 自己拼。K8s 那套的目录递归在这一点上更好。等真的有人被单文件烦到时
> 再加「多文件/目录」的入口，那是加法，不是改格式。

**多余的组件：不删，但列出来提醒。**

§4 第 14 条定死了 `apply` 不做删除。但差异也不能隐形：

```
⚠ 以下组件在集群里但不在这份文件里，**未被动过**：
    kafka-old（2 个实例）
  声明式不等于「文件里没有就删」。
  确要删：mechctl component remove kafka-old
```

> **没有做 `--prune`。** 它会把一条命令变成批量删除器——一份写漏了的
> 文件加上 `--prune` 会一次删掉多个组件，而 remove 那套二档确认在这里
> 很难逐个施加。删除保持单条、显式、可确认。

## 3. 实施顺序

| # | 内容 | 可验证的成果 | 状态 |
|---|---|---|:--:|
| 1 | `runState: removed` 下发 + mechlet 卸载编排 | 一个实例收到 removed 之后真的被卸载干净 | ✅ |
| 2 | Component 的 `removing` 状态 + 记录清理 | 全部实例卸载完之后记录才消失 | ✅ |
| 3 | `mechctl component remove` + 二档确认 + 影响面 | 10-cli §4.3 那张表逐行成立 | ✅ |
| 4 | `orphans list` / `purge` | 保留的数据目录找得到、清得掉 | ✅ |
| 5 | UI 的删除入口 | 与 CLI 同一套确认语义 | ✅ |
| 6 | `apply -f` | 一份声明文件能把集群带到那个状态 | ✅ |
| 7 | 补齐或移除其余文档动词 | `mechctl --help` 与 10-cli 一一对应 | ✅ |
| 8 | 端到端验收 | 见 §4 | ✅ |

**第 1 步先行**：它是唯一一处动节点侧的改动，后面全都依赖它。
反过来做（先写 CLI）会得到一个「能敲但什么也不会发生」的命令。

> 这句原来写的是「唯一一处动**协议**与节点侧的改动」。做完发现协议
> 一个字节都没动——ResolvedSpec 在 gRPC 里是 opaque 的 `spec_json`，
> 往里加字段不需要改 proto、不需要版本协商。留着这处更正，是因为
> 「以为要动协议」正是当初把它排在第一位的理由之一。

**第 4 步不能推迟到第 7 步**：数据目录默认保留是第 3 步就生效的行为，
而保留却发现不了等于制造垃圾。

### 第 1 步的交付物

| 位置 | 内容 |
|---|---|
| `internal/spec` | `RunStateRemoved`、`Removal`、`WantsRemoved()`；两者都不进 digest |
| `internal/runtime` | 接口加纯函数 `RefFor`（systemd / docker / compose 三处实现） |
| `internal/reconcile/remove.go` | 卸载编排 + 路径归类表 |
| `internal/state` | `Instance.Removed` 卸载收据 |
| `internal/resource` | `IdentityName`，供 `--purge-user` 取名字 |

验证：`internal/reconcile` 17 条单元测试 + `test/e2e` 4 条真机验收
（真 systemd、真 unit、真目录）。

**通读自己写的代码**又找出一条测试没覆盖的缺陷：一个从没
装成功过的实例收到 `removed` 时，会把规格里那些**从没被建出来的目录**
登记成保留下来的孤儿。原来的测试只断言了「不报错」，因此全绿。修法是
处置前先 `Lstat`：不存在的目录，保留侧与删除侧都不记。

**mechd 侧还发不出 `removed`**：目前只有 `mechlet apply -f` 这条调试入口
喂得进去。把它接到 `component remove` 上是第 2、3 步的事。

### 第 2 步的交付物

| 位置 | 内容 |
|---|---|
| 迁移 `00018` | `components` 加 `state` / 三个开关 / `removing_at`；`instance_status` 加 `removed` / `retained_paths` |
| `agent.proto` | `InstanceStatus` 加 `removed` 与 `retained_paths`（**这一步真的动了 proto**） |
| `internal/mechd/backend.go` | 下发时用组件状态盖掉逐实例运行态；上报时驱动收尾 |
| `internal/mechd/removal.go` | 进度统计、记录清理、写操作闸门 |

**为什么另给一个信号，不复用 digest**：Rollout 判收敛用的是「上报的 digest
== 期望的 digest」，而 `RunState` 不参与 digest——一个拆完的实例与一个装着
的实例上报的 digest 一模一样。那套判据在卸载路径上完全失效。

**闸门加在「取记录」上，不是逐个动词写 if**：写动词有十一个，漏掉任何一处
都会开出一条绕过闸门的路，而漏掉的那一处不会有任何症状，直到有人真的用它。
让 `componentForWrite` 本身带闸门，新加的写动词就只能默认是安全的。
读路径（`ListGroups`）走另一个函数——读一个正在被删的组件完全正当，
运维正想看它还剩什么。

> **实施时踩到的一个静默损坏**：sqlc 1.31 会把 `SET state = 'removing'`
> 里的**引号吃掉**，变成 `SET state = removing`（一个列引用），而且
> 退出码是 0。症状要到运行期才现形（`no such column: removing`）。
> 因此状态走绑定参数，不写字面量。这与 `queries/*.sql` 只能是 ASCII
> 是同一类问题——生成器在这个文件上会静默出错，判据只能是「生成出来的
> SQL 长什么样」。

> **另一处**：`make proto` 从**第一个 commit 起就跑不起来**——go.mod 里
> 从来没有 `tool` 段，而 `hack/protogen` 靠 `go tool` 调两个插件。CI 里的
> `make proto-check` 因此一直是失败的。已补上 `tool` 段（版本对齐已提交
> 的生成物：protoc-gen-go-grpc v1.6.1）。

### 第 5 步的交付物

| 位置 | 内容 |
|---|---|
| `ComponentActions.vue` | 移除对话框：先干跑算影响面 → 输组件名 → （若删数据）再确认一次 |
| `Components.vue` | 列表上标出「正在移除」 |
| `views/Orphans.vue` + 导航 | 孤儿页：两类分开显示，逐条清理 |
| `lib/orphans.ts` | 分类与「放了多久」的纯逻辑，可测 |
| `test/webui` | 两条验收：移除与清理**服务端也验确认串** |

**确认语义与 CLI 逐档对齐**（10-cli §7）：

```
移除              输组件名
移除 --purge-data 输组件名 ＋ 单独确认删数据
清理孤儿          输实例键
```

界面上的输入框挡得住手滑，挡不住一条直接打过来的请求——**因此服务端
也验一遍**，而那两条正是新加的验收要钉的。

> **「正在移除」必须在列表上就看得见。** 它不接受任何写操作，而若与
> 正常组件长得一样，用户点进去改配置只会撞上一句「不接受其它写操作」
> ——那时他才知道发生了什么。

> 前端逻辑抽进 `lib/orphans.ts` 才测得了：`isInstalled` 决定界面给不给
> 那个「清理」按钮，而判错的方向不对称——把「仍装着」当成「数据残留」，
> 用户点完会以为问题解决了，实际那个服务还在跑。

### 第 7 步之外：restart 与 ad-hoc 通道

`restart` 原本被归在「小动词」里，做的时候才发现它不小：**协议里没有
命令通道**。06-state-and-drift 说它该「绕过调和直接 Stop+Start」，而
17-protocol §7 说「下发的永远是期望状态」——两句都对，描述的是两条不同
的通道，而第二条从来没建过。

整条通道的设计见 [ADR-0038](../adr/0038-adhoc-task-channel.md)。分界写成
一句可判定的话：

```
期望状态  丢了下一轮重新确认；断连三天回来照做仍然正确
命令      丢了就是没执行，必须告诉人；断连三天回来**不该补做**
```

`orphans purge` 能做成状态，正因为「这些目录不该存在」可以重复确认；
「重启过一次」不行。

> 实现时踩到的三处：
>
> - **`mechd` 一度 import 了 `agent`**，只为一个常量。控制面依赖节点侧
>   的包是反的，搬到 `protocol`。
> - **结果按节点名对齐是错的**：一台机器上可以有同一组件的两个角色，
>   后一条会覆盖前一条，而少掉的那行看起来只是「没返回」。改成按入参
>   下标对齐。
> - **节点侧也要判命令过期**：一条已超时的命令，中心早已报成失败并返回
>   给用户，此时再动机器是一次没人预期的重启。

> **一次值得记的误诊。** restart 的真机验收失败过，现象是「n2 联系不上，
> 中心却报告已重启」——看起来像凭空捏造成功。查下来是**测试用错了断连
> 方式**：`setupThreeNodeSite` 对 n2/n3 用的是 `startAgent`（裸 nohup
> 进程），我却用 `systemctl stop` 去停一个几乎没在跑的 unit。于是
>
>     is-active   inactive    ← unit 确实停了
>     中心        仍然 online  ← 裸进程的流还挂着
>     命令        真的执行了    ← 裸进程收到并执行
>     journal     0 次         ← 裸进程写文件，不写 journal
>
> 四个观察各自都对，拼起来却指向一个不存在的 bug。**操纵夹具起的东西
> 要用夹具自己的手法**。

### 第 7 步：补齐还是划掉

清点结果：文档承诺 60 个动词，代码里有 44 个。缺的 16 个分成两类处置。

**补齐的只有 `restart`**，而它不是糖——一个删得掉、装得上却踢不动进程的
部署工具说不过去。它带出了整条 **ad-hoc 命令通道**（[ADR-0038](../adr/0038-adhoc-task-channel.md)），
那是 M9 里第二处动协议的地方。

**其余全部从文档里划掉，标成「待真实需求」**，各自写明为什么先不做：

| 动词 | 理由 |
|---|---|
| `component show` / `assign` / `set-profile` | 能力已有（`status` / `deploy --update`），只差一层糖 |
| `component adopt-path` | 它服务的那条路（手工迁移数据目录）本身还没人走过 |
| `component verify` | 没有真实的怀疑场景之前，它只是一个更慢的 `status` |
| `component exec` / `logs`、`node exec` / `logs` | **通道已就位**，但流式输出与权限不是一行糖 |
| `node drain` | 没有调度器，它与 `cordon` 的差别只剩「顺带停实例」，而那是 `component stop --node` |
| `node facts` | 事实已在 `node show` 里；`refresh --apply` 要先想清楚与「路径固化不可变」怎么共处 |
| `site` 名词族 | 一个 mechd 管一个站点；多站点还没有人提出来 |
| `context` 名词族 | 目前用 `--server` / `--token` / `--site` |
| `pack pull` | 取包供离线转运，而 `mechpack bundle` 已经能产出 `.mpack` |
| `config diff --from/--to` | 跨版本比对要两个 generation 的渲染结果，而那是 `component diff` 的另一种切法 |

**「划掉」不是「删掉」**：命令表里留着，但标注清楚未实现与理由。M8 收尾
审查发现的正是「读 10-cli 的人会以为它们已经能用」，而把它们悄悄删掉会让
那些设计连同理由一起消失。

### 一处没有解决、只是定位清楚的脆弱性

M9 收尾时，多节点**全量**套件里 `TestRolloutBatchesGateRealMachines` 与
`TestResumePicksUpWhereItStopped` 会失败，现象是某台机器的 workload 停在
systemd 的 `starting`，而三台都报「已收敛、健康」——于是健康门禁不放行
下一批，升级卡到超时。

**不是回归**，判据有三条：

1. 两条测试**单独跑 fresh 集群都通过**
2. 失败集合在两次全量之间会变（脏集群 3 条 → clean 集群 2 条，
   `TestCordonStopsReconcileOnly` 自己好了）
3. M9 中途的一次 clean 全量是 28/28 通过的

结论是**负载/时序敏感**：那台开发机连续跑了几小时的容器负载。

**没有把它当成已解决。** 它是一个真实的脆弱性——一个只在「机器忙」时
失败的验收套件，迟早会在 CI 上变成一条被无视的红。真要修，方向是给
健康门禁的等待加上「systemd 仍在 activating」这个明确的中间态判定，
而不是把超时调长。留给 M10。

> **[§5.6](#56-带进-m10-的) 的收尾审查复查过这一条：状态未变。** 那次
> 审查没有重跑容器套件（见 [§5.5](#55-门禁现状)），因此这里写的仍是它
> 最后一次真机的结论——**不是「后来好了」，也不是「后来又坏了」，
> 是没有新证据**。下一次动它的时候，第一件事是在一台干净的机器上重跑
> 全量，先确定现象还在。

## 4. 验收判据具体化

| # | 场景 | 期望 |
|---|---|---|
| 1 | 一个实例的 `runState` 变成 `removed` | 进程停掉、unit/容器消失、generation 与 config 目录删掉 |
| 2 | 同上，`--purge-data` 未给 | **数据目录还在**，且登记为孤儿 |
| 3 | 同上，给了 `--purge-data` | 数据目录也删掉 |
| 4 | remove 一个有依赖者的 Component | 拒绝，并列出依赖者 |
| 5 | remove 时不输入 Component 名 | 拒绝；`-y` **也不能**跳过这一档 |
| 6 | remove 期间再 `config set` | 拒绝——正在被删的东西不该还能改 |
| 7 | 一台节点失联时 remove | 默认卡在 `removing`；`--force` 跳过它并登记孤儿 |
| 8 | 失联节点重新上线 | 它仍然收到 `removed`（若记录还在），或作为孤儿被发现 |
| 9 | `orphans list` | 列出保留下来的数据目录，含节点、路径、大小 |
| 10 | `orphans purge` | 二档确认；删掉之后 `list` 里消失 |
| 11 | `remove --ignore-not-found` 一个不存在的组件 | 静默成功（脚本可无脑调用） |
| 12 | `apply -f` 一份含新 Component 的文件 | 部署出来，与等价的 `deploy` 结果一致 |
| 13 | 同一份文件 `apply` 两次 | 第二次没有任何变化（幂等） |
| 14 | `apply -f` 删掉文件里的一个 Component | **不自动删除**——声明式不等于「文件里没有就删」 |
| 15 | `mechctl --help` 与 10-cli 的命令清单 | 一一对应，没有「文档有代码无」 |

第 5、6、14 条是安全底线。第 14 条要特别说明：**`apply` 不做删除**——
一份写漏了的文件不该删掉线上组件，删除必须走显式的 `remove`。

### 每一条落在哪

| # | 覆盖 | 真机 |
|---|---|:--:|
| 1 | `test/e2e/remove_linux_test.go` · `TestRemovedUninstallsForReal` | ✅ |
| 2 | 同上（数据目录里的记号原封不动）+ `test/multinode/orphans_linux_test.go`（登记为孤儿） | ✅ |
| 3 | `TestRemovedWithPurgeDataLeavesNothing` | ✅ |
| 4 | `internal/mechd` · `TestRemoveRefusesWhenDependentsExist` | — |
| 5 | `ctlcmd` · `TestConfirmNameIgnoresYes` + `TestRemoveSendsNothingDestructiveWithoutTheName` + `test/multinode` · `TestRemoveRefusesWithoutTheName` | ✅ |
| 6 | `TestRemovingRefusesWrites`（逐个动词）+ `TestRemoveStaysStuckThenTheNodeComesBack` 里的 `config set` | ✅ |
| 7 | `test/multinode/removeoffline_linux_test.go` 两条 | ✅ |
| 8 | 同上 | ✅ |
| 9 | `test/multinode/orphans_linux_test.go`（节点、路径；**大小按 §2.4 的取舍不做**） | ✅ |
| 10 | 同上（二档确认 + 清掉之后从列表消失 + 只清那一台） | ✅ |
| 11 | `TestRemoveIgnoreNotFound` + 反面 `TestRemoveWithoutIgnoreNotFoundFails` | — |
| 12 | `test/multinode/apply_linux_test.go` | ✅ |
| 13 | `TestApplyCreatesThenIsIdempotent` + 真机第二次 apply | ✅ |
| 14 | `TestApplyNeverDeletes` + 真机（文件里没提的组件原封不动且被列出） | ✅ |
| 15 | `ctlcmd/docdrift_test.go` 两条（正反两个方向） | — |

**第 15 条做成了自动化守卫**，而不是又一次人工核对。M8 收尾那次核对是
人工的，人工核对只会做一次——下一次漂移会在没人看的时候发生。

> 这条守卫抓到了**五处我刚刚人工清点时漏掉的**漂移：整个 `mechctl pack`
> 名词族、`component show`、`config diff --from/--to`。
>
> 更值得记的是**守卫的第一版自己是坏的**：写成
> `exists := real[key] || real[noun]`，于是任何已存在名词下的任意动词
> 都算通过——`mechctl component teleport` 也能过。它只抓得到整个名词
> 缺失，而真正常见的漂移恰恰是「名词还在、某个动词没做」。修好之后立刻又冒出
> 上面那两条真实缺口。
>
> **通过 ≠ 有效**，这是 M9 里这条纪律的第三次应验。

## 5. 里程碑审查

8 个步骤做完、15 条判据条条真机验过之后做的一次审查。**要回答的不是
「做完了吗」，而是「有没有哪一处是我以为验过、其实没验的」。**

### 5.1 最主要的发现：验收表全绿 ≠ 代码被测过

§4 那张表的每一行都填着覆盖，而这四处的**单元覆盖率是 0.0%**：

| 位置 | 是什么 | 为什么会漏 |
|---|---|---|
| `mechd.Service.Restart` | restart 的整个编排 | 只被 `test/multinode/restart_linux_test.go` 走过 |
| `agent.RunTask` / `agent.restart` | 节点侧执行 | 同上 |
| `mechd.applyGroup` | `apply -f` 的 `configGroups:` 段 | **一处都没走过**——单元与真机都没有 |
| `mechd.splitSecretKey` | apply 里 `setFile` 明文的路由 | 同上 |

前两处是「真机验过，但真机套件不产出覆盖数据」——于是「验过了」与
「有测试」被悄悄划了等号。后两处更糟：`configGroups:` 与「secret 只能走
setFile」都写在 `mechctl apply --help` 里，**发布了、文档里写着、一次都
没有执行过**。一个没做的字段至少会当场报错，一个没测过的字段不会。

补的测试挑的是**沉默的分支**——出错时不报错的那些：

- restart 的**「已停止但启动失败」**：服务停着，而笼统一句「重启失败」
  会让人以为它还在跑
- apply 的 secret 路由：拆键拆错会把 A 的口令写进 B，**两边都不报错**
- `outcomeState` 的 `timeout`：与 `failed` 混为一谈会让人以为没重启，
  于是再敲一次——而机器上可能真的正在重启

结果：

| 文件 | 测试 | 变异 |
|---|---|---|
| `internal/agent/tasks_test.go` | 7 条（新建） | 7 组，全部变红 |
| `internal/mechd/restart_test.go` | 7 条（新建） | timeout 折成 failed → 变红 |
| `internal/mechd/apply_test.go` | +8 条（配置组 5、secret 3） | 8 组，全部变红 |

覆盖率：`applyGroup` 0.0% → **100%**，`splitSecretKey` 0.0% → **100%**，
`agent.RunTask` 0.0% → **100%**，`agent.restart` → 89.7%，
`outcomeState` 60% → **100%**。

### 5.2 一条在 Windows 上「绿」的测试，绿错了

`purge.go` 的 `isAbs` 只认 `/` 开头（刻意的，见那里的注释）。于是在
Windows 上跑 `internal/agent`：

```
TestPurgeDeletesRetainedPathsAndReceipt        C:\... 被当成相对路径 → 红
TestPurgeSkipsWhenTheInstanceWasRedeployed     同样被跳过 → 绿，但绿错了
```

后一条本来要验「有真实实例时 purge 必须空跑」，实际验成了「Windows 上
什么都删不动」——**一个照删不误的实现同样能通过它**。

处置：文件改名 `purge_linux_test.go`（理由写在文件开头），**不去把
`isAbs` 放宽成 `filepath.IsAbs`**——那是为了让开发机变绿而动产品代码。

顺带钉住一处看起来像笔误的**故意的不一致**：`reconcile/disposePaths`
用的正是 `filepath.IsAbs`。两处问的不是同一个问题——

| | 问的是 |
|---|---|
| `agent/purge.go` | 「这条路径绝对到可以直接 `RemoveAll` 吗」——安全判据 |
| `reconcile/disposePaths` | 「这是物化过的路径，还是没展开的 generation 占位符」 |

后者对占位符在两个平台上答案都是「否」，因此那边用 `filepath.IsAbs`
反而让测试在 Windows 上也验得真。已就近写清楚，防下一个人顺手统一。

### 5.3 开发机上的覆盖率数字会系统性偏低

| 包 | Windows | Linux |
|---|--:|--:|
| `internal/reconcile` | 13.1% | **79.1%** |
| `internal/agent` | 26.2% | 29.8% |

拿 Windows 的数字判断「哪里没测」会指向完全错误的地方。**判断覆盖缺口
必须在 Linux 侧取数**——与「容器里跑通才算数」是同一条纪律的另一面。

### 5.4 其余修掉的

- **我自己刚写的一条测试就是坏的**：`res.Duration == 0` 在整包跑时随机
  失败（单跑必过），而它守的只是「字段填上了」。删掉——一条偶尔变红的
  测试比没有测试更贵，它最后换来的是所有人都不看红色。
- **CI 步骤名「M7 多节点验收」是谎话**：M9 把新测试加进了同一个包，
  面板上看是绿的 M7，实际跑的东西多了一倍。改成「多节点验收（M7 + M9）」。
- **决策号 207 被用了两次**，且其中一条漂到了 M9 段末尾，`同上` 因此
  指向 11-cli。重发为 342，出处写成具体文件（决策 341）。

### 5.5 门禁现状

| | 结果 |
|---|---|
| `gofmt -l .` / `go vet ./...` | 干净 |
| `go test ./...`（Windows） | 全绿 |
| `go test ./...`（WSL / Linux） | 全绿 |
| `go run ./hack/protogen` 重跑 | 生成物逐字节一致 |
| `sqlc generate` 重跑 | 生成物逐字节一致 |
| `go mod tidy` | 无变化 |
| `mechpack lint --hermetic --strict` | 12/12 |
| 前端 `npm run test` / `make webui` | 35 条通过 / 构建通过 |

> **这张表里没有容器套件。** `test/e2e` 与 `test/multinode` 在没有集群时
> 是 skip 的（`ok … 0.006s`），本次审查**没有重跑**它们——它们的最后一次
> 真机结果是 M9 第 7、8 步的验收，其中已知的问题就是 §3 那条负载敏感的
> 失败。按「容器里跑通才算数」，这张表只证明了单机那一层。

### 5.6 带进 M10 的

1. **多节点全量套件的负载敏感失败**（§3 那一节，决策 330）——本次审查
   没有改变它的状态，仍是唯一一处「定位清楚、没有解决」。
2. `checkSiteMatches` 的 `kind` 分支（57.1%）未测：改 kind 会被拒绝这条
   有测，「kind 相同因而放行」没有。风险低，但它是一条**拒绝迁移**的
   闸门，值得补。
3. 真机套件不产出覆盖数据，因此「哪些代码只被真机走过」目前靠人看。
   要么给真机跑覆盖插桩，要么接受它并在每次里程碑审查时复查一遍——
   本次是后者。

## 6. 相关决策

- [ADR-0001 Agent 模式](../adr/0001-agent-based.md) —— §2.1 期望状态驱动
- [ADR-0002 mechlet 不做判断](../adr/0002-mechlet-as-sole-engine.md) —— §决策
- [20-continuous-reconcile §2.4](20-continuous-reconcile.md) —— 孤儿永不自动删
- [10-cli §4.3](10-cli.md) —— remove 的完整设计
