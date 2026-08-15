# 安全模型

## 1. 信任模型：先说清楚边界

**mechlet 必然以 root 运行**——它要管理 systemd unit、创建系统用户、写 `/etc`、挂载文件系统。这意味着：

> **安装一个 Pack ≡ 在目标机器上执行 root 代码。**

这个事实不应被掩饰。它决定了整个安全设计的重心：**信任边界在「这个 Pack 是谁做的」，而不是「Pack 里的代码能做什么」。** 沙箱化一个必须建用户、写 /etc、控制 systemd 的东西是徒劳的。

因此安全设计的三个支柱是：**完整性校验**（内容没被传坏）、**mTLS**（传输可信）、**操作日志**（留痕，但还不是"事后可查"——见 §6 的代价）。**「这个 Pack 是谁做的」不在这三条里**——见 §2。

## 2. Pack 信任 —— 系统不校验，运维方自己负责

**Mecharion 不对 Pack 的来源做密码学意义上的身份认证。** 没有签名、没有可信发布者列表、没有任何"未知来源拒绝物化"的门禁。管理员决定在哪个 Site 上部署哪个 Pack，这个决定本身就是信任判断，系统不提供、也不假装提供额外的技术兜底。

这不是遗漏，是明确决定——见 [ADR-0040](../adr/0040-pack-trust-is-operator-responsibility.md)。此前 [ADR-0016](../adr/0016-mandatory-pack-signing.md) 定过一套强制签名机制（`mechpack sign`、`/etc/mecharion/trust/`、`packVerification: enforce`），但从决策写下到 M0–M9 走完，这套机制**没有任何一部分被实现**——这正是"设计说必需、代码全接受"的落差。ADR-0040 的结论是撤销这个决定，而不是补全它：签名/信任库是一个独立的小型 PKI 工程，收益（挡不住"可信发布者本身写了坏 Pack"，ADR-0016 自己也承认这一点）与代价（密钥管理、分发、轮换全套负担，每次本地迭代都多一步签名）不成比例，且与"管理员本来就要亲自决定装哪个 Pack"的前提重复。

### 2.1 保留、且与信任无关的机制：sha256 内容寻址

Pack 的 blob 按 sha256 内容寻址（[ADR-0005](../adr/0005-pack-logic-payload-split.md)）。`internal/protocol/client.go` 的 `FetchBlob` 在写盘前校验拉到的字节与文件名里的 sha256 一致，对不上就丢弃重拉（`internal/agent/agent.go` 的 `fetchBlobs`）。

**这挡的是传输/存储过程中的损坏，不是身份认证**——它回答"这份内容和我请求的是同一份"，不回答"这份内容是谁做的、能不能信"。一次主动的恶意替换（连同 sha256 一起换掉）不会被这层机制发现，这个边界必须写清楚，不能让人把"内容寻址"误读成"来源可信"。

### 2.2 代价必须写清

- 一个被替换或投毒的 Pack，系统不会拒绝——识别它是运维方自己的责任（获取渠道是否可信、部署前是否审查过内容）
- 离线场景（U 盘、内网文件服务器、跨网闸摆渡）下这条判断完全落在运维流程，系统没有额外兜底
- 如果将来出现真实的第三方 Pack 生态（陌生发布者、供应链场景），需要重新评估——那时的前提和现在不同，应写新 ADR

## 3. 传输安全

### 3.1 mechlet ↔ mechd

| 形态 | 传输 | 认证 |
|---|---|---|
| **多节点** | gRPC over TLS | **mTLS 双向认证**；join token（有 TTL、有次数上限、可吊销）换取每节点证书，剩余 <30 天自动轮换；mechd 维护 CA |
| **单机** | gRPC over unix socket<br>`/run/mecharion/mechd.sock` | **socket 权限 `0600 root:root`** |

**身份以证书 CN 为准，不是请求里的 `node_name`**（[ADR-0034](../adr/0034-node-join-and-identity.md)）。
两条监听两套身份来源，在 `internal/protocol/identity.go` 的 `nodeOf` 一处收口——
散在各个 RPC 里的话，「某个 RPC 忘了忽略 node_name」就是一个能绕过 mTLS 的洞。

