# Pack 格式规范 v1（草案）

- **状态**：**draft-stable**（2026-08-02 定型骨架；v0.1.0 发布后转为严格冻结）
- **schema 标识**：`pack/v1`

> **v0.1.0 之后这是对外契约。** 届时 Pack 格式一旦有第三方使用即被锁死，破坏性变更必须升版本号为 `pack/v2` 并由 mechlet 同时支持两个版本。**现在还没到那一步**——见下面「draft-stable」的含义。
>
> **「draft-stable」的含义**：格式经 [12 个真实组件示例](../../examples/packs/README.md)验证，L0–L3 全覆盖，已经稳定到可以据此开始实现、可以照着写 Pack。**但在有外部用户之前**，实现过程中发现的表达缺失或别扭之处仍可调整——届时同步更新本文与全部示例，变更会记录，不会悄悄发生。首个公开发布（v0.1.0）之后转为严格冻结，届时任何破坏性变更都必须走 `pack/v2`。
>
> 设计依据见 [ADR-0005](../adr/0005-pack-logic-payload-split.md)、[ADR-0006](../adr/0006-multi-role-pack.md)、[ADR-0007](../adr/0007-params-custom-subset.md)、[ADR-0008](../adr/0008-immutable-generation-linkinto.md)、[ADR-0015](../adr/0015-offline-first-hermetic.md)、[ADR-0020](../adr/0020-placement-constraints.md)、[ADR-0022](../adr/0022-deployment-profiles.md)、[ADR-0023](../adr/0023-node-facts.md)、[ADR-0024](../adr/0024-cross-pack-reference.md)、[ADR-0040](../adr/0040-pack-trust-is-operator-responsibility.md)。
>
> 完整可读的 Pack 示例见 [`examples/packs/`](../../examples/packs/)。

---

## 1. 约定

