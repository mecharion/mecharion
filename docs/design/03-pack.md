# Pack — 组件包

Pack 是 Mecharion 唯一改不动的东西。一旦第三方开始编写 Pack，格式就被锁死了。本文描述概念设计，字段级契约见 [spec/pack-v1.md](../spec/pack-v1.md)。

## 1. Pack 是什么

一个可分发的、自描述的组件包，包含：

- **元数据**：名称、版本、平台、依赖、OS 要求
- **参数定义**：类型、默认值、校验规则、运维语义（是否需重启、是否敏感、是否不可变）
- **路径声明**：组件希望的目录布局
- **资源清单**：需要存在的文件、目录、用户、模板
- **角色定义**：一个或多个 Role，各自的 workload 与资源
- **载荷 blob**：二进制、tarball、容器镜像
- **hooks**：逃生舱脚本

```
postgresql-16.4/
├── pack.yaml              # 全部声明
├── templates/             # 配置模板
│   └── postgresql.conf.tmpl
├── files/                 # 小的静态文件
├── hooks/                 # pre/post install|upgrade|remove
│   └── pre-upgrade.sh
└── blobs/                 # 载荷，按 sha256 命名
    └── sha256-ab12ef…
```

## 2. 核心设计：逻辑与载荷分离

Pack 的**逻辑**（pack.yaml + 模板 + hooks）通常几 KB；**载荷**（JDK 200MB、PostgreSQL 30MB、容器镜像 500MB）大几个数量级。混在一起会导致：改一行模板要重新分发 200MB；同一个 JDK 被三个 Pack 各打包一份。

因此 **blob 按 sha256 内容寻址**，pack.yaml 只引用摘要：

```yaml
blobs:
  main:
    linux/amd64: { sha256: "ab12ef…", size: 31457280 }
    linux/arm64: { sha256: "cd34ab…", size: 30214656 }
```

由此得到两种分发形态，**逻辑完全相同**：

| 形态 | 内容 | 用途 |
|---|---|---|
| **thin pack** | 逻辑 + blob 摘要引用 | 中心化环境。blob 从 mechd blob store 按需拉取，节点间与版本间自动复用 |
| **thick pack**（`.mpack`） | 逻辑 + 全部 blob | 离线交付。单文件，U 盘可携带 |

```
mechpack bundle   # thin → thick，把 blob 拉进来
mechpack push     # thick → thin，blob 入库
```

**边缘离线交付的载体就是一个 `.mpack` 文件**：拷到目标机，`mechctl component deploy` 即可，全程无网络（单机形态下 mechd 与 mechlet 同机，见 [ADR-0026](../adr/0026-standalone-runs-mechd.md)）。

内容寻址顺带解决：升级只传差异 blob、多版本共存去重、节点侧 blob 引用计数 GC。

> 见 [ADR-0005](../adr/0005-pack-logic-payload-split.md)

## 3. 一个 Pack 承载多个 Role

PostgreSQL 的 primary/replica、HDFS 的 NameNode/DataNode/JournalNode、Kafka 的 broker/controller——这些是**同一个软件的不同运行角色**，共享同一份二进制、同一套配置模板、同一个升级节奏。拆成多个 Pack 会导致版本漂移和依赖地狱。

```yaml
shared:                        # 所有角色共有（装一次即可）
  resources:
    - user:    { name: postgres, system: true }
    - archive: { blob: main, dest: "{{ .Paths.Generation }}", strip: 1 }

roles:
  - name: primary
    cardinality: "1"
    workload: { runtime: systemd, … }

  - name: replica
    cardinality: "0-N"
    requires: [primary]        # 决定启停与滚动升级顺序
    params:
      primary_host:
        type: string
        from: "topology.role('primary').nodes[0].address"   # 跨角色拓扑引用

  - name: client
    cardinality: "0-N"
    # 无 workload：只分发客户端配置
```

### 拓扑引用的架构后果

`from: topology.…` 意味着**模板渲染必须发生在角色放置确定之后**。因此渲染在 mechd（或单机形态下的 mechlet）完成，mechlet 收到的是**已解析的拓扑快照**。

这条约束换来一个重要性质：**mechlet 之间不需要互相查询**，每个节点只需要自己那份规格。系统的复杂度因此大幅降低。

> 见 [ADR-0006](../adr/0006-multi-role-pack.md)

## 4. 参数系统

采用**自定义类型子集**，不用完整 JSON Schema。

12 个类型：`string` `int` `float` `bool` `enum` `path` `port` `duration` `size` `cidr` `secret` `list<T>`

其中 `path` / `port` / `duration`（`30s`）/ `size`（`4GB`）/ `cidr` 是**运维语义类型**——它们让 UI 能渲染合适的控件、让引擎能做单位换算与校验。

关键的是这几个字段，它们是选择自定义子集的决定性理由：

| 字段 | 作用 | JSON Schema 能表达吗 |
|---|---|---|
| `immutable` | 创建后不可修改（如 PostgreSQL 的 encoding） | ❌ |
| `restartRequired` | 变更需重启 workload，**Rollout 据此决定是否重启** | ❌ |
| `reloadRequired` | 变更只需 reload | ❌ |
| `sensitive` | 日志与 UI 中脱敏 | ❌ |
| `from` | 由拓扑推导，不由用户填写 | ❌ |

