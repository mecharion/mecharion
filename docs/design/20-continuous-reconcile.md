# 持续调和与漂移处理

M5 的设计。模型见 [06-state-and-drift](06-state-and-drift.md)，
决策依据见 [ADR-0001 常驻 Agent](../adr/0001-agent-based.md)。

> **这个里程碑要兑现的是常驻 Agent 的存在理由。**
>
> Ansible 跑完就走；Mecharion 持续维持一个不变量。到 M4 为止，这句话在
> 代码里其实**还不成立**——mechlet 只在 mechd 推送期望状态时调和一次，
> 没人推就什么都不做。有人手工改了配置、手工停了服务，永远不会被发现。
>
> 因此 M5 的验收判据不是「有一个定时器」，而是：
> **手改配置 / 手停服务，一个调和周期内被检出并按策略处理，
> 且三个 Runtime 行为一致。**

---

## 1. 已经有的，不重做

M2/M3/M4 已经把资源侧铺完了。M5 不碰这些：

| 已有 | 在哪 |
|---|---|
| 资源级 Read → Diff → Apply | `internal/reconcile/reconcile.go` |
| `driftPolicy` 三态（report / reconcile / ignore） | 同上，`driftContext.decide` |
| 「期望变了」与「漂移」的判据分离（generation 换没换） | 同上 |
| 自动改回不得顺带重启（`allowDriftRestart`） | 同上 + lint R51 |
| `ack-drift` 抑制（有期限、有理由、仍然检测） | `internal/state` + mechd `AckDrift` |
| 摘要缓存 +「racily clean」防漏检 | `internal/resource/digestcache.go` |
| `notify` 聚合去重 | `internal/reconcile/notify` |
| `status` / `diff` / `ack-drift` 的 mechd 侧与 CLI | `internal/mechd` + `ctlcmd` |
| 三个周期的配置项 | `spec.ReconcileOptions` |

**缺的只有一件事，但它是根**：没有周期性触发。

## 2. 五个没有答案的问题

### 2.1 周期调和的期望状态从哪来 ★

现状：`Agent.apply` 在收到推送时调和，`a.last` 里存的是**报告**而不是规格。
周期调和需要一份本地的期望状态副本。

麻烦在密钥。按 [16-secrets](16-secrets.md)，规格里存的是哨兵
`@@m7n:secret:<id>@@`，**明文随推送一起到、用完即弃、绝不落盘**。
`spec.ResolveSecrets` 在缺明文时是硬错误，错误信息里那句
「重连后会全量重推」正是这个设计的体现。

于是：**mechlet 重启后，带密钥的实例在下一次推送之前无法自愈**——
而「重启后自己恢复」恰恰是常驻 Agent 最该做到的事。

| 做法 | 代价 |
|---|---|
| **A** 规格只留在内存 | 重启后到下次推送之间完全不调和。**断连 + 重启 = 完全不自愈**，而那正是最需要它的时候 |
| **B** 规格落盘（仍是哨兵），明文只在内存 | 无密钥的实例重启后立刻能自愈；带密钥的仍要等一次推送。**行为按实例分裂**，用户不容易预测 |
| **C** 规格落盘 + 明文进 mechlet 本地信封加密库 | 全部实例都能自愈。代价是**节点上多一份可解密的凭据副本**，且 KEK 就在同一台机器上——它把「节点被拿下」的后果从「拿到当前配置」扩大到「拿到全部凭据」 |

**决定：C**（[ADR-0033](../adr/0033-mechlet-local-desired-state.md)）。

选它而不是 B，关键在**可预测**：B 让「重启后会不会自愈」取决于这个组件
恰好有没有 `type: secret` 参数，而那不是用户脑子里会有的分类。断连 + 重启
是最坏的组合，也正是 A / B 都失效而 Agent 最该顶用的时候。

```
<data-dir>/desired/<component>-<role>.json   已解析规格（含哨兵，无明文）
<data-dir>/vault/                            DEK 加密后的密钥值
<data-dir>/kek                               KEK，0600
```

