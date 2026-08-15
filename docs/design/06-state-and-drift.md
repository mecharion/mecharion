# 状态管理、漂移与升级

## 1. 为什么选声明式

一级命令是 `apply` / `status` / `diff`，不是 `install` / `repair`。

这不是模仿 Kubernetes，而是产品定位的直接推论：目标里写着「状态管理」和「升级」，这两件事**只有在系统持有期望状态时才可能做对**。

- Ansible 不持久化期望状态，因此它永远回答不了「现在偏离了吗」
- Cloudera Manager 持有，因此它能

**Mecharion 与 Ansible 的差异化恰好就在这一点上**，放弃它就等于做一个更差的 Ansible。

命令式动词的归属：

| 用户想做的事 | Mecharion 的表达 |
|---|---|
| 首次安装 | `apply`（期望状态从无到有） |
| 修复被改坏的配置 | `apply --reconcile`，或 `driftPolicy: reconcile` 自动完成 |
| 临时执行脚本 | `mechctl node exec`（显式的逃生舱，不进入期望状态） |

## 2. 常驻 Agent 的回报

选择常驻 Agent 是为了拿到 agentless 拿不到的能力。这些能力必须**显式设计为一等特性**，而不是「顺便就有了」：

| 能力 | 说明 | Ansible 能做吗 |
|---|---|---|
| 持续调和 | mechlet 周期性 `Read` 实际状态并与期望比对 | ❌ 结构性做不到 |
| 漂移检测与自愈 | 配置被人手改了，能发现并按策略处理 | ❌ |
| 实时状态与日志流 | 直接从 agent 流到 UI，不需每次建 SSH 连接 | ❌ |
| 断连自治 | 失联期间继续自愈，重连后补报事件 | ❌ |
| 无 SSH 的临时执行 | `mechctl node exec -l role=db -- <script>` 走既有 agent 通道 | 需 SSH 常开 |

## 3. 资源模型

每个资源类型必须实现三个方法，缺一不可：

```go
type Resource interface {
    Read(ctx context.Context) (Observed, error)          // 探测当前实际状态
    Diff(desired, observed) Changes
    Apply(ctx context.Context, changes Changes) error
}
```

**`Read()` 是常驻 Agent 价值的来源。** 一个只能「执行」不能「观测」的资源类型无法参与漂移检测，也无法给出可信的 `status`。因此它是接口的强制部分，不是可选优化。

### v1 资源集（16 种）

| 类别 | 资源类型 |
|---|---|
| 文件系统 | `file` `template` `directory` `symlink` `archive` |
| 身份 | `user` `group` |
| 主机配置 | `sysctl` `limits` `hosts_entry` `mount` `timer` `systemd_unit` |
| 逃生舱 | `command` `script`（必须带 `unless` / `onlyif` / `creates` 守卫，否则不幂等） |
| 非 hermetic | `package`（OS 包，提供但被 `lint --hermetic` 拦截） |

