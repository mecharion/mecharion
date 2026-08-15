# 路径与磁盘布局

本文分两部分：Mecharion 自身的路径（固定，遵循 FHS），以及组件的路径（由 Pack 声明，可绑定多磁盘）。

---

## 第一部分：Mecharion 自身

遵循 Linux 文件系统层次标准（FHS）。数据目录在安装时指定，之后不支持切换。

```
/usr/local/lib/mecharion/       ← Mecharion 自身（安装时 --prefix 可改）
├── generations/                自身的版本目录（支撑自升级回滚）
│   ├── 0002-0.4.0/bin/{mechd,mechlet,mechctl,mechpack}
│   └── 0003-0.4.1/bin/…
└── current -> generations/0003-0.4.1

/usr/bin/mechctl  -> /usr/local/lib/mecharion/current/bin/mechctl
/usr/bin/mechpack -> …          ★ 四个都挂上，零 PATH 配置
/usr/bin/mechd    -> …
/usr/bin/mechlet  -> …

/etc/mecharion/
├── mechd.yaml              mechd 配置
├── mechlet.yaml            mechlet 配置（含 roots 与 volumes 声明）
├── secret.key              主密钥（0400，见 16-secrets §3）
├── pki/                    mTLS 证书与私钥
├── trust/                  可信发布者公钥
└── apps/<component>/       组件配置（默认位置）

/opt/mecharion/
└── apps/<component>/       ★ 只放被管理的组件，不再混 Mecharion 自身

/var/lib/mecharion/         ← 安装时 --data-dir 指定
├── mechd/                  SQLite 数据库（仅 mechd 节点）
├── mechlet/                agent 自身状态 + 事件缓冲
├── blobs/sha256/ab/ab12…   内容寻址 blob
├── packs/                  解开的 Pack 逻辑
└── apps/<component>/       组件数据（默认位置）

/var/log/mecharion/
├── mechd/  mechlet/
└── apps/<component>/

/run/mecharion/             unix socket、pid、hook 的临时密钥目录
```

### 三条布局判据

**① 命令挂在 `/usr/bin`，不是 `/usr/local/bin`。**

目标是**零环境变量配置**。`/usr/local/bin` 虽然在交互式 shell 的默认 PATH 里，
但 **RHEL 系的 `sudo secure_path` 不含它**：

```
# RHEL 8/9 的 /etc/sudoers
Defaults secure_path = /sbin:/bin:/usr/sbin:/usr/bin
```

于是 `sudo mechctl` 会以「command not found」失败，而直接 root 下又是好的——
这类「有时能用有时不能」的问题最消耗人。cron、systemd unit、CI 脚本里的
最小 PATH 也是同样的坑。

放在 `/usr/bin` 的代价是它名义上属于发行版包管理器的地盘。我们放的是
**符号链接而非实体文件**：冲突可见、易撤销，且没有哪个发行版会打出叫
`mechctl` 的包。Docker 的静态二进制安装说明同样指向 `/usr/bin`。

**② 实体文件在版本目录里，PATH 上只有软链。**

