# go-webapp — L1 基线

**这个示例的唯一职责是当基线。** 一个最普通的单二进制服务，其 Pack 应当短到没有任何仪式感。

当前 `pack.yaml` **66 行**，其中：

| 部分 | 行数 | 是否必需 |
|---|---|---|
| 元数据（`schema`/`name`/`version`/`platforms`） | 5 | 必需 |
| `description`/`license`/`keywords` | 3 | 可选 |
| `blobs`（双平台，每平台 4 行） | 11 | 必需 |
| `params`（3 个，含 `description` 与分组） | 20 | 视组件 |
| `roles`（resources + workload + health） | 27 | 必需 |

**结构性下限约 35 行**——去掉可选元数据、只留一个参数。这个数字可以接受：其中 27 行是 `roles`，而那是真正在描述「这个组件是什么」的部分，不是格式的仪式。

判定标准因此修正为：**格式本身的强制开销（非描述组件的部分）应少于 20 行。** 当前是 16 行（元数据 5 + blobs 11），达标。

## 验证了什么

| 规范条目 | 验证点 |
|---|---|
| [§1 复杂度分层](../../../docs/spec/pack-v1.md#复杂度分层) | ✅ 全程未出现 `profiles` / `placement` / `cardinality` / `linkInto` / `kind: multi` / `shared` |
| [§11 单角色简写](../../../docs/spec/pack-v1.md#11-roles--角色) | ✅ `roles[0]` 省略 `name`，默认为 `default` |
| [§4 `revision` 默认值](../../../docs/spec/pack-v1.md#4-packyaml-顶层字段) | ✅ 省略，默认 `1` |
| [§7.2 运维语义类型](../../../docs/spec/pack-v1.md#72-类型12-种) | ✅ `port` / `enum` / `size`；模板中用 `.Bytes` 取字节数 |
| [§7.3 `restartRequired` / `reloadRequired`](../../../docs/spec/pack-v1.md#73-字段) | ✅ `port` 需重启，`log_level` 与 `max_body_size` 只需 reload |
| [§19 规则 25](../../../docs/spec/pack-v1.md#参数与路径) | ✅ 声明了 `execReload`，因此 `reloadRequired` 合法 |
| [§8.2 默认路径](../../../docs/spec/pack-v1.md#82-预定义路径名) | ✅ 全部使用默认值，一行 `paths` 都没写 |

## 部署后的磁盘布局

```bash
mechctl deploy go-webapp          # Component 名默认等于 Pack 名
```

```
/opt/mecharion/apps/go-webapp/
├── generations/0001-1.2.0-1/bin/webapp
└── current -> generations/0001-1.2.0-1
/etc/mecharion/apps/go-webapp/app.yaml
/var/lib/mecharion/apps/go-webapp/
/var/log/mecharion/apps/go-webapp/
```

配置在 generation 之外，无软链——这是 `layout: separate` 且不声明 `linkInto` 的默认形态，也是绝大多数现代应用（接受 `--config` 参数）适用的形态。

### 路径中的名字是 **Component 名**，不是 Pack 名

默认路径模板是 `{{ .Node.Roots.opt }}/apps/{{ .Component }}`。Component 名由用户在部署时决定，**省略 `-c` 时默认等于 Pack 名**，所以上面两者一致。若显式命名：

```bash
mechctl deploy go-webapp -c webapp     # → /opt/mecharion/apps/webapp/
```

**必须用 Component 名而非 Pack 名，理由是决定性的**：同一个 Pack 可以在一个 Site 里部署多份（`pg-main` 与 `pg-report` 都来自 `postgresql`）。若路径按 Pack 名，两份部署会撞进同一个目录——数据目录冲突，直接毁库。

> `pack.yaml` 里的 `user: webapp` 是操作系统用户名，与路径、Pack 名都无关。

### 路径何时确定、能否修改

```
① Pack 声明        paths.home.default: "…/apps/{{ .Component }}"   模板
② mechd 放置阶段    渲染 → /opt/mecharion/apps/go-webapp            具体路径
③ mechlet 首次物化  把解析后的绝对路径写入本地状态                     固化
④ 后续每次调和      读取已固化的路径，不重新推导
```

第 ④ 步是关键：若每次调和都重新推导，用户改了 `Node.Roots.data`、或 Pack 升级时改了默认路径，已装好的组件就会**静默搬家**，旧数据变成孤儿。

因此 **已存在 RoleInstance 的路径不可变**——检测到解析结果与已固化值不一致时，mechlet 拒绝调和并报错，而不是自动迁移。这与 mechd 自身 `--data-dir` 不可变是同一条原则。

## 发现

**无。** 规范在 L1 场景下没有暴露问题——这正是期望的结果。

一处**主观判断**留待更多 L1 示例检验：`resources` 里的 `user` 与 `directory` 两条略显啰嗦。是否值得为「建用户 + 建数据目录」提供更高层的糖（如 `workload.systemd.user` 自动隐含建用户）？

**当前倾向：不加。** 隐式建用户会让「这个 uid 是谁建的」变得不可追溯，与[原则六（显式优于隐式）](../../../docs/design/00-overview.md#原则六显式优于隐式)冲突。两行换来可追溯性是划算的。等第二波的 nginx 示例再确认一次。
