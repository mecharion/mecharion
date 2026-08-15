# ADR-0023: 节点事实（Node Facts）——校验用实时值，渲染用固化快照

- **状态**：已接受
- **日期**：2026-08-02
- **相关**：[ADR-0009](0009-node-volumes-multidisk.md)、[ADR-0007](0007-params-custom-subset.md)、[ADR-0021](0021-config-group.md)

## 背景

大量组件的配置默认值依赖节点硬件：

| 组件 | 惯例 |
|---|---|
| Elasticsearch | 堆 = 物理内存的一半，且不超过 31GB（压缩指针边界） |
| Kafka | 堆通常 6GB，但小内存机器需下调 |
| PostgreSQL | `shared_buffers` ≈ 内存的 25% |
| HDFS DataNode | 堆随管理的块数增长；`du.reserved` 随盘容量 |

第一波示例（postgresql、hdfs）用固定默认值 + ConfigGroup 按机型分组绕过。这在同构集群可行，在异构集群会退化为「每台机器一个配置组」。

同时，`requires.resources.memory: 2GB` 这类前置校验也需要节点硬件信息，当前无处可取。

## 问题：直接暴露实时事实是危险的

若配置取值跟随实时事实：

```
节点加了内存 16GB → 32GB
  ↓ 下次调和
heap 从 8G 变成 16G
  ↓ restartRequired
服务在业务时间被重启
```

更糟：某次事实采集出 bug 报了 0 字节 → `heap=0` → 服务起不来。

**这与 [ADR-0009](0009-node-volumes-multidisk.md) 中拒绝 `volumeClass` 自动选盘是同构的风险**——自动跟随外部变化，会把「运维觉察不到的事实变动」变成「生产环境的配置变更」。

## 候选方案与调研

| 系统 | 做法 | 观察 |
|---|---|---|
| **Ansible** | `setup` 模块采集 facts，**每次运行实时采集** | 无固化概念——因为 Ansible 本就没有持久化期望状态，每次运行是独立的 |
| **Puppet** | Facter 采集，编译 catalog 时使用，**catalog 缓存但事实每次刷新** | 同样是实时 |
| **Chef** | Ohai attributes，**持久化到 node object**，可被 override | **有固化概念**，最接近所需 |
| **Kubernetes** | Node status（capacity/allocatable）用于**调度**，不用于渲染容器配置 | 只用于校验，不用于取值——与本决策的切分一致 |

Kubernetes 的做法是最有价值的信号：它有节点资源信息，但**从不用它渲染工作负载配置**，只用于调度决策。

## 决策

**引入 `.Node.Facts`，但严格区分两种用途：**

| 用途 | 数据源 | 事实变化时 |
|---|---|---|
| **判定条件**（`requires.resources.memory`） | **实时** | 不满足则快速失败——这是一次检查，不产生值 |
| **配置取值**（`defaultFrom`） | **放置时快照，固化到 RoleInstance** | 只**上报漂移**，不自动改配置 |

这个切分与既有的 `requires`（被校验）/ `params`（被渲染）划分对齐，不引入新概念。

### 参数中的用法：`defaultFrom`

事实**不直接在模板中使用**，而经由参数：

```yaml
params:
  heap:
    type: size
    defaultFrom: '{{ min (div .Node.Facts.Memory.Total 2) "31GB" }}'
    default: 2GB
    restartRequired: true
```

这样值成为一等参数：出现在 UI、可被用户覆盖、受 `restartRequired` 管理、自然获得快照固化。

新增 `defaultFrom` 而非复用 `from`，因为二者语义不同：

| 字段 | 语义 | 用户可否覆盖 |
|---|---|---|
| `from` | 部署的**客观事实**（primary 在哪台机器上） | ❌ |
| `defaultFrom` | 计算出的**初始默认值** | ✅ |

两者都用同一套模板引擎求值，不引入第二种表达式语言。`defaultFrom` 求值失败时回落到 `default` 并告警，**不中止部署**——一个采集不到内存的节点不该阻断整个 Rollout。

### 漂移呈现

复用既有的漂移机制，由人决定何时应用：

```
$ mechctl node facts diff node-7
  memory.total    渲染时: 16GB   当前: 32GB
  受影响参数: es-prod/data.heap = 8GB
  → mechctl node facts refresh node-7 --apply
```

### 自定义事实

mechlet 执行 `/etc/mecharion/facts.d/*.sh`，输出 JSON 合并到 `facts.custom`（对标 Ansible `facts.d`）。硬件 SKU、机架编号、租户标识等站点特有事实，用户不必等 Mecharion 加字段。

**不违反 hermetic 约束**——`facts.d` 是节点侧运维配置，不是 Pack 内容。

### facts 与 labels 并存

| | 来源 | 语义 | 用途 |
|---|---|---|---|
| `labels` | 用户**声明** | **意图**：我认为这台机在 r12 机架 | 节点选择、`placement` 的 `scope` |
| `facts` | mechlet **观测** | **现实**：这台机报告 32GB 内存 | `requires` 校验、`defaultFrom` 取值 |

判据：**参与放置约束的写 label，只作数据用的写 fact。**

## 理由

**核心理由：自动跟随外部变化，是这类工具最容易造成生产事故的模式。**

Mecharion 的价值主张是「状态可控、变更可预期」。一个因为运维加了内存条而在深夜自动重启的服务，恰好摧毁这个主张。快照固化把「事实变了」和「配置该变吗」拆成两个决定，后者由人做。

Kubernetes 只用节点资源做调度、不用它渲染配置，是同一个判断的另一种表达。

**次要理由：`capabilities` 并入 facts 简化了模型。** 此前 `Node.capabilities`（来自 Probe）是独立字段，但它本质就是一条事实。合并后一套采集机制、一个命名空间；`requires.capability` 只是针对它的匹配器，不受影响。

## 后果

### 收益

- 组件可以给出符合硬件的合理默认值，异构集群不必逐机配置
- 配置变更始终是人的决定，不会因硬件变动自动发生
- `facts.custom` 让站点特有信息无需等待上游支持
- `requires.resources` 有了真实数据源

### 代价

- **快照会过期**：换了内存的机器，其配置仍按旧事实。以 `diff-facts` 呈现、`refresh-facts --apply` 显式应用来缓解，但用户必须知道要去看
- **`defaultFrom` 引入了「参数值可能来自计算」的复杂度**：调试「这个值为什么是 8GB」需要追溯到事实快照。`mechctl config explain` 必须输出完整来源链（Pack 默认 → defaultFrom 求值 → 事实快照 → ConfigGroup 覆盖）
- **事实采集本身是跨平台工作量**：内存、CPU、文件系统、网卡在不同发行版与内核版本上的获取方式有差异。初期只保证 systemd + 主流发行版，异常时事实缺失而非报错
- **`facts.custom` 是任意脚本执行**：以 root 运行、无沙箱。但它是节点侧运维自己放的，与 Pack 信任模型无关；文档需明确这一点
- **facts 与 labels 的区别需要教育**：用户会问「机架应该写 label 还是 custom fact」。文档必须给出明确判据

## 参考

- Ansible `setup` / `facts.d`
- Puppet Facter
- Chef Ohai（持久化到 node object 的先例）
- Kubernetes Node capacity/allocatable（只用于调度，不用于渲染——正面参照）
