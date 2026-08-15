# kafka — L3

**这个示例的价值在于：它的 profile 轴与 hdfs 完全不同。**

hdfs 的三种形态是**规模**递进（1 节点 → 2 节点 → 4 节点），profile 之间有天然的升级链。kafka 的三种形态是**架构选择**——`kraft-combined` 与 `kraft-separated` 可以用同样多的节点，`zookeeper` 更是一条正在被淘汰的平行路线。

若 `profiles` 只能表达「规模档位」，它就会在 kafka 上失效。结果是：**没有失效**。profile 的定义（一组角色 + 数量 + 放置 + 参数默认）与「规模」无关，架构轴天然适用。

## 三种形态

| profile | 角色 | 特征 |
|---|---|---|
| `kraft-combined`（默认） | `combined` ×1-N | 每节点同时是 broker 与 controller，中小规模首选 |
| `kraft-separated` | `controller` ×3 + `broker` ×1-N | 元数据不受数据流量影响，大规模集群 |
| `zookeeper` | `broker` ×1-N + 外部 ZK | 3.x 遗留形态，仅供迁移 |

```bash
mechctl deploy kafka -c kafka-prod --profile kraft-separated
```

## 验证了什么（新增部分）

| 规范条目 | 验证点 |
|---|---|
| [§7.4 `defaultFrom`](../../../docs/spec/pack-v1.md#74-from-与-defaultfrom) | ✅ **首个真实用例**：`heap` 按内存的 1/4 计算且不超过 6GB，用户可覆盖 |
| [§9.4 事实固化](../../../docs/spec/pack-v1.md#94-nodefacts--节点事实) | ✅ 加内存不会导致 broker 被自动重启 |
| [§4.2 `upgradePolicy`](../../../docs/spec/pack-v1.md#42-upgradepolicy--升级兼容范围) | ✅ 声明 `~3`——跨大版本升级需按顺序调 `inter.broker.protocol.version`，不能直接换 generation |
| [§5.1 `scope: site`](../../../docs/spec/pack-v1.md#51-requirespacks--依赖的两种-scope) | ✅ `zookeeper` profile 追加 ZK 依赖，`zk_connect` 由拓扑推导 |
| [§13.4 profile 不覆盖 resources](../../../docs/spec/pack-v1.md#134-profile-不能覆盖的部分) | ✅ KRaft / ZK 两套配置模板差异过大，用 `when:` 整模板切换而非片段组合 |
| [§12 `antiAffinity` 跨角色 + `preferred`](../../../docs/spec/pack-v1.md#12-placement--放置约束) | ✅ 分离模式下 controller ⊥ broker 是 `preferred`——**同置不会坏，只是抵消了分离的意义** |

## 一个角色三种身份：`process.roles` 由角色名决定

`combined` / `controller` / `broker` 三个角色共用同一份 `server-kraft.properties.tmpl`，模板开头据角色名分派：

```
{{- $isController := or (eq .Role "controller") (eq .Role "combined") }}
{{- $isBroker     := or (eq .Role "broker")     (eq .Role "combined") }}
```

这比为三个角色各写一份模板好——它们 80% 的内容相同，分开会立刻漂移。

## `node.id` 的稳定性与偏移

Kafka 要求 `node.id` **全集群唯一且永久稳定**——改变它等同于让集群认为这是一个全新节点。

```
{{- if eq .Role "broker" }}
node.id={{ add .Topology.Ordinal 100 }}
{{- else }}
node.id={{ .Topology.Ordinal }}
{{- end }}
```

分离模式下 `controller` 与 `broker` 是**两个独立的 ordinal 序列**，都从 0 开始。若不加偏移，controller-0 与 broker-0 会撞 id。

这暴露了 `topology.ordinal` 的一个使用陷阱：**它在角色内唯一，不是全 Component 唯一**。规范的措辞（「当前实例在同角色中的序号」）是准确的，但 Pack 作者极易误读。已在下方「发现」中记录。

## 发现

### ① `scope: once` 与「集群级唯一」不是一回事 ✅ 无需改规范

写 `kraft-format.sh` 时的第一反应是照搬 hdfs 的 `scope: once`——错了。

| | HDFS `namenode -format` | Kafka `kafka-storage.sh format` |
|---|---|---|
| 谁执行 | **只有一个实例** | **每个实例都要** |
| 唯一性体现在 | 「只执行一次」这个动作 | `cluster_id` **参数**在集群内一致 |
| scope | `once` | `perInstance`（默认） |

两者看起来都是「集群级初始化」，但一个是动作唯一、一个是参数唯一。**规范不需要新机制，但文档应当给出这组对照**——否则 Pack 作者会习惯性地把所有初始化都标成 `once`，导致除首节点外全部启动失败。

### ② `topology.ordinal` 的作用域易被误读

如上文所述。建议规范在 §9.3 的表格中把措辞加重为「**在同角色中**（不是全 Component）唯一」，并给出 kafka 的偏移写法作为惯用法。

### ③ `.properties` 格式仍然只能靠模板 —— 与 hdfs 的结论一致

Kafka 配置是纯键值对，比 Hadoop XML 简洁得多，模板表达没有明显痛点。

但**跨形态的键集合差异很大**（KRaft 有 `process.roles`/`controller.quorum.voters`，ZK 模式有 `zookeeper.connect`），若有 `properties` 资源类型支持「按键合并」，两个模板可以合成一个基础集 + 两个覆盖集。

**当前判断：收益不足以进核心资源集。** 理由是 kafka 与 hdfs 给出的信号相反——hdfs 的痛点是 XML 冗长（4 行/属性），kafka 完全没有这个问题。一个只对 XML 有明显收益的资源类型，不如让 Hadoop 系 Pack 自己用模板片段解决。

**结论：`properties` / `xml_properties` 不进 v1。** 待 elasticsearch（YAML 配置）给出第三个数据点后写入规范的未决问题结论。
