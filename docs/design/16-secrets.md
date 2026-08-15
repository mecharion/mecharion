# 密钥

从生成到落地的完整链路。设计依据与同类方案调研见
[ADR-0030](../adr/0030-secret-storage-and-delivery.md)。

---

## 1. 一条守得住的不变式

先说**做不到**的那条：「密钥从不出现在任何渲染产物中」。应用要从配置文件里读
口令，那个文件就必然是明文。Cloudera Manager 渲染进 `hive-site.xml`、
Ansible Vault 解密后落目标机、Kubernetes 把 Secret 挂成 tmpfs 明文——无一例外。

**加密解决的是静态存储与传输，消费点必然落明文。**

守得住的是这条：

> **密钥只出现在最终消费点的那个文件里；不进 spec、不进 generation 归档、
> 不进审计、日志、UI、diff。**

它挡住的正是绝大多数真实泄漏途径：备份、支持包、误配的同步、翻历史记录。

## 2. 生成

```yaml
app_password:
  type: secret
  generate: { length: 32 }
```

| | |
|---|---|
| 时机 | 参数解析时，**仅首次**。已有值（用户给的或已生成的）一律不动 |
| 键 | `(component, param)`——同一 Component 的所有实例共用一份凭据 |
| 字符集 | 默认 `alnum`。**排除符号是为了让口令能安全穿过** shell、连接串、`EnvironmentFile` 与各家应用自己的解析器 |

### 用户给的口令同样要进 Vault

不只是 `generate` 出来的。`type: secret` 且 `required: true`（MinIO 的
`root_password` 就是这样——一个有默认口令的对象存储比没有更糟）时，运维用
`--set-file` 给的值**也必须固化**。

只在内存里传一圈是不够的：它虽然在 `Params` 里被抹成空值，明文却仍留在
**渲染出的配置内容**里，随规格进归档、审计与 diff——那正是 §1 那条不变式
要挡住的事。

> 这个洞是用示例 Pack 跑管线时暴露的：早先的实现只遮蔽 `generate` 的密钥。
> 自造的最小 Pack 里每个 secret 都带 `generate`，因而永远碰不到这条路径。

固化时**值没变就不写**：`Put` 每次都会自增版本，而版本进 digest。
无条件写的后果是每轮调和都产生新 generation、重渲染、重启服务——
一个什么都没改的部署会永远滚动下去。

**无人值守与离线由此同时成立**：运维不输入任何东西，引擎也不联系任何外部密钥
服务。这是相对「必须有 Vault 可达」类方案的实质差异。

> 每轮调和重新生成会让密码每 60 秒换一次，服务永远连不上。固化不是优化，
> 是正确性。

## 3. 存储：信封加密

```
/etc/mecharion/secret.key              0400 root   主密钥（KEK），32 字节随机
/var/lib/mecharion/mechd/mechd.db      被 KEK 包裹的数据密钥（DEK）+ 密文值
```

启动时读 KEK → 解开 DEK → DEK 常驻内存。值用 AES-256-GCM 加密，附加数据绑定
该条密钥的 id（防止把密文从一行搬到另一行）。Go 标准库，**无 CGO**。

**为什么两层**：轮换主密钥只需重新包裹 DEK 那几十字节，而不是重加密整库；
将来接 KMS 时换个地方存 KEK 即可，**数据一个字节不动**。

### 它挡什么，不挡什么

| 场景 | |
|---|---|
| `mechd.db` 被单独拷走（备份、快照、支持包、误配同步） | ✅ 前提是主密钥没跟着一起拷 |
| 整机备份 / 整盘镜像 | ❌ 两个文件都在里面 |
| mechd 主机被拿到 root | ❌ |
| 内存 dump | ❌ DEK 在内存里 |

**它把「偷一个文件」变成「必须偷两个，而其中一个通常不在备份范围里」。**
有边界的收益，不是万灵药——不做安全剧场。

默认开启，可关；关掉即明文 + `0600`。

### 主密钥丢失

| 密钥来源 | 后果 |
|---|---|
| `generate` 生成的 | **可恢复**：重新生成 + 重新部署，provider 的 hook 会改掉数据库里的口令 |
| 运维手填的 | **真的没了**，只能回源头重取 |

因此两条配套要求：

- 首次生成时**明确打印一次**：单独备份，不要和 DB 放进同一份备份
- 主密钥缺失但库里有密文时**拒绝启动并说清原因**，不静默把口令当空值

## 4. 下发：spec 里放引用，不放值

```
mechd 渲染 → 敏感值处放 @@m7n:secret:<id>@@
             spec 另带 secretRefs: [{ id, version }]      ← 只有 id 与版本号
             ↓
gRPC 消息    DeliverSpec{ spec, secrets: {id → 明文} }    ← secrets 字段不落盘
             ↓
mechlet      写盘的最后一刻做字面量替换                     ← 与 .Paths.Generation 同一套机制
```

