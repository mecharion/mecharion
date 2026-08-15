# ADR-0018: 项目命名体系 Mecharion / m7n / mech 词根

- **状态**：已接受
- **日期**：2026-08-01
- **相关**：[ADR-0019](0019-namespace-domain.md)

## 背景

需要一个项目名，并在其之上建立可延续的组件命名体系——参照 Kubernetes → k8s、kubelet/kubectl/kubeadm 的模式：**一个共享词根让所有组件名自然拼接，一个数字缩写供社区口语使用。**

命名维度考虑过：神话人物（中西方）、形象化命名（如 Docker）、缩写构造（如 i18n / k8s）、原创组合。

## 决策

**项目名：Mecharion** = **mech**anism（机械/机制）+ **Arion**（希腊神话中波塞冬之子、能言的不死神马）

- **社区缩写**：`m7n`（9 字母：`M` + 中间 7 个 + `n`，与 K8s / i18n 构造规则一致）
- **公共词根**：`mech`，**仅用于拼接，不用于指代项目**

| 二进制 | 说明 |
|---|---|
| `mechctl` | CLI |
| `mechd` | 控制面 |
| `mechlet` | 节点代理 |
| `mechpack` | 打包工具 |

**发音：MEK-uh-RY-un**（/ˌmɛkəˈraɪən/），rhymes with *Orion*。

## 理由

### ① 词根拼接干净

`mechctl` / `mechlet` / `mechd` / `mechpack` 没有一个产生三连字母或歧义单词。这在四字母词根中并不常见——对比 `mill` + `let` = `millet`（小米）、`sene` + `ctl` 读起来断裂。

### ② 意象与动词互相强化

「机械生命体」这个意象天然支持 install / inspect / repair / diagnose / reconcile 这一整套运维动词。

Docker 从未做到这件事（鲸鱼与 build/push 毫无关系），Kubernetes 也只在 helm/chart 那一层做到。**名字能反向指导 CLI 词汇表**，这是较高的设计完成度。

### ③ 视觉化成本低

机械体比「舵手」（Kubernetes 的词源）好画得多，mascot、贴纸、社区文化的成本很低。

### ④ 占用情况干净

调研时 `Mecharion` 与 `mechctl` 均为零命中，这在命名阶段很难得。

## 关键约束：裸词根 `mech` 不得用于指代项目

`mech` 命名空间拥挤：

- `mech-lang/mech` 是一个用于构建数据驱动系统的编程语言
- `github.com/mech` 个人账号已被占用
- `mecha-org` 组织已存在
- `mecha` 本身是日语中对 mechanical 的缩写，指巨型机器人的科幻类型词

因此说「我们在用 mech」会被理解成那个编程语言或者机甲。

**规则**：

- ✅ 文档全程使用 **Mecharion** 或 **m7n**
- ✅ 命令名 `mechctl` 等照常使用（它们完全干净）
- ❌ 不用「Mech」单独指代项目

代价：少了一个像 "kube" 那样的短称。已接受。

**补救**：GitHub Topics 必须设置准确，这是别人找到项目的主要途径。

```
mecharion  m7n  application-lifecycle-management
deployment  configuration-management  golang  devops
```

## 发音：本名唯一的硬伤

英语读者看到 `-char-` 会被 chariot / charity / charcoal 强烈引导到 /tʃ/，读成 **"meh-CHAIR-ee-on"**——这一读法**直接切断了与词根 `mech`（/mɛk/）的联系**，词根设计就白费了。

正确读法走希腊语源规则：**ch 在 a/o/u 前发 /k/**（mechanic、chaos、chorus、architect、monarch 全是如此）。

| 候选 | 音标 | 拼读 | 采用 |
|---|---|---|---|
| **A** | /ˌmɛkəˈraɪən/ | MEK-uh-RY-un（重音第三音节） | ✅ |
| B | /mɛˈkɑːriən/ | meh-KAR-ee-un（重音第二音节） | ❌ |

**选 A 的关键理由**：A 的第一个音节独立就是 /mɛk/ = mech，听觉上词根完整可分离；B 中 k 被吸进第二音节（"meh-KAR"），听不出 mech——**词根设计的价值只有在 A 中才能兑现**。

A 还附带一个记忆锚点：**与 Orion（猎户座）押韵**。

统一话术（README 第一屏、组织首页）：

> **Mecharion** — *MEK-uh-RY-un*, rhymes with *Orion*. From **mech**anism + **Arion**, the immortal horse of Greek myth. Community shorthand: **m7n**.

一行同时解决发音、词源、缩写三件事。

## 后果

### 收益

- 组件命名体系可无限延伸且拼接自然
- 意象反向指导 CLI 词汇表
- 视觉识别成本低
- 名称占用干净

### 代价

- **发音需要主动教育**：必须在 README 第一屏、官网首页、每次公开演讲中重复。这是持续成本
- **无短称可用**：社区不能像叫 "kube" 那样叫 "mech"
- **`m7n` 视觉辨识度弱于 `k8s`**：首尾字母 m 与 n 字形相近。这个改不了，接受
- **裸词根搜索污染**：靠 Topics 与官网 SEO 补救，无法根治

## 关于「科幻命名是否够企业级」

不必担心。Ansible 一词直接来自厄休拉·勒古恩的小说（超光速通信装置），Nomad、Consul、Vault 全是这个路子。基础设施领域对科幻命名的接受度非常高。

只需 logo 克制——**简洁线条机械体，不做成高达**——就不会有廉价感。

## 参考

- Kubernetes → k8s / kubelet / kubectl / kubeadm 命名体系
- i18n / l10n 的数字缩写构造
- Ansible（词源来自 Ursula K. Le Guin 的小说）
- 希腊语源词的 ch 发音规则（mechanic / chaos / chorus / architect）
