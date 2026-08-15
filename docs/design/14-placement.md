# 放置

把「用户说要装在哪些节点上」变成一份 **RoleInstance 列表**，并校验它没有违反
Pack 声明的约束。

> **Mecharion 没有调度器。** 节点由用户显式指定，`placement` 是**校验规则**
> 而非调度输入（[spec §12](../spec/pack-v1.md#12-placement--放置约束)）。
> 这条边界让整个环节从「约束求解」降为「约束检查」——前者是 NP 问题且失败时
> 无从解释，后者是遍历且失败时能精确指出是哪两个实例冲突。

---

## 1. 输入

```bash
mechctl component deploy hdfs --profile ha \
    --role namenode=n1,n2 \
    --role journalnode=n1,n2,n3 \
    --role datanode=n4,n5,n6
```

或在 `site.yaml` 里声明同样的内容。缺省规则：

| 情形 | 行为 |
|---|---|
| 只给 `--nodes n1,n2` | 所有 `cardinality` 允许的角色都放在这些节点上 |
| 某角色未指定且 `cardinality` 下限为 0 | 该角色**不部署** |
| 某角色未指定且下限 ≥ 1 | **拒绝**，列出还缺哪些角色 |

## 2. 输出

```go
type RoleInstance struct {
    Component string
    Role      string
    Node      string
    Ordinal   int      // ★ 一次分配后固化，见 ADR-0028
    ConfigGroup string
}
```

## 3. ordinal 的分配

**这是本环节唯一有副作用的部分**，也是最容易做错的部分。

```
新实例   → 该角色当前最大 ordinal + 1（首个为 0）
已有实例 → 原样保留
被移除   → 序号不回收，留下空洞
```

序号**与节点名、成员集合都无关**。按名字排序会让一次扩容改掉所有已有节点的
身份（ZooKeeper 的 `myid`、Kafka 的 `node.id`），集群当场损坏——
详见 [ADR-0028](../adr/0028-stable-ordinals.md)。

因此放置的签名是：

```go
func Compute(in Input) (*Plan, error)   // Input.Existing 必填
```

**必须带上 `Existing`**。一个只看期望节点列表的实现是不可能正确的。

分配本身**不在这里发生**：`Plan.Add` 中的 `Ordinal` 为 `-1`，真正的取号在
提交时由 `store.InstanceRepo.Ensure` 完成——它把「查已有 / 取最大序号 / 插入」
放进同一个事务。拆开会有竞态，两个并发放置请求可能拿到同一个序号
（[07-persistence §1.5](07-persistence.md#15-存储访问必须走接口)）。

> 校验的对象是**放置后的全部实例**（`Keep + Add`），不只是新增的那些。
> 只看新增会漏掉「新加的 SNN 与已有的 NN 撞在一起」。

## 4. 校验

四类，全部在放置阶段完成，**任何一条不过就拒绝，不做部分部署**。

### 4.1 cardinality

```
放置校验失败: hdfs-prod
  角色 namenode 要求 cardinality "2"，实际给了 3 个节点: n1, n2, n3
```

### 4.1b 节点列表不得重复

同一角色的节点列表里出现重复（`--role server=n1,n1`）直接拒绝。

不拦的话后果是**静默的不一致**：cardinality 按 2 算过了关，提交时
`InstanceRepo.Ensure` 却因为已存在而只建出 1 个——计划说两个、实际一个，
而没有任何地方报错。

> 由此还得到一个推论：**`antiAffinity` 单角色 + `scope: node` 是结构上恒真的**。
> `(component, role, node)` 在存储层就唯一，重复列表又被这条拦掉，同角色的
> 两个实例根本落不到同一台机器上。它真正有意义的是 `rack` / `zone` 这类 scope。
> 仍然保留这条检查，是为了在模型将来变化时不至于无声失效。

### 4.2 placement 约束

按 `scope` 分组后逐条检查（[spec §12](../spec/pack-v1.md#12-placement--放置约束)）：

| 形式 | 检查 |
|---|---|
| `antiAffinity` 含 2+ 角色 | 两两不得落在同一 scope |
| `antiAffinity` 含 1 角色 | 该角色的多个实例不得落在同一 scope |
| `affinity` 含 2+ 角色 | 必须落在同一 scope |

`scope` 是 `node` 或任意 Node label key。**节点缺少该 label 时**：
`required` 报错（无法验证不等于通过），`preferred` 告警。

错误信息必须指名到实例，并带上 Pack 作者写的 `reason`——那是现场唯一能解释
「为什么不让我这么放」的东西：

```
放置校验失败: hdfs-prod
  约束  antiAffinity[namenode, secondarynamenode]  scope=node  (required)
    namenode           → node-1
    secondarynamenode  → node-1    ← 冲突
  原因: SNN 与 NN 同节点时无法承担元数据恢复职责
```

### 4.3 requires.packs 的 scope

| scope | 检查 |
|---|---|
| `node` | 本 Component 的**每个**实例所在节点上，被依赖 Component 也必须有实例 |
| `site` | 同 Site 内存在满足版本约束的 Component 即可 |

`node` 依赖失败的错误要说清是**哪台机器**缺什么：

```
放置校验失败: java-webapp
  依赖 jdk11 (scope: node) 在下列节点上缺失: n5, n6
  → 先在这些节点上部署 jdk11，或把 java-webapp 的实例移到已有 jdk11 的节点
```

### 4.4 requires.capability / os / resources

`capability` 在放置阶段查 Node 上报的能力；`os` / `resources` 留到 mechlet
物化前查（[spec §5](../spec/pack-v1.md#5-requires--前置条件)）——前者 mechd 知道，
后者只有节点自己知道。

## 5. quorum 与 Rollout 的耦合

角色声明 `quorum: true` 时，放置阶段额外做两件事：

- 实例数为偶数 → **告警**（不拒绝，用户可能是临时状态）
- 把 `maxUnavailable ≤ (N-1)/2` 写进 Rollout 的约束

第二条是放置阶段唯一影响后续流程的输出。它必须在这里算，因为只有此刻才同时
知道「角色声明了仲裁」和「实际有几个实例」。

## 6. 变更时的差异

重新 deploy 一个已存在的 Component 时，放置产出三个集合：

```
保留  existing ∩ desired    → ordinal 不变
新增  desired  - existing   → 分配新 ordinal
移除  existing - desired    → 走 remove 流程，ordinal 不回收
```

**移除是危险动作**，因此：

| | |
|---|---|
| 默认 | 拒绝，提示用哪个 flag |
| `--purge-data` | 连数据一起删 |
| 保留数据 | 卸载工作负载与配置，数据目录留着 |

这与 `deploy` 默认拒绝覆盖已有 Component 是同一条纪律：
**缩小规模必须是显式意图**，不能是「我少写了一个节点名」的后果。

## 7. 相关决策

- [ADR-0028 ordinal 一次分配后固化](../adr/0028-stable-ordinals.md)
- [ADR-0020 放置约束](../adr/0020-placement-constraints.md)
- [spec §12 placement](../spec/pack-v1.md#12-placement--放置约束)
