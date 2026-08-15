# postgresql — L2

多角色 + 双形态 + 拓扑引用。**它是「配置位于数据目录内」这一布局的正面用例**，也是第一个暴露出规范边界的示例。

## 验证了什么

| 规范条目 | 验证点 |
|---|---|
| [§6 `sourceUrl`](../../../docs/spec/pack-v1.md#6-blobs--载荷) | ✅ 记录上游 tarball 地址，供供应链追溯；部署阶段绝不访问 |
| [§8 `paths`](../../../docs/spec/pack-v1.md#8-paths--路径声明) | ✅ **第三种配置布局**：`config` 位于 `{{ .Paths.Data }}/pgdata`。数据目录本就跨 generation 存活，**不变式自动满足，无需 `linkInto`** |
| [§10 `shared`](../../../docs/spec/pack-v1.md#10-shared--角色共有部分) | ✅ 建用户、解压载荷在三个角色间只做一次 |
| [§11 多角色](../../../docs/spec/pack-v1.md#11-roles--角色) | ✅ `primary` / `replica` / `client`；`client` **无 workload**，只分发配置 |
| [§11 `requires`](../../../docs/spec/pack-v1.md#requires) | ✅ `replica` 依赖 `primary`，决定启停与滚动升级顺序 |
| [§9.3 `topology` 引用](../../../docs/spec/pack-v1.md#93-topology-引用) | ✅ `primary_host` 由 `from:` 推导；`pg_hba.conf` 用 `range` 枚举全部副本地址 |
| [§12 `placement`](../../../docs/spec/pack-v1.md#12-placement--放置约束) | ✅ 主备互斥、副本之间互斥，均带 `reason` |
| [§13 `profiles`](../../../docs/spec/pack-v1.md#13-profiles--部署形态) | ✅ `standalone` 禁用 `replica` 并把 `max_wal_senders` 默认改为 0；`replicated` 声明 `upgradeFrom: [standalone]` |
| [§16.4 `scope: once`](../../../docs/spec/pack-v1.md#164-scope--集群级一次性动作) | ✅ 创建流复制用户只需一次，不能在每个 primary 实例上重跑 |
| [§16.5 sensitive 参数传递](../../../docs/spec/pack-v1.md#165-执行环境) | ✅ `admin_password` 经临时文件传入（`MECHARION_PARAM_FILE_*`），不进环境变量 |
| [§14.5 守卫](../../../docs/spec/pack-v1.md#145-逃生舱) | ✅ `initdb.sh` / `basebackup.sh` 用 `creates: …/PG_VERSION` 保证幂等 |
| [§7.2 类型](../../../docs/spec/pack-v1.md#72-类型12-种) | ✅ `port` `size` `duration` `cidr` `secret` `enum` `bool` 七种；模板用 `.Milliseconds` |

## 形态迁移：standalone → replicated

```bash
mechctl component set-profile pg-main replicated
mechctl component assign pg-main replica --nodes node-2,node-3
```

引擎重新解析放置、校验反亲和、重新渲染 `pg_hba.conf`（此时才会出现 `host replication` 行）、生成 Rollout。副本节点上 `basebackup.sh` 被 `creates` 守卫放行并执行。

`upgradeFrom: [standalone]` 使这条路径合法；反方向（replicated → standalone）未声明，会被拒绝——降级需要人工决定副本如何下线，不该由工具替用户决定。

## 已知边界：主备身份不由 Mecharion 建模

**这是本示例暴露的最重要的一点，写在这里以免后来者误解模型。**

Mecharion 的 `primary` / `replica` 角色表达的是**初始角色分配**，不是**运行时主备身份**。

PostgreSQL 发生故障切换（无论手工 `pg_ctl promote` 还是 Patroni 之类的外部组件驱动）之后，实际的主库可能是 Mecharion 认为的 `replica` 实例。此时模型与现实不一致。

考虑过三种处理，选择了第三种：

| 方案 | 评价 |
|---|---|
| Mecharion 探测真实主备并自动更新角色归属 | ❌ 这要求 Mecharion 理解每种数据库的主备语义，是无边界的工作量 |
| 合并为单个 `postgres` 角色，不建模主备 | ❌ 无法表达反亲和、无法区分 initdb 与 basebackup、`pg_hba` 无从渲染 |
| **保留角色建模，明确其语义为「初始分配」** | ✅ 采用 |

因此：

- 故障切换后应执行 `mechctl component assign pg-main primary --nodes node-2` 显式更新模型，随后配置会重新渲染
- 若使用 Patroni 等自动故障转移组件，正确做法是**让该组件管理主备身份，Mecharion 只负责部署它**——此时 Pack 应当只有一个 `postgres` 角色

**一般原则：不建模自己无法控制的状态。** 这条同样适用于将来的 Kafka 分区 leader、Elasticsearch 主分片等场景。

## 发现（已反馈至规范）

| 发现 | 处理 |
|---|---|
| hooks 只能声明在 Pack 顶层，`bootstrap-roles.sh` 只该在 `primary` 上跑 | 新增 `roles[].hooks` |
| 创建复制用户不能在每个 primary 实例上重跑（虽然 `cardinality: "1"` 使其暂时无害，但 HA 场景必然出问题） | 新增 `hooks[].scope: once` |
| sensitive 参数若走环境变量会出现在 `/proc/<pid>/environ` | 规范明确 sensitive 参数经临时文件传递，注入 `MECHARION_PARAM_FILE_<NAME>` |

## 未解决

`shared_buffers` 的经验值是物理内存的 25%，但变量表中没有节点内存事实。当前用固定默认值 `128MB`（与 PG 自身默认一致）+ ConfigGroup 按机型分组绕过。

这个绕法可接受但不理想——第二波的 elasticsearch（堆大小 = 内存一半且上限 31GB）会正面撞上，届时决定是否引入 `.Node.Facts.*`。