**吊销走应用层状态检查，不做 CRL**（同一条 ADR）。只有一个 mechd 持有全部
状态，CRL 要解决的「校验方与签发方分离」在这里不存在。代价照实写：
**一张被吊销的证书仍能完成 TLS 握手**，任何依赖「握手成功即已授权」的中间件
（反向代理、L7 网关）在这里都会得出错误结论。`node remove` / `node revoke`
之后那张证书握手照样成功，但每个 RPC 都会被拒，并在审计里留一条。

**单机不使用 mTLS 是刻意的**：unix socket 的权限是内核强制、无法伪造的对端身份保证，比证书更强。在其上再叠一层 TLS 只增加启动复杂度与故障面（证书生成、轮换、时钟偏移），换不到任何安全收益（[ADR-0026](../adr/0026-standalone-runs-mechd.md)）。

连接方向恒为 mechlet → mechd：**节点不监听任何对外端口**，攻击面小于 Ansible 要求的 SSH 常开。

### 3.2 mechd 的 HTTP 接口

服务对象是 `mechctl` 与浏览器，因此必须走 TCP。

```yaml
# /etc/mecharion/mechd.yaml
http:
  listen: 0.0.0.0:8443
  tls:
    mode: self-signed          # self-signed | provided
```

**默认绑定 `0.0.0.0` 并启用 HTTPS。** 边缘现场的常见需求是「拿笔记本连门店那台机看 UI」，绑回环会让这件事做不了；而一旦对外监听，明文 HTTP 不可接受。

#### 自签证书与轮换

首次启动时自动生成，无需任何人工步骤：

| 项 | 有效期 | 位置 |
|---|---|---|
| CA | **10 年** | `/etc/mecharion/pki/ca.crt` + `ca.key`（`0600 root:root`） |
| 服务端证书 | **1 年** | `/etc/mecharion/pki/server.crt` + `.key` |

- SAN 包含 hostname、`127.0.0.1`、以及启动时探测到的全部本机 IP；额外地址用 `http.tls.extraSANs` 补充
- **剩余有效期 < 30 天时自动重新签发并热重载**，不需要重启
- 主机 IP 变化（DHCP）时下次轮换自动纳入

企业环境可用 `tls.mode: provided` 指定外部证书（企业 CA / Let's Encrypt），此时 Mecharion 不管理其生命周期。

#### 客户端如何信任

| 场景 | 做法 |
|---|---|
| **本机** | `mechctl` 直接读 `/etc/mecharion/pki/ca.crt`——零配置 |
| **远程** | `mechd ca export` 导出 CA，`mechctl context set <name> --ca-file <path>` |
| **浏览器** | 导入同一份 CA；文档给出各平台步骤 |

**不启用 TOFU（首次连接即信任）作为默认**——它会让中间人攻击在首次连接时无法被发现。可用 `--insecure-skip-verify` 显式跳过（仅供排障，会持续告警）。

#### 未认证入口的资源边界

`mechd` 的部分入口（`/auth/challenge`、`/auth/login`、`/auth/bootstrap`）在
设计上就是未认证的——那正是它们存在的理由。此前这些入口的 JSON 请求体既没有大小上限也没有独立的读取
超时：`http.Server` 只设了 `ReadHeaderTimeout`（护住请求头），正文可以
用一个正常连接以每次几个字节的速度慢慢喂，handler 会一直卡在解析里等，
一个这样的连接就占掉一个 goroutine 且永不释放。

修复统一收在 `decodeBody`（全部 JSON 请求体的唯一入口）：

| 边界 | 做法 |
|---|---|
| 体积 | `http.MaxBytesReader` 上限 1 MiB（Pack 上传走独立的 4 GiB 上限，不经过这里） |
| 读取时间 | 用 `http.ResponseController.SetReadDeadline` 给单个请求的正文单独设超时，与请求头的超时分开算，不影响 SSE（那条路径不读正文） |
| Content-Type | 必须是 `application/json`，不然把任意内容当 JSON 解是在帮攻击者省一次试探 |
| 请求体单值 | 显式检查第二个 JSON 值不存在——`{...}{...}` 这种拼接体，标准库的 `Decode` 只解析第一个、静默丢弃第二个 |

