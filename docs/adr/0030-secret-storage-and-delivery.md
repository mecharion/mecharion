# ADR-0030：密钥用信封加密存储，用不透明引用下发

- 状态：已接受
- 日期：2026-08-04
- 相关：[ADR-0015](0015-offline-first-hermetic.md)、[ADR-0016](0016-mandatory-pack-signing.md)、[16-secrets](../design/16-secrets.md)

## 背景

一个具体问题引出了整块设计：**go-webapp 要用 PostgreSQL 的口令，依赖机制怎么
把它送过去？**

约束是硬的：

- **边缘离线**：不能依赖任何外部密钥服务（Vault / KMS 可达）
- **无人值守**：不能要求运维在部署时输入任何东西
- **审计**：谁在什么时候拿到过什么，要查得到

## 同类方案调研

| 系统 | 静态存储 | 消费点 | 跨组件传递 |
|---|---|---|---|
| Cloudera Manager | 自有 DB | **明文渲染进 `hive-site.xml`** | 配置参数直接引用；UI/API/日志按 redaction 策略脱敏 |
| Ansible Vault | git 里密文 | **明文落目标机** | 变量引用；`no_log` 防日志 |
| Puppet hiera-eyaml | git 里密文 | **明文落目标机** | 同上 |
| Chef Vault | 数据袋密文，**按节点公钥加密** | **明文落目标机** | 只有授权节点的私钥能解 |
| Kubernetes + Operator | etcd（默认仅 base64，可选 KEK） | **明文**注入 env 或 tmpfs | **provider 建具名 Secret，consumer 按名引用**，RBAC 授权 |
| Vault + Nomad | Vault，**支持动态短期凭据** | **明文**渲染到 tmpfs | 最强，但**要求 Vault 可达** |
| systemd credentials | 磁盘上密文（TPM2 或宿主机密钥封装） | 启动时解密到 tmpfs | 应用读 `$CREDENTIALS_DIRECTORY` |

**共识非常一致：加密解决静态存储与传输，消费点几乎一律落明文。**
唯一例外是应用自身支持间接引用（Hadoop 的 `jceks://`、Spring 的 `ENC(...)`、
PostgreSQL 的 `.pgpass`）——而那是**应用侧的能力**，编排工具替它实现不了。

Vault 的动态凭据是最强的，但它要求 Vault 可达，直接否掉边缘离线。
systemd credentials 最接近「工具提供解密机制」，但需要 systemd ≥ 251，
而我们的支持下限是 239（RHEL 8），且仍要求应用能从文件读。

## 决策

### ① 不变式收窄

「密钥从不出现在渲染产物中」**做不到**，改为：

> 密钥只出现在**最终消费点的那个文件**里；不进 spec、不进 generation 归档、
> 不进审计、日志、UI、diff。

### ② 引擎生成口令（`params.generate`）

首次解析时生成一次并固化。**无人值守与离线由此同时成立**——运维不输入任何
东西，也不联系任何外部服务。这正是 Vault 类方案做不到的那一半。

### ③ 存储用信封加密

主密钥（KEK）单独文件 `0400`，数据密钥（DEK）被包裹后存 DB，值用
AES-256-GCM 加密。默认开启，可关（关掉即明文 + `0600`）。

**两层而非直接用主密钥加密每个值**：轮换主密钥只需重新包裹 DEK 那几十字节；
将来接 KMS 时换个地方存 KEK 即可，数据一个字节不动。

明确边界，不做安全剧场：它挡「DB 副本外流」（备份、快照、支持包、误配同步），
**不挡 mechd 主机的 root**。

### ④ 下发用不透明引用

spec 里放 `@@m7n:secret:<id>@@` 与 `secretRefs: [{id, version}]`，明文走 gRPC
消息的独立字段，mechlet 只在内存持有、写盘时做字面量替换。

**这一条的复杂度账要算清楚。** 若明文内联进 spec，为了不泄漏必须在**七处**
各写一次脱敏：mechd 的 generation 历史、mechlet 的 spec 归档、审计记录、
事件流、`config diff`、API 响应、Web UI——**漏一处要等出事才发现**。
用引用则七处天然干净，真正的新增只有「渲染时发 token」与「写盘时替换」，
而后者与 `{{ .Paths.Generation }}` 复用同一段代码。

**净复杂度是降低的。**

### ⑤ 跨组件传递走显式导出

provider 用 `exports.fields` 导出具名字段，consumer 不得读 provider 的参数
（规则 49）。被消费的凭据应当是**专为该依赖关系存在的账号**，不是 superuser——
给下游应用 superuser 口令是提权。

敏感标记由 mechd 在绑定时**自动传播**，不依赖 consumer 声明得对。

## 代价

| | |
|---|---|
| **主密钥丢失** | `generate` 的可重新生成；**运维手填的真的没了**。需配套备份提示与「缺密钥拒绝启动」 |
| **轮换会重启服务** | 口令在配置文件里，文件变了就要重启。见下 |
| **多一个文件要管** | 主密钥必须与 DB 分开备份，这是新的运维纪律 |
| **消费点仍是明文** | 承认它，并把重点放在「不扩散」而非「不存在」 |

## 一处推翻既有设计的记录

12-spec-and-state 原写「排除 SecretRefs，密钥轮换不应产生新 generation」。
**那是错的。**

口令一旦渲染进配置文件，文件内容就变了。digest 若不变 ⇒ 不产生新 generation
⇒ 资源层按漂移处理 ⇒ 默认 `report` 只上报不改 ⇒ **轮换永远发不出去**。

正确做法：值不进 digest（它根本不在 spec 里），但 `secretRefs.version` 进
digest。那个直觉把「值没进 spec」和「什么都没发生」混为一谈了。

## 不做

- **systemd credentials**：需 systemd ≥ 251 而下限是 239；且仍要求应用能从文件读
- **在模板函数里做 Jasypt / ENC 加密**：函数集刻意封闭；且要在 Go 里精确复刻
  Java 的 PBE 派生，是兼容性雷区。真需要时走 `script` 逃生舱
- **外部 KMS / Vault**：破坏离线约束。只留接口
