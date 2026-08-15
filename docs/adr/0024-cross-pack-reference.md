# ADR-0024: 跨 Pack 引用与 Pack 粒度判据

- **状态**：已接受
- **日期**：2026-08-02
- **相关**：[ADR-0005](0005-pack-logic-payload-split.md)、[ADR-0022](0022-deployment-profiles.md)、[ADR-0015](0015-offline-first-hermetic.md)

## 背景

写 hdfs 示例时，`hadoop-env.sh` 需要引用 JDK 的安装路径。初版靠约定：

```sh
export JAVA_HOME="{{ .Node.Roots.opt }}/apps/jdk/current"
```

隐含两条可能不成立的假设：**Component 名等于 Pack 名**、**使用默认 `home` 路径**。用户完全可能把它命名为 `jdk-prod`，或把它装在别的盘上。

同时，`ha` profile 要求用户手工填写 ZooKeeper quorum 串（`zk1:2181,zk2:2181,zk3:2181`）——这是运维最容易打错的一处，而这个信息 mechd 明明知道。

## 决策一：`.Requires.<pack>.*` 渲染上下文

`requires.packs` 中的每个依赖，在放置阶段解析为具体 Component，结果注入渲染上下文。

### 依赖有两种 scope，语义与校验强度都不同

| `scope` | 语义 | 放置校验 | 可用变量 |
|---|---|---|---|
| `node`（默认） | 依赖是**本机磁盘上的文件**（JDK、共享库） | 本 Component 的**每个** RoleInstance 所在节点上，依赖 Component 也必须有实例 | `.Paths.*` `.Version` `.Component` |
| `site` | 依赖是**网络可达的服务**（ZooKeeper、外部数据库） | 同 Site 内存在满足版本约束的 Component 即可 | `.Topology.Role(…)` `.Version` `.Component` |

**这个区分是本决策最重要的部分。** 此前 `requires.packs` 只检查「这个 Site 里有没有 jdk」，而 JDK 是本机文件——装在别的节点上毫无用处。`scope: node` 把它变成一条真正的放置校验。

`scope: site` **不暴露 `.Paths`** 是刻意的：那是别的机器上的路径，引用它必然是 bug，lint 直接报错。

### 用法

```sh
export JAVA_HOME="{{ .Requires.jdk11.Paths.Current }}"
```

```yaml
zk_quorum:
  from: "requires.zookeeper.topology.role('server').nodes | join(':2181,')"
```

### 绑定规则

| 满足版本约束的候选 | 行为 |
|---|---|
| 恰好 1 个 | 自动绑定 |
| 多个 | 拒绝，要求 `--require zookeeper=zk-hdfs` |
| 0 个 | 拒绝，列出缺什么 |

**绑定关系记录在 Component 状态中，之后不再重新解析**——否则新装一套 ZooKeeper 就可能让已有部署静默改指向。与路径固化（spec §8.7）是同一条原则。

## 决策二：Pack 粒度的两条独立判据

设计绑定规则时，一度准备引入 `alias` 字段处理「一个 Pack 同时依赖 jdk11 与 jdk17」。评估后放弃——**那是 Pack 粒度错误的症状，不是需要新机制的信号。**

| 判据 | 问题 | 处理 | 例 |
|---|---|---|---|
| **需要共存吗？** | 同一节点是否会同时需要多个大版本 | **拆 Pack**，大版本进名字 | `jdk11` / `jdk17` |
| **能原地升级吗？** | 能否用「换 generation、保数据目录」完成 | **`upgradePolicy.compatible`** | `postgresql` 声明 `~16` |

**两条判据必须分开，混用会导致过度拆包。**

JDK 命中第一条 → 拆包，`alias` 因此不需要（两个大版本本来就是两个 Pack，名字天然不同）。

PostgreSQL 只命中第二条：大版本升级需要 `pg_upgrade` 与新数据目录，直接换二进制会让 PG 17 去启动 PG 16 的数据目录。但它**不需要共存**（除迁移窗口外），所以不拆包，改为声明兼容范围：

```yaml
upgradePolicy:
  compatible: "~16"
```

若也用拆包解决，`postgresql15/16/17/18` 会永远拆下去，每个都有 95% 重复的模板。

### 调研：打包界的一致做法

| 生态 | 做法 |
|---|---|
| Debian | `openjdk-11-jdk` / `openjdk-17-jdk`，`postgresql-15` / `postgresql-16` |
| RPM | `java-11-openjdk` / `java-17-openjdk` |
| Homebrew | `openjdk@11` / `openjdk@17` |
| Python | `python3.11` / `python3.12` |

**所有主流打包系统对「多大版本共存的运行时」都把大版本放进包名。** 这不是巧合——它同时解决了共存与版本约束表达两个问题。

### 附带收益：消灭版本范围语法陷阱

```yaml
version: ">=17 <18"    # 还是 ~17？还是 ^17？三种生态三种含义
```

拆开之后大版本在名字里，约束只管小版本：

```yaml
- { name: jdk17, version: ">=17.0.9" }
```

**语义边界从「写对一个范围表达式」变成「选对一个 Pack」**，出错空间小一个量级。

## 理由

跨 Pack 引用本身是显然需要的（约定路径必然出错）。真正的设计工作在两处：

**① 区分 node/site scope**，让「JDK 必须同节点」成为可校验的约束，而不是文档里的一句提醒。

**② 拒绝 `alias`**，因为它会掩盖 Pack 粒度错误。一个机制若只在别处设计错误时才需要，正确做法是修正别处。

## 后果

### 收益

- 跨 Pack 引用不再依赖命名与路径约定
- `scope: node` 把「依赖必须同节点」变成放置期的硬校验
- ZooKeeper quorum 这类易错串可由拓扑推导
- Pack 粒度有了机械可判的准则，不再靠感觉
- 无需 `alias` 机制

### 代价

- **Pack 之间的耦合变成引擎可见的**，由此产生一系列新责任：
  - 被依赖 Component 的**删除必须被阻止**（引用计数）
  - **依赖环**需在 lint 与放置阶段检测
  - 依赖**升级会波及下游**：`.Version` 变化触发下游重渲染与可能的重启。Rollout 必须在执行前列出影响面，否则用户会被意外的连锁重启打击
- **`scope: node` 会让部署顺序变严格**：先装 hdfs 再装 jdk11 会失败。必须给出可操作的错误（「请先部署 jdk11 到这些节点」），而非干巴巴的拒绝
- **拆包带来模板重复**：`jdk11` 与 `jdk17` 的 pack.yaml 高度相似。这是有意接受的——Debian 也是这样做的，且这类 Pack 本身很简单（解压 + 设路径，无 workload）
- **`upgradePolicy` 是 Pack 作者的声明，引擎无法验证其真实性**：作者写错范围，引擎照单全收。与 `profiles.upgradeFrom` 的局限相同

## 参考

- Debian / RPM / Homebrew 的多版本运行时包命名
- Terraform module `source` + version constraint（跨模块引用的对照）
- Helm subchart 与 `.Values` 传递（紧耦合的反面案例：subchart 使父子 chart 版本绑死）
