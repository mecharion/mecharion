# java-webapp —— 消费方视角

**层级 L2。** 它存在的理由只有一个：**验证「消费方怎么拿到提供方的凭据」**。

其余示例都是提供方视角（zookeeper、jdk11、postgresql 导出东西给别人用），
消费侧此前只有 kafka → zookeeper 一条，而那条**不涉及凭据**。

## 压测了规范的哪些部分

| 规范 | 用到的地方 |
|---|---|
| [§5.1 两种 scope](../../../docs/spec/pack-v1.md#51-requirespacks--依赖的两种-scope) | ✅ 一个 Pack 里同时用 `scope: node`（jdk11）与 `scope: site`（postgresql），语义与校验强度都不同 |
| [§5.4 `exports.fields`](../../../docs/spec/pack-v1.md#54-exportsfields--具名字段与凭据) | ✅ **本示例的主角**：从 postgresql 的具名字段自行组装 JDBC URL |
| [§7.4 `defaultFrom`](../../../docs/spec/pack-v1.md#74-from-与-defaultfrom) | ✅ 按节点内存算堆上限，运维可覆盖 |
| [§9.5 `.Requires` 可用字段随 scope 而变](../../../docs/spec/pack-v1.md#951-可用字段随-scope-而变) | ✅ `.Requires.jdk11.Paths.Current` 合法；`.Requires.postgresql.Paths` 会被规则 40 拒绝 |
| [§19 规则 46](../../../docs/spec/pack-v1.md#资源与-hooks) | ✅ 含口令的 `env` 用 `0640`；主配置不含口令，`0644` 即可 |

## 口令不进主配置

```
{{ .Paths.Config }}/env                     0640   DB_PASSWORD=…      ← 唯一含口令处
{{ .Paths.Config }}/application.properties  0644   password=${DB_PASSWORD}
```

主配置常被复制进工单、支持包，甚至有人提交进 git。把口令从它里面拿走，
等于把最常见的泄漏途径直接堵掉——而这**不需要任何加密**，只需要分成两个文件。

Spring 用 `${DB_PASSWORD}` 取环境变量，systemd 用 `EnvironmentFile=` 注入。
两边都是原生能力，Pack 不必发明任何机制。

> 口令能安全穿过 `EnvironmentFile` 这一层，靠的是 `generate` 默认字符集
> `alnum`——[§7.6](../../../docs/spec/pack-v1.md#76-generate--引擎生成的密码) 排除符号正是为了避免转义 bug。

## 为什么是 `fields` 而不是 `format`

PostgreSQL **不可能知道**下游要什么形状：

```
libpq DSN     postgres://user:pw@host:5432/db
JDBC URL      jdbc:postgresql://host:5432/db  ＋ 分开的 user / password
Spring        spring.datasource.url / .username / .password
.pgpass       host:port:db:user:pw
```

本示例要的是第二种——**URL 里不含账号口令，两者单独给**。若提供方用
`format` 拼一个串，这里就得再把它拆开，那是提供方猜错了形状的成本转嫁。

`fields` 把「我有什么」和「你要什么形状」分开：提供方只说 `host` / `port` /
`database` / `username` / `password`，怎么拼是消费方自己的事。

## 为什么拿到的不是 superuser

`postgresql` 导出的是 `app_user`，**不是 `admin_password`**。

给下游应用 superuser 口令是提权：一个被攻破的 web 应用会直接拥有整个数据库
实例。要被消费的凭据应当是一个**专门为这段依赖关系存在的账号**，由提供方的
`postInstall` hook 建出来（见 [postgresql 的 bootstrap-roles.sh](../postgresql/hooks/bootstrap-roles.sh)）。

规则 49 把「伸手取提供方的参数」直接拦在 lint 阶段：

```
$ mechpack lint examples/packs/java-webapp
params.db_user:35   错误 [R49] 不得读取依赖 "postgresql" 的参数
  提示: 提供方一改参数名，所有消费方同时失效；而且它的超级用户口令本就不该给消费方。
        需要的值应由提供方通过 exports 显式导出（规范 §5.4）
```

## 口令从哪来

没有任何人输入过它——`app_password` 声明了 `generate: { length: 32 }`，
由引擎生成一次并固化。这是**无人值守 + 边缘离线**同时成立的关键：

- 运维不必在提供方与消费方各填一次同一个密码（那既是错误来源，轮换时更是灾难）
- 不需要联系任何外部密钥服务，因此断网环境照常工作

## 暴露的问题

| 发现 | 处理 |
|---|---|
| **`exports` 装不下凭据**——原有字段集（`role`/`port`/`separator`/`format`）只能拼出 `host:port` | 新增 [§5.4 `exports.fields`](../../../docs/spec/pack-v1.md#54-exportsfields--具名字段与凭据) |
| **让提供方拼连接串是错的**——它不知道消费方要 libpq / JDBC / Spring 哪种形状 | `fields` 导出具名值，形状由消费方决定；`format` 保留给形状确定的地址列表 |
| **没有机制阻止消费方读提供方的参数**——`requires.pg.params.admin_password` 原本合法 | 新增规则 49；`.Requires.<pack>.Params` 不存在 |
| **口令要运维在两处各填一次** | 新增 [§7.6 `params.generate`](../../../docs/spec/pack-v1.md#76-generate--引擎生成的密码) |
| **消费方可能忘了把接来的值标 `secret`**，于是口令进日志与 UI | lint 做不到（依赖方可能来自别处、单独发布）→ 改由 mechd 在绑定时**自动传播**敏感标记；`mechpack inspect` 展示导出契约供作者参考 |
| **过度标注同样有害**——本示例初稿把不含口令的 `jdbc_url` 也标成了 `secret`，结果是排障时连「连的哪个库」都看不到，而标记本身退化成噪音 | 污点必须**精确跟随数据流**：只有 `db_password` 是 secret |
| **规则 46 在规范里写了，代码里从来没实现** | 补上实现（含跟进 `{{ template }}` 片段），并新增**规则覆盖测试**自动核对规范 §19 与代码——同一次核对又查出 R8/R39/R43 也没实现，已在规范中用 ⏳ 标出 |

## 注意：这是验证夹具

`blobs` 里的 sha256 是占位值，`java-webapp-2.4.0.tar.gz` 并不存在。
本 Pack 用于压测规范，不是可部署的真实组件。