信封机制**复用 `internal/vault`**，与 mechd 侧同一套实现——两套信封意味着
两套边界条件，其中一套的测试覆盖必然更薄。

> **代价是明确接受的，不要在文档里含糊过去**：节点上多了一份可解密的凭据
> 副本，KEK 就在同一台机器上。这把「节点被拿下」的后果从「拿到落盘的配置」
> 扩大到「拿到该节点全部组件的凭据明文」。信封加密在这里保护的边界只有
> **磁盘被离线取走**一种；它挡不住在线的 root。理由与完整权衡见 ADR-0033。

### 2.2 workload 漂移：意图还是漂移 ★

有人 `systemctl stop nginx`、有人 `docker rm` 了容器。`driftPolicy` 是**资源级**
的，workload 上没有这个字段。

| 做法 | 代价 |
|---|---|
| **A** 一律拉起 | 运维为了维护停掉服务，60 秒后被工具拉起来——**和现场的人打架**，比漂移本身更糟 |
| **B** 一律只上报 | 「进程挂了会自动恢复」这条最基本的期待落空，等于把 Agent 降级成监控 |
| **C** 引入显式的期望运行态（`running` / `stopped`），漂移 = 实际 ≠ 期望；同时实现 `mechctl component stop` / `start` | 工作量最大，但这是唯一说得通的模型：**判据是有没有人表达过意图** |

