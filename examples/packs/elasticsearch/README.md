# elasticsearch — L2

**这个示例是 `.Node.Facts` 与 `linkInto` 两项设计的验收测试。**

## 验证了什么（新增部分）

| 规范条目 | 验证点 |
|---|---|
| [§7.4 `defaultFrom`](../../../docs/spec/pack-v1.md#74-from-与-defaultfrom) | ✅ **最典型的用例**：`min(memory/2, 31GB)`——固定默认值在此几乎必然是错的 |
| [§9.4 事实固化](../../../docs/spec/pack-v1.md#94-nodefacts--节点事实) | ✅ 加内存不会自动改堆并重启 |
| [§8.4 `linkInto`](../../../docs/spec/pack-v1.md#84-layout-separate--linkinto默认路径) | ✅ **三个**路径同时用它：`config`、`plugins`、`logs` |
| [§8.3 引擎自动建目录](../../../docs/spec/pack-v1.md#83-字段) | ✅ 全 Pack 无一条 `directory` 资源 |
| [§14.4 主机配置资源](../../../docs/spec/pack-v1.md#144-主机配置) | ✅ `sysctl` + `limits` 与组件部署在同一个 Pack 内——**主机配置不是独立子系统** |
| [§5 无 `packs` 依赖](../../../docs/spec/pack-v1.md#5-requires--前置条件) | ✅ ES 自带 JDK，与 kafka/hdfs 形成对照 |

## `heap` 为什么必须是 `defaultFrom`

ES 官方建议：堆 = 物理内存的一半，且**不超过 31GB**（超过则 JVM 失去压缩对象指针，实际可用内存反而下降）。

固定默认值在这里几乎必然是错的：

| 节点内存 | 正确堆 | 若固定默认 4GB |
|---|---|---|
| 8GB | 4GB | 恰好 ✓ |
| 64GB | 31GB | **浪费 27GB** |
| 4GB | 2GB | **OOM** |

```yaml
heap:
  defaultFrom: '{{ min (div .Node.Facts.Memory.Total 2) "31GB" }}'
  default: 1GB      # 事实采集失败时的兜底
```

**而它同时是「事实固化」最有说服力的场景**：运维给节点加内存是一次硬件操作，不应该在下一次调和时把 ES 重启——那是一次静默的、发生在非计划窗口的服务中断。加完内存后运维会自行决定何时调堆：

```
$ mechctl node facts diff es-node-3
  memory.total   渲染时: 32GB   当前: 64GB
  受影响参数: es-prod/node.heap = 16GB  →  31GB
  → mechctl node facts refresh es-node-3 --apply
```

## 三处 `linkInto`：ES 是这个机制的压力测试

ES 对目录布局的要求比 Kafka/Tomcat 更苛刻——**它要写入自己安装目录下的三个位置**：

| 路径 | ES 期望位置 | 为什么必须外置 |
|---|---|---|
| `config` | `$ES_HOME/config` | ES 会写入 keystore；且配置需跨版本存活 |
| `plugins` | `$ES_HOME/plugins` | 插件是用户安装的状态，升级不能丢 |
| `logs` | `$ES_HOME/logs` | 日志跨版本连续 |

三者用**同一个 `linkInto` 机制**处理，没有为任何一种情况发明新语法。这验证了 [ADR-0008](../../../docs/adr/0008-immutable-generation-linkinto.md) 中「`linkInto` 是通用机制」的判断。

> `plugins` 的 `default` 指向 `data` 根而非 `etc` 根——插件是二进制内容而非配置，放 `/var/lib` 下更合适。**路径名与它归属的 root 不必对应**，由 Pack 作者按内容性质决定。

## 主机配置与组件部署在同一个 Pack

```yaml
shared.resources:
  - sysctl: { key: vm.max_map_count, value: "262144" }
  - limits: { domain: elasticsearch, item: memlock, soft: unlimited, hard: unlimited }
```

ES 不满足这两项会直接拒绝启动。它们是**这个组件的部署要求**，不是「另外一件事」——因此不该拆到一个单独的「主机调优 Pack」里由用户记得先装。

这验证了 [06-state-and-drift §3](../../../docs/design/06-state-and-drift.md#主机配置与组件部署是同一个引擎) 的判断：主机配置不是独立子系统，只是资源引擎的一种用法。

## 发现

### ① YAML 配置对模板最友好——`properties` 资源类型的最终结论

三个数据点齐了：

| 组件 | 配置格式 | 模板表达的痛感 |
|---|---|---|
| HDFS | XML | **明显**：每个属性 4 行 |
| Kafka | `.properties` | 无 |
| Elasticsearch | YAML | 无，且列表结构比键值对更适合模板 |

**结论：`properties` / `xml_properties` 不进 v1。** 只有 XML 有明显收益，而为一种格式增加核心资源类型不划算——Hadoop 系 Pack 用模板片段组合已能解决（见 hdfs 示例）。

规范的未决问题条目据此关闭。

### ② `cluster.initial_master_nodes` 暴露了一个「一次性配置」的模式

这一项**只在集群首次成型时有意义**，之后 ES 会忽略它。但模板每次渲染都会写出它——若集群已成型后节点列表变化（加节点），配置文件会变，触发 `notify: restart`，而这次重启毫无必要。

三种处理：

| 方案 | 评价 |
|---|---|
| 给 `template` 资源加 `renderOnce` | ❌ 引入「配置文件不再反映期望状态」的例外，破坏漂移检测 |
| 该项单独一个文件 + `driftPolicy: ignore` | ⚠️ 可行，但 ES 只读一个 `elasticsearch.yml` |
| **接受重启，由 `notify` 的粒度控制** | ✅ 当前选择 |

当前 Pack 把整个 `elasticsearch.yml` 标为 `notify: restart`。加节点时全集群滚动重启，对 ES 是可接受的（Rollout 会逐个进行并等待集群转绿）。

**记录在此是因为这个模式会反复出现**（Kafka 的 `controller.quorum.voters`、ZK 的 `server.N`）。若将来发现足够多的场景，可考虑给 `template` 加 `notifyOn: <正则>`——只有匹配的行变化才触发动作。**v1 不做**，先积累用例。
