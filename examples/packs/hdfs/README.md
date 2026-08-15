# hdfs — L3

三形态、六角色、跨 Pack 依赖、集群级一次性动作。**这是规范复杂度的上界样本**——如果 HDFS 能被干净地表达，Hadoop 生态的其余组件都能。

## 三种形态

| profile | 节点数 | 角色 | 关键特征 |
|---|---|---|---|
| `standalone` | ≥1 | NN×1, DN×1 | 无冗余，`replication=1`，堆调小 |
| `distributed` | ≥2 | NN×1, SNN×1, DN×1-N | NN 单点，SNN 做检查点 |
| `ha` | ≥4 | NN×2, JN×3, ZKFC×2, DN×3-N | 双 NN + JournalNode 仲裁 + 自动故障转移，依赖 zookeeper pack |

```bash
mechctl deploy hdfs -c hdfs-prod --profile ha \
  --set nameservice_id=prod --set zk_quorum=zk1:2181,zk2:2181,zk3:2181
```

## 验证了什么

### 形态（§13）

| 验证点 | 结果 |
|---|---|
| `enabled: false` 关闭角色 | ✅ `standalone` 关掉 SNN/JN/ZKFC，`ha` 关掉 SNN |
| profile 覆盖 `cardinality` | ✅ `namenode` 在三形态下分别是 1 / 1 / 2 |
| profile 覆盖参数 `default` | ✅ `standalone` 把 `replication` 改为 1、堆减半 |
| profile **追加**专有参数 | ✅ `nameservice_id` / `zk_quorum` / `fencing_method` 只在 `ha` 下存在与可见 |
| profile 合并 `requires` | ✅ `ha` 追加 `zookeeper >= 3.6`；顶层的 `jdk >= 11` 保留 |
| profile **整体替换** `placement` | ✅ 三形态各自一套约束 |
| `upgradeFrom` 链 | ✅ `standalone → distributed → ha`，反向未声明即禁止 |

### 放置约束（§12）

`ha` 形态的五条约束覆盖了全部三种形式：

```yaml
- antiAffinity: [namenode]              # 单元素 → 同角色实例互斥
- antiAffinity: [journalnode]           # 同上
- affinity:     [namenode, zkfc]        # 跨角色必须同置
- antiAffinity: [datanode]
- antiAffinity: [journalnode]
  scope: rack                           # label key 作为 scope
  enforcement: preferred                # 软约束 → 仅告警
```

**`affinity: [namenode, zkfc]` 是 affinity 的第一个真实用例**——ZKFC 通过本地回环监控并 fence 其所在节点的 NameNode，不同置就完全失效。这条约束此前只有假想例子（DataNode/NodeManager），现在有了必须成立的场景。

### 模板片段组合（§9.1）

`hdfs-site.xml` 在 HA 与非 HA 之间差异极大（HA 多出 9 个 property 且键名含 nameservice 变量）。用片段组合而非整模板切换：

```
templates/
├── hdfs-site.xml.tmpl     主模板，只有 12 行
├── _hdfs-common.tmpl      全形态共有
├── _hdfs-simple.tmpl      standalone / distributed
└── _hdfs-ha.tmpl          ha
```

`templates/` 下所有文件作为一个 template set 解析、`_` 前缀视为片段——**这两条规则让片段组合零成本可用，无需任何新机制**。

### 拓扑渲染（§9.3）

最复杂的一处是 JournalNode 仲裁串：

```
qjournal://jn1:8485;jn2:8485;jn3:8485/mycluster
```

`range` + `{{ if $i }};{{ end }}` 处理分隔符，顺序由 `topology.ordinal` 的稳定排序保证。NameNode 的 `nn0` / `nn1` 编号同样依赖这个稳定顺序——**若排序不稳定，一次重新渲染就会让两个 NN 的身份互换，这是灾难性的**。规范中「按节点名排序，稳定」这句话在这里是硬要求，不是实现细节。

### 集群级一次性动作（§16.4）

四个 hook 恰好覆盖了 `scope` 的两种取值与它们的边界：

| hook | scope | when | 为什么 |
|---|---|---|---|
| `nn-format.sh` | `once` | — | 格式化只能做一次；在第二个 NN 上重跑会毁掉集群 |
| `nn-init-shared-edits.sh` | `once` | `profile == "ha"` | 共享日志初始化只能做一次 |
| `nn-bootstrap-standby.sh` | `perInstance` | `ha && ordinal != 0` | **每个备 NN 各做一次**——这正是 `once` 表达不了、而 `perInstance + when` 恰好覆盖的模式 |
| `zkfc-format.sh` | `once` | — | 两个 ZKFC 共用同一个 znode |

第三条证明了不需要为「除首个实例外都执行」发明新语法。

### 跨 Pack 依赖（§5）

同时用上了两种 scope：

```yaml
requires.packs:
  - { name: jdk11,     version: ">=11.0.20", scope: node }   # 本机文件，必须同节点
profiles[ha].requires.packs:
  - { name: zookeeper, version: ">=3.6",     scope: site }   # 网络可达即可
```

初版这里靠约定路径硬编码，是本示例最有价值的一处发现——见下文「发现 ③」。

## 发现

### ① `paths` 声明的目录应由引擎自动创建 ✅ 已修入规范

写 DataNode 角色时需要「为每块数据盘建目录」。`resources` 没有迭代机制，我一度临时发明了 `forEach` 字段。

