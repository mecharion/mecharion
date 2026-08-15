# ADR-0012: mechd 采用嵌入式 SQLite

- **状态**：已接受
- **日期**：2026-08-01
- **相关**：[ADR-0013](0013-mechlet-no-database.md)、[ADR-0014](0014-no-ha-in-v1.md)

## 背景

mechd 需要持久化：期望状态（Site/Component/Role/RoleInstance/Node）、观测状态（心跳、健康）、事件与审计、Rollout 状态机、Pack 元数据、RBAC。

产品定位要求**零外部依赖**——从中心化数据中心到边缘环境都要能跑，不能要求用户先架一个 PostgreSQL。因此选型范围限定为嵌入式存储。

### 先划清范围：blob 不进数据库

```
/var/lib/mecharion/blobs/sha256/ab/ab12ef…   ← 内容寻址文件，200MB 的 JDK 在这
/var/lib/mecharion/mechd.db                  ← 只存元数据与引用计数
```

这是前置约束，与选型无关。

### 规模估算

1000 节点 × 15 秒心跳 ≈ **66 写/秒**，全是小行 upsert。

**这个量级对任何候选都不构成压力。因此写吞吐不是选型依据——查询灵活性与可运维性才是。**

### 单机形态同样使用 SQLite

[ADR-0026](0026-standalone-runs-mechd.md) 确定边缘单机也运行 mechd，因此**单机与多节点的存储是同一份实现**。

曾考虑给单机做一个文件存储实现（数据规模只有几十条记录，SQL 的查询价值确实为零）。放弃的理由不是性能，而是：

- 两个 `Store` 实现意味着两套代码路径——正是 [ADR-0002](0002-mechlet-as-sole-engine.md) 极力避免的东西
- 文件存储同样需要 schema 演进（见 [ADR-0013](0013-mechlet-no-database.md) 中的 `schemaVersion`），只是把 goose 换成手写转换函数
- **审计是功能需求而非性能需求**：按用户 / 时间 / 组件过滤是合规基本要求，JSONL 扫文件不能算实现了审计

单机上 SQLite 的额外成本（约 8MB 二进制 + 几十 MB RSS）对任何能跑 PostgreSQL 或 JVM 的机器都是噪音。

## 候选方案与调研

| | 查询能力 | HA 路径 | 纯 Go | 现场可调试 | 上手成本 |
|---|---|---|---|---|---|
| **bbolt** | ❌ 纯 KV，每个索引手写 | ❌ 需自行接 raft | ✅ | ❌ 二进制不透明 | 低 |
| **SQLite**（modernc） | ✅ 完整 SQL + 索引 + JSON1 | ⚠️ 后期需 dqlite/rqlite 或迁移 | ✅ 纯 Go 转译 | ✅✅ `sqlite3 mechd.db` | 低 |
| **go-memdb + raft** | ✅ 多级二级索引 + MVCC 事务 | ✅✅ 原生 | ✅ | ❌ 需自建工具 | 高 |
| **Badger / Pebble** | ❌ 同 bbolt | ❌ | ✅ | ❌ | 中 |

### 各候选的具体考察

**bbolt** — etcd v2 曾用，成熟可靠、单写多读、内存映射 B+ 树。致命问题是纯 KV：Web UI 的每个筛选维度都要手写并维护复合索引。

**go-memdb + raft** — Consul 与 Nomad 的实际方案：状态在内存 MVCC radix tree（支持多级二级索引），持久化靠 raft log + snapshot。优点是天然 HA、性能好；缺点是全部状态需驻留内存、索引手工定义、快照/恢复代码量大、无法用通用工具检视。

**Badger / Pebble** — LSM 写吞吐更高，但本场景写吞吐不是瓶颈，而查询能力问题与 bbolt 相同，还额外引入 compaction 复杂度与内存占用。**在本场景是最差的权衡。**

**SQLite** — 完整 SQL、索引、事务、WAL 并发读、JSON1 扩展、成熟的 schema 迁移生态。两个 Go 绑定：
- `mattn/go-sqlite3`（cgo，性能好）
- `modernc.org/sqlite`（纯 Go 转译，性能约为前者的 1/2~1/4）

## 决策

**采用 `modernc.org/sqlite`（纯 Go）+ `sqlc` + `goose`。**

### 三条决定性理由

**① Web UI 的列表页会杀死 KV 方案。**

「筛选 site=X 且 label role=db 且 health!=OK，按最后心跳排序，分页」——SQL 一行；bbolt 里要手写并维护复合索引，每加一个筛选维度就要改索引与回填逻辑。可视化界面是产品的一等入口，这类查询会有几十个。

**② 现场可诊断性被严重低估。**

边缘机器出问题、无网络，远程指导运维执行 `sqlite3 mechd.db "select * from nodes where …"` 当场看清楚。KV 存储需要先分发一个版本匹配的 dump 工具，而边缘环境恰恰最难分发工具。这是[原则七](../design/00-overview.md#原则七现场可诊断)的直接体现。

**③ 必须是 modernc 而非 mattn。**

需要交叉编译到 linux/amd64 与 linux/arm64，且开发机为 Windows。cgo 交叉编译会持续制造痛苦，还会破坏「单静态二进制」这个整个产品依赖的性质。纯 Go 版性能损失在 66 写/秒面前完全无关。

### 工程约定

| 约定 | 原因 |
|---|---|
| WAL 模式 + `busy_timeout=5000` | 允许 1 写 + N 读并发 |
| **写操作收敛到单 goroutine / 单连接**，读用独立连接池 | SQLite 单写者，彻底避免 `SQLITE_BUSY` |
| params/values 存 JSON 列，结构化字段单独建列 | 兼顾灵活与可查询 |
| goose 迁移，SQL 文件 `go:embed` | 离线环境不能依赖外部迁移工具 |
| sqlc 生成访问层，**不用 GORM** | ORM 在「对象图 + 大量条件查询」场景持续添堵 |
| `VACUUM INTO` 备份 | 一条命令拿一致性快照，不停服 |
| 事件/审计单独表 + 保留策略 | 增长最快，避免拖累主表 |
| 未来的指标历史另开存储 | 不塞进主库 |

### 必须保留的抽象

sqlc 生成的 Querier 之上再包一层 domain repository 接口。这是**为 HA 预留的唯一必要抽象**——将来换实现时业务逻辑不用动（[ADR-0014](0014-no-ha-in-v1.md)）。

## 后果

### 收益

- UI 的任意筛选/排序/分页需求由 SQL 直接满足
- 现场用标准工具即可诊断，无需分发专用工具
- 交叉编译无痛，单静态二进制性质保持
- schema 迁移有成熟工具链（schema 会改很多次）
- 备份简单

### 代价

- **HA 不是免费的**：SQLite 单文件单写者。三条后续路径按代价排序：主备文件复制（Litestream 式）→ dqlite/rqlite → 迁移 go-memdb+raft。前提条件（repository 接口）已满足
- **纯 Go 版性能低于 cgo 版**：当前规模无影响；若未来节点数达到万级需重新评估（届时写新 ADR）
- **单写者需要工程纪律**：必须严格执行「写收敛到单 goroutine」，否则会遇到 `SQLITE_BUSY`。这是实现阶段的高风险点
- **不适合指标时序数据**：必须另开存储，不能图省事塞进主库

## 参考

- Consul / Nomad：go-memdb + raft 架构
- etcd v2 使用 bbolt 的经验
- `modernc.org/sqlite`（纯 Go SQLite）
- Litestream / rqlite / dqlite（SQLite 的 HA 路径）
