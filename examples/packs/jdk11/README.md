# jdk11 — L0

**全项目最简单的 Pack：无 workload、无 health、无 hooks、无 profiles、无模板。** 它只让文件出现在正确的位置。

`pack.yaml` **48 行**，其中 30 行是元数据与 blob 声明，实质内容 12 行。

## 它存在的意义

被 hdfs、kafka、zookeeper 三个 Pack 依赖，是 `scope: node` 的标准提供方。

```yaml
# 消费方
requires:
  packs:
    - { name: jdk11, version: ">=11.0.20", scope: node }
```
```sh
export JAVA_HOME="{{ .Requires.jdk11.Paths.Current }}"
```

## 验证了什么

| 规范条目 | 验证点 |
|---|---|
| [§11 无 workload 的角色](../../../docs/spec/pack-v1.md#11-roles--角色) | ✅ 整个 Pack 没有任何进程；`health` 也随之不存在——**模型自洽** |
| [§4.3 大版本进 Pack 名](../../../docs/spec/pack-v1.md#43-pack-粒度大版本何时进-pack-名) | ✅ `jdk11` 而非 `jdk` + 范围约束 |
| [§4.2 `upgradePolicy`](../../../docs/spec/pack-v1.md#42-upgradepolicy--升级兼容范围) | ✅ `~11`——11.0.x 之间可直接换 generation |
| [§9.5 作为 `scope: node` 提供方](../../../docs/spec/pack-v1.md#95-requires--依赖-pack-引用) | ✅ `.Paths.Current` 是稳定软链，小版本升级不引发下游重渲染 |
| [§14.2 `symlink.when`](../../../docs/spec/pack-v1.md#142-文件系统) | ✅ `register_alternatives` 默认 false——多 JDK 共存时不该抢占 `/usr/local/bin/java` |

## 为什么不需要 `exports`

zookeeper 声明了 `exports`，jdk11 没有。区别在于依赖的**性质**：

| | jdk11 | zookeeper |
|---|---|---|
| scope | `node` | `site` |
| 消费方要什么 | **本机路径** | **网络连接串** |
| 提供方式 | `.Paths.Current`（引擎已提供） | `exports.client`（需提供方拼装） |

`scope: node` 的依赖，消费方要的东西引擎已经知道（就是那个 Component 在本机解析出的路径）；`scope: site` 的依赖，连接串的拼装需要提供方的端口与格式知识，才需要 `exports`。

**规范中这条已写明**：`scope: node` 通常不需要 `exports`。

## 一个 CLI 层面的不便（非规范问题）

`scope: node` 要求 jdk11 部署到 hdfs 所在的每个节点。用户目前得把节点列两遍：

```bash
mechctl deploy jdk11 -c jdk11 --nodes n1,n2,n3,n4,n5
mechctl deploy hdfs  -c hdfs-prod --profile ha --nodes n1,n2,n3,n4,n5
```

顺序反了会被 `scope: node` 校验拒绝（这是对的——它防止了「装了 hdfs 却没 JDK」）。

建议 CLI 提供便捷式：

```bash
mechctl deploy jdk11 --same-nodes-as hdfs-prod
```

**这是 CLI 的事，不影响 Pack 格式**，记在这里以免遗忘。
