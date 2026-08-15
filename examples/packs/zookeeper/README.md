# zookeeper — L2

**这个示例验证 `.Requires` 的另一端：被依赖方。** 前两波的 hdfs / kafka 都是消费者，从未检验过「提供方该怎么写」。

结果是打出了本轮最重要的一个缺口。

## 发现

### ① 消费方在伸手进提供方的内部 ✅ 新增 `exports`

第二波里 hdfs 与 kafka 都这么引用 ZooKeeper：

```yaml
from: "requires.zookeeper.topology.role('server').nodes | join(':2181,')"
```

写 zookeeper Pack 时才看清这有多脆：

- 硬编码了提供方的**角色名** `server`。若 zookeeper Pack 改叫 `node`，两个消费者同时失效
- 硬编码了**端口拼装方式** `:2181`。而端口是 zookeeper 的参数，用户改了 `client_port`，消费者拿到的串是错的
- 消费者**完全不知道**自己依赖了这些内部细节，提供方也不知道自己不能改

提供方声明具名连接点：

```yaml
exports:
  client:
    role: server
    port: "{{ .Params.client_port }}"
    separator: ","
```

消费方只引用导出名：

```yaml
zk_connect:
  from: "requires.zookeeper.exports.client"     # → "h1:2181,h2:2181,h3:2181"
```

**连接串的拼装责任回到了知道端口的一方。** hdfs 与 kafka 已同步改用 `exports.client`。

`.Requires.<pack>.Topology` 保留可用，但规范中标注为「提供方未导出所需连接点时的临时手段」——提供方无义务保持其稳定。

### ② 仲裁语义只有 Pack 作者知道 ✅ 新增 `quorum: true`

ZooKeeper 的 ensemble 有两条约束，此前都无法表达：

**a. 实例数应为奇数。** 4 节点 ensemble 能跑，但容错能力与 3 节点相同——白多一台机器。这是「你大概不是这个意思」而非「坏了」，因此应当**告警而非拒绝**。

**b. 滚动重启不能打破多数派。** 这一条严重得多：3 节点 ensemble 若 Rollout 并发度设成 2，重启期间集群直接失去仲裁、全线不可用。

而**用户设置 `maxUnavailable` 时无从判断某个组件能同时下线几个实例**——这是组件的内在语义，只有 Pack 作者知道。

```yaml
roles:
  - name: server
    quorum: true
```

引擎据此：偶数实例告警；**Rollout 强制 `maxUnavailable ≤ (N-1)/2`**。

第二条是这个字段存在的主要理由。已回填到 hdfs 的 `journalnode`、kafka 的 `controller` 与 `combined`。

## 验证了什么

| 规范条目 | 验证点 |
|---|---|
| [§5.3 `exports`](../../../docs/spec/pack-v1.md#53-exports--对外导出的连接点) | ✅ 首个提供方；`client` 与 `quorum` 两个导出 |
| [§11 `quorum`](../../../docs/spec/pack-v1.md#quorum) | ✅ 首个用例 |
| [§9.5 `scope: node` 消费](../../../docs/spec/pack-v1.md#95-requires--依赖-pack-引用) | ✅ 自身依赖 `jdk11`，`java.env` 中用 `.Requires.jdk11.Paths.Current` |
| [§14.2 `file.content` + 模板函数](../../../docs/spec/pack-v1.md#142-文件系统) | ✅ `myid` 内容为 `{{ add .Topology.Ordinal 1 }}`——ordinal 从 0 起、ZK 从 1 起 |
| [§8.2 自定义路径](../../../docs/spec/pack-v1.md#82-预定义路径名) | ✅ `dataLogDir` 与 `data` 分离（ZK 强烈建议分盘，写入模式完全不同） |

## `myid` 与 `server.N` 必须一致

ZooKeeper 最经典的配置错误是 `myid` 与 `zoo.cfg` 里的编号对不上——集群起不来且报错难懂。

```yaml
- file:
    path:    "{{ .Paths.Data }}/myid"
    content: "{{ add .Topology.Ordinal 1 }}"
```
```
{{- range $i, $n := .Topology.Role "server" }}
server.{{ add $i 1 }}={{ $n.Address }}:…
{{- end }}
```

两处用**同一个稳定序号**，这类错误在结构上不可能发生。这也再次说明 `topology.ordinal` 的「按节点名排序、稳定」是硬要求：一旦排序变化，`myid` 与 `server.N` 会集体错位。

## 与 kafka KRaft 的对照

两者都是仲裁系统，但初始化方式相反：

| | ZooKeeper | Kafka KRaft |
|---|---|---|
| 节点身份 | `myid` 文件，**每节点写各自的值** | `node.id` 在配置文件里 |
| 集群标识 | 无（靠 `zoo.cfg` 的成员列表） | `cluster_id` 参数，全集群一致 |
| 初始化 | 无需 format | 每节点 `kafka-storage.sh format`（`perInstance`） |

这组对照说明 `quorum: true` 是**跨组件通用**的语义，而初始化流程不是——后者只能靠 hooks 各自表达。
