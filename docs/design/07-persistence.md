# 持久化

mechd 与 mechlet 的存储需求相差三个数量级，因此采用**不同的方案**。这不是不一致，是按需选型。

## 1. mechd

### 1.1 存什么

| 类别 | 特征 |
|---|---|
| 期望状态（Site / Component / Role / RoleInstance / Node） | 小、读多写少、**需要多维度筛选** |
| 观测状态（心跳、健康、各节点上报） | 高频 upsert，只保留最新 |
| 事件与审计 | 追加为主、按时间范围查询、需保留策略 |
| Rollout 状态机 | 小、频繁更新 |
| Pack 元数据、RBAC、Token | 小 |

**blob 绝不进数据库**：

```
/var/lib/mecharion/blobs/sha256/ab/ab12ef…   ← 内容寻址文件，200MB 的 JDK 在这
/var/lib/mecharion/mechd.db                  ← 只存元数据与引用计数
```

### 1.2 规模估算

1000 节点 × 15 秒心跳 ≈ **66 写/秒**，全是小行 upsert。这个量级对任何候选都不构成压力。

**因此写吞吐不是选型依据——查询灵活性与可运维性才是。**

> **单机形态同样使用 SQLite。** 边缘单机是 mechd + mechlet 同机部署（[ADR-0026](../adr/0026-standalone-runs-mechd.md)），因此存储只有一份实现。曾考虑给单机做文件存储（数据规模确实只有几十条记录），放弃的理由不是性能，而是「两个 `Store` 实现＝两套代码路径」，以及**审计是功能需求而非性能需求**——按用户/时间/组件过滤是合规基本要求，JSONL 扫文件不能算实现了审计。

### 1.3 选型：嵌入式 SQLite

**`modernc.org/sqlite`（纯 Go 转译版，无 cgo）+ `sqlc` + `goose`**

三条决定性理由：

**① Web UI 的列表页会杀死 KV 方案。** 「筛选 site=X 且 label role=db 且 health!=OK，按最后心跳排序，分页」——SQL 一行；bbolt 里要手写并维护复合索引，每加一个筛选维度就要改索引和回填逻辑。可视化界面是产品的一等入口，这类查询会有几十个。

