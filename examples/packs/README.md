# Pack 示例

这些示例的目的**不是**教学，而是**验证 [pack/v1 规范](../../docs/spec/pack-v1.md)**。

规范单靠推演一定有洞。写出真实组件的完整 Pack，是发现「表达不了」「表达得很别扭」「两种写法都对」的唯一有效手段。因此每个示例都配一份 README，记录它**压测了规范的哪些部分**、以及**暴露了什么问题**。

> 这些示例在 pack/v1 冻结前留在核心仓。冻结后官方 Pack 迁往 [`mecharion/packs`](https://github.com/mecharion/packs)，本目录只保留用于规范验证的最小集。

## 第一波（定型规范骨架）

| 示例 | 层级 | 压测什么 |
|---|---|---|
| [go-webapp](go-webapp/README.md) | L1 | **最小表面积基线**——若它超过 30 行，说明规范有冗余 |
| [postgresql](postgresql/README.md) | L2 | 多角色、拓扑引用、config 位于 data 内、双形态、`scope: once` |
| [hdfs](hdfs/README.md) | L3 | 三形态 profile、Pack 间依赖、模板片段组合、集群级一次性动作、复杂亲和性 |

## 第二波（压测边角）

| 示例 | 层级 | 压测什么 |
|---|---|---|
| [kafka](kafka/README.md) | L3 | profile 轴是**架构**而非规模（KRaft combined / separated / ZooKeeper）；`defaultFrom` 首个用例 |
| [elasticsearch](elasticsearch/README.md) | L2 | **三处 `linkInto`**（config/plugins/logs）、`defaultFrom` 最典型场景、主机配置与组件同 Pack |
| [minio](minio/README.md) | L2+ | **拓扑 × 多盘的组合渲染**——一口气打出三个规范缺口 |
| [nginx](nginx/README.md) | L1 | 全参数 `reloadRequired` 的「零重启组件」、静态链接、第二个 L1 基线 |

## 第三波（提供方与非组件形态）

| 示例 | 层级 | 压测什么 |
|---|---|---|
| [zookeeper](zookeeper/README.md) | L2 | **`.Requires` 的另一端**：被依赖方该怎么写；仲裁语义 |
| [jdk11](jdk11/README.md) | L0 | **无 workload / health / hooks / profiles / 模板**的最简形态；`scope: node` 提供方 |
| [host-tuning](host-tuning/README.md) | — | **无 blobs、无进程**的纯主机配置；profiles 作为调优预设 |
| [docker](docker/README.md) | L1 | **闭环**：用 systemd runtime 装出 docker/compose runtime 要用的 dockerd；第一个用 `preInstall` 的 Pack |

## 第四波（消费方视角）

| 示例 | 层级 | 压测什么 |
|---|---|---|
| [java-webapp](java-webapp/README.md) | L2 | **凭据怎么跨 Pack 传递**；`scope: node` 与 `scope: site` 同时出现在一个 Pack 里 |

### 第四波产出的规范变更

起因是一个具体问题：**go-webapp 要用 PG 的口令，而口令是受保护的，依赖机制怎么把它送过去？**

| 发现 | 来源 | 处理 |
|---|---|---|
| **`exports` 装不下凭据**——原有字段集只能拼出 `host:port` | java-webapp | 新增 **`exports.fields`**：导出具名字段，消费方自行组装（[§5.4](../../docs/spec/pack-v1.md#54-exportsfields--具名字段与凭据)） |
| **让提供方拼连接串是错的**——PostgreSQL 不可能知道下游要 libpq DSN、JDBC URL 还是 Spring 的三个分开字段 | java-webapp | `format` 保留给形状确定的地址列表（ZK/Kafka）；带凭据的一律走 `fields` |
| **没有机制阻止消费方读提供方的参数**——`requires.pg.params.admin_password` 原本合法，等于把 superuser 口令交给下游 | java-webapp | 新增**规则 49**；`.Requires.<pack>.Params` 不存在。要被消费的凭据须由提供方显式导出，且应当是专为该依赖关系存在的账号 |
| **口令要运维在提供方与消费方各填一次**，轮换时是灾难 | postgresql | 新增 **`params.generate`**（[§7.6](../../docs/spec/pack-v1.md#76-generate--引擎生成的密码)）：引擎生成一次并固化。**无人值守与离线由此同时成立** |
| **消费方可能忘了把接来的值标 `secret`**，口令随之进入日志与 UI | java-webapp | lint **做不到**——它只看得见一个 Pack，而依赖方可能来自别处、单独发布。改由 mechd 在绑定时**自动传播**敏感标记（不可遗忘），并提示作者补声明 |
| 导出字段是否携带凭据，消费方作者无从得知（手里没有提供方源码） | java-webapp | 敏感标记由「引用的参数是不是 secret」**推导**得出，`mechpack inspect` 展示。推导而非声明 → 不可能与实际不一致 |

### 第三波产出的规范变更

| 发现 | 来源 | 处理 |
|---|---|---|
| **消费方在伸手进提供方的内部**——硬编码其角色名与端口拼装方式，双方都不知道存在这层耦合 | zookeeper | 新增顶层 **`exports`**：提供方声明具名连接点，消费方用 `requires.zookeeper.exports.client`（[§5.3](../../docs/spec/pack-v1.md#53-exports--对外导出的连接点)）。hdfs / kafka 已同步改用 |
| **仲裁语义只有 Pack 作者知道**——用户设 `maxUnavailable` 时无从判断能同时下线几个实例，3 节点 ZK 并发度 2 的滚动重启会直接失去多数派 | zookeeper | 新增角色级 **`quorum: true`**：偶数实例告警；**Rollout 强制 `maxUnavailable ≤ (N-1)/2`**（[§11](../../docs/spec/pack-v1.md#quorum)）。已回填 hdfs `journalnode`、kafka `controller`/`combined` |
| **`systemd_unit` 资源在规范中缺失**——设计文档列了它，规范 §14 从未包含 | host-tuning | 补入，并写清与 `workload` 的分野：前者是「unit 应存在并处于某状态」，后者是「这个角色的受监管进程」 |

## 第二波产出的规范变更

| 发现 | 来源 | 处理 |
|---|---|---|
| **无法放置裸二进制载荷**——`archive` 要归档、`file.source` 只能引用 `files/` | minio | `file` 增加 `blob` 字段，与 `content`/`source` 互斥（[§14.2](../../docs/spec/pack-v1.md#142-文件系统)） |
| **pack.yaml 字段无法调用模板片段**——复杂表达式只能内联进 `systemd.exec` 单行 | minio | pack.yaml 字段表达式与 `templates/` **共享同一 template set**（[§9.1](../../docs/spec/pack-v1.md#91-引擎)） |
| **拓扑条目不携带已解析路径**——节点挂载点可以不同，不能用本机路径推断对端 | minio | `.Topology.Role(…)[i].Paths.*` 暴露该实例自己解析出的路径（[§9.3](../../docs/spec/pack-v1.md#93-topology-引用)） |
| `topology.ordinal` 的作用域易被误读为全 Component 唯一 | kafka | 规范加重措辞并给出按角色加偏移的惯用法 |
| **`scope: once` 与「集群级唯一」被混淆**——KRaft 每个节点都要 format，唯一性在 `cluster_id` 参数而非动作 | kafka | 规范加入 HDFS/Kafka 对照表与判据（[§16.4](../../docs/spec/pack-v1.md#164-scope--集群级一次性动作)） |

## 已关闭的未决问题

| 问题 | 结论 |
|---|---|
| `.Node.Facts.*` | ✅ **做**。elasticsearch 的 `min(memory/2, 31GB)` 证明固定默认值几乎必然是错的。校验用实时值、渲染用固化快照（[ADR-0023](../../docs/adr/0023-node-facts.md)） |
| 跨 Pack 路径引用 | ✅ **做**。`.Requires.<pack>.*`，依赖分 `scope: node` / `site`（[ADR-0024](../../docs/adr/0024-cross-pack-reference.md)） |
| `properties` / `xml_properties` 资源类型 | ❌ **不做**。三个数据点：HDFS(XML) 痛感明显、Kafka(`.properties`) 无痛感、ES(YAML) 无痛感。只对一种格式有明显收益，不值得进核心资源集——Hadoop 系用模板片段组合已能解决 |
| Pack 内测试声明 `tests:` | ❌ **不做**。正确性验证在 `mechpack lint` 阶段完成 |

## 第一波产出的规范变更

| 发现 | 处理 |
|---|---|
| **集群级一次性动作无法表达**——HDFS 的 `namenode -format`、`-initializeSharedEdits` 只能在一个实例上跑一次，但 hooks 是每实例执行的 | 新增 `hooks[].scope: once`（[spec §16.4](../../docs/spec/pack-v1.md#164-scope--集群级一次性动作)） |
| hooks 只能声明在 Pack 顶层，多角色 Pack 无法为不同角色声明不同 hook | hooks 可声明在 `roles[].hooks` |
| 同一生命周期点需要多个 hook（format → initializeSharedEdits） | hooks 支持列表形式 |
| **主备身份是运行时状态，不是部署时状态**——PG 故障切换后 Mecharion 的角色归属会与实际不符 | 不改规范。在 postgresql 示例中明确记录这一边界，见其 [README](postgresql/README.md#已知边界主备身份不由-mecharion-建模) |
| 多盘场景需要「为每块盘建目录」，而 `resources` 无迭代机制 | **`paths` 声明的目录由引擎自动创建**（[spec §8.3](../../docs/spec/pack-v1.md#83-字段)）。连带结论：`resources` **不需要**迭代机制——多盘是唯一会产生该需求的场景，已被 `paths` 吸收 |
| `requires` 引用的角色在某 profile 中被禁用时语义未定义 | 规范明确：**被禁用角色上的依赖自动忽略** |
| 跨 Pack 路径引用靠约定（`…/apps/jdk/current`），隐含「Component 名等于 Pack 名」 | 新增 `.Requires.<pack>.*`，并区分 `scope: node`（暴露 `.Paths`）与 `scope: site`（只暴露 `.Topology`） |
| 依赖同一 Pack 的多个大版本（jdk11 / jdk17）需要消歧 | **大版本进 Pack 名**，`alias` 机制因此不需要。仅保留「一个 Site 有多个同 Pack Component」时的显式绑定 |
| 跨大版本升级不能靠「换 generation、保数据目录」完成（PG 16→17 需 pg_upgrade） | 新增 `upgradePolicy.compatible`，引擎拒绝跨界升级——**避免了为此无限拆包** |
| 示例文档中路径写成 Pack 名，未说明其实是 Component 名 | 文档修正；并明确**路径在首次物化时固化，之后不可变**，检测到不一致时拒绝调和而非自动迁移 |


## 校验

```bash
mechpack lint --hermetic --strict examples/packs      # 全部
mechpack lint --hermetic examples/packs/hdfs          # 单个
mechpack inspect examples/packs/zookeeper
```

CI 强制这些示例通过 `--hermetic --strict`（警告也算失败）——它们是规范用法的表率。
`internal/pack` 的测试中也有一份 `TestExamplesPassLint` 做同样的断言。

## lint 实现后在示例中查出的真实缺陷

规则实现出来跑第一遍时，示例里被抓出 5 个**真错误**、3 个警告。它们都是写文档时想当然的产物：

| 缺陷 | 规则 | 实质 |
|---|---|---|
| hdfs 的 `replication`、kafka 的 3 个参数、zookeeper 的 `autopurge_snap_retain` 标了 `reloadRequired` | R25 | **这些组件根本没有 reload 机制**——HDFS/Kafka/ZK 都不响应 SIGHUP，配置只在启动时读取。三个 Pack 都没声明 `execReload`，改配置会「reload 了但什么也没发生」。已全部改为 `restartRequired` |
| hdfs / kafka 的 3 条 `required` 放置约束缺 `reason` | R13 | `reason` 会出现在校验失败的错误信息里，缺了运维就只能看到「违反了 antiAffinity 约束」这种无法行动的报错 |

R25 这一条尤其说明了[为什么 params 要用自定义类型子集](../../docs/adr/0007-params-custom-subset.md)——`reloadRequired` 是引擎行为而非 UI 装饰，因此它可以、也应该被静态校验。

示例中的 `blobs[].sha256` 为占位值（`0000…`），因此 `mechpack assemble` 与 `sign` 无法在其上运行——它们只用于校验**结构与模板**。
