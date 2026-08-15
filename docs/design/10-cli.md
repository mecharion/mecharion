# CLI 参考

## 1. 总则

### 1.1 名词优先

命令的规范形式是 **`mechctl <名词> <动词> [参数]`**：

```bash
mechctl component deploy postgresql -c pg-main
mechctl node cordon node-3
mechctl orphans purge --older-than 90d
```

理由与对比见 [ADR-0025](../adr/0025-noun-first-cli.md)。简言之：本工具有 8 个名词、40 余个操作，其中 13 个动词（`cordon` `drain` `pause` `move` `adopt-path` …）**只对单一名词成立**。动词优先会把它们全部挤进顶层——这正是 kubectl today 的处境。

**没有缩写别名，没有复数别名。** 每个名词只有一种写法。输入长度由 [shell 补全](#9-补全)解决，而不是靠让用户记住 `co` 是 component 还是 config。

### 1.2 顶层别名的准入规则

> **只有零歧义的动词才能做顶层别名。** 一个动词若对多个名词都成立、且参数类型相同，就必须留在名词之下。

据此，顶层只有四个命令：

| 顶层命令 | 为什么够格 |
|---|---|
| `apply -f` | **不属于任何名词**——它作用于一份可能同时含 Site / Component / ConfigGroup 的声明文件 |
| `deploy <pack>` | 只有 Component 能被 deploy，且参数是 **Pack 名**，与任何其他命令的参数类型都不同。规范形式仍是 `component deploy` |
| `version` | 惯例 |
| `completion` | 惯例 |

被这条规则挡在外面的（**故意不做别名**）：

| 候选 | 撞在哪 |
|---|---|
| `status` | 与 `rollout status` 撞，两者都接受 Component 名 |
| `diff` | 与 `config diff` 撞 |
| `logs` / `exec` | 与 `node logs` / `node exec` 撞 |
| `list` / `show` / `remove` | 对几乎所有名词都成立 |

`remove` 尤其重要：顶层的 `mechctl remove xxx` 无法判断 xxx 是 Component 还是 Node 还是 Site。**误删的代价远高于多打几个字符。** 结构本身消解了这个歧义，不需要靠约定去记。

### 1.3 声明式为主干

一级动作是 `apply` / `status` / `diff`。系统持有期望状态，所有操作都是「让实际状态向期望状态收敛」（[原则四](00-overview.md#原则四声明式为主干命令式为逃生舱)）。

`deploy` / `upgrade` / `rollback` 是**便捷入口**，写回同一份期望状态。这不是妥协：运维在命令行里想的是「部署一个 PG」而不是「让期望状态包含一个 PG」，让工具接受这种表达、内部归一到 apply，是易用性与模型纯度的正确折中。

### 1.4 连接目标由环境解析，不靠 flag

**`mechctl` 永远连 mechd**——单机形态下 mechd 就在本机（[ADR-0026](../adr/0026-standalone-runs-mechd.md)）。因此边缘单机上直接 `mechctl component list` 即可，不需要任何 flag。

解析链（优先级从高到低）：

```
① --server / --context                  显式指定
② MECHARION_SERVER / MECHARION_CONTEXT  环境变量（CI 友好）
③ ~/.config/mecharion/config.yaml       用户上下文（运维笔记本管多个 Site）
④ /etc/mecharion/client.yaml            机器级默认（安装时写入）
⑤ 探测到 /run/mecharion/mechd.sock      兜底，并告警「未找到 client.yaml」
```

③ 优先于 ④ 的理由：用户显式配了 context 就是有意为之。边缘机上 root 没有用户配置，自然落到 ④。

`/etc/mecharion/client.yaml` 由安装命令写入：

```yaml
# mechlet install --standalone  /  mechd install
target: unix:///run/mecharion/mechd.sock

# mechlet install --server https://mechd:8443 --token …
target:   https://mechd.example.com:8443
fallback: local          # mechd 不可达时降级为本机只读
```

### 1.5 `--local`：mechd 不可达时的现场诊断入口

```bash
mechctl --local component status
```

直连 `/run/mecharion/mechlet.sock`，**只读**。它存在的唯一理由是：网络断了、你 SSH 进现场那一刻。

受管节点上 mechd 不可达时会自动降级，并给出醒目提示：

```
! 无法连接 mechd (https://mechd:8443)：connection refused
  已降级为本机 mechlet 的只读视图（仅显示 node-7 上的实例）

COMPONENT   ROLE      GENERATION   状态
pg-main     replica   0007         Running (2d3h)
```

**降级必须只读**——否则本地会写出一份与 mechd 分叉的期望状态，重连后被覆盖。

### 1.6 目标定位语法

```
<component>                       # 当前 Site 内
<site>/<component>                # 跨 Site
<component>/<role>                # 定位到角色
<component>/<role>@<node>         # 定位到具体实例
```

---

## 2. 全局标志

| 标志 | 默认 | 说明 |
|---|---|---|
| `-o, --output` | `table` | `table` \| `json` \| `yaml` |
| `-s, --site` | 当前上下文 | 作用的 Site |
| `--local` | false | 直连本机 mechlet 的**只读**诊断视图（见 §1.5） |
| `--context` | 见 §1.4 | 连接哪个 mechd |
| `--server` | 见 §1.4 | 直接指定 mechd 地址 |
| `--log-level` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `--log-format` | 见下 | `text` \| `json` |
| `-y, --yes` | false | 跳过确认（**危险操作仍会二次确认**，见 §7） |

`--log-format` 默认值按二进制区分：`mechctl` / `mechpack` 用 `text`（交互式），`mechd` / `mechlet` 用 `json`（长驻服务）。

---

## 3. 名词总表

| 名词 | 操作 |
|---|---|
| [`site`](#41-site) | `list` `show` `create` `remove` |
| [`node`](#42-node) | `list` `show` `bootstrap` `cordon` `uncordon` `drain` `remove` `facts` `exec` `logs` |
| [`component`](#43-component) | `list` `deploy` `remove` `status` `diff` `upgrade` `rollback` `restart` `stop` `start` `set-drift-policy` `set-rollout` `ack-drift` `render` |
| [`config`](#44-config) | `get` `set` `explain` `diff` `group` |
| [`rollout`](#45-rollout) | `status` `pause` `resume` `abort` `history` |
| [`pack`](#46-pack) | `upload` `list` `show` `pull` |
| [`orphans`](#47-orphans) | `list` `purge` |
| [`context`](#48-context) | `list` `use` `set` `remove` |
| [`user`](#49-user) | `show` `passwd` `reset` |
| [`backup`](#410-backup) | `create` |

---

## 4. 逐名词

### 4.1 `site`

```bash
# ⚠ 整个 site 名词**尚未实现**，见下方说明
mechctl site list
mechctl site show <name>
mechctl site create <name> [--kind edge|cluster|standalone] [--label k=v]
mechctl site remove <name>
```

`site remove` 要求 Site 为空（无 Node、无 Component），且需输入 Site 名确认。

> **整个 `site` 名词尚未实现，且不再作为承诺。** 目前每个 mechd 管**一个**
> 站点（安装时建出来），而多站点是一个还没有人提出来的需求。
> 上面这些命令等真的有人要管两个站点时再连同设计一起做——那时也才知道
> 「切换站点」到底该长什么样（是 `--site` 参数、`context`、还是别的）。

### 4.2 `node`

```bash
mechctl node list [-l role=db]
mechctl node show <name>
mechctl node add <name> [--address A]                # 只登记，不代表它已经连上来
mechctl node token create [--node N] [--ttl 30m] [--uses 1]   # 明文只显示这一次
mechctl node token list
mechctl node token revoke <id>
mechctl node revoke <name> [-y]                       # 保留在册但切断它
mechctl node unrevoke <name>
mechctl node bootstrap ssh://root@host --token XXX   # 一次性 SSH 推送，之后不再用 SSH
mechctl node cordon <name>
mechctl node uncordon <name>
mechctl node drain <name>                            # ⚠ 未实现
mechctl node remove <name> [--force]
mechctl node facts show|diff|refresh <name> [--apply] # ⚠ 未实现
mechctl node exec <name> -- <cmd>                    # ⚠ 未实现（通道已就位）
mechctl node logs <name> [-f]                        # ⚠ 未实现（通道已就位）
```

> **实现状态**（M9）。已实现：`list` `show` `add` `remove`
> `cordon` `uncordon` `revoke` `unrevoke` `bootstrap` `token`。
>
> **尚未实现，且不再作为承诺**：
>
> | 动词 | 为什么先不做 |
> |---|---|
> | `drain` | 10-cli 自己讨论过「没有调度器，drain 不迁移实例」——那它与 `cordon` 的差别就只剩「顺带停掉这台上的实例」，而那是 `component stop --node` 已经能做的 |
> | `facts` | 事实已经在 `node show` 里看得到；`refresh --apply` 那一半要的是「重新固化」，而那条路要先想清楚它与「路径固化不可变」怎么共处 |
> | `exec` `logs` | **通道已就位**（[ADR-0038](../adr/0038-adhoc-task-channel.md) 的 Tasks 流），但流式输出与权限模型不是一行糖 |

**`add` 与「加入」是两件事。** `add` 只在册子上留一行「这台机器属于本
Site」，节点状态是 `pending`——它还没连上来过（`offline` 表示**来过但现在不在**，
两者刻意分开，见 [22-multi-node §6.13](22-multi-node.md)）。真正连上来还需要那台机器上
有一张证书（join token，或 `mechd ca issue` 的离线路径）。分开是因为授权者
不同：登记是控制面的管理动作，加入需要那台机器上有人。

**`cordon` 与 `drain` 的区别**——两者都有用，语义必须分清：

| | 作用 | 用途 |
|---|---|---|
| `cordon` | **暂停该节点上的调和**，运行中的进程不动 | 「我要手工调试这台机，别让 mechlet 把我的改动改回去」 |
| `drain` | cordon **＋ 停止全部 workload**（不删除、不迁移） | 「我要重启这台机 / 换硬件」 |

Mecharion **没有调度器**，因此 `drain` 不会把实例迁到别的节点——那需要用户显式 `component assign`。文档中必须写明这一点，否则来自 Kubernetes 的用户会有错误预期。

`node remove` 在该节点仍有 RoleInstance 时拒绝；`--force` 会连带移除，但**留下的数据目录登记为孤儿**。

**`node list` 带一列孤儿实例数，`node show` 列出明细。** 孤儿 = 机器上还在、
但下发里没有的实例，来源通常是组件被移除、或某次解析失败让它没被下发。

它们**绝不会被自动卸载**：卸载不可逆，而「mechd 少发了一条」与「用户真的
删了这个组件」在节点侧分辨不了（[20-continuous-reconcile §2.4](20-continuous-reconcile.md)）。
清除要显式走 `component remove`。

数字出现在 `list` 而不只在 `show` 里，是因为**一个只有 `node show <name>`
才看得到的问题等于没人会发现它**——而孤儿的典型来源恰好是「某次变更漏了
一台机器」，那台机器正是没人会去单独查的那台。

### 4.3 `component`

```bash
mechctl component list [--site S]
mechctl component show <name>                       # ⚠ 未实现（用 status）
mechctl component status <name>
mechctl component diff <name>
mechctl component diff -f site.yaml

mechctl component deploy <pack> [-c <name>] [--profile P]
                                [--role <role>=<nodes>] [--nodes n1,n2]
                                [--set k=v] [--set-file k=@path] [--set-stdin k]
                                [--require <pack>=<component>]
                                [--update] [--adopt-data|--purge-data]
                                [--dry-run] [--wait]

mechctl component upgrade  <name> [--version V] [--force] [--dry-run]
mechctl component rollback <name> [--to-version V] [--dry-run]
mechctl component restart  <name> [--role R] [--node N] [--timeout S]
mechctl component stop     <name> [--role R] [--node N]
mechctl component start    <name> [--role R] [--node N]
mechctl component remove   <name> [--purge-data] [--keep-config] [--purge-user]
                                  [--ignore-not-found] [--force]

mechctl component set-drift-policy <name> <report|ignore|none>
mechctl component set-rollout      <name> [--max-unavailable N] [--canary N]
mechctl component ack-drift   <name> [--resource <id>] --duration 4h --reason "…"

mechctl component render -f plan.yaml [--pack DIR] [-o DIR]
```

> **实现状态**（M9）。已实现：`list` `status` `diff` `deploy` `upgrade`
> `rollback` `stop` `start` `restart` `remove` `ack-drift`
> `set-drift-policy` `set-rollout` `render`。
>
> **尚未实现，且不再作为承诺留在这里**——它们从上面的命令表里划掉，
> 等真实需求出现时再连同设计一起加回来：
>
> | 动词 | 为什么先不做 |
> |---|---|
> | `show` `assign` `set-profile` | 能力已经有了（`status` / `deploy --update`），只差一层糖。等有人嫌绕再加 |
> | `adopt-path` | 它服务的是「手工迁移过数据目录之后更新记录」，而那条路本身还没有人走过 |
> | `verify` | 全量哈希比对绕开摘要缓存。**没有真实的怀疑场景之前，它只是一个更慢的 status** |
> | `logs` `exec` | 两者都要 ad-hoc 通道，而那条通道 M9 刚建起来（[ADR-0038](../adr/0038-adhoc-task-channel.md)）。**通道已就位，它们随时可加**——但加之前要想清楚流式输出与权限，那不是一行糖 |
>
> `restart` 是这批里唯一做了的，因为它不是糖：一个部署与状态管理工具
> 删得掉、装得上、却踢不动一个进程，是说不过去的。

**`render`** 是唯一**不连 mechd** 的 component 动词：它把
「Pack + 用户输入 + 节点事实」离线解析成每个 RoleInstance 的 ResolvedSpec，
走的是与真实部署完全相同的那条管线，只少了「落库 + 下发」两步。

因此它同时是 `--dry-run` 的底层与事故复盘的工具——「为什么这台机器上是
这份配置」必须能离线回答，而复盘时集群往往已经不在了。
**不另写一套预演逻辑**：两套实现迟早会不一致，而不一致的预演比没有预演更糟
（[15-render-pipeline §9](15-render-pipeline.md#9-可复现性)）。

输入文件描述的正是 mechd 从库里读出来的那些东西，字段与解析管线的入参
一一对应，拼错的字段直接报错。密钥用一次性值，产出里只有引用没有明文，
可以直接传阅。

**`ack-drift`** 给「临时修改」一个名分：运维凌晨救火改了一个值，此前只能
要么被永远报成异常、要么走一次正式变更。它**有期限**（到点自动恢复告警，
不会悄悄变永久）、**有理由**（进审计）、**仍然检测**（只是不告警，
`status` 里照常显示「已抑制」）。不给 `--resource` 时抑制整个实例，
用于整机维护窗口。见 [06-state-and-drift §4.1](06-state-and-drift.md#41-临时修改需要有名分)。

**`verify`** 强制全量哈希比对，绕过调和循环的 `(size, mtime)` 摘要缓存。
需要确定性结论时用它——那是低频的人工动作，不该让每 60 秒一次的调和
为它买单。

#### deploy

省略 `-c` 时 **Component 名默认等于 Pack 名**（见 [pack-v1 §8.8](../spec/pack-v1.md#88-路径中的名字是-component-名)）。

多角色 Pack 必须逐角色指定节点：

```bash
mechctl component deploy hdfs -c hdfs-prod --profile ha \
  --role namenode=n1,n2 \
  --role journalnode=n1,n2,n3 \
  --role datanode=n4,n5,n6 \
  --set nameservice_id=prod \
  --set-file admin_password=@/run/secrets/pg
```

单角色 Pack 用 `--nodes` 作糖。

**Component 已存在时默认拒绝**，防误操作：

```
✗ Component "pg-main" 已存在（postgresql 16.4-1，3 个实例）
  · 修改参数或拓扑:  加 --update 收敛到声明的状态
  · 升级版本:        mechctl component upgrade pg-main --version 16.5
  · 先移除再部署:    mechctl component remove pg-main
```

加 `--update` 后 deploy **幂等**——收敛到声明的状态。`upgrade` 隐含 `--update`。

> 覆盖参数叫 `--update` 而非 `--force`：`--force` 在别处已用于「跳过确认」，一词两义会让人搞不清它到底放行了什么。

**残留数据目录**：若 `/var/lib/mecharion/apps/<name>` 仍存在（上次 remove 保留的），deploy 拒绝并要求显式选择：

```
✗ 发现 pg-main 的残留数据目录（12.4 GB，2026-07-15 由 remove 保留）
  /var/lib/mecharion/apps/pg-main @ node-1, node-2
  · 接管这些数据:  --adopt-data
  · 清除后重新部署: --purge-data
```

**静默接管是真实的运维事故来源**——那可能是完全不同版本留下的数据。

#### secret 参数的三种输入

`--set` 用于 `secret` 类型参数时**直接报错**——它会同时进入 shell history 与 `ps aux`：

```
✗ 参数 admin_password 是 secret 类型，不能用 --set 传明文
  改用：
    --set-file admin_password=@/run/secrets/pg     无人值守首选
    --set-stdin admin_password                     管道
```

| 方式 | 场景 | 无人值守 |
|---|---|---|
| `--set-file k=@path` | CI / 自动化**主力**，对齐 Docker/K8s secret 的文件挂载惯例 | ✅ |
| `--set-stdin k` | `vault read -field=pw \| mechctl component deploy … --set-stdin admin_password` | ✅ |
| TTY 交互 | 人工操作 | ❌ 仅在 stdin 是终端时提示；**非 TTY 环境不会卡住等待输入**，而是报错并给出上面两种写法 |

#### remove

> **已实现**（M9 第 1–3 步）。落地形态与下面的设计有两处出入，都在
> [24-lifecycle-completion](24-lifecycle-completion.md) 里记了理由：
>
> - 卸载**不是一条新指令**，而是 `runState` 的第三个值 `removed`——
>   指令是事件，丢一次就永远丢了。
> - 第 ③ 步**没有分批**：分批是为了保可用性，而 remove 是把整个组件
>   删掉，没有可用性可保；跨组件的顺序由「有依赖者就拒绝」覆盖。

**这是整个工具里最危险的操作**——它在 N 台机器上停进程、删文件。

默认删什么、默认留什么：

| 对象 | 默认 | 开关 |
|---|---|---|
| generation 目录 | **删** | — |
| 配置目录 | **删** | `--keep-config` |
| **数据目录** | **保留** | `--purge-data` |
| 系统用户 / 组 | **保留** | `--purge-user`（危险：可能有其他文件以它为属主） |
| blob | 引用计数 −1，归零后可被 GC | — |

数据目录默认保留，与「升级永不触碰数据目录」是同一条原则的延续。保留下来的数据**登记为孤儿**，可用 [`orphans`](#47-orphans) 发现与清理——保留而不提供发现机制等于把问题推给未来。

执行阶段：

```
① 前置校验
   · 引用计数：有 Component 依赖它 → 拒绝并列出依赖者
   · 打印影响面：N 个节点、M 个实例、数据目录合计大小
② 二次确认：需输入 Component 名，-y 不能跳过
③ 下发 runState: removed（**全部实例一次性**，不分批）
   每实例：preStop → Stop → postStop → preRemove → Runtime.Remove
           → 删 generation / config → 数据按开关处理 → postRemove
④ 全部实例报告拆干净之后，清理 Component 元数据；
   保留的数据由节点侧的孤儿上报自动浮出来，不需要中心侧另行登记
```

**失联节点**：`--force` 跳过它们之后，那些机器上的实例会变成**孤儿**——
[20-continuous-reconcile §2.4](20-continuous-reconcile.md) 定死了孤儿
**永不自动删**（「mechd 少发了一条」与「用户真的删了这个组件」在节点侧
分辨不了，而卸载不可逆）。因此它们要靠 `orphans` 发现与清理，
**不会**在重新上线时自己消失。

> 早先这里写的是「mechlet 据此自行清理」，与 §2.4 直接冲突。以 §2.4 为准
> ——那条纪律有明确的理由，而「自行清理」会让一次 mechd 侧的下发故障
> 变成一次静默卸载。这处矛盾是 M8 收尾审查时发现的。

`--ignore-not-found` 让「移除一个不存在的 Component」静默成功，便于脚本无脑调用。

### 4.4 `config`

```bash
mechctl config get     -c <component> [-r <role>]
mechctl config set     -c <component> -r <role> [--node n7] k=v
mechctl config explain <param> -c <component> [--node n7]
mechctl config diff    -c <component> --from 3.6.0 --to 3.7.0   # ⚠ 未实现

mechctl config group list   -c <component> -r <role>
mechctl config group create <name> -c <component> -r <role> --nodes n1,n2
mechctl config group set    <name> -c <component> -r <role> k=v
mechctl config group move   <node> --to <group> -c <component> -r <role>
mechctl config group diff   <group-a> <group-b> -c <component> -r <role>
mechctl config group remove <name> -c <component> -r <role>
```

`config set --node n7` **自动创建只含该节点的 ConfigGroup** 并提示——模型中不存在无名的 per-node 覆盖（[ADR-0021](../adr/0021-config-group.md)）。

> **实现状态**：整族命令在 M8 第 7 步落地（[23-web-ui §4.4](23-web-ui.md)）。
> 其中 `config diff --from/--to`（跨版本比对）**尚未实现**——它要的是两个
> Pack 版本的解析结果对比，与组间 diff 不是一件事。

`config explain` 输出完整取值来源链，这是 ConfigGroup 多出一层解析后的必要补偿：

```
$ mechctl config explain max_xcievers -c hdfs-prod --node n21
  8192
  ← ConfigGroup "12-disk-nodes"          8192
    Role "datanode"                      4096
    Component "hdfs-prod"                （未设置）
    Pack 默认值                           4096
```

### 4.5 `rollout`

```bash
mechctl rollout status  <component>          # 没有进行中的则显示最近一条
mechctl rollout pause   <component>          # 冻结判定，并停在当前这批
mechctl rollout resume  <component>          # 恢复；因故障停下时从断点续做
mechctl rollout abort   <component> [-y]     # **真的**回退到起始版本
mechctl rollout history <component> [--limit N]
```

`pause` 冻的是「这次算成功还是失败」这个判定，**并且停在当前这批**：
已经放行的那批照常跑完，但下一批不会被放行。人敲它是因为要去看现场，
而状态在他看的过程中自己往前走，等于没有暂停。

> M6 时它只冻判定（单机只有一批，没什么可停）。M7 有了分批之后，
> 「别急着宣布失败」与「别急着往前走」是同一个意思。

**状态词表**（`rollout status` / `history`）：

| 状态 | 含义 | 还能往前走吗 |
|---|---|---|
| `running` | 正在推进 | — |
| `succeeded` | 每一批都过了门禁 | — |
| `paused` | **人**按了 pause | 能 —— `resume` |
| `halted` | **系统**停的：某批没过门禁 | 能 —— 修好机器后 `resume` |
| `failed` | 系统停的，且没有前路 | 不能 |
| `aborted` | 人 abort 了，且已经退回去 | 不能 |

`halted` 与 `paused` 分开，是因为脚本判 `state == "paused"` 时分不出
「我按的」和「出事了」。`halted` 与 `failed` 分开，是因为前者有前路——
`failed` 留给节点已经自动回滚的情形，那个版本在节点侧已被锁住，
再推一遍也不会动。

`resume` 从**没过门禁的那一批**续做：那一批重新等门禁（窗口清零、
批次超时从头算），**已完成的批次一个都不重做**。它不检查机器是不是真的
修好了——mechd 判断不了，而敲命令的人比它清楚。

`abort` 走的是与 `component rollback` 完全相同的那条路径——一条只把记录
标成「已中止」的 abort，会让运维以为世界回到了升级前，而机器上跑的还是
新版。它因此按 §7 需要 y/N 确认；非交互环境下读不到回答时**拒绝执行**，
要在脚本里用必须显式加 `-y`。

### 4.9 `user`

```bash
mechctl user show                          # 是否已初始化、口令上次何时改的
mechctl user bootstrap [--password-file <path>]  # 完成首次初始化，不用打开浏览器
mechctl user passwd [--password-file <path>]
mechctl user reset [-y]                    # 抹掉 admin，重新打开初始化窗口
```

**只有一个账号，名字固定是 `admin`，不支持增加账户**
（[ADR-0037](../adr/0037-login-is-full-privilege.md)）。它的口令**默认在首次
访问 Web UI 时由本人设定**，那是为了无人值守部署：离线场景下脚本装完工具与
组件包，人只在最后打开一次浏览器就拿到管理能力。

因此这组命令大多是**服务器侧的补救通道**，不是日常入口：口令忘了、或者要把
机器交给下一任（`reset` 之后下一个打开 UI 的人完成初始化）。

`bootstrap` 是例外——它是**浏览器初始化的自动化替代**，面向完全无人值守的
部署：脚本在 `mechlet install --standalone` 之后紧接着跑这条命令就能设好
口令，不需要人去找那个只打印一次的初始化令牌。能这样做是因为 `mechctl`
本机零配置就能读到与那个令牌同一个值（[ADR-0039](../adr/0039-bootstrap-token-gate.md)），远程执行时用 `--token`
显式给出。已经初始化过时同样返回 409——一次性规则对这条入口和浏览器那条
是同一条。

> `reset` 会**重新打开抢注窗口**：抹掉之后到下一次初始化完成之间，任何能
> 访问这个地址的人都能成为管理员。命令的确认提示里写明了这一点。

**口令永远不走命令行参数。** 命令行会进 shell 历史，也出现在同机任何用户的
`ps` 输出里。因此只有两条路：交互式输入（不回显、输两遍），或
`--password-file` 从文件读。**非交互且没给文件时直接拒绝**，不从 stdin 裸读
——那会把调用者推向 `echo pw | mechctl …`，而那条命令本身就进了历史。
这与 `component deploy` 的敏感参数必须走 `--set-file` 是同一条纪律。

口令下限 12 个字符，**不要求含大小写数字符号**：那类规则会把人推向
`Passw0rd!` 这种可预测模式，而长度才真正拉高爆破成本。

**没有找回流程。** 忘了就在服务器上 `mechctl user passwd` 重设——找回需要
邮件通道，而那是一个外部依赖（部署时零外部依赖是这个项目的底线）。

> **登录即全权**：现阶段没有角色划分，任何能登录 UI 的人都能做任何操作。
> 需要权限隔离的场景，在角色做出来之前不要开放 UI。

### 4.6 `pack`

```bash
mechctl pack upload <file>.mpack     # 上传一个 .mpack 到 mechd 的 Pack 集合
```

Pack 的**制作**在 `mechpack`（§10）；`mechctl pack upload` 是命令行侧唯一的**写**
动词——把 `mechpack assemble`/`bundle` 产出的 `.mpack` 送进 mechd（`POST /packs`，
与 Web UI 的上传按钮走同一个接口）。补它是因为不然的话命令行完全走不通
「部署一个带真实 payload 的 Pack」这条路：`mechctl component deploy` 能找到
本地 pack-dir 扫到的 Pack 定义，但那只是元数据，节点按 sha256 取的 blob
要经过这条上传接口的入库步骤才会真的存在。

```bash
# ⚠ 这三个查询动词**尚未实现**
mechctl pack list                    # mechd 注册表中的 Pack
mechctl pack show <name>[@<version>]
mechctl pack pull <name>@<version> -o <file>.mpack
```

> 注册表里有哪些 Pack，目前从 Web UI 的部署页看得到（它读 `GET /packs`）；
> 命令行上还没有对应的查询动词，且不再作为近期承诺。
>
> 这一条最早是 M9 第 8 步那条**文档漂移守卫**抓出来的——人工清点时漏了它。
> 那条守卫（`internal/cli/ctlcmd/docdrift_test.go`）会走一遍 cobra 命令树，
> 把 10-cli 里「既不存在、也没标未实现」的命令报成失败。

### 4.7 `orphans`

```bash
mechctl orphans list  [--site S] [--node N]
mechctl orphans purge <node> <instance>      # 需输入实例键确认
```

```
$ mechctl orphans list
NODE   INSTANCE          类型      SINCE  路径
n1     web__default      数据残留  3d     /var/lib/mecharion/apps/web
n2     web__default      数据残留  3d     /var/lib/mecharion/apps/web
n4     kafka-old__broker 仍装着    91d    （整个实例，不只是目录）

清理：mechctl orphans purge <node> <instance>
```

**孤儿分两类，列表上就分得开**——它们的处置完全不同：

| 类型 | 是什么 | purge 管不管用 |
|---|---|---|
| 数据残留 | `remove` 故意留下的目录 | 管用，就是删那几个目录 |
| 仍装着 | 下发里没有它了，但机器上**还装着、可能还在跑** | **不管用**——它只删目录，停不掉进程 |

后者服务端直接拒绝并说明原因。混为一谈会让人以为问题已经解决了。

**定位用 `<node> <instance>` 而不是一个 `orp-a3f1` 那样的合成 ID。**
同一个实例键会出现在多台机器上（一个三副本组件被 remove 之后留下三份
数据），而人往往只想清其中一台。合成 ID 要求先 list 再复制粘贴，多一步
且看不出自己在删哪台。

> 这一节原来写的是 `purge <id>` / `--all` / `--older-than`，并在列表里
> 显示**大小**。落地时改成了上面这样：
>
> - **不显示大小**：算它要 walk 整个目录树，而孤儿常常是几 TB、几百万
>   文件、放好几个月的数据目录，上报默认 15 秒一次。代价是「这堆数据
>   值不值得留」要人自己登机 `du`——明确的取舍，见
>   [24-lifecycle §2.4](24-lifecycle-completion.md)。
> - **`--all` 与 `--older-than` 没有做**，也不再作为承诺留在这里。
>   批量清理是个放大器：一条命令删掉几十台机器上的数据，而 purge 不可
>   撤销。等真的有人被逐条清理烦到时再加，那时也才知道该按什么筛。
>   （§7 里 `orphans purge --all` 那一行同此。）

### 4.8 `context`

```bash
# ⚠ 整个 context 名词**尚未实现**：目前用 --server / --token / --site
mechctl context list
mechctl context use <name>
mechctl context set <name> --server https://… --site <site>
mechctl context remove <name>
```

### 4.10 `backup`

```bash
mechctl backup create [--out <path>]
```

对 mechd 的 SQLite 数据库做一次一致性快照（`VACUUM INTO`），不用停服。**只有 `create`**——这是梳理运维手册时发现的缺口：`Store.Backup` 早就实现了，但此前没有任何命令能触发它，运维手册讲不出一条真的能执行的备份命令。`--out` 是 mechd **所在那台机器**上的本地路径，不是把备份文件传回客户端——与 `mechd ca export`/`issue` 同一个模型；留空时落在数据目录下的 `backups/` 子目录，只是一个凑手的默认值，不代表那是一个安全的备份位置（同一块盘上的备份在磁盘故障时和原件一起丢）。

只备份数据库本身，不是全部期望状态：主密钥与 PKI 必须分开单独备份。完整清单与操作步骤见 [docs/ops/backup-and-restore.md](../ops/backup-and-restore.md)。

---

## 5. 顶层命令

```bash
mechctl apply -f site.yaml [--dry-run]
mechctl deploy <pack> …            # = component deploy
mechctl version [--short]
mechctl completion bash|zsh|fish|powershell
```

`apply` 是声明式主干的入口：它接受一份可能同时含 Site、Component、ConfigGroup
的文件，因此不属于任何单一名词。

> **已实现**（M9 第 6 步）。文件格式见
> [24-lifecycle-completion §2.5](24-lifecycle-completion.md)：单文件、
> 顶层按名词分段（`site` / `components` / `configGroups`），字段名与
> `component deploy` 的参数一一对应。
>
> `--reconcile` 与 `--wait` **没有做**，也不再作为承诺留在这里：
> apply 走的就是 deploy 那条路，而那条路本来就会下发并触发调和；
> 「等它收敛」目前用 `component status` 看。真需要一个阻塞式的等待时
> 再加，那是加法。
>
> （`mechlet apply -f` 是另一回事：那是节点侧按一份**已解析规格**调和
> 本机，低一层。）

---

## 6. 输出格式

- **`table`**（默认）：人类可读。宽度自适应，管道输出时自动去掉颜色与边框
- **`json`**：单个对象或数组，字段名 `lowerCamelCase`
- **`yaml`**：同 json 的结构

**`-o json|yaml` 的输出结构保证向后兼容**——只增字段，不改语义、不删字段。`table` 的列与措辞不做此保证，脚本请勿解析。

`sensitive` 参数在**所有**输出格式中一律脱敏为 `<redacted>`。

---

## 7. 交互与安全约定

**危险操作二次确认，`-y` 不能全部跳过：**

| 操作 | 确认方式 |
|---|---|
| `component remove` | 需输入 Component 名 |
| `component remove --purge-data` | 需输入 Component 名 **＋** 单独确认删除数据 |
| `site remove` | 需输入 Site 名，且 Site 必须为空 |
| `node remove --force` | 需输入 Node 名 |
| ~~`orphans purge --all`~~ | 未实现，见 §4.7；单条 purge 需输入实例键 |
| `rollout abort` | y/N |
| 违反 `placement` 的部署 | 需显式 `--ignore-placement <约束名>`，并记入审计 |
| 触发下游重启的依赖升级 | 先列出影响面，再确认 |

**退出码：**

| 码 | 含义 |
|---|---|
| 0 | 成功 |
| 1 | 一般错误 |
| 2 | 用法错误（参数非法） |
| 3 | 校验失败（placement / cardinality / requires 不满足） |
| 4 | 部分失败（Rollout 中途暂停） |
| 5 | 连接不上 mechd / mechlet |

`diff` 在有差异时返回 **1**（对齐 `diff(1)` 与 `terraform plan -detailed-exitcode` 的惯例），便于脚本判断。

---

## 8. 错误信息的要求

错误必须**可行动**。「违反了 antiAffinity 约束」是无用的，正确的形式是：

```
✗ 放置校验失败: hdfs-prod
  约束  antiAffinity[namenode, secondarynamenode]  scope=node  (required)
    namenode           → node-1
    secondarynamenode  → node-1    ← 冲突
  原因: SNN 与 NN 同节点时无法承担元数据恢复职责
  · 改为 --role secondarynamenode=node-2
  · 或本次豁免: --ignore-placement antiAffinity[namenode,secondarynamenode]
```

三个要素缺一不可：**违反了什么**、**为什么这是个问题**（Pack 作者写的 `reason`）、**怎么做**。

---

## 9. 补全

名词优先的可发现性收益全靠补全兑现，因此补全是**一等功能**而非附属品：

```bash
mechctl completion bash > /etc/bash_completion.d/mechctl
mechctl completion zsh  > "${fpath[1]}/_mechctl"
```

必须支持动态补全（向 mechd 查询）：

| 位置 | 补全内容 |
|---|---|
| `mechctl <TAB>` | 名词 + 4 个顶层命令 |
| `mechctl component <TAB>` | 该名词的全部操作 |
| `mechctl component remove <TAB>` | **当前 Site 的 Component 名** |
| `mechctl node cordon <TAB>` | **节点名** |
| `mechctl component deploy <TAB>` | **注册表中的 Pack 名** |
| `--profile <TAB>` | 该 Pack 声明的形态 |
| `--set <TAB>` | 该 Pack 的参数名 |

---

## 10. mechpack

```bash
mechpack init <name> [--dir D]                       # 生成 Pack 骨架          ✅
mechpack assemble [dir] [--out D] [--source-root R]  # 组装，不是 build        ✅
mechpack lint [dir...] [--hermetic] [--strict]       # 校验                    ✅
mechpack inspect [dir]                               # 查看内容                ✅
mechpack bundle <pack>                               # → .mpack                ✅
mechpack push <pack>                                 # 入库                    ⏳
```

**没有 `mechpack push`**：上传/入库现在走 `mechctl pack upload`（§4.6），
直接调用 mechd 已经实现的接口。`mechpack push` 若要做，大概率是同一件事
换个入口，暂不重复实现。

**没有 `mechpack sign`**：Pack 签名/可信发布者校验决定不做，见 [ADR-0040](../adr/0040-pack-trust-is-operator-responsibility.md)。

`lint` 接受目录或**包含多个 Pack 的父目录**——`mechpack lint examples/packs` 会展开其下每个含 `pack.yaml` 的子目录。

`assemble --source-root` 用于「构建产物不在 Pack 目录内」的常见情形：

```bash
make dist                                   # 产物落在 ./dist
mechpack assemble packs/myapp --source-root .
```

典型流程：

```bash
mechpack init myapp
# … 用你自己的工具链构建产物，并在 sources 段指向它们 …
mechpack assemble myapp            # → dist/myapp-0.1.0-1/
mechpack lint --hermetic dist/myapp-0.1.0-1
```

> `mechpack` 是开发者工具，命令数量少且各自独立，不套名词层级——名词优先解决的是「多名词 × 多动词」的组合爆炸，`mechpack` 没有这个问题。

---

## 11. mechd / mechlet

```bash
mechd serve [--config /etc/mecharion/mechd.yaml]
mechd install [--data-dir /var/lib/mecharion]
mechd ca export [--out ca.crt]    # 导出自签 CA，供远程 mechctl 与浏览器信任
mechd ca issue --node <name> --out-dir <dir> [--validity 720h]  # **离线**签节点证书

mechlet run [--config /etc/mecharion/mechlet.yaml]
mechlet install --standalone [--data-dir /var/lib/mecharion]   # 装 mechd + mechlet 两个 unit
mechlet install --join https://mechd:8443 --token XXX --ca-hash sha256:…   # 受管节点
mechlet probe                     # 打印本机 facts 与 capabilities
mechlet status
```

两者也都支持 `version`。

### 单机安装做了什么

```bash
mechlet install --standalone
```

1. 安装并启动 `mecharion-mechd.service`——gRPC 监听 `unix:///run/mecharion/mechd.sock`，HTTP 监听 `0.0.0.0:8443`（自签 TLS）
2. 安装并启动 `mecharion-mechlet.service`——连接本机 mechd 的 unix socket
3. 写 `/etc/mecharion/client.yaml`，`target` 指向本机 mechd
4. 打印**只显示一次**的初始 admin token

用户视角是一条命令，`systemctl` 里会看到两个服务。

**mechlet 不用 systemd `Requires=` 依赖 mechd**，而是连接重试——多节点形态下本机根本没有 mechd，用同一套逻辑覆盖两种形态，避免 unit 文件分叉。

### 升级

mechd 与 mechlet **始终同步升级**，由 `mechlet install` 一并处理二进制的原子切换（generation + symlink，见下方"实体文件在版本目录里"）。跨版本时先 mechd 后 mechlet（控制面向后兼容 agent）。

**`mechlet install` 不会自动重启正在跑的服务**——它只把新二进制装好、原子切换软链、`systemctl daemon-reload`；已经在跑的 `mecharion-mechd`/`mecharion-mechlet` 进程不会因为软链变了就自己换代码，还差最后一步 `systemctl restart mecharion-mechd mecharion-mechlet` 才真正切到新版本。SQLite schema 迁移在 `mechd serve` 每次启动时自动跑（`goose` UpContext，不需要单独的迁移命令），因此这一步顺带会跑完新版本带来的迁移。运维手册的完整升级步骤见 [docs/ops/upgrade.md](../ops/upgrade.md)。

数据库备份用 `mechctl backup create`（[§4.10](#410-backup)），不是 `mechd` 的本地子命令——它走认证过的 HTTP 入口，复用 mechd 已经打开的数据库连接（`Store.Backup`，VACUUM INTO，不用停服）。

---

## 12. 尚未定稿

| 议题 | 何时定 |
|---|---|
| `mechctl node top`（实时资源视图）是否需要 | M6 之后视 UI 情况 |
| Rollout 的并发控制标志（`--parallel` / `--max-unavailable`）的默认值 | M5 |
| 插件机制（`mechctl-foo` 外部命令，kubectl 式） | 有生态需求时 |
| `component exec` 与 `node exec` 的权限模型是否需要区分 | M5 |