`restartRequired` 不是 UI 装饰，它直接决定引擎行为。用完整 JSON Schema 就得在旁边再挂一套 `x-mecharion-*` 扩展，得不偿失。

**类型集是封闭的，不提供 JSON Schema 逃生舱。** 若 12 种类型不够用，正确做法是**为 pack/v1 增补新类型**（向后兼容，Pack 用 `requires.mecharion` 声明引擎版本下限），而不是引入第二套 schema 体系——后者会让 `restartRequired` 等运维语义在部分 Pack 上静默失效。

> 见 [ADR-0007](../adr/0007-params-custom-subset.md)

## 5. Hermetic（免编译、免源依赖）

**部署阶段不允许任何外部服务依赖。** 这是产品的核心约束，通过静态检查强制：

```
$ mechpack lint --hermetic
✗ hooks/pre-install.sh:12   外部依赖调用: apt-get
✗ hooks/post-install.sh:4   外部依赖调用: curl https://…
✗ pack.yaml:34              资源类型 `package` 需要 repo，非 hermetic
```

拦截清单：`apt-get` `yum` `dnf` `apk` `zypper` `curl` `wget` `git clone` `npm` `pip` `mvn` `gradle` `go build` `make` `docker pull` 等。

官方 Pack 仓库的 CI 强制此检查通过。

`package`（OS 包）资源类型仍然提供——总有用户在有 repo 的环境里需要它——但被明确标记为 non-hermetic 并被 lint 拦截。

### 与 OS 层依赖的现实冲突

理想主义在这里会破功：PostgreSQL 需要 libicu，某些组件需要 libaio，glibc 版本差异会直接导致崩溃。三条务实的应对，官方 Pack 同时采用：

1. **优先静态链接构建**
2. 必须动态链接时，**把 .so 一起打进 generation 目录**，通过 systemd unit 的 `LD_LIBRARY_PATH` 或容器镜像解决
3. `requires.os` 在安装前**校验并快速失败**，给出人类可读的错误，而不是运行时段错误

**绝不尝试自动修复宿主机依赖**——那就退回到 repo 依赖了。

> 见 [ADR-0015](../adr/0015-offline-first-hermetic.md)

## 6. mechpack 的职责边界

**mechpack 不构建你的软件，它只组装 Pack。**

命令刻意叫 `assemble` 而非 `build`，从命名上杜绝误解：

```
mechpack init       # 生成 Pack 骨架
mechpack assemble   # 按 pack.yaml 的 sources 收集本地产物 → 计算 sha256 → 生成 Pack
mechpack lint       # 格式校验 + hermetic 校验
mechpack bundle     # thin → thick (.mpack)
mechpack push       # 推入 mechd 注册表
mechpack inspect    # 查看内容 / 依赖 / blob 清单
```

开发者的完整流程：

```
自己的构建工具（make / mvn package / go build / 下载上游 tarball）
        ↓  产物
mechpack assemble  →  mechpack lint  →  mechpack bundle/push
```

前半段完全是开发者自己的事，Mecharion 一行都不管。这条边界必须清晰，否则 mechpack 会不断被要求增加构建能力，最终变成一个糟糕的构建系统。

**没有 `mechpack sign`**：早先设计过一套强制签名机制，但决定不做——见 §7。

## 7. Pack 信任是运维方自己的责任

**装一个 Pack ≡ 在目标机执行 root 代码。** Mecharion 不对 Pack 的来源做密码学意义上的身份认证：没有签名、没有可信发布者列表、没有"未知来源拒绝物化"的门禁。在哪个 Site 上装哪个 Pack，这个信任判断由管理员自己做，系统不提供额外的技术兜底。

保留的是与信任无关的**完整性**校验：Pack 的 blob 按 sha256 内容寻址，mechlet 拉取时会校验字节与文件名里的 sha256 一致，对不上就丢弃重拉——这挡的是传输/存储过程中的损坏，不是身份认证；一次连同 sha256 一起换掉的主动替换不会被它发现。

> 见 [ADR-0040](../adr/0040-pack-trust-is-operator-responsibility.md)（取代 [ADR-0016](../adr/0016-mandatory-pack-signing.md)）

## 8. 依赖关系

Pack 之间可以声明依赖（java-webapp 需要 jdk）：

```yaml
requires:
  packs:
    - { name: jdk, version: ">=17" }
```

解析规则：**只在本地可用的 Pack 集合内解析，绝不联网获取**。缺失依赖时快速失败并列出缺什么，由用户自行准备。这与 §5 的离线约束一致。

## 9. 相关决策

- [ADR-0005 Pack 逻辑与载荷分离，blob 内容寻址](../adr/0005-pack-logic-payload-split.md)
- [ADR-0006 一个 Pack 承载多个 Role](../adr/0006-multi-role-pack.md)
- [ADR-0007 params 采用自定义类型子集](../adr/0007-params-custom-subset.md)
- [ADR-0015 离线优先与 hermetic 约束](../adr/0015-offline-first-hermetic.md)
- [ADR-0016 Pack 签名为必需项](../adr/0016-mandatory-pack-signing.md)