**② 现场可诊断性。** 边缘机器出问题、没有网络，远程指导运维执行 `sqlite3 mechd.db "select * from nodes where …"` 当场看清楚。KV 存储需要先分发一个版本匹配的 dump 工具（[原则七](00-overview.md#原则七现场可诊断)）。

**③ 必须是 `modernc.org/sqlite` 而非 `mattn/go-sqlite3`。** 需要交叉编译到 linux/amd64 与 linux/arm64，cgo 交叉编译会持续制造痛苦，还会破坏「单静态二进制」这个整个产品依赖的性质。纯 Go 版性能约为 cgo 版的 1/2~1/4，在 66 写/秒面前完全无关。

> 完整候选对比（bbolt / SQLite / go-memdb+raft / Badger）见 [ADR-0012](../adr/0012-mechd-embedded-sqlite.md)

### 1.4 工程约定

| 约定 | 原因 |
|---|---|
| 开启 WAL 模式，`busy_timeout=5000` | 允许 1 写 + N 读并发 |
| **写操作收敛到单个 goroutine / 单连接**，读用独立连接池 | SQLite 单写者，彻底避免 `SQLITE_BUSY` |
| params/values 存 JSON 列，结构化字段单独建列 | 兼顾灵活与可查询（JSON1 可查进去） |
| 迁移用 goose，SQL 文件 `go:embed` 进二进制 | 离线环境下不能依赖外部迁移工具 |
| 访问层用 sqlc，**不用 GORM** | ORM 在「对象图 + 大量条件查询」场景下持续添堵 |
| 备份用 `VACUUM INTO` | 一条命令拿到一致性快照，不用停服 |
| 事件/审计单独表 + 保留策略定期清理 | 增长最快，避免拖累主表 |
| 未来的指标历史另开存储 | 不塞进主库 |

### 1.5 存储访问必须走接口

```go
type Store interface {
    Sites() SiteRepo
    Components() ComponentRepo
    Nodes() NodeRepo
    // …
}
```

sqlc 生成的 Querier 之上再包一层 domain repository。这是**为 HA 预留的唯一必要抽象**——将来换实现时业务逻辑不用动。

它还有一个当下就成立的理由：**类型转换只发生一次**。SQLite 没有原生的时间、
JSON 与可空整型，sqlc 生成的行结构里全是 `string` / `int64`。把它直接端到
业务层，意味着每个调用点各自解 JSON、各自 parse 时间——那种重复迟早会出现
两种解法。领域类型（`store.Node` 等）与行结构刻意分开，转换收在一处。

`InstanceRepo.Ensure` 值得单独一提：它把「查已有 / 取最大序号 / 插入」
放进**同一个事务**。拆成两步会有竞态——两个并发的放置请求可能拿到同一个
序号，而 ordinal 一旦写错就是集群身份错乱（[ADR-0028](../adr/0028-stable-ordinals.md)）。

#### 跨多个 repo 的事务：走 context，不改签名

`Deploy` 要写的东西横跨 Component、Instance、Facts、依赖 Bindings 四类，
此前分四次独立提交——任何一步失败，前面已经写完的部分留在库里不会退回。修法不是给每个 repo 方法加一个
`tx` 参数：渲染管线在 mechd 内部有十几处调用点，绝大多数根本不关心事务，
逐一改签名的代价和出错面都不成比例。

**`Store.InTx` 把活跃事务挂在 `context.Context` 上而不是把 `*sql.Tx` 交给
调用方**：

```go
func (s *Store) InTx(ctx context.Context, fn func(context.Context) error) error
```

`wq(ctx)`/`rq(ctx)` 内部认出 ctx 里挂着的事务就改用它，认不出就照旧走各自
的连接——因此**这条路径以外的任何调用方，行为一字不变**。`Service.Deploy`
只需要把 ctx 换成 `InTx` 回调给的那个，`persistComponent`、`ensureInstances`、
`freezeFacts`、`renderComponent`（连同它内部顺手固化依赖绑定的那次
`Bindings().Create`）全部原样调用，自动全部并入同一个事务。

**可安全嵌套**：ctx 里已经挂着事务时，`InTx` 直接在那个事务里跑 fn，不再
`BeginTx`。这不是可选的优化——`s.write` 只有一个连接
（[§1.4](#14-工程约定)），嵌套开事务会卡在等一个永远不会被外层放出来的
连接上。`InstanceRepo.Ensure`、`Repos.ClaimNode`（[ADR-0034](../adr/0034-node-join-and-identity.md) 的 Join 原子化）这类自带 `InTx`
的方法因此既能独立调用，也能被 Deploy 这样更大的事务包住而不用改代码。

**代价与一个真实踩过的坑**：`internal/vault` 与 mechd 主库共用同一个
SQLite 数据库（它直接 `import` 了 sqlcgen，两者本就不是完全隔离的两个
包），此前把自己的 Querier 固定绑死在写连接池上。Deploy 的事务一旦持有
那唯一的写连接，渲染管线里生成新密钥去问 Vault、Vault 再去问连接池要
一个新连接——池子只有一个，被 Deploy 自己攥着，直接死锁。用真实的
`-timeout` 验证过，goroutine dump 精确指到 `vault.(*Vault).Generate`
卡在 `database/sql.(*DB).conn`。修法是让 Vault 也认得 ctx 里的事务
（`Store.WriteQueries(ctx)`，`wq` 面向包外的版本），跟 Deploy 共用同一个
连接而不是另开一个——这也顺带把「生成了新密钥但事务回滚」的情形变得
无害：那条密文从未被任何规格引用过，是一个孤儿，与 [ADR-0034](../adr/0034-node-join-and-identity.md)
里「签发了但没被用上的证书」同一个道理。**这条边界只在这两个包之间
成立**：`internal/vault` 是唯一一个绕过 `store.Repos` 接口、直接碰
sqlcgen 的外部包，因此也是唯一需要跟着 `wq`/`rq` 一起演进的。

#### 生成物提交进仓库

```bash
make tools     # 装 sqlc（只有改 SQL 的人需要）
make sqlc      # 由 internal/store/queries 重新生成
make sqlc-check   # 改了 SQL 却忘了重新生成时报错
```

离线环境下构建不能依赖再跑一次生成，因此 `internal/store/sqlcgen/` 是提交
进仓库的——CI 与普通开发者都不需要装 sqlc。

> **`queries/*.sql` 必须是纯 ASCII。** sqlc 按字节偏移剥离注释，多字节字符
> 会让偏移错位、**静默**生成出被截断的 SQL（实测产出过 `INSEid`、`GetC`
> 这样的标识符，而退出码是 0）。本项目其余地方一律用中文注释，因此这条极易
> 被无意破坏——由 `TestQueryFilesAreASCII` 守着，每条查询的中文理由写在
> Go 侧的仓储层。schema（`migrations/*.sql`）不受影响，走的是另一条解析路径。

### 1.6 表结构概要

按生命周期分四组。**期望状态与观测状态严格分开**——前者是用户的意图，
必须备份；后者随时可以从节点重新采集，丢了不心疼。

**期望状态**（备份的核心）

```
sites            id, name, kind, labels, created_at
nodes            id, site_id, name, address, labels, roots, volumes, status
components       id, site_id, name, pack_name, pack_version, revision,
                 profile, params_json, drift_policy_override
config_groups    id, component_id, role, name, members_json, params_json
role_instances   id, component_id, role, node_id, ordinal, config_group_id
                 ★ ordinal 在此固化，见 ADR-0028
pack_bindings    component_id, require_name, bound_component_id
                 ★ 绑定在此固化，不重新解析
secrets          id, component_id, param, version, ciphertext, created_at
                 ★ 见 16-secrets §3
```

**观测状态**（可重建）

```
instance_status  role_instance_id, digest, generation, result,
                 workload_state, health, reported_at        ← 高频 upsert
node_facts       node_id, facts_json, collected_at
drift_reports    role_instance_id, resource_id, changes_json, seen_at
suppressions     role_instance_id, resource_id, reason, by, at, until
```

**过程**

```
rollouts         id, component_id, state, batches_json, started_at
rollout_batches  rollout_id, seq, targets_json, state
```

**事件与审计**（单独表 + 保留策略）

```
events           id, at, site_id, kind, subject, payload_json
audit            id, at, actor, action, target, pack_ref, result, detail_json
```

`audit` 与 `events` 分开是因为**保留策略不同**：事件是运维诊断素材，
可以按天清理；审计是合规要求，保留期以年计且不允许被普通清理任务碰到。

### 1.7 什么必须备份

> **mechd 的数据库是期望状态的一部分，不是缓存。**

这一条要写进运维文档。`role_instances.ordinal` 与 `pack_bindings` 都是**分配
出来的、无法重算的**值——丢了数据库再从 `site.yaml` 恢复，所有实例会被当成
新实例重新编号，ZooKeeper 的 `myid` 与 Kafka 的 `node.id` 随之错乱
（[ADR-0028](../adr/0028-stable-ordinals.md)）。

备份清单：

| | |
|---|---|
| `mechd.db` | `VACUUM INTO` 拿一致性快照，不用停服 |
| `/etc/mecharion/secret.key` | **单独备份，不要和 DB 放同一份**——放一起就抵消了信封加密（[16-secrets §3](16-secrets.md#3-存储信封加密)） |
| `/etc/mecharion/pki/` | 自签 CA，丢了要重新分发信任 |
| `blobs/` | 可从 Pack 重新组装，但离线环境下重建成本高 |

## 2. mechlet：不使用数据库

**范围**：这里说的是 mechlet **自身的本地状态**。事件与审计的**权威存储在 mechd 的 SQLite 中**（单机形态下 mechd 就在同一台机器上）；mechlet 的 JSONL 只是**断连期间的缓冲**，重连上报后即可删除，不承担长期查询职责。

mechlet 的状态很小（装了哪些 RoleInstance、各自 generation 与状态），采用：

```
/var/lib/mecharion/mechlet/
├── desired/<component>-<role>.json     mechd 下发的已解析规格（本地缓存，供断连时继续调和）
├── observed/<component>-<role>.json    观测状态
├── generations.json                    generation 台账
└── events/2026-08-01.jsonl             事件缓冲，有上限、按天轮转
```

写入方式：**写临时文件 + `rename`**（POSIX 原子重命名），保证任何时刻崩溃都不会留下半个文件。

理由：

- mechlet 要以单静态二进制铺到成千上万台边缘机器——**少一个存储引擎就少一类崩溃恢复问题**，二进制也小几 MB
- 现场用 `cat` / `jq` 就能看状态，不需要任何专用工具
- 状态规模与查询需求和 mechd 差三个数量级，SQL 的价值在这里不存在

> 见 [ADR-0013](../adr/0013-mechlet-no-database.md)

## 3. 高可用：v1 不做

**理由：mechd 不在数据面上。**

它挂掉时所有 mechlet 仍按最后已知的期望状态继续调和与自愈，业务完全不受影响（见 [01-architecture.md §5](01-architecture.md#5-断连自治)）。这与 Kubernetes apiserver 挂掉的严重性完全不同。

在这个前提下，为 HA 提前选择 go-memdb + raft（Consul/Nomad 的路子）是过早优化——那是几个月的工作量，而这几个月投到 Pack 格式与资源引擎上回报高得多。

需要时的三条路径，按代价排序：

1. **主备 + 文件复制**（Litestream 式流复制到备机）——秒级 RPO，故障时脚本切换。对 ALM 场景通常足够
2. **dqlite / rqlite**——保留 SQL，raft 复制，改动可控
3. **迁移到 go-memdb + raft**——最彻底，代价最大

前提条件已经满足：§1.5 的 repository 接口层。

> 见 [ADR-0014](../adr/0014-no-ha-in-v1.md)