| 记号 | 含义 |
|---|---|
| **必填** | 缺失则 `mechpack lint` 报错 |
| **可选** | 可省略，有默认值时给出 |
| `{{ … }}` | Go `text/template` 表达式，求值时机见 [§9](#9-模板) |

字段名一律 `lowerCamelCase`。YAML 为唯一的序列化格式。

### 复杂度分层

**每个高级特性都可省略且有合理默认。** 简单组件的 Pack 不应为复杂组件的能力付出任何代价。

| 层级 | 代表组件 | 需要的字段 |
|---|---|---|
| **L1** 单角色单形态 | go-webapp、nginx | `name` / `version` / `platforms` / `blobs` / `params` / `roles[1]` |
| **L2** 多角色单形态 | postgresql 主备、elasticsearch | ＋多个 `roles`、`requires`、`placement` |
| **L3** 多角色多形态 | hadoop、kafka | ＋`profiles` |

### 一条格式设计原则

> **spec 中只允许「常量默认值」，不允许「从其他字段推导」。**

默认值是可预测的（`revision` 不写就是 `1`）；推导需要读者先理解规则，且往往在边界情况上出错。需要推导的便利一律放到**打包期**由 `mechpack assemble` 完成，发布产物永远是显式、自描述、可交叉校验的。

---

## 2. 目录结构

```
<pack-dir>/
├── pack.yaml           必填  全部声明
├── templates/          可选  配置模板（*.tmpl），`_` 前缀为片段
├── files/              可选  静态文件（原样拷贝）
├── hooks/              可选  逃生舱脚本
└── blobs/              可选  载荷，文件名为 sha256-<digest>
```

没有 `pack.sig`——Pack 签名决定不做，见 [ADR-0040](../adr/0040-pack-trust-is-operator-responsibility.md)。

`templates/` `files/` `hooks/` 内可有任意层级子目录，引用时使用相对该目录的路径。

---

## 3. 打包格式 `.mpack`

**thick pack**（自包含，离线交付）：

```
<name>-<version>-<revision>.mpack     # tar + zstd
```

归档内为上述目录结构，`blobs/` 完整。

**thin pack**：同样的 tar+zstd 结构，但 `blobs/` 为空——blob 由 mechd blob store 或节点本地缓存按 sha256 解析。

```bash
mechpack bundle   # thin → thick（拉入 blob）
mechpack push     # thick → thin（blob 入库）
```

**归档要求**（保证可复现）：条目按路径排序、时间戳归零、uid/gid 归零、不含符号链接、不含绝对路径与 `..`。

---

## 4. pack.yaml 顶层字段

```yaml
schema: pack/v1              # 必填
name: postgresql             # 必填  [a-z0-9]([a-z0-9-]*[a-z0-9])?  最长 63
version: "16.4"              # 必填  上游软件版本，字符串
revision: 1                  # 可选  Pack 自身迭代，默认 1
description: "PostgreSQL 关系数据库"   # 可选
homepage: "https://postgresql.org"    # 可选
license: "PostgreSQL"                 # 可选
maintainers:                          # 可选
  - { name: "…", email: "…" }
keywords: [database, sql]             # 可选

platforms: [linux/amd64, linux/arm64] # 必填（发布产物中）

upgradePolicy: { … }         # 可选  见 §4.2
requires:   { … }            # 可选  见 §5
exports:    { … }            # 可选  见 §5.3
blobs:      { … }            # 可选  见 §6
params:     { … }            # 可选  见 §7
paths:      { … }            # 可选  见 §8
shared:     { … }            # 可选  见 §10
roles:      [ … ]            # 必填  至少一个，见 §11
placement:  [ … ]            # 可选  见 §12
profiles:   [ … ]            # 可选  见 §13
hooks:      { … }            # 可选  见 §16
```

### 4.1 版本语义

| 字段 | 变更时机 | 影响 |
|---|---|---|
| `version` | 上游软件版本变化 | 触发载荷更新 |
| `revision` | 载荷不变，仅模板/资源/参数定义变化 | 触发重新物化，不重新下载 blob |

`version` + `revision` 共同构成 Pack 的唯一标识。**同一 `name` 下 (`version`, `revision`) 组合不可重复发布。**

### 4.2 `upgradePolicy` — 升级兼容范围

Mecharion 的升级模型是「物化新 generation → 原子切换 → 数据目录不动」。**这个模型对某些大版本升级是错的**：PostgreSQL 16 → 17 需要 `pg_upgrade` 与新的数据目录，直接换二进制会让 PG 17 去启动 PG 16 的数据目录。

```yaml
upgradePolicy:
  compatible: "~16"        # 只接受从 16.x 升级而来
```

引擎在升级前检查：**目标版本 Pack 的 `compatible` 是否包含当前已安装的版本**。不满足则拒绝，并提示走「新建 Component + 数据迁移」路径。

```
✗ 无法将 pg-main 从 15.7 升级到 16.4
  postgresql 16.4 声明 upgradePolicy.compatible = "~16"，不包含 15.7
  PostgreSQL 大版本升级需要 pg_upgrade 与新的数据目录，请新建 Component 后迁移数据
```

缺省为 `"*"`（任意版本可升级而来），适用于绝大多数组件。

### 4.3 Pack 粒度：大版本何时进 Pack 名

两条**独立**判据，对应两种不同处理。混用会导致过度拆包。

| 判据 | 问题 | 处理 | 例 |
|---|---|---|---|
| **需要共存吗？** | 同一节点是否会同时需要多个大版本 | **拆 Pack**，大版本进名字 | `jdk11` / `jdk17`（不同应用要不同 JDK） |
| **能原地升级吗？** | 能否用「换 generation、保数据目录」完成 | **`upgradePolicy.compatible`** | `postgresql` 声明 `~16` |

JDK 命中第一条 → 拆包。PostgreSQL 只命中第二条 → 不拆，加声明——否则 `postgresql15/16/17/18` 会永远拆下去，每个都有 95% 重复的模板。

把大版本放进 Pack 名还消灭了一类版本范围语法陷阱：`">=17 <18"` / `"~17"` / `"^17"` 在不同生态含义不同，而选 Pack 不会选错。这与 Debian（`openjdk-17-jdk`）、RPM（`java-17-openjdk`）、Homebrew（`openjdk@17`）的做法一致。

> 见 [ADR-0024](../adr/0024-cross-pack-reference.md)

---

## 5. `requires` — 前置条件

```yaml
requires:
  os:
    family: [debian, rhel]         # 可选  debian|rhel|suse|alpine|arch
    minVersion: "8"                # 可选  发行版主版本下限
    glibc: ">=2.17"                # 可选
    kernel: ">=3.10"               # 可选
    cgroup: v2                     # 可选  v1|v2|any（默认 any）

  capability:                      # 可选  Node 上报的 Runtime 能力
    docker: ">=20.10"
    compose: ">=2.0"

  packs:                           # 可选  Pack 间依赖，见 §5.1
    - { name: jdk11,     version: ">=11.0.20", scope: node }
    - { name: zookeeper, version: ">=3.6",     scope: site }

  resources:                       # 可选  资源下限，安装前校验
    memory: 2GB
    diskFree: 10GB

  mecharion: ">=0.5.0"             # 可选  引擎版本下限，见 §21
```

**校验时机与行为**

| 项 | 时机 | 失败行为 |
|---|---|---|
| `capability` | mechd 放置阶段 | 拒绝放置，给出可执行的修复提示 |
| `packs` | mechd 放置阶段 | **仅在本地可用 Pack 集合内解析，绝不联网获取**（[ADR-0015](../adr/0015-offline-first-hermetic.md)）。缺失则列出缺什么 |
| `os` / `resources` | mechlet 物化前 | 拒绝物化，报人类可读错误 |

> **绝不尝试自动修复宿主机依赖。** 缺 glibc、缺 docker、磁盘不足一律快速失败。

### 5.1 `requires.packs` — 依赖的两种 scope

Pack 依赖有两种，**语义与校验强度都不同**：

| `scope` | 语义 | 放置校验 | 可用变量（见 §9.5） |
|---|---|---|---|
| `node`（默认） | 依赖是**本机磁盘上的文件**（JDK、共享库） | 本 Component 的**每个** RoleInstance 所在节点上，依赖 Component 也必须有实例 | `.Paths.*` `.Version` |
| `site` | 依赖是**网络可达的服务**（ZooKeeper、外部数据库） | 同 Site 内存在满足版本约束的 Component 即可 | `.Topology.Role(…)` `.Version`，**无 `.Paths`** |

`scope: site` 不暴露 `.Paths` 是刻意的——那是别的机器上的路径，引用它必然是 bug。

### 5.2 绑定规则

一个 Site 里可能有多个来自同一 Pack 的 Component（两套 ZooKeeper：`zk-hdfs` / `zk-kafka`）。绑定按下列规则解析：

| 满足版本约束的候选 | 行为 |
|---|---|
| 恰好 1 个 | 自动绑定 |
| 多个 | **拒绝**，要求 `mechctl deploy … --require zookeeper=zk-hdfs` |
| 0 个 | **拒绝**，列出缺什么及版本要求 |

**绑定关系记录在 Component 状态中，之后不再重新解析。** 否则新装一套 ZooKeeper 就可能让已有部署静默改指向——与「路径固化」（§8.7）是同一条原则。

> **`requires.packs` 没有 `alias` 字段。** 若一个 Pack 需要同时依赖同一软件的两个大版本，那本来就是两个 Pack（`jdk11` / `jdk17`），名字天然不同。见 §4.3。

**依赖环**在 lint 与放置阶段均检测。被依赖的 Component 在有依赖者时**拒绝删除**（引用计数）。

### 5.3 `exports` — 对外导出的连接点

消费方**不应伸手进提供方的角色内部**：

```yaml
# ✗ 消费方硬编码了提供方的角色名与端口拼装方式
zk_connect:
  from: "requires.zookeeper.topology.role('server').nodes | join(':2181,')"
```

提供方一旦把角色改名（`server` → `node`），或改了默认端口，所有消费方同时失效——而它们完全不知道自己依赖了这些内部细节。

提供方声明具名的连接点：

```yaml
# zookeeper pack
exports:
  client:
    description: "客户端连接串"
    role: server
    port: "{{ .Params.client_port }}"
    separator: ","
```

消费方只引用导出名：

```yaml
# kafka pack
zk_connect:
  from: "requires.zookeeper.exports.client"     # → "h1:2181,h2:2181,h3:2181"
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `role` | ✅ | 该连接点由哪个角色提供 |
| `port` | ✅ | 端口，可为模板表达式 |
| `separator` | | 多实例的连接符，默认 `,` |
| `format` | | 单实例的格式模板，默认 `{{ .Address }}:{{ .Port }}`；如 ZK 的 `qjournal` 形式可自定义 |
| `fields` | | 具名字段，见 §5.4 |
| `description` | | UI 与 `mechpack inspect` 展示 |

**这是 Pack 之间唯一被推荐的耦合方式。** `.Requires.<pack>.Topology` 仍然可用，但它属于**内部细节访问**——文档中标注为「provider 未导出所需连接点时的临时手段」，且提供方无义务保持稳定。

`scope: node` 的依赖通常不需要 `exports`（消费方要的是 `.Paths.Current` 这类本机路径，见 §9.5）。

### 5.4 `exports.fields` — 具名字段与凭据

上面那种「拼好的连接串」适合**地址列表**（ZooKeeper、Kafka 的 broker 列表）——形状是确定的。但一旦牵扯到带凭据的连接，提供方就**没法知道消费方要什么形状**：libpq DSN、JDBC URL、拆开的 host/port/user/password、`.pgpass`……让提供方去猜，等于把消费方的实现细节写进了提供方。

因此 `exports` 还可以导出**具名字段**，由消费方自己拼：

```yaml
# postgresql pack —— 只说「我有什么」，不猜「你要什么形状」
params:
  app_user:     { type: string, default: appuser }
  app_db:       { type: string, default: app }
  app_password:
    type: secret
    generate: { length: 32 }        # 见 §7.6

exports:
  app:
    description: "应用账号连接信息"
    role: primary
    port: "{{ .Params.port }}"
    fields:
      host:     "{{ .Address }}"
      port:     "{{ .Params.port }}"
      database: "{{ .Params.app_db }}"
      username: "{{ .Params.app_user }}"
      password: "{{ .Params.app_password }}"
```

消费方按需组装：

```yaml
# go-webapp pack
requires:
  packs:
    postgresql: { version: ">=14", scope: site }

params:
  db_url:
    type: secret                                    # 见下方「污点」
    from: >-
      postgres://{{ .Requires.postgresql.Exports.app.username }}:{{ .Requires.postgresql.Exports.app.password }}@{{ .Requires.postgresql.Exports.app.host }}:{{ .Requires.postgresql.Exports.app.port }}/{{ .Requires.postgresql.Exports.app.database }}
```

| | `format`（§5.3） | `fields`（本节） |
|---|---|---|
| 产出 | 一个字符串 | 一组具名值 |
| 适用 | 形状确定的地址列表 | 带凭据、消费方形状各异 |
| 多实例 | 按 `separator` 连接 | 取 `role` 的**第一个**实例；需要全部实例时用 `format` |

两者可以同时声明，互不冲突——`format` 本质上是 `fields` 的一个便利特例。

#### 凭据与「污点」

字段值引用了 `type: secret` 的参数时，**该字段自动带上敏感标记**，不需要额外语法（也就不会与实际不一致）。`mechpack inspect` 会展示它，让消费方作者在没有提供方源码时也能看到契约：

```
$ mechpack inspect postgresql
exports:
  app  (role: primary)
    host      string
    port      int
    username  string
    password  secret      ← 敏感
```

**敏感标记会随取值传播**：mechd 在绑定时若发现某参数的值来自敏感字段，**直接把该参数标为 sensitive**，不管消费方 Pack 声明的是什么。

> 这条**不是** `mechpack lint` 能检查的——lint 只看得见一个 Pack，而依赖方可能来自别处、单独发布。放在 mechd 的绑定阶段既是唯一有全局视角的地方，也让它**不可能被遗忘**：消费方 Pack 写没写 `type: secret` 都不影响安全性。
>
> 消费方未声明时，`mechctl component deploy` 会提示建议补上，但**不阻断部署**——不能因为别人 Pack 的声明问题卡住你的部署。

#### 消费方绝不能伸手取提供方的参数

```yaml
# ✗ 非法：绕过导出契约直接读提供方的参数
from: "requires.postgresql.params.admin_password"
```

`.Requires.<pack>.Params` **不存在**。理由有两层：

- 与 §5.3 的哲学一致——提供方一旦改参数名，所有消费方同时失效
- **提供方的超级用户口令不该给消费方**。需要被消费的凭据应当是一个**专门为这段依赖关系存在的账号**（上例的 `app_user`），由提供方的 `postInstall` hook 建出来（见 [postgresql 示例](../../examples/packs/postgresql/)）

---

### 5.5 版本范围表达式

`requires.packs[].version` 与 `upgradePolicy.compatible` 用同一套语法。

| 写法 | 含义 |
|---|---|
| `*`（或留空） | 任意版本 |
| `>=16` `>16` `<=16` `<16` | 比较 |
| `=16` 或裸写 `16` | **精确匹配已写出的各段**：`16` 匹配 16.4，`16.4` 不匹配 16.5 |
| `~16.4` | 同一次版本内（`>=16.4`, `<16.5`）；`~16` 即 16.x |
| `^16.4` | 同一大版本内（`>=16.4`, `<17`） |
| `>=14, <16` | 逗号分隔表示**同时满足** |

> **比较是逐段的**：`<=16` 等价于 `<=16.0.0`，**不匹配 16.4**——
> 因为 16.4 大于 16.0。想表达「16.x 都行」要写 `~16` 或 `<17`。
> 这与 npm / cargo 的语义一致，但确实容易误读，所以在这里点明。

**不支持 `||` 析取**——真实依赖里没见过需要它的场景，而它会让「为什么这个
版本没被选中」难以解释。**不支持 `!=`**——依赖应当声明「需要什么」而非
「不要什么」。

版本号本身**宽松解析**：只取各段的前导数字，其余当作预发布后缀
（`3.6.0-rc1`、`252.22-1~deb12u1` 都能用）。比较时逐段比，全部相等则
**有后缀的更小**（预发布先于正式版）。缺省段视为 0，因此 `16` 与 `16.0` 相等。

> 多个版本同时满足时取**最高**的：依赖声明的是下限，用户装了更新的版本
> 通常就是想用它。

## 6. `blobs` — 载荷

```yaml
blobs:
  main:
    linux/amd64:
      sha256:    "ab12ef…"
      size:      31457280
      filename:  "postgresql-16.4-linux-amd64.tar.gz"
      sourceUrl: "https://ftp.postgresql.org/pub/source/v16.4/…"   # 可选
    linux/arm64:
      sha256:   "cd34ab…"
      size:     30214656
      filename: "postgresql-16.4-linux-arm64.tar.gz"

  image:                              # 容器镜像（docker save 输出）
    linux/amd64:
      sha256:    "ef56…"
      size:      104857600
      filename:  "postgres-16.4-amd64.docker.tar"
      mediaType: docker-archive
```

| 字段 | 必填 | 说明 |
|---|---|---|
| `sha256` | ✅ | 小写十六进制 64 字符。**内容寻址的唯一依据** |
| `size` | ✅ | 字节数，用于进度显示与空间预检 |
| `filename` | ✅ | **原始文件名**。blob 在存储中按摘要命名（`sha256-ab12ef…`），失去可读性；此字段保留来源文件名，供组件包运维追溯「这个 blob 到底是什么」 |
| `sourceUrl` | | 上游来源地址。**仅作记录，部署阶段绝不访问**；用于供应链追溯与合规审计 |
| `mediaType` | | `tar` \| `tar.gz` \| `tar.zst` \| `zip` \| `raw` \| `docker-archive`。缺省由使用它的资源类型推断 |

平台键必须是 `platforms` 的子集。**每个声明的平台都必须在每个 blob 中有条目**，否则 lint 报错。

### 6.1 `sources` — 源 Pack 专用，不进入发布产物

作者写的 pack.yaml 用 `sources` 描述载荷从哪里来；`mechpack assemble` 据此计算摘要并把它换成 `blobs`。

```yaml
sources:
  main:
    linux/amd64: dist/app-amd64.tar.gz                    # 简写：路径
    linux/arm64: { path: dist/app-arm64.tar.gz, mediaType: tar.gz }
  image:
    linux/amd64:
      path:      dist/app-amd64.docker.tar
      mediaType: docker-archive
      sourceUrl: "https://…"
```

| 字段 | 说明 |
|---|---|
| `path` | **必填**。相对 Pack 目录；构建产物在别处时用 `assemble --source-root` 指定基准目录 |
| `mediaType` / `sourceUrl` | 原样写入生成的 blob 条目 |
| `filename` | 覆盖记入 blob 的原始文件名，缺省取 `path` 的 basename |

### 6.2 `mechpack assemble` 的填充职责

源 pack.yaml 可以省略下列字段，`assemble` 会计算并写入**发布产物**：

| 字段 | 来源 |
|---|---|
| `blobs[*][*].sha256` / `size` / `filename` | 从 `sources` 指向的本地文件计算 |
| `platforms` | 各 blob 的平台键**并集**；若各 blob 的平台键集合不一致则**报错**，要求补齐 blob 或显式声明 `platforms` |
| `revision` | 未写时显式落盘为 `1`——产物不应依赖「默认值」这一实现细节 |

```
$ mechpack assemble
✗ blob "image" 缺少平台 linux/arm64（其他 blob 声明了该平台）
  请补齐该平台的 blob，或显式声明 platforms 以缩小支持范围
```

推导只发生在打包期——作者就在现场，是给出错误的最佳时机。发布产物中 `platforms` 永远显式存在，[§19](#19-校验规则汇总) 的交叉校验因此始终有效。

**产物中的 pack.yaml 保留作者写的注释与字段顺序。** `assemble` 对 YAML 节点树做外科式修改（`sources` 就地换成 `blobs`），而不是「解析成结构体再序列化回去」——后者会丢掉全部注释，产出一份能用但读不懂的文件。

载荷按内容寻址落盘为 `blobs/sha256-<digest>`，**内容相同的载荷只存一份**。

> 无载荷的 Pack（纯主机配置）**必须显式声明 `platforms`**——没有 blob 可供推导。

---

## 7. `params` — 参数定义

### 7.1 声明形式

```yaml
params:
  port:
    type: port
    default: 5432
    description: "监听端口"
    group: "网络"
    restartRequired: true

  max_connections:
    type: int
    default: 100
    min: 10
    max: 10000
    advanced: true
    reloadRequired: true

  encoding:
    type: enum
    values: [UTF8, LATIN1]
    default: UTF8
    immutable: true

  admin_password:
    type: secret
    required: true

  primary_host:
    type: string
    from: "topology.role('primary').nodes[0].address"

  heap:
    type: size
    defaultFrom: '{{ min (div .Node.Facts.Memory.Total 2) "31GB" }}'
    default: 2GB
    description: "JVM 堆，默认为物理内存的一半且不超过 31GB"
    restartRequired: true
```

### 7.2 类型（12 种）

**这是完整且封闭的类型集，不存在自定义或外部 schema 逃生舱。** 若某个场景无法表达，正确做法是**为 pack/v1 增补新类型**（向后兼容，配合 `requires.mecharion` 声明引擎版本下限），而不是引入第二套 schema 体系。理由见 [ADR-0007](../adr/0007-params-custom-subset.md)。

| 类型 | YAML 表示 | 说明 |
|---|---|---|
| `string` | `"abc"` | 支持 `pattern`（RE2 语法） |
| `int` | `100` | 支持 `min` / `max` |
| `float` | `1.5` | 支持 `min` / `max` |
| `bool` | `true` | |
| `enum` | `UTF8` | 必须同时给 `values` |
| `path` | `/var/lib/x` | 校验为绝对路径；UI 渲染路径选择器 |
| `port` | `5432` | 范围 1–65535 |
| `duration` | `30s` `5m` `1h` | Go duration 语法，模板中可取 `.Seconds` / `.Milliseconds` / `.Nanoseconds` |
| `size` | `4GB` `512MB` `1Gi` | 十进制（KB/MB/GB/TB）与二进制（Ki/Mi/Gi/Ti）均可，模板中可取 `.Bytes` |

> **`size` 与 `duration` 在模板里是带访问器的值，不是裸字符串。**
> `{{ .Params.tick_time }}` 得到 `2000ms`，`{{ .Params.tick_time.Milliseconds }}`
> 得到 `2000`——nginx 要秒数、PostgreSQL 与 ZooKeeper 要毫秒、Go 应用的 YAML
> 要纯字节数，三种都得取得到，否则 Pack 作者只能在模板里手写单位换算。
>
> 它们也可直接参与算术：`{{ div .Params.heap 2 }}` 不必先写 `.Bytes`。
> 落进已解析规格时只留字面量——规格是线格式，不需要这层渲染期的便利。
| `cidr` | `10.0.0.0/8` | |
| `secret` | `"…"` | 隐含 `sensitive: true`，不落盘于事件与日志 |
| `list<T>` | `[a, b]` | `T` 为上述任一标量类型，如 `list<path>` |

### 7.3 字段

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `type` | string | — | **必填** |
| `default` | 同 type | — | 无 default 且 `required: true` 时用户必须提供 |
| `required` | bool | `false` | |
| `description` | string | — | UI 与 `mechctl component show` 显示 |
| `min` / `max` | 数值 | — | `int` `float` `port` `size` `duration` 适用 |
| `pattern` | string | — | RE2 正则，`string` 适用 |
| `values` | list | — | `enum` **必填** |
| `unit` | string | — | UI 显示单位后缀 |
| `group` | string | `"常规"` | UI 分组 |
| `advanced` | bool | `false` | UI 默认折叠 |
| `sensitive` | bool | `false` | 日志/事件/UI/API 响应中脱敏 |
| `immutable` | bool | `false` | 创建后拒绝变更，提示需重建 Component |
| `restartRequired` | bool | `false` | 变更后重启 workload |
| `reloadRequired` | bool | `false` | 变更后 `Runtime.Reload` |
| `from` | string | — | **完全推导**，用户不可填。见 §7.5 |
| `defaultFrom` | string | — | **计算出的初始默认值**，用户可覆盖。见 §7.5 |
| `generate` | map | — | 由引擎生成初始值，仅 `secret` 适用。见 §7.6 |

`restartRequired` 与 `reloadRequired` 互斥；同时为真则 lint 报错。
`from` 与 `defaultFrom` 互斥；`from` 与 `default` 互斥。
`generate` 与 `default` / `defaultFrom` / `from` 均互斥。

### 7.4 `from` 与 `defaultFrom`

两者都是**模板表达式**，用同一套引擎与函数集求值（§9.1），不引入第二种表达式语言。区别在于**用户能否覆盖**：

| 字段 | 语义 | 用户可否设值 | 典型来源 |
|---|---|---|---|
| `from` | 该值是部署的**客观事实**，由引擎推导 | ❌ 不可 | `topology.*` `requires.*` |
| `defaultFrom` | 计算出的**初始默认值** | ✅ 可以 | `.Node.Facts.*` |

```yaml
# 客观事实：primary 在哪台机器上，不是用户能"选择"的
primary_host:
  from: "topology.role('primary').nodes[0].address"

# 建议值：按内存算出一个合理的堆，但运维完全可以按经验改
heap:
  defaultFrom: '{{ min (div .Node.Facts.Memory.Total 2) "31GB" }}'
  default: 2GB          # defaultFrom 求值失败时的兜底，defaultFrom 存在时必填
```

`defaultFrom` 求值失败（事实缺失、除零等）时回落到 `default` 并记录告警，**不中止部署**——一个采集不到内存的节点不应该阻断整个 Rollout。

**求值时机**：两者都在 mechd 放置阶段求值，结果作为参数值固化。事实此后变化不会自动改变已固化的值，见 §9.4。

### 7.5 作用域与优先级

`params` 可出现在三处：

- **顶层** — Component 级，所有角色共享
- **`roles[].params`** — 该角色专有
- **`profiles[].params`** — 该形态专有（见 §13），或覆盖上述两者的 `default`

取值解析顺序（后者覆盖前者）：

```
Pack 默认值  →  Component 级取值  →  Role 级取值  →  ConfigGroup 级取值
```

> **ConfigGroup** 是「共享同一份参数覆盖的、具名的 RoleInstance 子集」。单节点的差异化配置表现为一个只含该节点的组——**模型中不存在无名的 per-node 覆盖**。这只影响部署侧的取值来源，Pack 格式中 `params` 的声明方式不受影响。见 [ADR-0021](../adr/0021-config-group.md)。

---

### 7.6 `generate` — 引擎生成的密码

让运维在提供方和消费方**各填一次同一个密码**，是错误的来源，轮换时更是灾难。
`generate` 把这件事交给引擎：

```yaml
app_password:
  type: secret
  generate: { length: 32 }
```

| 字段 | 默认 | 说明 |
|---|---|---|
| `length` | `32` | 生成长度 |
| `charset` | `alnum` | `alnum` \| `alnumSymbol` \| `hex` |

**只有 `type: secret` 可以用 `generate`。** 其它类型的值要么是客观事实（用 `from`），要么该由人决定（用 `default` / `defaultFrom`）。

行为：

- **首次部署时生成一次**，之后固化，重复 `apply` 不会重新生成——否则每轮调和都会换一次密码
- 运维**可以**显式覆盖（提供自己的值），此时 `generate` 不再生效
- 生成值由 mechd 持有，**从不显示**；需要轮换时用 `mechctl component rotate-secret`

`charset` 默认排除符号，是因为口令常常要穿过 shell、连接串、YAML 与各家应用自己的解析器，**符号是转义 bug 的主要来源**。确实需要更高熵时用 `alnumSymbol`，但要先确认消费方处理得了。

> **无人值守与离线由此成立**：运维不需要输入任何东西，引擎也不需要联系任何外部密钥服务。这是相对「必须有 Vault 可达」类方案的实质差异。

### 7.7 口令怎么交给应用

**先说一条做不到的**：让密钥不出现在任何渲染产物中。应用要从配置里读口令，
那个文件就必然是明文——Cloudera Manager 渲染进 `hive-site.xml`、Ansible Vault
解密后落目标机、Kubernetes 把 Secret 挂成 tmpfs 明文，无一例外。**加密解决的
是静态存储与传输，消费点必然落明文。**

能选的是**落在哪儿**。按泄漏面从大到小：

| 形态 | 谁看得到 | 应用支持度 | 建议 |
|---|---|---|---|
| 内联进主配置 | 那份配置被传阅即泄漏（工单、支持包、有人提交进 git） | 全部 | 没有更好选择时才用 |
| **内联 `Environment=`** | **unit 文件是 0644 全体可读，且 `systemctl show -p Environment` 原样打印** | — | ❌ **规则 50 禁止** |
| `envFile` + `${VAR}` | `/proc/<pid>/environ`；`systemctl show` 只显示路径不显示值 | 大部分 | ✅ **默认选这个** |
| 文件路径间接（`password_file`） | 只有能读该文件的用户 | 少数，各家写法不同 | ✅ 应用支持就用 |

第三行是推荐做法，**不需要任何加密**——只需要把口令拆到另一个文件：

```yaml
resources:
  - template: { src: env.tmpl,  dest: "{{ .Paths.Config }}/env", mode: "0640" }
  - template: { src: app.tmpl,  dest: "{{ .Paths.Config }}/app.conf", mode: "0644" }
workload:
  systemd:
    envFile: "{{ .Paths.Config }}/env"
```

主配置里只有 `${DB_PASSWORD}` 占位，因此可以随手贴进工单；口令留在 0640 的
那个文件里。这正是 Jasypt / `ENC(...)` 类方案真正想解决的问题，而代价是零。

第四行更好，但**没有标准**：`PGPASSFILE`、Docker 的 `*_FILE` 约定、
Prometheus 的 `password_file`、Grafana 的 `$__file{}`——各写各的。
应用恰好支持时用一个 `template` 资源渲染出那个文件即可，本规范不需要新机制。
它还有个运维上的好处：**轮换只需重写一个小文件**，而环境变量必须重启进程。

> `generate` 默认字符集是 `alnum`（§7.6），正是为了让口令能安全穿过
> `EnvironmentFile`、连接串与各家应用自己的解析器，不必操心转义。

## 8. `paths` — 路径声明

### 8.1 不变式

> **generation 目录是只读的。任何需要可写、或需要跨版本存活的路径，其物理存储必须在 generation 目录之外。**

`layout: inline`（§8.5）是这条不变式的**受控例外**：配置物理位于 generation 内，但由于每次配置变更都产生新 generation，generation 本身仍然从不被原地修改。

### 8.2 预定义路径名

| 名称 | 默认值 | 用途 |
|---|---|---|
| `home` | `{{ .Node.Roots.opt }}/apps/{{ .Component }}` | 安装根 |
| `config` | `{{ .Node.Roots.etc }}/apps/{{ .Component }}` | 配置 |
| `data` | `{{ .Node.Roots.data }}/apps/{{ .Component }}` | 数据（升级永不触碰） |
| `logs` | `{{ .Node.Roots.logs }}/apps/{{ .Component }}` | 日志 |
| `runtime` | `{{ .Node.Roots.run }}/apps/{{ .Component }}` | 运行期可写临时目录 |

`generation` 与 `current` 为只读派生值：

- `.Paths.Generation` = `{{ .Paths.Home }}/generations/<seq>-<version>-<revision>`
- `.Paths.Current` = `{{ .Paths.Home }}/current`（软链，运行期一律引用它）

Pack 可覆盖任一预定义路径，也可定义自定义路径名。

### 8.3 字段

```yaml
paths:
  config:
    default:  "{{ .Node.Roots.etc }}/apps/{{ .Component }}"
    linkInto: "{{ .Paths.Generation }}/config"
    distDir:  "config"
    mode:     "0750"
    owner:    postgres

  dataDirs:
    kind: multi
    default: ["{{ .Node.Roots.data }}/apps/{{ .Component }}/dfs"]
    subpath: "dfs/dn"
```

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `default` | string \| list | — | **必填**。`kind: multi` 时为 list |
| `kind` | enum | `single` | `single` \| `multi` |
| `layout` | enum | `separate` | `separate` \| `inline`，见 §8.5 |
| `linkInto` | string | — | 在 generation 内建软链指向 `default`。见 §8.4 |
| `distDir` | string | — | 载荷中自带的同名目录，物化时改名为 `<distDir>.dist` 保留 |
| `subpath` | string | — | `kind: multi` 时，用户给卷名，引擎追加此子路径 |
| `mode` | string | `"0755"` | |
| `owner` / `group` | string | `root` | |

**引擎自动创建 `paths` 中声明的全部路径**，按各自的 `owner` / `group` / `mode`。`kind: multi` 时逐个创建。

因此 Pack **不需要**为这些路径再写 `directory` 资源——那是重复声明，且两处的 `owner`/`mode` 可能不一致。只有 `paths` 之外的目录才需要 `directory` 资源。

> 这条规则消除了多盘场景下「为每块盘建一个目录」的需求，因而 `resources` 不需要迭代机制。见 [examples/packs/hdfs](../../examples/packs/hdfs/README.md)。

### 8.4 `layout: separate` + `linkInto`（默认路径）

以 Kafka（配置必须位于 `$KAFKA_HOME/config/`）为例：

1. 解开 blob 到 generation 目录
2. 若声明了 `distDir`，把 generation 内的 `config/` 改名为 `config.dist/`
3. 渲染权威配置到 `default` 指向的位置（`/etc/mecharion/apps/kafka/`）
4. 建软链：`<generation>/config -> /etc/mecharion/apps/kafka`

三种配置布局全部由此覆盖：

| 布局 | 声明方式 |
|---|---|
| 配置独立（nginx、Go 应用） | 只写 `default`，不写 `linkInto` |
| 配置在 home 内（Kafka/Tomcat/ES） | `default` + `linkInto` + `distDir` |
| 配置在 data 内（PostgreSQL） | `default: "{{ .Paths.Data }}/pgdata"`，无需 `linkInto` |

### 8.5 `layout: inline`

极少数应用会解析符号链接并因此行为异常（自行计算 `$HOME` 后拼接路径、拒绝跟随软链读配置等）。此时配置直接渲染进 generation 目录：

```yaml
paths:
  config:
    layout:  inline
    default: "{{ .Paths.Generation }}/conf"
    distDir: "conf"
```

| | `separate`（默认） | `inline` |
|---|---|---|
| 配置物理位置 | generation 之外 | generation 之内 |
| 是否有软链 | 视 `linkInto` | 无 |
| 配置变更的效果 | 原地渲染，可 `reload` | **产生新 generation**，走完整切换流程 |
| 回滚配置 | 切回上一 generation | 切回上一 generation（完全相同） |
| 备份配置 | 备份 `/etc/mecharion/apps/<x>` | 备份 generation 目录 |

因为「配置变更 → 新 generation」本就是既定语义（[ADR-0008](../adr/0008-immutable-generation-linkinto.md)），`inline` 不引入任何新的回滚概念——它只是让每次配置变更的物化成本变高。

**实现要求**：mechlet 生成新 generation 时，未变化的文件从上一 generation **硬链接**，避免重复解压载荷。

**约束**：`default` 必须位于 generation 内；与 `linkInto` 互斥；不得用于 `data` / `logs`（它们必须跨 generation 存活）。

**何时使用**：只在 `separate` 确认不可行时。`inline` 会让每次配置微调都产生一次完整的 generation 切换与服务重启窗口。

### 8.6 多盘

Node 在 `mechlet.yaml` 中声明卷：

```yaml
volumes:
  - { name: data1, path: /data1, class: bulk }
  - { name: data2, path: /data2, class: bulk }
  - { name: ssd1,  path: /ssd1,  class: fast }
```

`class` 为可选的自由文本标签，v1 中**仅用于 CLI/UI 的筛选与展示**，不参与自动选盘（理由见 [04-paths-and-storage](../design/04-paths-and-storage.md)）。

用户在 ConfigGroup 上按卷名显式绑定：

```yaml
configGroups:
  - name: 12-disk-nodes
    nodes: [node-7, node-8]
    paths:
      dataDirs: [data1, data2]     # → /data1/dfs/dn, /data2/dfs/dn
```

模板中 `{{ .Paths.DataDirs }}` 为字符串列表，顺序与声明顺序一致且稳定。

### 8.7 路径的解析与固化

```
① Pack 声明        paths.home.default: "…/apps/{{ .Component }}"   模板
② mechd 放置阶段    渲染 → /opt/mecharion/apps/pg-main               具体路径
③ mechlet 首次物化  把解析后的绝对路径写入本地状态                     ★ 固化
④ 后续每次调和      读取已固化的路径，不重新推导
```

**第 ④ 步是硬要求。** 若每次调和都重新推导，则用户修改 `Node.Roots.data`、或 Pack 升级时改了 `paths` 默认值，已安装的组件会**静默搬家**——新路径下是空目录，旧数据变成无人认领的孤儿。

> **已存在 RoleInstance 的路径不可变。** mechlet 检测到解析结果与已固化值不一致时，**拒绝调和并报错**，而不是自动迁移。

```
✗ pg-main@node-3 路径变更被拒绝
  paths.data  已固化: /var/lib/mecharion/apps/pg-main
              本次解析: /data1/apps/pg-main
  迁移数据目录是运维动作，请手工迁移后使用 mechctl component adopt-path 更新记录
```

这与 mechd 自身 `--data-dir` 安装后不可变、以及依赖绑定不重新解析（§5.2）是同一条原则。

### 8.8 路径中的名字是 Component 名

默认路径模板用的是 **`.Component`（Component 名）**，不是 Pack 名。

**必须如此**：同一个 Pack 可以在一个 Site 里部署多份（`pg-main` 与 `pg-report` 都来自 `postgresql`）。若按 Pack 名，两份部署会落进同一个数据目录——直接毁库。

`mechctl deploy <pack>` 省略 `-c` 时，**Component 名默认等于 Pack 名**，因此常见情况下两者一致；只有用户主动改名时才分叉，而那时用户是知情的。

---

## 9. 模板

### 9.1 引擎

Go `text/template`。`templates/` 下所有文件作为**一个 template set** 解析，因此 `{{ define }}` / `{{ template }}` 组合开箱可用。**`_` 前缀的文件视为片段**，不会被单独渲染。

**pack.yaml 中的字段表达式与 `templates/` 共享同一个 template set。** 解析顺序为「先解析 `templates/`，再求值 pack.yaml 字段」，因此字段中可以调用片段：

```yaml
params:
  endpoints:
    from: '{{ template "minio-endpoints" . }}'
```

这让复杂表达式（跨节点、跨磁盘的枚举）可以从 `systemd.exec` 这类单行字段中抽出来，写在有换行与注释的片段里。

受限函数集：

```
default  quote  squote  upper  lower  trim  replace  join  split
indent   nindent  b64enc  b64dec  toYaml  toJson
add sub mul div  min max
```

**不提供**：`env`、`exec`、文件读取、网络访问等任何可绕过 hermetic 约束的函数。

渲染时启用 `missingkey=error`——引用不存在的变量是错误，不是空串。

### 9.2 变量表

| 变量 | 类型 | 说明 |
|---|---|---|
| `.Pack.Name` `.Pack.Version` `.Pack.Revision` | string | |
| `.Profile` | string | 当前部署形态名；未声明 `profiles` 时为 `""` |
| `.Site.Name` `.Site.Kind` `.Site.Labels` | | |
| `.Component` | string | Component 名 |
| `.Role` | string | 当前角色名 |
| `.Params.<key>` | 按 type | 已解析的参数值 |
| `.Paths.Home` `.Config` `.Data` `.Logs` `.Runtime` `.Generation` `.Current` | string | 已解析的绝对路径 |
| `.Paths.<custom>` | string \| list | 自定义路径 |
| `.Node.Name` `.Node.Labels` `.Node.Address` | | 当前节点 |
| `.Node.Roots.opt/etc/data/logs/run` | string | 节点路径根 |
| `.Node.Volumes` | map | 卷名 → `{path, class}` |
| `.Node.Facts.*` | | 节点事实，见 §9.4 |
| `.Requires.<pack>.*` | | 依赖 Pack 的解析结果，见 §9.5 |
| `.Topology` | | 见 §9.3 |
| `.Topology.Ordinal` | int | 当前实例在同角色中的序号 |
| `.Generation.Seq` `.Generation.Previous` | | |

### 9.3 `topology` 引用

```yaml
params:
  primary_host:
    type: string
    from: "topology.role('primary').nodes[0].address"

  zk_quorum:
    type: string
    from: "topology.role('zookeeper').nodes | join(',')"
```

模板中亦可直接使用：

```
{{ range .Topology.Role "datanode" }}{{ .Address }}:9866
{{ end }}
```

**求值时机**：`topology` 引用要求**模板渲染发生在角色放置确定之后**。渲染由 mechd（单机形态下为 mechlet）完成，mechlet 收到的是**已解析的拓扑快照**，节点之间不需要互相查询。

| 表达式 | 返回 |
|---|---|
| `topology.role('<name>')` | 该角色的 RoleInstance 列表 |
| `.nodes` | 节点列表 |
| `.nodes[i].address` / `.name` / `.labels` | 节点属性 |
| **`.paths.*`** | **该实例已解析的路径**（见下） |
| `topology.self` | 当前 RoleInstance |
| `topology.ordinal` | 当前实例**在同角色中**的序号（从 0 起，**一次分配后永不改变**） |

> **ordinal 是分配出来的，不是算出来的。** 它在实例首次创建时取「当前最大 + 1」，
> 之后固化——与节点名、成员集合都无关。因此 Pack 可以放心用它生成节点身份
> （ZooKeeper 的 `myid`、Kafka 的 `node.id`）。
>
> 代价是**序号会有空洞**：实例被移除后序号不回收，可能看到 `0, 2, 5`。
> 这是刻意的——回收会让新实例拿到刚被释放的编号，而集群里其它成员的元数据
> 中可能还留着对旧成员的引用。
>
> 早期版本曾定义为「按节点名排序」，那是错的：给一个部署在 `n2,n3,n4` 的
> ZooKeeper 加入 `n1`，会让三个已有节点的 `myid` 同时改变，集群直接损坏。
> 见 [ADR-0028](../adr/0028-stable-ordinals.md)。

#### 拓扑条目携带已解析的路径

拓扑中每个 RoleInstance 都带 `.Paths.*`——**该实例在它自己那台节点上解析出的路径**，而非当前节点的。

这是必需的：节点之间的挂载点可以不同，因此**不能用本机的 `.Paths.DataDirs` 去推断对端**。MinIO 的端点列表是典型场景：

```
{{- range .Topology.Role "server" }}{{ $n := . }}
{{- range .Paths.DataDirs }} http://{{ $n.Address }}:9000{{ . }}{{ end }}
{{- end }}
```

**这与 `scope: site` 依赖不暴露 `.Paths`（§9.5.1）不矛盾**：那里引用的是**别的 Component** 在别的机器上的路径，属于跨越封装边界；这里是**同一个 Component 内部**的对等实例，Pack 作者本就该知道它们的布局。

> **`ordinal` 在角色内唯一，不是全 Component 唯一。** 多个角色各有一条从 0 开始的独立序列。需要全 Component 唯一的编号（如 Kafka 的 `node.id`）时，按角色加偏移：
>
> ```
> {{- if eq .Role "broker" }}node.id={{ add .Topology.Ordinal 100 }}
> {{- else }}node.id={{ .Topology.Ordinal }}{{ end }}
> ```

---

### 9.4 `.Node.Facts` — 节点事实

由 mechlet 采集并上报的节点客观属性。

#### 9.4.1 两种用途，规则完全不同

| 用途 | 数据源 | 事实变化时 |
|---|---|---|
| **判定条件**（`requires.resources.memory: 2GB`） | **实时** | 不满足则快速失败——这是一次检查，不产生值 |
| **配置取值**（`defaultFrom` 算出的 heap） | **放置时快照，固化到 RoleInstance** | 只**上报漂移**，不自动改配置 |

第二条是硬要求。若配置取值跟随实时事实：

```
节点加了内存 16GB → 32GB
  ↓ 下次调和
heap 从 8G 变成 16G
  ↓ restartRequired
服务在业务时间被重启
```

更糟的情况：某次事实采集出 bug 报了 0 字节 → `heap=0` → 服务起不来。

**这与 `volumeClass` 自动选盘是同构的风险**——自动跟随外部变化，会把「运维觉察不到的事实变动」变成「生产环境的配置变更」。

事实漂移复用已有的漂移呈现机制，由人决定何时应用：

```
$ mechctl node facts diff node-7
  memory.total    渲染时: 16GB   当前: 32GB
  受影响参数: es-prod/data.heap = 8GB
  → mechctl node facts refresh node-7 --apply   重新解析并生成 Rollout
```

#### 9.4.2 事实集

```yaml
hostname / fqdn / machineId / bootId
arch:         amd64 | arm64
os:           { family, distro, version, kernel }
cpu:          { sockets, cores, threads, model }
memory:       { total, available }                      # size
filesystems:  [ { mountpoint, device, fstype, total, free, rotational } ]
interfaces:   [ { name, mac, addresses[], mtu, speed } ]
capabilities: { systemd: {…}, docker: {…}, compose: {…} }
custom:       { … }
```

`capabilities` 是事实的一部分——一套采集机制、一个命名空间。`requires.capability`（§5）是针对它的匹配器。

#### 9.4.3 自定义事实

mechlet 执行 `/etc/mecharion/facts.d/*.sh`，各脚本输出 JSON，合并到 `facts.custom`（对标 Ansible 的 `facts.d`）。硬件 SKU、机架编号、租户标识这类站点特有的事实，用户不必等 Mecharion 增加字段。

> 这**不违反 hermetic 约束**——`facts.d` 是节点侧的运维配置，不是 Pack 内容，与「部署阶段不依赖外部服务」无关。

#### 9.4.4 facts 与 labels 的区别

| | 来源 | 语义 | 用途 |
|---|---|---|---|
| `labels` | 用户**声明** | **意图**：我认为这台机在 r12 机架 | 节点选择、`placement` 的 `scope` |
| `facts` | mechlet **观测** | **现实**：这台机报告 32GB 内存 | `requires` 校验、`defaultFrom` 取值 |

**判据：参与放置约束的写 label，只作数据用的写 fact。** 二者都存在且不重复。

> 见 [ADR-0023](../adr/0023-node-facts.md)

---

### 9.5 `.Requires` — 依赖 Pack 引用

`requires.packs` 中声明的每个依赖，在放置阶段被解析为具体的 Component，其结果注入渲染上下文。

#### 9.5.1 可用字段随 scope 而变

| scope | 可用 | 例 |
|---|---|---|
| `node` | `.Paths.*` `.Version` `.Component` | `{{ .Requires.jdk11.Paths.Current }}` |
| `site` | `.Topology.Role(…)` `.Version` `.Component` | `{{ .Requires.zookeeper.Topology.Role "server" }}` |

`scope: site` 的依赖**不提供 `.Paths`**——那是别的机器上的路径，引用它必然是 bug。lint 直接报错（规则 40）。

#### 9.5.2 用法

```sh
# hadoop-env.sh.tmpl —— node-scoped，取本机路径
export JAVA_HOME="{{ .Requires.jdk11.Paths.Current }}"
```

```yaml
# site-scoped，从依赖的拓扑推导连接串（手敲 quorum 是最易出错的一步）
zk_quorum:
  type: string
  from: "requires.zookeeper.topology.role('server').nodes | join(':2181,')"
```

#### 9.5.3 稳定性

`.Paths.Current` 是**稳定软链**：被依赖 Pack 打小版本升级时该路径不变，因此不引发无谓的重渲染。

`.Version` 会随依赖升级而变——引用它的模板会重新渲染，可能触发重启。这是正确行为，但 Rollout 必须在执行前告知影响面：

```
升级 jdk11 16.4 → 16.5 将影响 2 个下游 Component:
  hdfs-prod  (3 个角色，共 12 个实例将重启)
  spark-prod (2 个角色，共 6 个实例将重启)
```

> 见 [ADR-0024](../adr/0024-cross-pack-reference.md)

---

## 10. `shared` — 角色共有部分

```yaml
shared:
  resources:
    - user:    { name: postgres, system: true }
    - archive: { blob: main, dest: "{{ .Paths.Generation }}", strip: 1 }
```

同一节点上同一 Component 的多个角色共存时，`shared.resources` **只应用一次**（按资源身份去重）。

---

## 11. `roles` — 角色

```yaml
roles:
  - name: primary                # 可选  单角色时可省略，默认 "default"
    description: "主库"          # 可选
    scope: node                  # 可选  node（默认）| cluster（v1 不支持）
    cardinality: "1"             # 可选  默认 "0-N"
    quorum: false                # 可选  本角色是否构成多数派仲裁，见下
    requires: [zookeeper]        # 可选  同 Component 内的角色依赖
    params:    { … }             # 可选  角色专有参数
    paths:     { … }             # 可选  覆盖顶层 paths
    resources: [ … ]             # 可选  见 §14
    workload:  { … }             # 可选  无此段 = 只落文件不起进程
    health:    { … }             # 可选  见 §15.4
    hooks:     { … }             # 可选  仅对该角色生效，见 §16
```

### `cardinality`

| 值 | 含义 |
|---|---|
| `"1"` | 恰好 1 个 |
| `"0-1"` | 至多 1 个 |
| `"1-N"` | 至少 1 个 |
| `"0-N"` | 任意（默认） |
| `"2"` `"3"` | 恰好 N 个 |

在 mechd 放置阶段校验，不满足则拒绝。

### `quorum`

声明本角色的实例集合**构成多数派仲裁**（ZooKeeper、etcd、HDFS JournalNode、Kafka KRaft controller）。引擎据此得到两个行为：

| 行为 | 说明 |
|---|---|
| **实例数建议为奇数** | 偶数只**告警**不拒绝——4 节点 ZK 能跑，只是容错能力与 3 节点相同、白白多一台机器 |
| **Rollout 强制 `maxUnavailable ≤ (N-1)/2`** | 滚动重启永不打破仲裁 |

第二条是 `quorum` 存在的主要理由。**仲裁语义只有 Pack 作者知道**——用户设置 Rollout 的 `maxUnavailable` 时无从判断某个组件能同时下线几个实例。没有这条声明，一次「并发度 2」的滚动重启就能让 3 节点 ZK 集群失去多数派。

### `requires`

同 Component 内的角色依赖，决定**启动顺序**（被依赖者先启动）、**停止顺序**（反向）、**滚动升级顺序**（拓扑排序后分批）。不得成环，lint 检测。

> `requires` 只约束**时序**，不约束**位置**。位置约束见 §12。

若被依赖的角色在当前 profile 中 `enabled: false` 或 `cardinality: "0"`，**该依赖自动忽略**，不报错。否则每个 profile 都要重写一遍 `requires`。

**惯用写法**：顶层 `roles` 中写 `cardinality: "0"` 表示「默认不启用，由某个 profile 打开」。

---

## 12. `placement` — 放置约束

声明**角色之间的位置关系**。Mecharion 没有调度器——节点由用户显式指定——因此这些约束是 mechd 放置阶段的**校验规则**，而非调度输入。

```yaml
placement:
  - antiAffinity: [namenode, secondarynamenode]
    scope: node
    enforcement: required
    reason: "SNN 与 NN 同节点时无法承担元数据恢复职责"

  - antiAffinity: [zookeeper]        # 单元素 = 同角色的多个实例之间互斥
    scope: node

  - antiAffinity: [journalnode]
    scope: rack                       # 任意 Node label key
    enforcement: preferred

  - affinity: [datanode, nodemanager]
    scope: node
    enforcement: preferred
    reason: "计算贴近数据"
```

| 字段 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `antiAffinity` | 二选一 | — | 角色名列表。列出的角色**不得**落在同一 `scope` |
| `affinity` | 二选一 | — | 角色名列表。列出的角色**必须**落在同一 `scope` |
| `scope` | | `node` | `node`，或任意 Node label key（`rack` / `zone` / `az`…） |
| `enforcement` | | `required` | `required`：违反则拒绝放置<br>`preferred`：仅告警 |
| `reason` | | — | 强烈建议填写——**它会出现在校验失败的错误信息中** |

| 形式 | 含义 |
|---|---|
| `antiAffinity` 含 2+ 角色 | 这些角色两两不得同处一个 scope |
| `antiAffinity` 含 1 角色 | 该角色的**多个实例**不得同处一个 scope |
| `affinity` 含 2+ 角色 | 这些角色必须同处一个 scope |
| `affinity` 含 1 角色 | 无意义，lint 报错 |

`scope` 为 label key 时，节点缺少该 label：`required` 报错（无法验证不等于通过），`preferred` 告警。

**校验失败输出**

```
放置校验失败: hdfs-prod
  约束  antiAffinity[namenode, secondarynamenode]  scope=node  (required)
    namenode           → node-1
    secondarynamenode  → node-1    ← 冲突
  原因: SNN 与 NN 同节点时无法承担元数据恢复职责
```

**边界**：`placement` 只表达角色之间的关系。「某角色必须落在带 X 标签的节点上」属于部署时的节点选择，由用户在 Component 中指定；「节点必须具备某种能力」由 `requires.capability` 表达。

---

## 13. `profiles` — 部署形态

同一个组件常有多种部署形态：Hadoop 的 单机 / 分布式 / HA，Kafka 的 KRaft / ZooKeeper 模式，PostgreSQL 的 单机 / 主备。这些形态**共享同一份载荷**，但在角色集合、数量约束、放置约束、参数默认值与外部依赖上不同。

`profiles` 为这条变化轴命名。

### 13.1 核心性质：profile 是预设，不是变体

profile 在 **mechd 放置阶段被解析掉**——解析后得到具体的 `(roles, cardinality, placement, params, requires)`，mechlet、资源引擎与 Runtime **完全不知道 profile 存在**。

因此 profile 不给引擎增加任何概念，只给 Pack 作者与用户增加一个表达维度。

### 13.2 声明

```yaml
profiles:
  - name: standalone
    default: true
    description: "单机，无冗余，仅供开发测试"
    minNodes: 1
    roles:
      namenode: { cardinality: "1" }
      datanode: { cardinality: "1" }
      secondarynamenode: { enabled: false }
    params:
      replication: { default: 1 }

  - name: distributed
    description: "分布式，无 HA，NameNode 单点"
    minNodes: 2
    roles:
      namenode:          { cardinality: "1" }
      secondarynamenode: { cardinality: "1" }
      datanode:          { cardinality: "1-N" }
    placement:
      - antiAffinity: [namenode, secondarynamenode]
        scope: node
        reason: "SNN 与 NN 同节点时无法承担元数据恢复职责"

  - name: ha
    description: "高可用，双 NameNode + JournalNode 仲裁"
    minNodes: 4
    upgradeFrom: [distributed]
    roles:
      namenode:          { cardinality: "2" }
      journalnode:       { cardinality: "3" }
      zkfc:              { cardinality: "2" }
      datanode:          { cardinality: "3-N" }
      secondarynamenode: { enabled: false }
    requires:
      packs: [{ name: zookeeper, version: ">=3.6" }]
    params:
      nameservice_id: { type: string, default: "mycluster", immutable: true }
    placement:
      - antiAffinity: [namenode]
        scope: node
      - antiAffinity: [journalnode]
        scope: node
      - affinity: [namenode, zkfc]
        scope: node
        reason: "ZKFC 必须与其监控的 NameNode 同置"
```

### 13.3 profile 可以覆盖的五项

| 项 | 覆盖语义 |
|---|---|
| `roles[<name>].enabled` | `false` 时该角色在本形态中不存在 |
| `roles[<name>].cardinality` | 覆盖角色声明的 `cardinality` |
| `placement` | **整体替换**顶层 `placement`（不做合并——合并约束列表的语义难以直觉理解） |
| `params` | 覆盖已有参数的 `default`，或**追加本形态专有的新参数** |
| `requires` | **合并**进顶层 `requires`（追加依赖，不移除） |

其余字段（`name` / `description` / `default` / `minNodes` / `upgradeFrom`）是 profile 自身的元数据。

`minNodes` 仅用于 CLI 与 UI 的提示，不作为硬校验——真正的约束由 `cardinality` 与 `placement` 表达。

### 13.4 profile 不能覆盖的部分

`blobs` / `resources` / `workload` / `paths` / `shared` **不可被 profile 覆盖**。

这些位置的形态差异用**既有机制**表达——`.Profile` 变量配合 `when:`：

```yaml
resources:
  - template:
      src:  hdfs-site.ha.xml.tmpl
      dest: "{{ .Paths.Config }}/hdfs-site.xml"
      when: '{{ eq .Profile "ha" }}'
  - template:
      src:  hdfs-site.simple.xml.tmpl
      dest: "{{ .Paths.Config }}/hdfs-site.xml"
      when: '{{ ne .Profile "ha" }}'
```

差异较小时优先用模板片段组合（§9.1）而非整模板切换。

**这条边界是刻意的**：profile 只管**结构**（哪些角色、多少个、能不能同处一台机器），不管**内容**。内容差异沿用已有的条件机制，避免 profile 变成一个什么都能覆盖的万能层。

### 13.5 形态迁移

```yaml
upgradeFrom: [distributed]
```

声明本形态可以从哪些形态原地迁移而来。未列出的迁移路径被拒绝。

```bash
mechctl component set-profile hdfs-prod ha
```

引擎会重新解析放置、重新渲染配置、生成 Rollout。**但组件自身的数据面迁移动作**（如 HDFS 的 `-initializeSharedEdits`）必须由 Pack 作者用 hooks 完成，通常配合 `scope: once`（§16.4）。

`upgradeFrom` 缺省表示**不允许迁入该形态**，只能新建 Component。

### 13.6 用户侧

```bash
$ mechctl pack show hadoop
部署形态：
  standalone   (默认)  ≥1 节点   单机，无冗余，仅供开发测试
  distributed          ≥2 节点   分布式，无 HA，NameNode 单点
  ha                   ≥4 节点   高可用，双 NameNode + JournalNode 仲裁

$ mechctl deploy hadoop -c hdfs-prod --profile ha --set nameservice_id=prod
```

WebUI 中 profile 是部署向导的**第一步**（形态卡片选择器），而非参数表单中的一个下拉框。

### 13.7 未声明 `profiles` 时

`.Profile` 为空字符串，`roles` / `placement` / `params` 按顶层声明直接生效。**L1 与 L2 的 Pack 完全不需要接触本节。**

---

## 14. `resources` — 资源清单

每个资源是一个单键映射：`{ <type>: { …args… } }`，按声明顺序应用。

### 14.1 通用字段

| 字段 | 默认 | 说明 |
|---|---|---|
| `id` | 自动生成 | 资源唯一标识，用于 `notify` 与状态追踪 |
| `when` | — | 模板表达式，求值为 false 时跳过 |
| `driftPolicy` | `report` | `report` \| `reconcile` \| `ignore` |
| `notify` | — | 变更时触发的动作：`restart` \| `reload` \| `<hook 名>` |

### 14.2 文件系统

```yaml
- file:      { path, content?, source?, blob?, mode?, owner?, group? }
- template:  { src, dest, mode?, owner?, group?, notify? }
- directory: { path, mode?, owner?, group?, recursive? }
- symlink:   { path, target, force? }
- archive:   { blob, dest, strip?, exclude? }
```

`file` 的内容有**三个互斥来源**：

| 字段 | 来源 | 用途 |
|---|---|---|
| `content` | 内联字符串 | 短小的静态内容 |
| `source` | `files/` 下的相对路径 | 随 Pack 分发的静态文件 |
| `blob` | `blobs` 中声明的键 | **裸二进制载荷**（非归档），如 MinIO、Prometheus 等单文件发行的组件 |

`archive.blob` 用于**归档类**载荷（`tar` / `tar.gz` / `zip`），`strip` 为剥离的路径层数。载荷是单个可执行文件时用 `file.blob`（配合 blob 的 `mediaType: raw`），不要为了套用 `archive` 而把二进制打进一个多余的 tar。

`template.src` 引用 `templates/` 下的相对路径。

### 14.3 身份

```yaml
- user:  { name, uid?, group?, groups?, home?, shell?, system? }
- group: { name, gid?, system? }
```

`system: true` 使用系统 uid/gid 段。删除 Component 时**不自动删除 user/group**（可能有遗留文件属主）。

### 14.4 主机配置

```yaml
- sysctl:       { key, value }
- limits:       { domain, item, type?, soft?, hard? }
- hosts_entry:  { ip, hostnames[] }
- mount:        { path, device, fstype, options?, persistent? }
- timer:        { name, schedule, command, user? }        # systemd timer
- systemd_unit: { name, content, enabled?, state? }       # 见下
```

`timer.schedule` 使用 systemd `OnCalendar` 语法。

#### `systemd_unit` 与 `workload` 的分野

| | `workload.runtime: systemd` | `systemd_unit` 资源 |
|---|---|---|
| 语义 | **这个角色的受监管进程** | **一个 unit 应当存在并处于某状态** |
| 参与 | 健康检查、generation 切换、Rollout 编排 | 只是一条资源，参与漂移检测 |
| 典型 | 长驻服务 | `Type=oneshot` 的开机任务、target、path 单元 |

关闭透明大页是典型场景——它需要每次开机执行一次，但它不是任何角色的「工作负载」：

```yaml
- systemd_unit:
    name: mecharion-disable-thp.service
    content: |
      [Unit]
      Description=Disable Transparent Huge Pages
      [Service]
      Type=oneshot
      RemainAfterExit=yes
      ExecStart=/usr/local/sbin/disable-thp.sh
      [Install]
      WantedBy=basic.target
    enabled: true
    state:   started      # started | stopped | absent
```

**不要用 `workload` 表达 oneshot 任务**——`workload` 的健康检查、重启策略、Rollout 语义对它全都不适用。

### 14.5 逃生舱

```yaml
- command: { run, cwd?, env?, user?, timeout?, unless?, onlyif?, creates? }
- script:  { src, args?, cwd?, env?, user?, timeout?, unless?, onlyif?, creates? }
```

**守卫至少三选一**，否则 lint 报错——没有守卫的命令不幂等，无法参与调和：

| 守卫 | 含义 |
|---|---|
| `creates` | 指定路径已存在则跳过 |
| `unless` | 命令退出码为 0 则跳过 |
| `onlyif` | 命令退出码非 0 则跳过 |

`script.src` 引用 `hooks/` 下的相对路径。

### 14.6 非 hermetic

```yaml
- package: { name, version?, state? }
```

需要 OS 包仓库，**被 `lint --hermetic` 拦截**。提供它是为了有 repo 的环境，官方 Pack 不得使用。

---

## 15. `workload` 与 `health`

无 `workload` 段的角色只落文件不起进程（客户端配置分发场景）。

### 15.1 `runtime: systemd`

```yaml
workload:
  runtime: systemd
  systemd:
    exec:        "{{ .Paths.Current }}/bin/postgres -D {{ .Paths.Config }}"
    execReload:  "…"                  # 可选，声明后 Runtime.Reload 可用
    user:        postgres
    group:       postgres
    workingDir:  "{{ .Paths.Current }}"
    env:
      PGDATA: "{{ .Paths.Config }}"
    envFile:     "{{ .Paths.Config }}/env"     # 可选
    restart:     always                        # always|on-failure|no
    restartSec:  5s
    limitNofile: 65536
    killMode:    mixed
    timeoutStop: 90s
    extraUnit: |                               # 可选，原样追加到 [Service]
      OOMScoreAdjust=-500
```

生成的 unit 名：`mecharion-<component>-<role>.service`

**`exec` / `execReload` 的第一个词必须是绝对路径。** systemd 不经过 shell，
不会在 `PATH` 中查找命令；写成相对路径或裸命令名，要到 `start` 那一刻才会以
`Failed at step EXEC` 失败，且错误信息完全看不出是路径问题。`mechlet` 在物化
阶段就会拦下这种写法。

> **发信号别用 `/bin/kill`。** systemd 手册里的惯用写法是
> `ExecReload=/bin/kill -s HUP $MAINPID`，但 `kill` 由 `procps` 提供，
> **最小化镜像（`debian:*-slim` 等）里并不存在**——照抄会得到一个「声明了
> `execReload` 却永远 reload 失败」的 Pack。
>
> 用 shell 内建的 `kill`：
>
> ```yaml
> execReload: "/bin/sh -c 'kill -HUP $MAINPID'"
> ```
>
> `$MAINPID` 由 systemd 展开后再交给 `sh`，因此单引号不影响它生效。

> **别把可执行文件放在 `/tmp`。** 许多系统（含 systemd 自带的 `tmp.mount`）
> 把 `/tmp` 挂成 `noexec`，从那里启动进程会得到 `203/EXEC Permission denied`，
> 而错误信息只字不提 `noexec`。载荷一律解在 `{{ .Paths.Generation }}` 之下，
> 这条不变式已经规避了该问题。

### 15.2 `runtime: docker`

```yaml
workload:
  runtime: docker
  requires: { capability: { docker: ">=20.10" } }
  docker:
    imageBlob: image                  # 引用 blobs 中的 docker-archive
    command:   ["postgres"]
    args:      ["-c", "max_connections={{ .Params.max_connections }}"]
    env:       { POSTGRES_DB: app }
    user:      "999:999"
    mounts:                           # ★ 一律 bind mount，不用 named volume
      - { from: "{{ .Paths.Data }}",   to: /var/lib/postgresql/data }
      - { from: "{{ .Paths.Config }}", to: /etc/postgresql, readOnly: true }
    ports:
      - { host: "{{ .Params.port }}", container: 5432, protocol: tcp }
    network:      bridge              # bridge|host|none|<自定义网络名>
    restart:      unless-stopped
    capAdd:       []
    securityOpt:  []
    ulimits:      { nofile: 65536 }
```

mechlet 自动附加标签，**且只操作带这些标签的容器**：

```
dev.mecharion.site  dev.mecharion.component  dev.mecharion.role
dev.mecharion.generation  dev.mecharion.managed-by
```

### 15.3 `runtime: compose`

```yaml
workload:
  runtime: compose
  requires: { capability: { compose: ">=2.0" } }
  compose:
    file:        compose.yaml.tmpl    # templates/ 下的模板
    imageBlobs:  [web, worker, redis] # 物化时逐个 docker load
    projectName: "{{ .Component }}"   # 默认 mecharion-<component>
    envFile:     "{{ .Paths.Config }}/.env"
    execService: web                  # exec 探针进哪个 service
```

整个 compose project 作为**一个不透明的 workload**。`Observe()` 聚合 project 下全部容器的状态，逐 service 结果放入 `Raw`。**compose 内的 service 不映射为 Role。**

`file` 在管道两侧含义不同：Pack 里是 `templates/` 下的模板名，已解析规格里是**渲染产物的绝对路径**（`{{ .Paths.Config }}/compose.yaml`）。渲染流水线自动产出该文件，Pack 不需要、也不应再为它声明一条 `template` 资源。

`execService` 指定 `exec` 探针与 `mechctl component exec` 进哪个 service。project 只有一个 service 时可省略；**有多个而未声明时报错**——猜错会在一个无关的容器里跑诊断命令。

### 15.4 `health`

```yaml
health:
  http: { path: /healthz, port: "{{ .Params.port }}", scheme: http, expectStatus: [200] }
  # 或 tcp:  { port: "{{ .Params.port }}" }
  # 或 exec: { command: ["pg_isready", "-p", "{{ .Params.port }}"] }
  startupGrace:     60s
  interval:         15s
  timeout:          5s
  failureThreshold: 3
  successThreshold: 1
```

三种探针互斥。健康检查在 Runtime 接口**之上**执行，跨 Runtime 行为一致。Runtime 原生健康信息（docker HEALTHCHECK、systemd watchdog）经 `Observe()` 汇入，不需在此声明。

---

## 16. `hooks`

### 16.1 声明位置

| 位置 | 生效范围 |
|---|---|
| 顶层 `hooks` | 所有角色的所有实例 |
| `roles[].hooks` | 仅该角色的实例 |

同一生命周期点两处都有时，执行顺序为：顶层 → 角色级。

### 16.2 生命周期点

```
preInstall  postInstall
preUpgrade  postUpgrade
preRemove   postRemove
preStart    postStart
preStop     postStop
```

### 16.3 两种写法

简写（字符串）等价于 `{ script: <path>, scope: perInstance }`：

```yaml
hooks:
  preUpgrade: hooks/pre-upgrade.sh
```

完整形式，**可为列表**——同一生命周期点允许多个 hook，按声明顺序执行：

```yaml
hooks:
  postInstall:
    - script: hooks/nn-format.sh
      scope:  once
    - script: hooks/nn-bootstrap-standby.sh
      when:   '{{ and (eq .Profile "ha") (ne .Topology.Ordinal 0) }}'
      timeout: 600s
```

| 字段 | 默认 | 说明 |
|---|---|---|
| `script` | — | **必填**，`hooks/` 下的相对路径 |
| `args` | `[]` | |
| `scope` | `perInstance` | 见 §16.4 |
| `when` | — | 模板表达式，false 则跳过 |
| `timeout` | `300s` | |
| `user` | `root` | |

### 16.4 `scope` — 集群级一次性动作

| 值 | 语义 |
|---|---|
| `perInstance`（默认） | 该角色的每个实例都执行 |
| `once` | **整个 Component 内只执行一次**，在该角色 `ordinal` 最小的实例上 |

`scope: once` 是「集群级一次性动作」的表达方式。典型用例：

- HDFS 的 `namenode -format`、`-initializeSharedEdits`
- 数据库的 schema 初始化
- 分布式系统的 cluster bootstrap

引擎保证：

- **执行结果记录在 Component 状态中**，重复 `apply` 不会重跑
- 失败会中止**整个 Rollout**，而不只是那一个实例
- 承载该 hook 的实例被移除时**标记不重置**——`once` 的语义是「这个 Component 生命周期内一次」，不是「这个实例上一次」

> 需要「除第一个实例外都执行」这类模式时，用 `perInstance` + `when` 配合 `.Topology.Ordinal`，不需要额外语法。

**`once` 表达的是「动作唯一」，不是「集群级唯一」。** 这两者极易混淆：

| | HDFS `namenode -format` | Kafka `kafka-storage.sh format` |
|---|---|---|
| 谁执行 | **只有一个实例** | **每个实例都要** |
| 唯一性体现在 | 「只执行一次」这个动作本身 | `cluster_id` **参数**在集群内一致 |
| 应声明 | `scope: once` | `scope: perInstance`（默认） |

把后者误标为 `once`，会导致除首节点外全部启动失败。**判据：如果每个实例都要在本地留下痕迹，就是 `perInstance`。**

### 16.5 执行环境

- 以 `user`（默认 root）执行，`cwd` 为 generation 目录
- 环境变量注入：`MECHARION_COMPONENT` `MECHARION_ROLE` `MECHARION_PROFILE` `MECHARION_GENERATION` `MECHARION_ORDINAL` `MECHARION_PATHS_*` `MECHARION_PARAM_*`
- **`sensitive` 参数通过临时文件传递**，不进环境变量（环境变量会出现在 `/proc/<pid>/environ` 与进程列表中）
- 非零退出视为失败；`preInstall` / `preUpgrade` 失败会中止并回滚

**hooks 受 hermetic 检查约束**（§17）。

---

## 17. Hermetic 规则

`mechpack lint --hermetic` 对 `hooks/` 下全部脚本与 `command`/`script` 资源做静态扫描。

**拦截清单**

```
包管理器  apt apt-get aptitude yum dnf zypper apk pacman rpm dpkg
下载      curl wget scp rsync ftp git-clone svn
语言生态  npm yarn pnpm pip pip3 gem cargo composer go-get
构建      make cmake mvn gradle ant go-build gcc g++ javac
容器      docker-pull podman-pull crictl-pull skopeo-copy
```

**资源类型**：`package` 直接标记为 non-hermetic。

**已知局限**：静态扫描可被变量拼接绕过（`C=cur"l"; $C …`）。这是有意接受的——lint 的目标是**防止无意违反**，不对抗蓄意绕过。系统不做 Pack 签名/来源校验（[ADR-0040](../adr/0040-pack-trust-is-operator-responsibility.md)），因此蓄意绕过 lint 的恶意 Pack 完全由运维方自己判断是否要部署，没有另一层机制兜底。

官方 `packs` 仓库的 CI 强制此检查通过。

---

## 18. 没有签名

Pack 不带 `pack.sig`，`mechpack` 没有 `sign` 子命令。系统不对 Pack 的来源做密码学意义上的身份认证——见 [ADR-0040](../adr/0040-pack-trust-is-operator-responsibility.md)（取代早先决定做强制签名的 [ADR-0016](../adr/0016-mandatory-pack-signing.md)）。

**保留的是完整性，不是身份认证**：blob 按 sha256 内容寻址（§2），mechlet 拉取时校验字节与文件名一致，对不上则丢弃重拉。这挡传输/存储损坏，不挡一次连同 sha256 一起换掉的主动替换——识别这种情况是运维方自己的责任。

---

## 19. 校验规则汇总

> **⏳ 标记的规则尚未实现**，`mechpack lint` 目前不会检查它们。
> 明确标出来，是因为「以为有检查在守着、其实没有」比「知道没有」危险得多。
> 是否实现由 `internal/pack` 的规则覆盖测试自动核对，规范与代码不会各说各话。

| 规则 | 为什么还没做 | 计划 |
|---|---|---|
| 8 | 归档内容在 lint 阶段拿不到（blob 不在源码 Pack 里），属于 `assemble` 的职责。**运行期已有等价防护**：解压时拒绝逃逸条目与逃逸软链 | 随 `assemble` 的完整性校验一起做 |
| 39 | 需要把 profile 守卫与依赖声明的来源关联起来，机制有但接线未做 | M3 |

> 规则 8 的措辞已从「无符号链接」改为「无**逃逸的**符号链接」——JDK 这类
> 发行包大量使用指向归档内部的相对软链，一刀切拒绝会让它们无法安装。
> 运行期的实现就是按「是否逃出目标目录」判定的。

### 结构

| # | 规则 |
|---|---|
| 1 | `schema` 必须为 `pack/v1` |
| 2 | `name` 符合 DNS label 规则，长度 ≤ 63 |
| 3 | `version` 非空字符串；`revision` ≥ 1 |
| 4 | `platforms` 非空，且**每个平台在每个 blob 中都有条目** |
| 5 | 每个 blob 条目的 `sha256` / `size` / `filename` 均已填写 |
| 6 | 所有 `blob` 引用存在于 `blobs` 声明中 |
| 6b | `file` 的 `content` / `source` / `blob` 三者恰有其一 |
| 6c | `archive.blob` 的 `mediaType` 不得为 `raw`（裸二进制应用 `file.blob`） |
| 7 | 所有 `templates/` `files/` `hooks/` 引用的文件真实存在 |
| 8 | 归档中无绝对路径、无逃逸的 `..`、无逃逸的符号链接 ⏳ |

### 角色与放置

| # | 规则 |
|---|---|
| 9 | `roles` 非空；角色名唯一 |
| 10 | `requires`（角色依赖）不成环 |
| 11 | `cardinality` 语法合法 |
| 12 | `scope: cluster` 在 v1 中报错「尚未支持」 |
| 13 | `placement` 中引用的角色全部存在于 `roles` |
| 14 | `affinity` 不得只含一个角色 |
| 15 | 同一组角色不得在相同 `scope` 下同时出现在 `affinity` 与 `antiAffinity` |

### 形态

| # | 规则 |
|---|---|
| 16 | profile 名唯一；至多一个 `default: true`；若无则第一个为默认 |
| 17 | profile 中引用的角色全部存在于 `roles` |
| 18 | profile 的 `placement` 不得引用在该 profile 中 `enabled: false` 的角色 |
| 19 | profile 的 `cardinality` 之和须与 `minNodes` 及 `placement` 自洽——**存在一个满足全部约束的放置方案**（不可满足时报错，如「3 个 journalnode 互斥但 minNodes=2」） |
| 20 | `upgradeFrom` 引用的 profile 存在，且不构成自环 |
| 21 | **对每个 profile 独立渲染一遍全部模板**（`missingkey=error`），确保引用的参数在该形态下存在 |

### 参数与路径

| # | 规则 |
|---|---|
| 22 | `params` 的 `type` 必须是 §7.2 的 12 种之一 |
| 23 | `enum` 类型必须有 `values`；`default` 必须在 `values` 中 |
| 24 | `restartRequired` 与 `reloadRequired` 不同时为真 |
| 25 | 声明了 `execReload` 才允许参数使用 `reloadRequired` |
| 26 | `from` 表达式语法合法，引用的角色/依赖存在 |
| 26b | `from` 与 `defaultFrom` 互斥；`from` 与 `default` 互斥 |
| 26c | 声明了 `defaultFrom` 必须同时声明 `default`（求值失败时的兜底） |
| 26d | `generate` 只能用于 `type: secret`，且与 `default` / `defaultFrom` / `from` 互斥 |
| 26e | `generate.charset` 取值合法（`alnum` \| `alnumSymbol` \| `hex`）；`length` ≥ 8 |
| 27 | `paths` 中 `kind: multi` 的 `default` 必须是 list |
| 28 | `linkInto` 的目标必须位于 `{{ .Paths.Generation }}` 之内 |
| 29 | `layout: inline` 时 `default` 必须位于 generation 内，且不得同时声明 `linkInto` |
| 30 | `layout: inline` 不得用于 `data` / `logs` |

### 资源与 hooks

| # | 规则 |
|---|---|
| 31 | `command` / `script` 至少有一个守卫 |
| 32 | `scope: once` 的 hook 所属角色 `cardinality` 下限须 ≥ 1（否则可能无实例可承载） |
| 33 | `--hermetic` 模式下无拦截清单命中 |

### 依赖与升级

| # | 规则 |
|---|---|
| 34 | `requires.packs` 中同一 `name` 不得出现两次——需要两个大版本时应是两个 Pack（§4.3） |
| 34b | `requires.packs[].version` 与 `upgradePolicy.compatible` 是合法的版本范围表达式（见 §5.5） |
| 35 | `requires.packs[].scope` 只能是 `node` 或 `site` |
| 36 | `upgradePolicy.compatible` 是合法的版本范围表达式 |
| 37 | 依赖不成环（lint 在本地可用 Pack 集合内检测；放置阶段再全量检测一次） |
| 38 | `.Requires.<name>` 引用的 `<name>` 存在于 `requires.packs`（含 profile 追加的） |
| 39 | 仅在某 profile 中声明的依赖，其 `.Requires.<name>` 引用必须处于该 profile 的条件分支内 ⏳ |
| 40 | **`scope: site` 的依赖不得被引用 `.Paths.*`** ——那是别的机器上的路径 |
| 41 | `.Node.Facts.*` 只出现在 `defaultFrom` 或模板中，不出现在 `from`（事实是可变的，不构成客观部署事实） |
| 42 | `exports[].role` 引用的角色存在于 `roles` |
| 43 | `.Requires.<pack>.Exports.<name>` 引用的导出名，在被依赖 Pack 的 `exports` 中存在（需本地有该 Pack；缺席时降级为警告） |
| 44 | 声明 `quorum: true` 的角色，其 `cardinality` 下限须 ≥ 1；实例数为偶数时**告警**（不拒绝） |
| 45 | `systemd_unit.content` 是可解析的 unit 文件；`name` 以 `.service` / `.timer` / `.target` / `.path` 等合法后缀结尾 |
| 46 | **引用了 `sensitive` 参数的模板，渲染它的资源的 `mode` 的 others 位必须为 0**（`mode & 0007 == 0`） |
| 47 | `exports[].fields` 的值是合法模板表达式，引用的参数存在于本 Pack |
| 50 | **`workload.systemd.env` 不得引用 `sensitive` 参数**——内联 `Environment=` 会把值写进 0644 的 unit 文件，且出现在 `systemctl show` 输出里。改用 `envFile` |
| 51 | `driftPolicy: reconcile` 与 `notify: restart` 同用时**告警**——手改一个文件会让服务在运维手底下重启。需要它时由部署方显式打开 `allowDriftRestart` |
| 52 | **`workload.docker.mounts[].from` 与 `workload.compose` 的挂载不得引用 `.Paths.Current`**——Docker 在创建容器时解析路径，软链解析后绑死在当时那个 generation 上；之后切换 generation，容器里看到的仍是旧的，而 `ls -l` 看软链一切正常。配置挂 `.Paths.Config`、数据挂 `.Paths.Data`、要 generation 内的东西显式写 `.Paths.Generation` |
| 48 | `exports[]` 至少声明 `format` 或 `fields` 之一（只有 `role` + `port` 时按默认 `format` 处理，不报错） |
| 49 | **`.Requires.<pack>.Params.*` 一律拒绝**——消费方不得绕过导出契约读提供方的参数（§5.4） |

> **跨 Pack 的敏感传播不在 lint 范围内。** lint 只看得见一个 Pack，而依赖方可能来自别处、单独发布。
> 「消费方引用了敏感字段却没声明 `type: secret`」由 mechd 在绑定阶段**自动传播并提示**（§5.4），
> 那是唯一有全局视角的地方。

> **规则 46 的取舍**：约束的是 world-readable，而非「只有属主能读」。`0640`（同组的多个服务读同一份凭据）是合法的部署模式，不应被禁止；`0644` 则让任何本地用户都能读到密码。
>
> 之所以选择「限制文件权限」而非「在 `mechctl config diff` 输出中脱敏」：**引擎没有可靠手段识别渲染产物中的敏感片段**——它只知道渲染前的参数值，不知道渲染后文件里哪一段是它，文本替换既不可靠又会误伤。限制权限保护的是**磁盘上的文件**，比脱敏某一次输出实在得多。
>
> 参数值本身的脱敏（§7.3 `sensitive`）不受影响，照常执行。

> 规则 19 与 21 是 `profiles` 带来的关键收益：**每个形态的自洽性在打包阶段被独立验证**，而不是等到用户选中某个形态部署时才暴露。

---

## 20. 最小示例

L1 的完整 Pack——**不涉及 profiles / placement / cardinality / linkInto / 多盘中的任何一项**：

```yaml
schema: pack/v1
name: go-webapp
version: "1.2.0"
platforms: [linux/amd64, linux/arm64]

blobs:
  main:
    linux/amd64: { sha256: "a1b2c3…", size: 12582912, filename: "webapp-1.2.0-linux-amd64.tar.gz" }
    linux/arm64: { sha256: "d4e5f6…", size: 11534336, filename: "webapp-1.2.0-linux-arm64.tar.gz" }

params:
  port:      { type: port, default: 8080, restartRequired: true }
  log_level: { type: enum, values: [debug, info, warn, error], default: info, reloadRequired: true }

roles:
  - resources:
      - user:      { name: webapp, system: true }
      - archive:   { blob: main, dest: "{{ .Paths.Generation }}" }
      - directory: { path: "{{ .Paths.Data }}", owner: webapp, mode: "0750" }
      - template:
          src:    app.yaml.tmpl
          dest:   "{{ .Paths.Config }}/app.yaml"
          owner:  webapp
          mode:   "0640"
          notify: reload

    workload:
      runtime: systemd
      systemd:
        exec:       "{{ .Paths.Current }}/bin/webapp --config {{ .Paths.Config }}/app.yaml"
        execReload: "/bin/sh -c 'kill -HUP $MAINPID'"
        user:       webapp
        restart:    always

    health:
      http: { path: /healthz, port: "{{ .Params.port }}" }
      startupGrace: 15s
```

> **L2 与 L3 的完整示例见 [`examples/packs/`](../../examples/packs/)**——`postgresql`（多角色 + 拓扑引用 + 双形态）与 `hdfs`（三形态 + Pack 间依赖 + 集群级一次性动作）。规范中不重复这些示例，避免两处内容漂移。

---

## 21. 版本演进策略

| 变更类型 | 处理 |
|---|---|
| 新增可选字段 | 允许，`pack/v1` 内兼容。旧 mechlet 遇到未知字段应**报错而非忽略**——静默忽略会导致行为与 Pack 作者预期不符 |
| **新增 params 类型** | 允许，`pack/v1` 内兼容。旧 mechlet 无法解析，因此 Pack 必须声明 `requires.mecharion: ">=x.y.z"` |
| 新增资源类型 | 同上 |
| 修改字段语义 | 破坏性，必须升 `pack/v2` |
| 删除字段 | 破坏性，必须升 `pack/v2` |

**params 类型集的演进方式已经确定**：类型不够用时**增补新类型**，不引入第二套 schema 体系。见 [ADR-0007](../adr/0007-params-custom-subset.md)。

`pack/v2` 出现时，mechlet 必须同时支持 v1 与 v2 至少两个大版本周期。

---

## 22. 未决问题

| 问题 | 状态 |
|---|---|
| `properties` / `xml_properties` 资源类型 | **待定**。Java 生态（Hadoop XML、Kafka/ZK `.properties`）的配置本质是键值映射，专门的资源类型可支持「按键合并」与「按键 diff」。当前用模板表达，可行但冗长（`hdfs-site.xml` 每个属性 4 行 XML）。第二波示例（kafka、elasticsearch）将给出结论 |
| `volumeClass` 自动选盘 | **v1 只做 `class` 声明字段，不做自动选择。** 自动选盘容易、自动取消选盘危险（静默减少 data dir 等同静默丢数据），需配套盘健康探测、容量门槛、减盘确认工作流。待有真实异构大集群用户后基于实际形态设计 |
| 增量 blob 传输 | **v1 不做，且延后成本为零。** blob 内容寻址使得增量传输纯属传输层优化——补丁只是「产生一个 sha256 已知的 blob」的另一种方式，Pack 格式无需任何变更。将来实现时优先选 `zstd --patch-from`（几百行，无新存储子系统），分块寻址（casync 式）留给 blob 总量大到去重收益压倒复杂度的场景 |
