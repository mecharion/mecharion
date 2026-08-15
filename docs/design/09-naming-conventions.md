# 命名体系与命名空间约定

## 1. 项目名

**Mecharion** = **mech**anism（机械/机制）+ **Arion**（希腊神话中波塞冬之子、能言的不死神马）

意象：**一个管理应用、节点与生命周期操作的机械生命体。** Arion 不知疲倦地奔跑、载着使命抵达——与部署工具的语义贴合。

### 发音

这是这个名字唯一的硬伤，必须在 README 第一屏解决。

英语读者看到 `-char-` 会被 chariot / charity 引导到 /tʃ/，读成 "meh-CHAIR-ee-on"——这一读法**切断了与词根 `mech` 的联系**，词根设计就白费了。

正确读法走希腊语源规则（**ch 在 a/o/u 前发 /k/**，如 mechanic、chaos、chorus、architect、monarch）：

| | 音标 | 拼读 | 中文近似 |
|---|---|---|---|
| **采用** | /ˌmɛkəˈraɪən/ | **MEK-uh-RY-un**（重音在第三音节） | 梅卡**莱**恩 |
| 未采用 | /mɛˈkɑːriən/ | meh-KAR-ee-un | 梅**卡**里恩 |

选前者的关键理由：**第一个音节独立就是 /mɛk/ = mech**，听觉上词根完整可分离；后者中 k 被吸进第二音节，听不出 mech，词根设计的价值无法兑现。

记忆锚点：**Mecharion 与 Orion（猎户座）押韵。**

README 第一屏与组织首页统一使用这句话：

> **Mecharion** — *MEK-uh-RY-un*, rhymes with *Orion*. From **mech**anism + **Arion**, the immortal horse of Greek myth. Community shorthand: **m7n**.

一行同时解决发音、词源、缩写三件事。

## 2. 缩写 m7n

Mecharion 是 9 个字母：`M` + 中间 7 个 + `n` = **m7n**

与 Kubernetes → K8s（10 字母）、internationalization → i18n 的构造规则完全一致。

已知代价：`m7n` 的首尾字母 `m` 和 `n` 字形相近，视觉辨识度弱于 k8s 的 `k…s`。这个改不了，接受。

## 3. `mech` 词根：只用于拼接，不用于指代

| 二进制 | 说明 |
|---|---|
| `mechctl` | CLI |
| `mechd` | 控制面 |
| `mechlet` | 节点代理 |
| `mechpack` | 打包工具 |

词根拼接干净——没有一个组合产生三连字母或歧义单词。

**但裸词根 `mech` 不得用于指代项目本身。** 原因：`mech` 命名空间拥挤（mech-lang/mech 是一个编程语言，`github.com/mech` 个人账号已占用，`mecha-org` 组织存在，mecha 本身是日语中对 mechanical 的缩写、指巨型机器人的科幻类型词）。说「我们在用 mech」会被理解成那个编程语言或者机甲。

因此：

- ✅ 文档中全程使用 **Mecharion** 或 **m7n**
- ✅ 命令名 `mechctl` 等照常使用（它们完全干净）
- ❌ 不用「Mech」单独指代项目

代价是少了一个像 "kube" 那样的短称。已接受。

### 搜索可见性补救

因裸词根搜索污染严重，GitHub Topics 必须设置准确——这是别人找到项目的主要途径：

```
mecharion  m7n  application-lifecycle-management
deployment  configuration-management  golang  devops
```

## 4. 命名空间与域名约定

官网域名：**`mecharion.dev`**（源码托管与版本发布仍在 GitHub）

### 4.1 使用反向 DNS 的地方

`dev.mecharion.*` 用于需要**全局唯一、避免与他人冲突**的键空间：

| 场景 | 形式 | 例 |
|---|---|---|
| 容器标签 | `dev.mecharion.<key>` | `dev.mecharion.site` `dev.mecharion.component` `dev.mecharion.role` `dev.mecharion.generation` `dev.mecharion.managed-by` |
| systemd unit 前缀 | `mecharion-<component>-<role>.service` | 不用反向 DNS（systemd 生态惯例是短前缀） |
| OCI artifact media type | `application/vnd.mecharion.pack.v1+json` | 遵循 OCI 惯例 |
| 对象注解/标签 key（未来） | `dev.mecharion/<key>` | 对齐 Kubernetes 惯例 |
| Go module path | `github.com/mecharion/mecharion` | 保持与代码托管一致，不用 vanity import |

容器标签选择反向 DNS 是因为 Docker 生态的既定惯例（对比 `org.opencontainers.image.*`），且必须与用户自有容器的标签空间无冲突。

### 4.2 不使用域名的地方

**`pack.yaml` 的 `schema: pack/v1` 保持不变，不改成 `apiVersion: mecharion.dev/v1`。**

理由：Pack 是一个**文件格式**，不是 API 组。

| 对照 | 做法 |
|---|---|
| Helm Chart | `apiVersion: v2` |
| Docker Compose | `version: "3.8"` |
| Kubernetes CRD | `apiVersion: <group>/<version>` ← 需要域名，因为存在多个第三方 API 组共存于同一 API Server |

Mecharion 不存在「第三方定义自己的 Pack 格式组并与官方格式共存」的场景——Pack 格式只有一个，由项目定义。套上域名只增加每个 Pack 文件的噪音，不带来任何区分能力。

若将来 mechd 的 REST API 需要支持第三方扩展资源类型，届时再为 **API 对象**引入 `dev.mecharion/v1` 形式的组，与 Pack 文件格式互不影响。

## 5. GitHub 组织与仓库

```
github.com/mecharion/
├─ .github            组织首页 README、issue/PR 模板、复用 workflow
├─ mecharion          ⭐ 核心 monorepo：mechctl / mechd / mechlet / mechpack / webui
└─ packs              ⭐ 官方组件包：go-webapp / docker / jdk / nginx / postgresql / minio …
```

**核心仓叫 `mecharion`**（`mecharion/mecharion`），照 `kubernetes/kubernetes` 惯例——明确传达「这是主仓」，GitHub 搜索中也最易被找到。

**包仓库叫 `packs`** 而非 `packages`——打包工具叫 `mechpack`，产物的名词就是 **pack**，术语链闭合：`mechpack assemble` → 产出一个 pack → 存放在 `packs` 仓库。组织名已承载品牌，仓库名不再重复 `mecharion-` 前缀。

### 两条明确不做的事

- **不给每个二进制开独立仓库**——`mechctl`/`mechlet` 等在核心 monorepo 的 `cmd/` 下，Go 的多 `cmd/` 结构天然支持
- **不为 Web UI 开独立仓**——源码在核心仓 `webui/`，通过 `go:embed` 打进 `mechd`，等前端复杂到需要独立发版再拆

### 按需要再加

| 仓库 | 何时开 |
|---|---|
| `website` | 文档超过 README 承载量时（配合 `mecharion.dev`） |
| `examples` | 有第二种拓扑（HA / 多节点）时 |
| `packs-community` | 有第一个外部贡献者提包时（与官方包物理隔离，准入宽松但自动校验） |
| `homebrew-tap` | 要做 brew 分发时（仓库名必须是 `homebrew-` 前缀，Homebrew 硬约束） |
| `enhancements` | 有外部贡献者提大改动时（KEP 风格设计提案） |

## 6. 关于「科幻命名是否够企业级」

不必担心。Ansible 一词直接来自厄休拉·勒古恩的小说（超光速通信装置），Nomad、Consul、Vault 全是这个路子。基础设施领域对科幻命名的接受度非常高。

只需 logo 克制——**简洁线条机械体，不做成高达**——就不会有廉价感。

> 见 [ADR-0018](../adr/0018-project-naming.md)、[ADR-0019](../adr/0019-namespace-domain.md)

## 7. 运行期标识符（Component / Node / Site / ConfigGroup）

Pack 自己的 `name` 字段与 Role 名早已被 `mechpack lint` 的 R02/R09 强制为 DNS label（`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`，≤ 63 字符），但运维在部署时手填的 Component、Node、Site、ConfigGroup 名字此前完全没有统一校验——它们会被直接拼进 agent 本地文件名、默认受管路径与离线证书文件名，构成目录逃逸与错误位置文件写入的风险。

### 规则

复用 Pack 自己已经在用的规则，让「名字」在整个项目里只有一种合法形态：

| 标识符 | 形式 | 长度上限 | 理由 |
|---|---|---:|---|
| Component / Site / ConfigGroup | RFC 1123 **label**（同 Pack 的 `name`/`Role.name`） | 63 | 单段名字，直接拼进单一路径段与 DB 唯一键 |
| Node | RFC 1123 **subdomain**（点号分隔的 label 序列） | 253 | 默认取自 `os.Hostname()`，真实主机名常见 FQDN 形式（含点号） |

两种形式都天然排除空白、`..`、正反斜杠、绝对路径前缀、URL 保留字符与非 ASCII（含 Unicode 混淆字符）——charset 本身就是防线，不需要为每一类危险输入单独拉黑名单。校验统一在 `internal/ident` 包实现，唯一强制生效的位置是 `internal/store` 的四个 repo 写入口（`Sites().Create`、`Nodes().Upsert`、`Components().Create`、`ConfigGroups().Upsert`）——这是全仓库唯一能落库这四类实体的地方，包括 `mechlet install --standalone` 直接调用 repo 的路径,因此不需要在每个 CLI/HTTP 入口重复校验。非法名字返回统一前缀 `invalid_identifier`（`errors.Is` 可判），并归类为 `faults.Permanent`——接入 `internal/mechd/httpapi.go` 里「显式标记优先」的 `isUserError` 判据，400 而非 500。

### 代价

- **拒绝了一部分此前能跑通的名字**：含下划线的 Component/Site/ConfigGroup 名字（此前无校验，理论上能建出来）今后会被拒绝。项目仍在 pre-alpha，没有需要兼容的历史数据。
- **`mechlet install --standalone` 的默认节点名不再是裸 `os.Hostname()`**：改为先转小写（对齐 kubelet 对节点名的处理方式），仍不合法时**报错而非静默改名**（原则六：显式优于隐式）并提示用 `--node` 显式指定——一台主机名带下划线的机器，默认安装会在这一步失败，需要运维多敲一个 flag。
- **数据库 CHECK 约束本次未做。** 曾考虑过在 DB 层加一道 CHECK 约束，但 SQLite 给已有列加 CHECK 需要整表重建（建新表→搬数据→删旧表→改名），而 `store` 是全仓库唯一能触达这四张表的代码路径（已逐一确认），Go 层的校验已经是完备、无法绕过的强制点。整表重建对 pre-alpha 阶段仍在快速变动的 schema 是不成比例的风险，因此把它列为已知欠账而不是现在就做——真的出现「绕开 Go 层直接写库」的场景（比如未来加一个独立运维工具）时再补。
- **Role 名不在这次的校验范围内**：它来自 Pack 定义,已经由 `mechpack lint` 的 R09 用同一条 DNS label 规则强制,但 lint 是可选步骤,一个没 lint 过的 Pack 仍可能带着危险的 Role 名被部署。这属于 Pack 信任边界的问题（装 Pack 等价于执行 root 代码，ADR-0016），不是运维标识符的问题，因此这里只在 mechlet 侧真正落盘文件名的地方（`internal/agent/desired.go` 的实例状态文件路径）加了一道通用的「结果必须落在预期目录内」的兜底检查,不在 `internal/ident` 里重复 Pack 自己的规则。