`mechlet` 要升级正在运行的自己（[01-architecture §4](01-architecture.md#4-mechlet-自升级)），
靠的就是 generation + 原子切软链 + watchdog 回退。若把实体二进制直接放进
`/usr/bin`，这套机制整个没了——覆盖一个正在执行的文件既不原子也无法回滚。

`current -> generations/<seq>-<version>` 的形状与[组件的 `current`](#2-generation)
**刻意一致**：同一套心智模型，不为 Mecharion 自身另发明一种。

**③ Mecharion 自身与被管理的组件分开。**

`/opt/mecharion/` 现在只剩 `apps/`。此前两者混在一起，`du /opt/mecharion` 分不清
哪部分是产品、哪部分是组件；而组件动辄几十 GB（JDK、Hadoop），Mecharion 自身
只有 40MB——「/opt 该分多大」这个问题原本没有清晰答案。

> **`/usr` 只读时安装会失败并明确报错**，提示用 `--prefix /opt/mecharion-self`
> 之类的可写位置。不静默改路径——安装位置是运维必须知道的事实。
> 不可变发行版（Fedora CoreOS 等）通常把 `/usr/local` 软链到 `/var/usrlocal`，
> 因此默认值在那里照常可写。

**数据目录不可变**：mechlet 启动时校验 `--data-dir` 与记录值一致，不一致则**拒绝启动并给出明确错误**，不做自动迁移。迁移数据目录是运维动作，工具不该替用户决定（[原则六](00-overview.md#原则六显式优于隐式)）。

---

## 第二部分：组件路径

### 1. 唯一的不变式

早期设计曾把「home / config / data 三目录分离」写成约定，这是错的——它把**实现手段**当成了**目标**。真正必须守住的只有一条：

> **generation 目录是只读的。任何需要可写、或需要跨版本存活的路径，其物理存储必须在 generation 目录之外。**

「分离布局」只是满足这条不变式的一种方式，不是唯一方式。**config 可以出现在 generation 目录内部——只要它是一个指向外部的软链。**

这正是 Debian 打包 Tomcat 的做法（`/var/lib/tomcat9/conf -> /etc/tomcat9`），在真实世界运行了二十年。

### 2. generation

```
/opt/mecharion/apps/kafka/
├── generations/
│   ├── 0006-3.6.0-1/       ← 序号自增，附「上游版本-Pack revision」便于现场排障
│   │   ├── bin/  libs/
│   │   ├── config.dist/    ← tarball 自带的默认配置，保留为该版本基线
│   │   └── config -> /etc/mecharion/apps/kafka
│   └── 0007-3.7.0-1/
└── current -> generations/0007-3.7.0-1
```

**generation = 一次完整物化，是原子切换与回滚的单位。** 它的物理形态因 Runtime 而异：

| Runtime | generation 的物理形态 |
|---|---|
| systemd | 一个目录 |
| docker | （镜像 digest + 配置目录）的组合 |
| compose | （渲染后的 compose.yaml + 镜像 digest 集合 + 配置目录） |

**配置变更同样产生新 generation**，因此回滚是统一动作：`mechctl component rollback kafka --on node-7` 就是切回上一个 generation，不区分「回滚版本」还是「回滚配置」。

### 3. 机制一：`linkInto` — 应对不接受分离布局的应用

许多应用要求配置位于安装目录内部：Kafka 的 `config/`、Tomcat 的 `conf/`、Elasticsearch 的 `config/`、ZooKeeper 的 `conf/`。

Pack 声明应用的**原生布局**，mechlet 负责把它调和到不变式：

```yaml
paths:
  home:
    default: "{{ .Node.Roots.opt }}/apps/{{ .Component }}"

  config:
    default:  "{{ .Node.Roots.etc }}/apps/{{ .Component }}"   # 权威存储位置
    linkInto: "{{ .Paths.Generation }}/config"                # 应用期望看到的位置
    distDir:  "config"                                        # tarball 自带的默认配置目录名

  data:
    default:  "{{ .Node.Roots.data }}/apps/{{ .Component }}"

  logs:
    default:  "{{ .Node.Roots.logs }}/apps/{{ .Component }}"
    linkInto: "{{ .Paths.Generation }}/logs"

  runtime:                                                    # 运行期可写目录（Tomcat 的 work/temp）
    default:  "/run/mecharion/apps/{{ .Component }}"
    linkInto: "{{ .Paths.Generation }}/work"
```

mechlet 物化时：

1. 解开 blob 到 generation 目录
2. 把 tarball 自带的 `config/` 改名为 `config.dist/`
3. 渲染权威配置到 `/etc/mecharion/apps/kafka/`
4. 建软链 `<generation>/config -> /etc/mecharion/apps/kafka`

Kafka 看到 `$KAFKA_HOME/config/server.properties`，完全符合预期；配置的真身在 `/etc` 下，跨版本存活、可备份、可审计、升级不丢。

**`linkInto` 是一个通用机制**：配置、日志、运行期可写目录、插件目录（Elasticsearch 的 `plugins/`）用的是同一套，不需要为每种情况发明一个模式。

#### 为什么保留 `config.dist/`

升级时新版本可能新增或重命名配置项。保留每个 generation 的默认值基线，才能提供：

```
mechctl config diff -c kafka --from 3.6.0 --to 3.7.0
```

所有包管理器都在这件事上栽过跟头（Debian 的 `.dpkg-dist`、RPM 的 `.rpmnew` 都是同一问题的补丁式解法）。一开始就留位置，成本几乎为零。

#### 第三种变体：配置在数据目录内

PostgreSQL 的配置默认位于 `PGDATA` 内。这个用 `paths` 直接表达即可，连 `linkInto` 都不需要：

```yaml
paths:
  config:
    default: "{{ .Paths.Data }}/pgdata"    # 数据目录本来就跨版本存活，不变式自动满足
```

这说明设计粒度是对的：**Pack 声明位置，引擎只负责保证不变式**，不预设任何「标准布局」。

#### `layout: inline`（v1 支持）

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
| 配置变更的效果 | 原地渲染，可 `reload` | **产生新 generation**，走完整切换流程 |
| 回滚配置 | 切回上一 generation | 切回上一 generation（完全相同） |
| 备份配置 | 备份 `/etc/mecharion/apps/<x>` | 备份 generation 目录 |

因为「配置变更 → 新 generation」本就是既定语义，`inline` **不引入任何新的回滚概念**——它只是让每次配置变更的物化成本变高。

实现要求：生成新 generation 时**未变化的文件从上一 generation 硬链接**，避免重复解压载荷。

约束：`default` 必须位于 generation 内、与 `linkInto` 互斥、不得用于 `data` / `logs`（它们必须跨 generation 存活）。

**何时使用**：只在 `separate` 确认不可行时。`inline` 会让每次配置微调都产生一次完整的 generation 切换与服务重启窗口。

### 4. 机制二：Node volumes — 多磁盘与按服务分盘

**Pack 绝不硬编码绝对路径**，否则多盘和按节点差异化都无从谈起。

节点声明它有什么盘（`class` 为可选的自由文本标签，**v1 中仅用于 CLI/UI 的筛选与展示，不参与自动选盘**）：

```yaml
# /etc/mecharion/mechlet.yaml
roots:
  opt:  /opt/mecharion
  etc:  /etc/mecharion
  data: /var/lib/mecharion       # 安装时 --data-dir 指定
  logs: /var/log/mecharion
  run:  /run/mecharion

volumes:
  - { name: data1, path: /data1, class: bulk }
  - { name: data2, path: /data2, class: bulk }
  - { name: ssd1,  path: /ssd1,  class: fast }
```

Pack 声明它要几块盘：

```yaml
# HDFS DataNode
paths:
  dataDirs:
    kind: multi                                    # 渲染进模板时是列表
    default: ["{{ .Node.Roots.data }}/apps/{{ .Component }}/dfs"]
    subpath: "dfs/dn"                              # 用户只给盘名，引擎补子路径
```

用户按 [参数优先级](02-object-model.md#4-参数解析优先级)覆盖：

```yaml
configGroups:
  - name: 3-disk-nodes
    nodes: [node-7, node-8]
    paths:
      dataDirs: [data1, data2, data3]    # → /data1/dfs/dn, /data2/dfs/dn, /data3/dfs/dn
```

**「不同服务放不同盘」** 就是一次普通的 Component 级覆盖：`paths.data: bulk1`。不需要额外机制。

### 为什么 v1 不做自动选盘

`volumeClass` 的完整形态是：Pack 声明 `prefer: bulk, select: all`，引擎在每个节点上自动挑出该类的全部卷。这对异构大集群（有的机器 12 盘、有的 4 盘）收益明显。

**但自动选盘容易，自动「取消选盘」致命。** 若某块盘掉线或被从 `volumes` 中删除，自动选择会**静默地少给一个 data dir**——对 HDFS / MinIO 这类把数据分散在多盘上的组件，等同于静默丢数据。

要做安全就得引入一整套：盘健康探测、容量门槛、「减盘必须显式确认」的工作流、减盘前的数据迁移检查。这是一个子系统，不是一个字段。

因此 v1 只保留 `class` 声明（零成本，避免将来 schema 破坏性变更），等有真实的异构大集群用户后基于实际形态设计选择语义——现在设计等于凭空猜测。

> 见 [ADR-0008](../adr/0008-immutable-generation-linkinto.md)、[ADR-0009](../adr/0009-node-volumes-multidisk.md)

### 5. 数据目录永不触碰

升级序列中，数据目录既不删除也不移动也不覆盖：

```
物化新 generation → 渲染新配置 → preUpgrade hook → 停止 workload
→ 原子切换 current 软链 → 启动 → 健康检查
→ 失败：切回旧软链 → 启动 → 上报回滚
```

因为旧 generation 目录仍在，**回滚是一次软链切换，秒级完成**。这是 Cloudera Manager 用 parcel 做到、而 Ansible 在结构上做不到的事。

对容器化组件，同一不变式通过 **bind mount** 保持：数据从 `{{ .Paths.Data }}` 挂进容器，**不使用 docker named volume**。理由见 [05-runtime.md](05-runtime.md#6-数据用-bind-mount不用-named-volume)。