**决定：C。** `component stop` / `start` 本来就在
[CLI 动词表](10-cli.md#43-component)里，M5 是它们自然的位置——
没有它们，A 和 B 都只是在两种坏结果之间挑一个。

```
desired: running | stopped | removed   每个角色实例一个，存在 mechd 侧

没人说过话          → 观测到 Stopped   = 漂移 → 恢复
component stop      → desired=stopped        → 不再拉起
desired=stopped 却在跑 → 也是漂移            → 停回去
component remove    → desired=removed        → 卸载（M9）
```

第三个值 `removed` 是 M9 加的，同一个模型的自然延伸：
**「这个实例不该存在」与「这个实例不该在跑」是同一类陈述**。
把卸载做成状态而不是指令，正是为了守住本节的那条纪律——
指令是事件，丢一次就永远丢了；状态可以重复确认。
见 [24-lifecycle-completion §2.1](24-lifecycle-completion.md)。

**第三行不能省。** 只做「停了就拉起」而不做「不该跑的要停掉」，
`component stop` 就成了一句没人执行的声明——有人手工把它启动起来，
系统会一直默认那是对的。

还有一层必须分清：**Runtime 自己的重启策略要先让路**。systemd 的
`Restart=always` 与 docker 的 `--restart` 已经会把崩掉的进程拉起来。引擎要
区分「进程崩了但 Runtime 正在处理」与「被人停掉了」——前者每 60 秒插一脚
只会互相干扰。判据是观测到的状态：`Starting` / `restarting` 属于前者，
`Stopped` 才是后者。

### 2.3 `driftPolicy` 的 Component 级覆盖

[§4.2](06-state-and-drift.md#42-谁说了算pack-作者还是现场运维) 定了
「现场的人说了算，覆盖只能放松不能收紧」，`00001_init.sql` 的列注释也写了，
**但没有实现**。M5 补齐：覆盖存在哪、放松的判定、以及 API 侧拒收收紧。

放松序：`reconcile` → `report` → `ignore`。反向一律拒绝。

### 2.4 孤儿实例

`reportOrphans` 目前只写一条 warn 日志。M5 把它升级为**进 status**，
但**仍然不自动删**——理由与 M3 时相同：「mechd 少发了一条」与「用户真的
删了这个组件」在这一层分辨不出来，而卸载不可逆。真正的移除走
`mechctl component remove`。

### 2.5 单机形态怎么持续调和

`mechlet apply -f` 是一次性的。**不为它新增一个「守护模式」**：单机形态
在 M3 已经有答案了——`mechlet install --standalone` 把 mechd 与 mechlet 都
装在本机，走的是同一条推送 + 周期调和的路。`apply -f` 保持一次性，
它是调试与验收的入口，不是运行形态。

## 3. 周期调和循环长什么样

```
                 ┌──── 推送（期望变了）────┐
                 │                        ↓
  ┌──────────────┴───────────┐   ┌────────────────┐
  │ 定时器（reconcile.interval）│──→│  一次调和      │
  └──────────────────────────┘   │  ①…⑦（06 §3.1）│
                 ↑                └───────┬────────┘
                 └──────── 循环 ───────────┘
```

三条纪律：

1. **推送与定时器不能并发跑同一个实例。** 一把每实例的锁，后来者跳过而不是
   排队——排队会在慢调和时堆出一串已经过期的任务。
2. **一轮里一个实例失败不影响其它实例。** 失败记进报告，下一轮照常重试。
3. **周期调和不产生新 generation。** 它用的是落盘的那份期望状态，digest 没变
   就是复用——这正是 `driftPolicy` 生效的前提（06 §4）。

## 4. 验收判据具体化

「手改配置，一个周期内被检出并按策略处理（三个 runtime 一起过）」拆成：

| # | 动作 | 期望 | systemd | docker | compose |
|---|---|---|:--:|:--:|:--:|
| 1 | 手改一个 `driftPolicy: report` 的配置文件 | 一周期内 status 显示漂移，**文件不被改回** | ✓ | ✓ | ✓ |
| 2 | 手改一个 `driftPolicy: reconcile` 的配置文件 | 一周期内被改回 | ✓ | ✓ | ✓ |
| 3 | 手改一个 `driftPolicy: ignore` 的文件 | 不报也不改 | ✓ | ✓ | ✓ |
| 4 | 手工停掉服务 / 删掉容器 | 一周期内恢复 | ✓ | ✓ | ✓ |
| 5 | `component stop` 之后手工启动 | 一周期内被停回去 | ✓ | ✓ | ✓ |
| 6 | `ack-drift` 之后手改 | status 显示「已抑制」，不告警 | ✓ | — | — |
| 7 | 抑制到期后 | 重新告警 | ✓ | — | — |

第 4 条在三个 Runtime 上是三件不同的事（`systemctl stop` / `docker rm` /
`compose down`），而上层代码必须一份——**这是 M4 那个接缝的第二次验收**。

## 5. 实施顺序

| # | 内容 | 可验证的成果 |
|---|---|---|
| 1 | 期望状态落盘 + 周期调和循环 | 不推送也会调和，重启后仍然调和 |
| 2 | 期望运行态 + `component stop` / `start` | 停了就不会被拉起来 |
| 3 | workload 漂移检测与恢复 | 手停/手删，一周期内恢复（三个 runtime） |
| 4 | `driftPolicy` 的 Component 级覆盖 | 收紧被拒，放松生效 |
| 5 | 孤儿实例进 status | 手工造一个孤儿，status 里看得到且**没被删** |
| 6 | `status` / `diff` 的漂移视图补齐 | 上表 1–3 在 CLI 上看得出来 |
| 7 | 三个 Runtime 的漂移 e2e | 上表整张过 |
| 8 | 抑制到期恢复 + 审计 | 上表 6–7 |

第 1 步先行，是因为其余七步全都要靠它才**观察得到**——没有周期触发，
任何漂移都要靠手工再 apply 一次才现形，那测的就不是漂移检测。

## 6. 相关决策

- [ADR-0001 常驻 Agent](../adr/0001-agent-based.md) ·
  [ADR-0002 mechlet 不做判断](../adr/0002-mechlet-as-sole-engine.md)
- [06-state-and-drift](06-state-and-drift.md) ·
  [11-resource-engine](11-resource-engine.md) ·
  [16-secrets](16-secrets.md)