> `systemd_unit` 归在**主机配置**而非工作负载：它表达的是「一个 unit 应当存在并处于某状态」（如 `Type=oneshot` 的开机任务），与 `workload.runtime: systemd`（这个角色的受监管进程）是两回事。分野见 [spec §14.4](../spec/pack-v1.md#144-主机配置)。

每种类型的 `Read` / `Diff` / `Apply` / `Remove` 具体行为见 [11-resource-engine.md §4](11-resource-engine.md#4-资源类型清单)。

### 主机配置与组件部署是同一个引擎

**没有 `workload` 的 Pack 就是纯主机配置。** 这不是巧合，是刻意设计——Ansible 式的主机管理不需要独立子系统，它只是资源引擎的一种用法。

```yaml
# 一个纯主机调优 Pack
name: host-tuning-db
roles:
  - name: apply
    cardinality: "0-N"
    resources:
      - sysctl: { key: vm.swappiness, value: "1" }
      - sysctl: { key: vm.max_map_count, value: "262144" }
      - limits: { domain: postgres, item: nofile, hard: 65536 }
    # 无 workload
```

## 3.1 调和循环

Mecharion 不是「执行一次命令就结束」，而是**持续维持一个不变量**：实际状态 == 期望状态。

```
┌────────────────────────────────────────────┐
│ ① 读取期望状态（mechd 下发的已解析规格）      │
│ ② 自动创建 paths 声明的目录                  │
│ ③ 逐资源 Read → Diff                       │
│ ④ 有差异的资源 Apply，记下它的 notify        │  ← notify 只在这里产生
│ ⑤ notify 去重后统一执行                     │
│ ⑥ 健康检查                                  │
│ ⑦ 上报观测状态                              │
└────────────────────────────────────────────┘
        ↑                             │
        └────────── 循环 ──────────────┘
```

**② 早于 ③、`shared` 早于 `role`** 是必要的——`shared` 里通常有建用户与解压载荷，角色资源依赖它们。

### 触发时机

| 触发源 | 何时 |
|---|---|
| 期望状态变化 | 用户 apply / mechd 下发新规格 → 立即调和 |
| **周期性** | 定时器到期 → **检测漂移** |
| 事件驱动（可选优化） | systemd 单元状态变化、inotify |

第二条是[常驻 Agent 相对 Ansible 的核心价值](../adr/0001-agent-based.md)：有人手工改了 `nginx.conf`、手工 `systemctl stop` 了服务——只有周期性重读才能发现。

### 检测的开销

持续调和的代价必须是可忽略的，否则它会被运维关掉——那样这个产品最核心的
能力就没了。

单轮调和读一整套资源：`stat` 文件、查 systemd、读用户库。真正贵的只有一项：
**`file` / `template` 需要文件的 sha256 才能比对内容**。一个装了十个组件、
各带 50MB 二进制的节点，不做优化就是**每分钟读取并哈希 500MB**，而绝大多数
轮次里什么都没变。

因此引擎缓存 `(size, mtime) → sha256`，把常态下的「每轮全量哈希」降为
**「每轮一次 stat」**。缓存随实例状态持久化，mechlet 重启后仍然有效。

**这里有一个必须堵住的窟窿。** 只比 `(size, mtime)` 的话，若一次改写落在
「记录摘要的同一 mtime 刻度」内且长度未变——`port: 8080` 改成 `port: 1234`
恰好同样长度，而某些文件系统的 mtime 粒度就是 1 秒——stat 结果与缓存永远
一致，**这处漂移会永久检测不到**。

git 的索引遇到过同一问题。解法是：**mtime 与记录时刻挨得太近的条目一律当作
脏的**，重算一次。引擎要求 mtime 比记录时刻早出 1 秒以上才信任缓存。
生产上这条几乎不花钱——文件是上一轮写的，下一轮在 60 秒之后。

> 需要确定性结论时走 `mechctl component verify`（强制全量哈希）。那是低频的
> 人工动作，不该让每 60 秒一次的调和为它买单。

### 三个独立的周期

它们经常被混为一谈，必须分清：

| | 默认 | 作用 | 开销 |
|---|---|---|---|
| **调和间隔** | **60s** | 重读全部资源的实际状态 | 几十毫秒（stat 文件、查 systemd、读用户库） |
| **健康检查间隔** | **15s** | 探测服务是否活着（一次 HTTP/TCP/exec） | 极小 |
| **状态上报（心跳）间隔** | **15s** | 把观测状态推给 mechd | 一个小结构 |

三者独立可配，不应共用一个数值。调和要读一整套资源，因此间隔明显更长。

> [ADR-0012](../adr/0012-mechd-embedded-sqlite.md) 估算 mechd 写入压力时用的「15 秒心跳」是**第三项**，与调和间隔无关。

## 4. 漂移策略

每个资源可声明处理方式：

```yaml
- template:
    src: nginx.conf.tmpl
    dest: "{{ .Paths.Config }}/nginx.conf"
    driftPolicy: reconcile      # report | reconcile | ignore
```

| 策略 | 行为 | 适用 |
|---|---|---|
| `report` | 发现漂移则上报并告警，**不修改** | 默认。多数场景下人应当先知道发生了什么 |
| `reconcile` | 自动改回期望状态 | 明确不允许人工干预的关键配置 |
| `ignore` | 不比对 | 应用运行时自行改写的文件 |

**默认 `report` 而非 `reconcile`** 是刻意的：自动改回一个运维人员为了救火而临时修改的配置，是比漂移本身更严重的事故。

### `driftPolicy` 只管漂移，不管期望变更

这两件事极易被混成一件，但后果完全相反：

| 情形 | 判据 | 行为 |
|---|---|---|
| **期望状态变了** | spec digest 变了 → 新 generation（或回滚） | **无条件收敛**，`driftPolicy` 不参与 |
| **期望没变、机器变了** | digest 相同 → 复用当前 generation | 这才叫漂移，按 `driftPolicy` 处理 |

若让 `driftPolicy` 一并管住第一种情形，一个默认 `report` 的 `template`
会**让所有配置变更永远发不出去**——用户改了参数，`apply` 报「已上报漂移」，
文件却纹丝不动，而且没有任何报错。

> 实现上判据就是 generation 有没有换。这也是[「配置变更同样产生新 generation」](04-paths-and-storage.md#2-generation)
> 这条设计的一个额外回报：它让「期望变了」有了一个**可判定的信号**，
> 而不需要引擎去猜某处差异的来源。

### 4.1 临时修改需要有名分

一个真实场景：运维凌晨救火，临时改了一个配置值。他现在只有两个坏选择——
改文件然后被永远报成异常，或者走一次正式变更（凌晨三点未必想干）。
**模型里缺少「我知道，是我改的，先别管」这个表达。**

```bash
mechctl component ack-drift pg-main \
    --resource template:/etc/mecharion/apps/pg-main/postgresql.conf \
    --duration 4h \
    --reason "排查慢查询，临时调 log_min_duration"
```

三条性质缺一不可：

| | 为什么 |
|---|---|
| **有期限** | 到点自动恢复告警。不会悄悄变成永久——那等于把这个组件从管理中摘掉了 |
| **有理由** | 进操作日志——现在还没有查询入口，「事后复盘查得到」是待补的能力，不是现状（[08-security §6](08-security.md#6-操作日志不是合规级审计)） |
| **仍然检测** | 只是不告警。`status` 里照常显示「已抑制的漂移 (1)，4 小时后恢复」 |

不指定 `--resource` 时抑制整个实例，用于整机维护窗口。

> **抑制不等于 `driftPolicy: ignore`。** 后者是 Pack 作者说「这文件本来就会
> 被应用改」，永久且对所有部署生效；前者是运维说「这一次是我干的」，
> 有期限、有理由、只对这个实例生效。

### 4.2 谁说了算：Pack 作者还是现场运维

`driftPolicy` 写在 Pack 里，等于**Pack 作者决定了运维现场的临时修改能不能活下来**。
这个权责关系是反的——**现场的人应该说了算**。

因此 Site / Component 级可以覆盖 Pack 声明的策略：

```
Pack 声明        →  Component 级覆盖  →  实际生效
reconcile           report               report
```

覆盖只能**放松**不能收紧（`reconcile` → `report` 可以，反过来不行）：
Pack 作者标 `report` 通常是因为那个文件本来就允许被改，站点策略没有理由
比它更激进。

落地时定下的两条（M5 第 4 步）：

- **`ignore` 不阻止首次物化，也不阻止升级。** 它只在「期望状态没变」时
  生效——那正是 §4 开头那条规则（driftPolicy 只管漂移，不管期望变更）在
  `ignore` 上的落地。早先它在读取之前就跳过了，于是一个标了 `ignore` 的
  配置文件**从来不会被创建**，而 Pack 作者标它恰恰是因为「应用自己会
  改写这个文件」——那个文件仍然得先有个初值。
- **取更松的那个，而不是「覆盖直接赢」。** 一个 Component 级的单值要同时
  作用于几十条资源：Pack 把某个文件标成 `ignore`（应用自己会改写它），
  一个 `report` 覆盖不该把它拽回来报警——那不是运维想表达的意思，
  而是单值粒度的副作用。
- **`driftPolicy` 不进 spec digest。** 否则这条命令会切一次 generation、
  把服务重启一遍。放松策略多半发生在**事故当中**，而 [§4.3](#43-自动改回不得顺带重启)
  正是在说这件事不能发生——从另一条路径犯同一个错。

### 4.3 自动改回不得顺带重启

`driftPolicy: reconcile` 与 `notify: restart` 同用意味着：**手改一个文件会让
服务在运维手底下重启**。运维只是想试个参数，服务没了。

这是最不该由工具自作主张的一类动作，因此引擎的行为是：

```
检出漂移 → 策略要求改回 → 但会连带重启 → 降级为「只上报」
                                          除非 reconcile.allowDriftRestart 显式打开
```

`mechpack lint` 对这个组合发**警告**（规则 51），提醒 Pack 作者它默认不会真的执行。

## 5. 升级与回滚

```
1. 物化新 generation（旧 generation 完整保留）
2. 渲染新配置
3. preUpgrade hook
4. Runtime.Stop
5. 原子切换 current 软链           ← 唯一的不可分割时刻
6. Runtime.Start
7. 健康检查（超时/失败进入 8）
8. 失败：切回旧软链 → Start → 上报回滚，Rollout 暂停
9. postUpgrade hook
```

因为旧 generation 目录仍在，**回滚是一次软链切换，秒级完成**。数据目录全程不被触碰。

### 参数变更如何触发动作

由 params 的运维语义字段驱动，无需用户操心：

| 字段 | Rollout 行为 |
|---|---|
| `restartRequired: true` | 变更后重启 workload |
| `reloadRequired: true` | 变更后 `Runtime.Reload`，不中断服务 |
| `immutable: true` | 拒绝变更，提示需重建 Component |
| 都未设置 | 仅更新配置文件，不动进程 |

这是 [ADR-0007](../adr/0007-params-custom-subset.md)（自定义 params 子集）的直接收益——完整 JSON Schema 表达不了这些，就只能让用户自己判断要不要重启。

## 6. Rollout 编排

多节点变更由 mechd 编排：

- **排序**：按 Role 的 `requires` 拓扑排序（ZK → JournalNode → NameNode → DataNode）
- **分批**：`maxUnavailable` / 固定批大小 / canary 首批
- **健康门禁**：每批完成后校验健康，不通过则暂停
- **暂停 / 恢复 / 回滚**：人工可介入
- **幂等**：Rollout 中断后重新 `apply` 从断点继续，不重做已完成的批次

单机形态下 Rollout 退化为「一批一个实例」，逻辑完全相同。

## 6.1 `notify` 的聚合

`notify` 表达的是：**这个资源变了，运行中的进程需要知道**。典型只有三类场景——配置文件变了需要 reload、配置变了但进程不支持热加载需要 restart、依赖的证书轮换需要 reload。

> **换 generation（升级）不走 notify。** 那是 generation 切换流程本身处理的（停服务、切软链、起服务）。notify 只服务于「generation 没变、但其中某个资源变了」。

四条规则：

| 规则 | 说明 |
|---|---|
| **收集去重，调和结束后统一执行** | 三个 template 都 `notify: reload`，只 reload 一次 |
| **`restart` 吸收 `reload`** | 同一轮里既有 restart 又有 reload，只执行 restart |
| **Diff 为空不触发** | 见下 |
| **notify 失败算调和失败** | 配置改了但服务没重载，等于变更没生效 |

### 为什么 Diff 为空绝对不能触发

调和每 60 秒跑一次。若无差异也触发 notify：

```
12:00:00  调和 → 无差异 → restart → PostgreSQL 重启（冷启动 30s）
12:01:00  调和 → 无差异 → restart → 又重启
12:02:00  ⋮
```

**服务会每 60 秒被重启一次，永远无法稳定运行。**

而且这不是「配置一下就能避免」的——`apply` 与周期性调和走**同一条代码路径**，引擎无法区分「用户主动触发」与「定时器到期」。

> **幂等是声明式模型的全部基础。** 一旦调和有了副作用，它就不能被安全地重复执行，而「安全地重复执行」正是持续调和存在的前提。

「我改了外部依赖，想强制重启一下」是**显式意图**，用显式命令：

```bash
mechctl component restart pg-main                    # 整个组件
mechctl component restart pg-main/replica@node-3     # 单个实例
```

它绕过调和逻辑直接调 `Runtime.Stop` + `Start`，不影响期望状态。

**它走的是独立的 `Tasks` 命令流**（[ADR-0038](../adr/0038-adhoc-task-channel.md)），
不是期望状态那条：重启是一个**事件**，丢一次就是没执行，而一台断连三天的
机器回来之后补做一次重启是纯粹的伤害。因此离线节点如实报告
「不可达、未执行」，不排队、不补做。

**原则：显式意图用显式命令，不要让调和循环承担「强制生效」的语义。**

## 7. 健康检查

健康检查在 Runtime 接口**之上**，跨 Runtime 共享：

```yaml
health:
  http: { path: /healthz, port: "{{ .Params.port }}", timeout: 5s }
  # 或 tcp: { port: … } / exec: { command: […] }
  startupGrace: 60s          # 启动宽限期内失败不计
  interval: 15s
  failureThreshold: 3
```

Runtime 原生的健康信息（docker HEALTHCHECK、systemd watchdog）经 `Observe()` 的 `Health` 字段汇入，不单独建立机制。

## 8. 相关决策

- [ADR-0001 采用常驻 Agent 架构](../adr/0001-agent-based.md)
- [ADR-0008 generation 不可变与 linkInto 路径调和](../adr/0008-immutable-generation-linkinto.md)
