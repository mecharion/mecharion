# nginx — L1

第二个 L1 基线，用来回答 go-webapp 留下的一个悬念，并验证 `reload` 语义。

## 验证了什么

| 规范条目 | 验证点 |
|---|---|
| [§7.3 `reloadRequired`](../../../docs/spec/pack-v1.md#73-字段) | ✅ **全部 6 个参数都是 `reloadRequired`**——nginx 改配置永不需要重启 |
| [§15.1 `execReload`](../../../docs/spec/pack-v1.md#151-runtime-systemd) | ✅ `nginx -s reload`；lint 规则 25 因此放行 |
| [§8.3 路径与 root 不必对应](../../../docs/spec/pack-v1.md#83-字段) | ✅ `webroot` 放在 `data` 根下，`runtime` 放在 `/run` |
| [§8.4 无需 `linkInto` 的情形](../../../docs/spec/pack-v1.md#84-layout-separate--linkinto默认路径) | ✅ nginx 用 `-c` 指定配置，配置可完全独立——与 Kafka/ES 形成对照 |
| [§14.2 `file.source`](../../../docs/spec/pack-v1.md#142-文件系统) | ✅ 从 `files/` 分发静态首页 |
| [§7.2 类型](../../../docs/spec/pack-v1.md#72-类型12-种) | ✅ `duration` 在模板中取 `.Seconds`（nginx 要秒数不带单位） |

## 「零重启组件」是一个值得单独确认的形态

nginx 的六个参数**没有一个** `restartRequired`。这意味着：

```bash
mechctl config set -c web http_port=8080
# → 渲染新配置 → notify: reload → nginx -s reload → 无中断
```

这验证了 [ADR-0007](../../../docs/adr/0007-params-custom-subset.md) 里那条决定性论证——`restartRequired` / `reloadRequired` **不是 UI 装饰，它直接决定引擎行为**。若参数系统用完整 JSON Schema（表达不了这两个字段），nginx 改端口会被当成需要重启处理，白白造成一次服务中断。

## 静态链接是 hermetic 约束的首选解法

blob 是**静态构建**的 nginx，不依赖宿主机的 openssl / pcre / zlib。

这对应 [ADR-0015](../../../docs/adr/0015-offline-first-hermetic.md) 中「OS 层依赖」三条应对的第一条。nginx 是最容易做到静态构建的一类组件；做不到时才退到第二条（把 `.so` 打进 generation 目录）。

## 回答 go-webapp 留下的悬念

go-webapp 的 README 提过一个待确认项：

> `resources` 里的 `user` 与 `directory` 两条略显啰嗦。是否值得为「建用户 + 建数据目录」提供更高层的糖？

**两个 L1 示例都写完后，结论是不加。**

- `directory` 那半个问题已被「引擎自动创建 `paths` 声明的目录」消解——本 Pack 一条 `directory` 都没有
- `user` / `group` 保持显式的两行。nginx 需要 `shell: /sbin/nologin`、go-webapp 不需要；nginx 的 systemd `user: root`（要绑 80 端口）与配置里的 `user nginx nginx` 是两个不同的用户概念。**这些差异恰恰是隐式建用户会抹掉的信息。**

两行换来可追溯性，划算。悬念关闭。

## 发现

**无。** L1 场景在两个不同组件上都没有暴露规范问题——这正是期望结果。

`pack.yaml` 共 120 行，其中 60 行是六个参数的完整声明（含 `description` / `group` / `advanced`）。格式本身的强制开销仍在 20 行以内。