超限返回 413，其余三类返回 400，均由 `statusFor` 统一分类，不需要在
每个路由各写一遍。

#### 错误响应契约：500 不泄漏内部信息，分类不依赖文案

`writeErr` 把任何错误的 `err.Error()`
原样塞进响应体，不分状态码——500 的时候，SQL 语句、内部路径、配置
细节可能就在这段文本里，直接回给了客户端；而决定一个错误算不算
「用户的问题」（该 400 还是 500）靠的是拿错误文案去匹配一份中文
关键词表（`isUserError` 里的子串兜底），只要有人把某句提示的措辞
改顺口一点，状态码就可能悄悄从 400 变成 500，或者反过来。

**响应体现在是三个字段**：

```json
{"error": "...", "code": "invalid_argument", "requestId": "a1b2c3d4e5f6"}
```

| 字段 | 来源 | 说明 |
|---|---|---|
| `code` | HTTP 状态码的固定映射（`errCode`） | 稳定、机器可读，不看错误文案；程序化分支应当读它，不该匹配 `error` 里的中文 |
| `requestId` | `withRequestID` 中间件按请求生成 | 也回填在 `X-Request-Id` 响应头；500 时是客户端唯一能带给运维的线索 |
| `error` | 视状态码而定 | 400/404/409/429 等：原样返回，这些文案本来就是写给用户看的说明；**500：换成固定的通用文案**，真实的 `err.Error()` 连同 `requestId`、方法、路径一起写进服务端日志（`a.S.log().Error(...)`），不出现在响应体里 |

**分类只认显式类型标记**：`isUserError` 现在只看 `errors.As(err, &faults.Error)`
且 `Class == faults.Permanent`，子串兜底表已经删掉。落地时把当时能
匹配上那份关键词表的全部错误构造点（`internal/mechd` 与
`internal/placement` 各处，约 60 余处）显式包了一层
`faults.Permanentf`/`faults.Wrap(faults.Permanent, ...)`，转换前后逐条核对
过实际影响面没有变化——这不是一次行为变更，是把此前靠字符串猜出来的
判断，换成看得见、编译期检查得到的类型标记。

**为什么不索性把 `faults.ClassOf` 的默认值也算成 400**：`faults.ClassOf`
对**未分类**的错误统一答 `Permanent`（这是 reconcile 循环需要的默认
值——未知错误按不可重试处理更安全）。如果 HTTP 层直接拿这个当 400
的判据，会让任何一处没打过类型标记的内部错误（未包装的 SQL 失败、
JSON 编码错误……）都变成 400，正好是这次要修的那类「掩盖服务端故障」
的问题在另一个地方重演一次。因此 `isUserError` 只认**显式**
`faults.Wrap(faults.Permanent, ...)`/`faults.Permanentf(...)`，不去问
`faults.ClassOf` 的默认答案。

**已知欠账**：显式打类型标记这件事只做到了 `internal/mechd` 与
`internal/placement`（外加因一次真实回归而补上的 `internal/spec/drift.go`，
见开发日志）。Deploy/渲染路径还会经过 `internal/render`、`internal/pack`、
`internal/spec` 的其余部分（`digest.go`/`secret.go`），那两个包里同样有
不少 `fmt.Errorf`，性质比 mechd/placement 更混杂——校验错误与内部
不变式检查交织在一起，逐条判断需要的谨慎程度更高。当前用一整套
容器化验收（M2–M9）兜底，没有发现除 drift.go 之外的同类问题，但那
只说明「当前测试覆盖到的路径」状态码是对的，不等于「全部路径」都
已经打过类型标记。留给后续单独收口，不在这里假装已经做完。

#### 基础安全响应头

管理界面此前只设置内容/缓存头（`internal/webui/webui.go`）与请求边界（`internal/mechd/httpapi.go`），没有任何防点击劫持、MIME 嗅探、Referrer 泄漏的响应头——管理界面可以被第三方页面用 `<iframe>` 嵌入。

修复是一个中间件（`API.withSecurityHeaders`），包在 API 路由与 Web UI 静态资源共用的最外层 handler 上，对**全部**响应生效，不分是哪条路径：

