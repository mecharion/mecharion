# ADR-0008: generation 不可变，用 linkInto 调和应用原生布局

- **状态**：已接受
- **日期**：2026-08-01
- **相关**：[ADR-0009](0009-node-volumes-multidisk.md)、[ADR-0011](0011-docker-compose-in-v1.md)

## 背景

早期设计把「home / config / data 三目录分离」写成硬性约定：

```
/opt/mecharion/apps/<x>/versions/<v>/     不可变
/etc/mecharion/apps/<x>/                  配置
/var/lib/mecharion/apps/<x>/              数据
```

随后遇到反例：**大量应用要求配置位于安装目录内部**。

| 应用 | 配置位置 |
|---|---|
| Kafka | `$KAFKA_HOME/config/` |
| Tomcat | `$CATALINA_HOME/conf/` |
| Elasticsearch | `$ES_HOME/config/`（另有 `plugins/`、`modules/` 在 home 内） |
| ZooKeeper | `$ZK_HOME/conf/` |
| **PostgreSQL** | **`$PGDATA/`（在数据目录内，第三种变体）** |

若强行要求分离，这些 Pack 要么无法编写，要么必须用 hook 脚本绕过——后者会让引擎失去对配置的管理能力。

## 候选方案与调研

### 方案 A：强制分离布局，不兼容者用 hook 绕过

- ❌ 引擎失去对配置的管理能力（无法 diff、无法漂移检测、无法跨版本携带）
- ❌ 大量主流组件被排除

### 方案 B：允许配置写进版本目录

- ❌ **破坏不可变性**：配置变更即修改「不可变」目录，回滚保证失效
- ❌ 配置随版本目录重复，升级时会丢失用户修改

### 方案 C：软链farm — 配置权威存储在外，软链进版本目录 ⭐

调研对照：

| 实践 | 做法 |
|---|---|
| **Debian tomcat9** | `/var/lib/tomcat9/conf -> /etc/tomcat9` — **完全相同的手法，运行二十年** |
| **Debian/RPM 通用** | 配置一律进 `/etc`，必要时软链回程序目录 |
| **Nix / GoboLinux** | 不可变 store + 符号链接组装运行时视图 |
| **Elasticsearch 官方包** | 支持 `ES_PATH_CONF` 环境变量指向 `/etc/elasticsearch` |

### 方案 D：叠加层（overlayfs / hardlink farm）

版本目录保持纯净，另行组装一个「运行时根」。

- ✅ 最灵活
- ❌ 复杂度显著高于方案 C，且需要 overlayfs 支持（边缘老内核不一定有）
- 收益不足以覆盖成本，否决

## 决策

**采用方案 C，并把不变式重新表述——这是本 ADR 最重要的部分。**

> **不变式：generation 目录是只读的。任何需要可写、或需要跨版本存活的路径，其物理存储必须在 generation 目录之外。**

「分离布局」只是满足这条不变式的**一种方式**，不是目标本身。早期设计把手段当成了目标，这是需要纠正的认知错误。

### 机制：`linkInto`

Pack 声明应用的**原生布局**，mechlet 负责调和到不变式：

```yaml
paths:
  config:
    default:  "{{ .Node.Roots.etc }}/apps/{{ .Component }}"   # 权威存储位置
    linkInto: "{{ .Paths.Generation }}/config"                # 应用期望看到的位置
    distDir:  "config"                                        # tarball 自带的默认配置目录名
```

物化流程：

1. 解开 blob 到 generation 目录
2. 把 tarball 自带的 `config/` 改名为 `config.dist/`
3. 渲染权威配置到 `/etc/mecharion/apps/kafka/`
4. 建软链 `<generation>/config -> /etc/mecharion/apps/kafka`

**`linkInto` 是通用机制**：配置、日志、运行期可写目录（Tomcat 的 `work/`、`temp/`）、插件目录（ES 的 `plugins/`）共用同一套，不为每种情况发明新模式。

### 术语修正：version dir → generation

因 [ADR-0011](0011-docker-compose-in-v1.md) 将 docker/compose 纳入 v1，「版本目录」一词不再够用——docker 的「版本」是镜像 digest 而非目录。统一为 **generation**：

> generation = 一次完整物化，是原子切换与回滚的单位。物理形态因 Runtime 而异。

由此得到一个重要的统一：**配置变更同样产生新 generation**，因此「回滚」是单一动作，不区分「回滚版本」与「回滚配置」。

### `config.dist/` 的作用

升级时新版本可能新增或重命名配置项。保留每个 generation 的默认值基线，才能提供 `mechctl config diff -c kafka --from 3.6.0 --to 3.7.0`。

所有包管理器都在这件事上栽过跟头（Debian 的 `.dpkg-dist`、RPM 的 `.rpmnew` 都是同一问题的补丁式解法）。一开始就留位置，成本几乎为零。

## 理由

**把约束定在「不变式」而非「布局」这一层，是让设计同时具备刚性与弹性的唯一方式。**

- 刚性：不可变 + 原子切换 + 秒级回滚的保证从不松动
- 弹性：应用的原生布局千差万别，Pack 作者只需如实声明，不需要与引擎的成见搏斗

PostgreSQL 的案例最能说明粒度是对的——它的配置在数据目录内，用 `paths.config.default: "{{ .Paths.Data }}/pgdata"` 直接表达即可，**连 `linkInto` 都不需要**，不变式自动满足。引擎不预设任何「标准布局」，只保证不变式。

## 后果

### 收益

- 覆盖三种配置布局（独立 / 在 home 内 / 在 data 内），无需 hook 绕过
- 不可变性与回滚保证在所有情况下成立
- 配置跨版本存活、可备份、可审计、可 diff
- 单一机制服务四种用途（配置、日志、运行时目录、插件目录）
- `generation` 术语统一了裸机与容器的回滚语义

### 代价

- **软链的边界情况**：极少数应用会解析软链并因此行为异常。留 `layout: inline` 逃生舱（配置直接渲染进 generation，每次改配置产生新 generation；原子切换与回滚仍成立，只是多占磁盘）。v1 不实现，schema 保留位置。
- **配置目录必须可写**：Tomcat 会写入 `conf/Catalina/`。`/etc/mecharion/apps/<x>/` 需保持可写，不能设为只读。
- **Pack 作者需理解 `linkInto`**：比「照约定放」多一个概念。缓解：默认值覆盖多数场景，只有非常规布局的 Pack 才需要显式声明。
- **磁盘占用**：多 generation 并存。需要保留策略（默认保留最近 N 个）与 GC。

## 参考

- Debian `tomcat9` 包的 `/var/lib/tomcat9/conf -> /etc/tomcat9`
- Elasticsearch `ES_PATH_CONF`
- Debian `.dpkg-dist` / RPM `.rpmnew`（配置升级问题的既有解法）
- Nix profile generations（不可变 + 原子切换）
