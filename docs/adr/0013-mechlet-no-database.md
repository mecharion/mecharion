# ADR-0013: mechlet 不使用数据库

- **状态**：已接受
- **日期**：2026-08-01
- **相关**：[ADR-0012](0012-mechd-embedded-sqlite.md)

## 背景

mechlet 也需要持久化：已安装的 RoleInstance 列表、各自的 generation 与观测状态、断连期间的事件缓冲。

直觉上应与 mechd 保持一致（都用 SQLite），少一种技术栈。

## 候选方案与调研

| 方案 | 二进制体积 | 崩溃恢复 | 现场可读 | 查询能力 |
|---|---|---|---|---|
| **SQLite**（与 mechd 一致） | +5~8MB | 依赖 WAL 恢复 | 需 sqlite3 工具 | 强（但用不上） |
| **bbolt** | +1~2MB | 依赖 mmap 事务 | ❌ 不可读 | 弱 |
| **普通文件 + 原子 rename** ⭐ | 0 | POSIX rename 原子性保证 | ✅ `cat` / `jq` | 无（不需要） |

### 关键观察：状态规模差三个数量级

| | mechd | mechlet |
|---|---|---|
| 记录数 | 数万～数十万 | 数十 |
| 查询需求 | 多维筛选、排序、分页（UI 驱动） | 「读全部」 |
| 数据量 | 几十 MB～GB | 几百 KB |

**mechd 选 SQLite 的三条理由（UI 查询、现场可诊断、交叉编译）中，只有第二条适用于 mechlet——而普通文件在这一条上比 SQLite 更强。**

## 决策

**mechlet 不引入任何数据库。**

> **范围澄清（[ADR-0026](0026-standalone-runs-mechd.md) 之后）**
>
> 本 ADR 讨论的是 **mechlet 自身的本地状态**：已安装的 RoleInstance、generation 台账、观测状态、断连期间的事件缓冲。这些仍然是 JSON 文件，本 ADR 的结论不变。
>
> **事件与审计的权威存储在 mechd 的 SQLite 中**（单机形态下 mechd 与 mechlet 同机）。mechlet 的 JSONL 只是**断连期间的缓冲**——重连后上报给 mechd 即可删除，不承担长期查询职责。
>
> 这个分工让两边各取所长：mechd 有 SQL 支撑审计查询，mechlet 保持零存储依赖与现场 `cat` 可读。

```
/var/lib/mecharion/mechlet/
├── desired/<component>-<role>.json     期望状态
├── observed/<component>-<role>.json    观测状态
├── generations.json                    generation 台账
└── events/2026-08-01.jsonl             事件缓冲，有上限、按天轮转
```

写入方式：**写临时文件 + `rename`**（POSIX 原子重命名），保证任何时刻崩溃都不会留下半个文件。

事件缓冲为有上限的 append-only JSONL，按天轮转，超限丢弃最旧记录并记录丢弃计数。

## 理由

**① 少一个存储引擎就少一类崩溃恢复问题。**

mechlet 要铺到成千上万台边缘机器，其中很多会经历非正常断电（工业现场、门店、车载）。POSIX `rename` 的原子性是内核保证的，而数据库的崩溃恢复依赖 WAL 回放、文件锁、mmap 一致性等更多层次——每一层都是一类可能的边缘故障。

**② 现场用 `cat` 就能看状态。**

边缘环境最难做的事是分发工具。JSON 文件用系统自带的 `cat` / `grep` 就能读；SQLite 需要 `sqlite3` 二进制（很多精简系统没有），bbolt 需要项目自建的 dump 工具且版本必须匹配。

**③ 二进制体积。**

`modernc.org/sqlite` 是转译产物，会给 mechlet 增加 5~8MB。对一个要铺到上万台机器、且需要通过带宽受限链路自升级的 agent 而言，这不是可忽略的。

**④ SQL 的价值在这里不存在。**

mechlet 的查询就是「把状态全读出来」。为一个不需要查询的场景引入查询引擎，是纯粹的成本。

### 关于「两端存储不一致」

这不是不一致，是**按需选型**。mechd 与 mechlet 的状态规模与查询需求相差三个数量级，用同一个方案才是错误的一致性追求。

## 后果

### 收益

- mechlet 二进制更小，自升级传输更快
- 崩溃恢复语义简单且由内核保证
- 现场零工具依赖即可诊断
- 无 schema 迁移负担

### 代价

- **无事务**：跨多个文件的一致性需要自己保证。缓解：状态设计为「每个 RoleInstance 一个文件」，天然无跨文件事务需求；`generations.json` 是唯一的聚合文件，采用整体重写 + rename
- **无索引**：全量读取。在数十条记录规模下无意义
- **事件缓冲需自行实现轮转与上限**：约百行代码，可接受
- **状态文件格式变更需要自己处理兼容**：无 goose 之类的迁移工具。缓解：JSON 结构中带 `schemaVersion` 字段，mechlet 启动时按需转换
- **两套持久化实现**：mechd 与 mechlet 的存储代码无法复用。这是有意的，见上文

## 参考

- POSIX `rename(2)` 原子性保证
- Kubernetes kubelet 的 checkpoint 文件（同样采用普通文件而非数据库）
- systemd 的状态文件实践