| 头 | 值 | 为什么 |
|---|---|---|
| `X-Frame-Options` | `DENY` | 老浏览器认的防嵌入机制 |
| `Content-Security-Policy` | 见下 | 现行标准，`frame-ancestors 'none'` 是明确要求的一条 |
| `X-Content-Type-Options` | `nosniff` | 防 MIME 嗅探 |
| `Referrer-Policy` | `same-origin` | 组件名、节点名都在 URL 里，不该当 Referer 泄漏给第三方 |
| `Strict-Transport-Security` | `max-age=31536000; includeSubDomains` | **只在 HTTPS 模式下发**（`API.EnableHSTS`，由 `serve.go` 按 `!--insecure-http` 设置）——明文模式下发这个头没有意义，浏览器规范本就无视非 HTTPS 连接上收到的它 |

CSP 是按这个 Web UI 的实际构建产物量身定的，不是抄一份通用模板：

```
default-src 'self';
script-src 'self' 'wasm-unsafe-eval';
style-src 'self' 'unsafe-inline';
img-src 'self' data:;
connect-src 'self'; font-src 'self';
frame-ancestors 'none';
base-uri 'self'; form-action 'self'
```

三条不是显而易见、需要专门记下来的：

- **`'wasm-unsafe-eval'`**：登录页的 PoW 求解在浏览器里靠 WebAssembly 算 Argon2id（[23-web-ui §3](23-web-ui.md)）。第一版 CSP 没给这条——**用真实 headless 浏览器跑一遍登录页才发现的**：PoW 卡在 0% 不动，登录按钮永远点不开，控制台报的正是 `WebAssembly.compile()` 被 CSP 拦下。不给更宽的 `'unsafe-eval'`：那会连 `eval()`/`Function()` 都放开，攻击面比编译一个 WASM 模块大得多。
- **`style-src 'unsafe-inline'`**：Vue 的 `:style` 绑定与 Element Plus 组件大量依赖内联 `style` 属性；CSP 的 `style-src` 同时管 `<style>` 标签与内联属性，禁掉后者会让整个界面掉样式。
- **`img-src data:`**：SliderCaptcha 的背景图与拼图块是服务端生成、以 `data:` URI 下发的（不额外开一个图片接口），没有这条就是一张裂图。

**这条 CSP 是拿真实构建产物在真实浏览器里跑过的，不是纸面推导**——上面 `wasm-unsafe-eval` 那条本身就是这样验证出来的，记录下来提醒以后改 CSP 时同样要过这一关，不能只看 curl 的响应头对不对。

### 3.3 初始凭据

`0.0.0.0` 监听意味着**认证不能是可选的**。mechd 首次启动时生成一个初始 admin token 并**只打印一次**：

```
mechd 首次启动完成。

  初始 admin token: m7n_a1b2c3d4e5f6...
  这个 token 只显示这一次，请立即保存。

  mechctl context set local --server https://<host>:8443 --token m7n_a1b2c3...
```

**这个 token 身兼两职。** 它是 `mechctl`/脚本的 Bearer 凭据，**同时也是
Web UI 首次初始化的门禁**——`POST /auth/bootstrap` 必须带上它才能设定
管理员口令（[ADR-0039](../adr/0039-bootstrap-token-gate.md)），取代了早先接了一半的 PoW/滑块。两者共用同一个值不是巧合：
它们要证明的是同一件事——「知道它，就说明是刚装完这台机器、看得到
它输出或读得到 `admin.token` 文件的人」。

### 3.4 mechctl ↔ mechlet（`--local`）

`/run/mecharion/mechlet.sock`，权限 `0600 root:root`。**只读**——它是 mechd 不可达时的现场诊断入口，不接受任何写操作（[ADR-0026](../adr/0026-standalone-runs-mechd.md)）。

### 3.5 不创建专用用户或组

`mechlet` 必须以 root 运行——它要管理 systemd unit、创建系统用户、写 `/etc`、挂载文件系统，无法降权。

因此 socket 权限是 **`0600 root:root`**，`mechctl` 需要 root。**不创建 `mecharion` 用户或组。**

理由来自 Docker 的教训：`/var/run/docker.sock` 是 `0660 root:docker`，而加入 docker 组**等价于拿到 root**（能挂载宿主根文件系统）。Mecharion 的 socket 更甚——部署一个 Pack ≡ 在本机执行 root 代码（§1）。