三点收益：

| | |
|---|---|
| **归档天然干净** | 旧口令不会永久躺在历史 generation 的 spec 里——否则「轮换」等于没轮换 |
| **不需要七处脱敏** | 否则 mechd 的 generation 历史、mechlet 的 spec 归档、审计、事件流、`config diff`、API、UI **每一处都要各写一次**，漏一处要等出事才发现 |
| **轮换语义正确** | 见 §5 |

**mechlet 只在内存里持有明文**，随规格一起来、随进程退出而去。重启后重新
向 mechd 要一次——这与「重连即全量重推」（[13-mechd §6](13-mechd.md#6-断连与重连)）
是同一条路径，不额外设计。

> 单机调试入口 `mechlet apply -f` 允许规格里直接带明文（无 mechd 可问）。
> 此时**不写 spec 归档**，并在输出中提示这是调试路径。

### 哨兵串的选择

`@@m7n:secret:<id>@@` 中的 id 是随机的，实际内容撞上它的概率可以忽略。
但**不依赖概率**：mechd 在渲染时检查源内容是否含 `@@m7n:secret:` 前缀，
含则直接报错。mechlet 侧替换后若仍有残留同样报错——与
`{{ .Paths.Generation }}` 的处理完全一致。

## 5. 轮换必须产生新 generation

早期设计写的是「排除 SecretRefs，密钥轮换不应产生新 generation」。**那是错的。**

口令一旦渲染进配置文件（而这**无法避免**），文件内容就变了。此时若 digest 不变：

```
不产生新 generation
  → 资源层检出差异，但 generation 没换 ⇒ 按漂移处理
  → 默认 driftPolicy: report ⇒ 只上报不改
  → 轮换永远发不出去
```

因此：**值不进 digest（它根本不在 spec 里），但 `secretRefs.version` 进 digest。**

```
mechctl component rotate-secret pg-main --param app_password
  → 生成新值，version+1
  → provider 的 rotate hook 改掉数据库里的口令
  → 消费方 digest 变 → 新 generation → 重渲染 → 重启
```

**「轮换不该产生新 generation」这个直觉本身是错的**——它把「值没进 spec」和
「什么都没发生」混为一谈了。配置文件确实变了，重启是应当的。

### v1 接受一个切换窗口

provider 先改、consumer 后改，中间有一小段 consumer 拿旧口令连不上。
干净的做法是双凭据（建新角色 → 切消费方 → 删旧角色），那需要 Rollout 编排
配合，留给 M6。v1 明确记录这个窗口，并让 rotate 走依赖拓扑序以尽量缩短它。

## 6. hook 的密钥注入

```
MECHARION_PARAM_<NAME>        普通参数，环境变量
MECHARION_PARAM_FILE_<NAME>   敏感参数，指向一个 0600 临时文件的路径
```

**敏感参数不进环境变量**：环境变量会出现在 `/proc/<pid>/environ` 与崩溃转储里，
且被子进程继承。文件在 hook 结束后立即删除，无论成败。

临时文件落在 mechlet 的私有目录（`/run/mecharion/hooks/<uuid>/`），
`tmpfs`、`0700`，进程退出时整目录清理。

## 7. 交给应用的形态

按泄漏面从大到小（详见 [spec §7.7](../spec/pack-v1.md#77-口令怎么交给应用)）：

| 形态 | 谁看得到 | 建议 |
|---|---|---|
| 内联主配置 | 那份配置被传阅即泄漏 | 没有更好选择时才用 |
| 内联 `Environment=` | **unit 是 0644 全体可读，`systemctl show` 原样打印** | ❌ 规则 50 禁止 |
| `envFile` + `${VAR}` | `/proc/<pid>/environ`；`systemctl show` 只显示路径 | ✅ 默认 |
| `password_file` 路径间接 | 只有能读该文件的用户 | ✅ 应用支持就用 |

第三行**不需要任何加密**就达成了 Jasypt / `ENC(...)` 真正想解决的问题：
把口令拆到另一个文件，主配置就可以随手贴进工单。

## 8. 相关决策

- [ADR-0030 密钥的存储与下发](../adr/0030-secret-storage-and-delivery.md)
- [ADR-0016 强制 Pack 签名](../adr/0016-mandatory-pack-signing.md)
- [08-security §5](08-security.md#5-敏感信息) · [spec §5.4](../spec/pack-v1.md#54-exportsfields--具名字段与凭据) · [spec §7.6](../spec/pack-v1.md#76-generate--引擎生成的密码)