但更好的答案是：**`paths` 已经声明了 `owner` / `group` / `mode`，引擎就应当自动创建这些目录**。写 `directory` 资源是重复声明，两处的 owner/mode 还可能不一致。

规范已补充（[§8.3](../../../docs/spec/pack-v1.md#83-字段)）：

> 引擎自动创建 `paths` 中声明的全部路径，`kind: multi` 时逐个创建。因此 Pack 不需要为这些路径再写 `directory` 资源。

**连带收益：`resources` 因此不需要迭代机制。** 多盘是唯一会产生「同一资源重复 N 次」需求的场景，它被 `paths` 吸收了。这是个好结果——迭代机制会让资源清单从声明式滑向脚本式。

postgresql 示例同步删掉了冗余的 `directory` 资源。

### ② `requires: [journalnode]` 在角色被禁用时的语义

`namenode` 声明 `requires: [journalnode]`，但 `standalone` / `distributed` 形态下 JournalNode 是 `enabled: false`。

当前处理：**被禁用角色上的依赖自动忽略**，不报错。这是唯一合理的语义（否则每个 profile 都要重写一遍 requires），但规范中未明确写出。**建议补一句。**

### ③ 跨 Pack 的路径引用靠约定，不够稳固 ✅ 已改为 `.Requires`

初版 `hadoop-env.sh` 把 `JAVA_HOME` 硬编码为 `{{ .Node.Roots.opt }}/apps/jdk/current`，隐含两条可能不成立的假设：Component 名等于 Pack 名、使用默认 `home` 路径。

现改为引用解析后的路径：

```sh
export JAVA_HOME="{{ .Requires.jdk11.Paths.Current }}"
```

**顺带确立了「Pack 依赖有两种」这一区分**，它决定了校验强度与可用变量：

| scope | 语义 | 校验 | 可用变量 |
|---|---|---|---|
| `node` | 依赖是本机磁盘上的文件（JDK） | 本 Component 的**每个** RoleInstance 所在节点上，依赖 Component 也必须有实例 | `.Paths.*` `.Version` |
| `site` | 依赖是网络可达的服务（ZooKeeper） | 同 Site 内存在即可 | `.Topology.Role(…)` `.Version`，**无 `.Paths`**——那是别人机器上的路径，引用它必然是 bug |

`ha` profile 因此可以把最易出错的一步自动化——用户不再需要手敲 quorum 串：

```yaml
zk_quorum:
  from: "requires.zookeeper.topology.role('server').nodes | join(':2181,')"
```

### ③b Pack 粒度：大版本何时进 Pack 名

初版写 `{ name: jdk, version: ">=11" }`，现改为 `{ name: jdk11, version: ">=11.0.20" }`。

两条**独立**判据，对应两种不同处理——混用会导致过度拆包：

| 判据 | 问题 | 处理 | 例 |
|---|---|---|---|
| **需要共存吗？** | 同一节点是否会同时需要多个大版本 | **拆 Pack**，大版本进名字 | `jdk11` / `jdk17`（不同应用要不同 JDK） |
| **能原地升级吗？** | 能否用「换 generation、保数据目录」完成 | **`upgradePolicy.compatible`** 声明兼容范围 | `postgresql` 声明 `~16`，拒绝 15→16 跨界升级 |

JDK 命中第一条 → 拆包。PostgreSQL 只命中第二条 → 不拆，加声明（否则 `postgresql15/16/17/18` 会永远拆下去，每个都有 95% 重复的模板）。

把大版本放进 Pack 名还消灭了一类版本范围语法陷阱：`">=17 <18"` / `"~17"` / `"^17"` 在不同生态含义不同，而选 Pack 不会选错。这与 Debian（`openjdk-17-jdk`）、RPM（`java-17-openjdk`）、Homebrew（`openjdk@17`）的做法一致。

**连带结论：`requires.packs` 不需要 `alias` 字段。** 真需要两个 JDK 时它们本来就是两个 Pack，名字天然不同。仍需保留的只有**显式绑定**——一个 Site 里可能有两套 ZooKeeper（`zk-hdfs` / `zk-kafka`），此时用 `--require zookeeper=zk-hdfs` 消歧。

### ④ `cardinality: "0"` 作为「默认关闭」的表达方式偏隐晦

JournalNode / ZKFC / SecondaryNameNode 在顶层 `roles` 中声明 `cardinality: "0"`，靠 profile 打开。读起来不直观——`"0"` 看着像笔误。

考虑过 `enabled: false` 作为角色的顶层默认值，但那会与 profile 的 `enabled` 覆盖语义重复。

**当前判断：保持现状，但文档中明确「顶层 `cardinality: "0"` 是『默认不启用，由 profile 打开』的惯用写法」。** 若第二波示例中再次出现同样的别扭感，再考虑加 `defaultEnabled: false`。

## 未解决

- **堆大小与节点内存的关系**：`nn_heap` / `dn_heap` 目前是固定默认值。真实运维中 DataNode 堆通常按内存比例设置，异构集群下需按机型分 ConfigGroup。第二波 elasticsearch 会正面撞上 `.Node.Facts.*` 的需求。
- **XML 配置的表达冗长**：`hdfs-site.xml` 的每个属性都是 4 行 XML。`xml_properties` 资源类型（键值映射 → 自动生成 XML）能大幅压缩，并支持按键 diff 与按键合并。但它是 Java 生态专用的，是否进核心资源集需要评估——第二波 kafka（`.properties` 格式）会提供第二个数据点。