> **与其建一个营造「受限权限」假象的组，不如诚实地要求 root。**

`mechd` 本身不碰系统（只读写自己的数据目录与监听端口），**将来可降权运行**；v1 为简单起见同样以 root 运行。

> 注意区分：Pack 里的 `user: nginx` 是**组件的运行用户**，那是必须创建的（`user` 资源类型）。这里说的只是 Mecharion 自身不需要专用账户。

### 3.6 bootstrap 阶段的 SSH

SSH 只在 `mechctl node bootstrap` 时使用一次，之后不再使用。这是 Agent 模式相对 Ansible 的安全收益：**长期暴露面从「SSH 常开」降为「零入站端口」**。

## 4. 权限模型

- RBAC 授权到 **Site 级**（见 [02-object-model.md](02-object-model.md#site站点)）
- 角色划分：`viewer` / `operator`（可 apply/rollout）/ `admin`（可管 Node 与 RBAC）
- Pack 发布权限与部署权限分离——能推 Pack 的人和能部署到生产 Site 的人可以不是同一批

## 5. 敏感信息

### 5.1 一条能守住的不变式

先说一条**做不到**的：「密钥从不出现在任何渲染产物中」。应用要从配置文件里
读口令，那个文件就必然是明文——这不是设计缺陷，是所有同类系统的共同结局
（Cloudera Manager 渲染进 `hive-site.xml`、Ansible Vault 解密后落目标机、
Kubernetes 把 Secret 挂成 tmpfs 明文）。**加密解决的是静态存储与传输，
消费点必然落明文。**

能守住的是这一条：

> **密钥只出现在最终消费点的那个文件里；不进 spec、不进 generation 归档、
> 不进审计、日志、UI、diff。**

它挡住的正是绝大多数真实泄漏途径：备份、支持包、误配的同步、翻历史记录。

### 5.2 实现方式：spec 里放引用，不放值

mechd 渲染时遇到敏感值**不内联**，而是放一个不透明引用；spec 另带
`secretRefs`（**只有 id 与版本号，没有值**）：

```
mechd 渲染 → @@m7n:secret:<id>@@ + secretRefs: [{ id, version }]
mechlet    → 写盘的最后一刻做字面量替换
```

三点收益：

| | |
|---|---|
| **归档天然干净** | 旧口令不会永久躺在历史 generation 的 spec 里——否则「轮换」等于没轮换 |
| **不需要七处脱敏** | 否则 mechd 的 generation 历史、mechlet 的 spec 归档、审计、事件流、`config diff`、API、UI **每一处都要各写一次**，漏一处要等出事才发现 |
| **轮换语义正确** | digest 覆盖 `secretRefs`，version 变 → digest 变 → 新 generation → 重渲染 → 重启。配置文件确实变了，重启是应当的 |

替换机制与 `{{ .Paths.Generation }}` 是同一套（字面量替换，非第二个渲染阶段）。

### 5.3 hook 拿密钥走文件，不走命令行与环境变量

命令行会出现在同机任何用户的 `ps` 输出里；环境变量会出现在
`/proc/<pid>/environ` 与崩溃转储里。引擎把值落到 `0600` 的临时文件，
以 `MECHARION_PARAM_FILE_<NAME>` 传路径，hook 结束即删。

### 5.4 跨组件传递

见 [pack-v1 §5.4](../spec/pack-v1.md#54-exportsfields--具名字段与凭据)。三条约束：

- 提供方**显式导出**具名字段，消费方**不得**读提供方的参数（规则 49）
- 被消费的凭据应当是**专为该依赖关系存在的账号**，不是 superuser
- 敏感标记由 mechd 在绑定时**自动传播**——不依赖消费方 Pack 声明得对

### 5.5 mechd 侧的静态存储

**默认信封加密**（可关，关掉即明文 + `0600`）：

```
/etc/mecharion/secret.key                0400 root，主密钥（KEK）
/var/lib/mecharion/mechd/mechd.db        被 KEK 包裹的数据密钥 + 密文值
```

两层而非直接用主密钥加密每个值，是为了让**轮换主密钥**只需重新包裹几十字节，
以及将来接 KMS 时**不需要迁移数据**。实现用 Go 标准库的 AES-256-GCM，无 CGO。

**说实话它挡什么：**

| 场景 | |
|---|---|
| DB 被单独拷走（备份、快照、支持包、误配同步） | ✅ 前提是主密钥没跟着一起拷 |
| 整机备份 / 整盘镜像 | ❌ 两个文件都在里面 |
| mechd 主机被拿到 root | ❌ |

它把「偷一个文件」变成「必须偷两个，而其中一个通常不在备份范围里」——
有边界的收益，不是万灵药。

**代价必须写清**：主密钥丢失时，`generate` 生成的口令可重新生成并重新部署，
**运维手填的口令则真的没了**。mechd 首次生成密钥时明确提示单独备份；
密钥缺失但库中有密文时**拒绝启动并说清原因**，不静默当空值。

> 没选「开机让运维输入口令」是因为它直接否掉无人值守；没选 TPM 封装是因为
> 换机器或恢复备份就解不开，而大量边缘设备与虚机没有 TPM。

### 5.6 其它

- params 标记 `sensitive: true` 的值在**日志、事件、UI、API 响应**中一律脱敏
- 渲染后落盘的配置文件按最小权限设置 mode/owner；含敏感值的文件 others 位必须为 0（[规则 46](../spec/pack-v1.md#资源与-hooks)）
- v1 不内置密钥管理系统，也**不依赖任何外部密钥服务**——这是边缘离线场景的硬要求。对接 Vault / KMS 只留接口

## 6. 操作日志（不是合规级审计）

记录：

```
谁（actor） · 何时 · 对哪个 Site 的哪个 Component
· 把哪个 Pack 的哪个 version+revision · 应用到哪些目标 · 结果如何
```

写在 mechd 的 SQLite 里，与事件表分开存储（见 [07-persistence.md](07-persistence.md#14-工程约定)）。

**如实说清楚它现在是什么、不是什么**——这里此前写的"全量记录，不可绕过"过于乐观，更正：

- **写失败是 best-effort**：一条记录写不进去（比如磁盘满），触发它的那个动作照常成功，不会因为记不下日志就让部署/移除本身失败。代价是"全量记录"并不成立：极端情况下会有记录缺失，而调用方看不到、也不会收到任何提示。
- **actor 目前只有一种粒度**：单账号模型下（[ADR-0037](../adr/0037-login-is-full-privilege.md)）记的是 `admin` 或某个 token 字符串，不区分到具体的人；RBAC 落地前，"记到人"做不到。
- **还没有查询入口**：数据确实落了库，但目前没有任何 `mechctl` 命令或 HTTP API 能把它读出来——"事后可查"现在只能靠运维直接打开 SQLite 文件查，不是一个产品化的能力。
- **没有保留策略**：表会无限增长，没有清理或归档机制。

现在更准确的定位是**操作日志**：留痕、排障时能看，但不满足"审计"这个词通常暗示的完整性、可查询性、防篡改与保留策略。真要做到那些是一次独立的工程，不是现在就有的能力。

## 7. 容器场景的隔离规则

当 mechlet 使用**用户既有的 docker**（`managed: false`）时，一条硬规则防止误伤：

> **mechlet 只操作带 `dev.mecharion.*` 标签的容器。**

漂移检测、`Observe`、清理全部按标签过滤。没有这条，一次误清理就能删掉用户与 Mecharion 无关的生产容器。

## 8. 非目标

| 不做 | 原因 |
|---|---|
| Pack 执行沙箱 | 与「必须以 root 完成系统级配置」根本矛盾，虚假的安全感比没有更危险 |
| 内置密钥管理服务 | 应对接既有 KMS/Vault，不重复造 |
| 网络策略 / 微隔离 | 超出 ALM 范畴 |
| 任何形式的遥测/使用数据外发 | 与「离线优先」（[ADR-0015](../adr/0015-offline-first-hermetic.md)）矛盾——边缘现场可能完全没有出网能力，默默拨号回家在那种环境下是一个真实的失败模式，不是隐私姿态问题。全仓库搜不到任何遥测/分析 SDK 或外发调用，默认零外发遥测——这不是承诺要做到，是已经如此，写在这里防止以后有人"顺手"加一个 |
