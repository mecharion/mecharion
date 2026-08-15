# 编码质量、安全与静态缺陷审阅

## 1. 编码质量总评

Go 代码总体高于同阶段项目平均水平：命名清楚、错误普遍带上下文、context 传递完整、排序与稳定输出受到重视、平台差异使用 build tags 隔离，生产代码中未见随意 panic。大量注释解释“不这样做会怎样”，显示出对故障语义的主动设计。

但 AI Agent 主导开发的典型痕迹也很明显：

- 注释密度高且经常写入里程碑、旧决策和实现状态，部分已与代码不符；
- 单次大提交和超长文件使审阅粒度过大；
- 局部代码很严谨，但跨函数/跨 Repository 的整体不变式没有同样严谨的边界；
- 为了快速闭环，HTTP、领域服务和客户端共享实现类型，形成后续契约债务；
- “已经在文档说明”有时替代了代码入口处的强制校验。

建议的治理原则是：**注释解释不变式，测试固定不变式，类型和事务执行不变式。** “还没做什么”“M8/M9 为什么”应移到 issue/roadmap，不应长期留在入口源码。

## 2. 高风险静态发现

### 2.1 用户标识符未统一验证，可能逃逸文件与受管路径

严重度：**P0 / 发布阻断**

证据链：

1. `internal/mechd/service.go:197-203` 直接采用请求里的 Component 名；为空时才回退到已 lint 的 Pack 名，没有格式校验。
2. Pack 名和 Role 名使用 DNS label lint，但 Component、Node、Site、ConfigGroup 没有共享的领域级 validator。`AddNode` 在 `service.go:823-828` 只检查非空。
3. 默认路径在 `internal/render/paths.go:16-21` 把 `.Component` 插入 `/opt|etc|var/.../apps/{{ .Component }}`。
4. `ResolvedSpec.InstanceKey()` 在 `internal/spec/spec.go:325-326` 返回 `<component>__<role>`。
5. agent 在 `internal/agent/desired.go:214-224` 明确假设 InstanceKey “不含斜杠”，并直接 `filepath.Join(s.dir, key+".json")`。这个假设没有由来源端保证。
6. 离线证书签发在 `internal/pki/pki.go:333-372` 只检查 Node 非空，随后把 Node 拼入 `nodes/<node>.crt|key`。

因此带有 `/`、`..`、查询保留字符或平台分隔符的名字可导致：

- desired/applied 状态文件写到预期目录之外；
- 默认受管路径归一化后落到 `apps` 根之外，后续调和或 remove 作用于错误位置；
- `mechd ca issue` 在高权限上下文中把证书写出 `pki/nodes`；
- 已落库对象无法通过 REST path 或 CLI 正常寻址；
- systemd unit、容器标签、实例键发生冲突或产生非法值。

这里的主要威胁不是“未认证远程提权”——Component 写入当前需要 admin，而 admin/Pack 本就能要求 root 级资源动作——而是**破坏产品承诺的受管路径边界，并把输入错误升级成高权限文件操作或误删事故**。未来加入 RBAC 后还会成为权限边界问题。

整改：建立单一 `identifier` 包，至少使用小写 DNS label 子集并限制 63 字符；Site/Component/Node/Role/ConfigGroup 分别定义类型。所有 HTTP、apply 文档、CLI、Join、PKI 和存储写入入口校验；PKI、agent 文件存储等危险 sink 仍应做防御性校验和“结果仍在根目录内”的 containment check。为 `/`、`..`、反斜杠、Unicode 变体、超长、空白和 URL 保留字符增加回归测试。

### 2.2 Join 不是原子状态转换，可产生重复身份和超额使用

严重度：**P0 / 发布阻断**

`internal/mechd/join.go:184-242` 的顺序是：

1. 按 hash 读 token；
2. 在内存检查 expires/revoked/used；
3. 查询节点名是否存在；
4. 签 CSR；
5. `Nodes().Upsert`；
6. `JoinTokens().Use` 增加 used；失败只记录 warning，Join 仍成功。

数据库的 Use 只是 `UPDATE join_tokens SET used = used + 1 WHERE id = ?`，没有 `used < max_uses`、未过期、未吊销的条件。

两个并发 Join 可以同时看到节点不存在、token 可用，各自拿到相同 CN 的有效证书，之后 Upsert 同一节点行。两个证书都会代表同一个受信身份。不同节点并发时也可突破 max uses；计数写失败还会留下可继续复用的 token。

整改应把 Join 建模为条件状态机，而不是读后写：

- 单事务执行条件 token 消耗：未过期、未吊销、`used < max_uses`；使用 `UPDATE ... WHERE ... RETURNING`；
- 节点身份用 INSERT/claim，不用 Upsert；唯一冲突即失败；
- 允许 claim 明确的 pending 预注册节点，但不能覆盖已加入/已上报节点；
- cert 签发与数据库无法处于同一事务，应先原子预留 identity/token use，再签发；签发失败执行可审计补偿或让 reservation 可重试；
- 加真实并发测试：同 token、同 node，不同 token、同 node，同 token、不同 node。

### 2.3 Deploy 业务动作非原子，失败会留下半份期望状态

严重度：**P1**

`Service.Deploy` 在 `internal/mechd/service.go:239-265` 中先持久化 Component 和实例、再冻结事实、最后渲染。渲染期间 `resolveBindings` 还可能创建依赖绑定。任何后续失败都不会回滚先前写入。

`ensureInstances` 又对每个新增实例单独调用一个事务，并逐条删除移除实例。一批操作中第 N 项失败，会留下前 N 项已提交。

这与 `internal/store/store.go:222-224` 的注释“不允许留下半份状态”直接矛盾，也会让失败的 deploy 改变后续 placement/ordinal/binding 行为。

整改：渲染和计划阶段禁止持久化；用临时 ordinal/binding 预览完成全部校验。提交时在一个 Unit of Work 中写 Component、实例、事实、binding 和 rollout 初态，并通过 revision 检查预计算依据。提交成功后才发事件和通知节点。

### 2.4 Challenge 签发没有实际限流

严重度：**P1**

`internal/mechd/authapi.go:33-45` 注释称每道挑战需要 Argon2 和两张图片，因此签发也限流；代码只调用 `Limiter.Check`。而 `Limiter.Check` 只读取失败桶，只有 login 验证失败时的 `Limiter.Fail` 才创建/增加桶。

连续请求 `GET /auth/challenge` 永远不会被记为失败，所以攻击者可以无限触发昂贵的未认证工作。当前 limiter 是“登录失败锁定器”，不是“请求速率限制器”。

整改：挑战签发使用独立 token bucket/sliding window，按 IP 和全局两级限速，限制并发 Issue；挑战池设置总数量、过期回收和每 IP outstanding 上限。反向代理部署时必须有显式 trusted proxy 配置，否则不可直接信任 `X-Forwarded-For`。

### 2.5 普通 HTTP 请求体与读取时间无界

严重度：**P1**

只有 Pack upload 使用 `MaxBytesReader`。其余 JSON handler 直接 decode body；server 在 `internal/cli/mechdcmd/serve.go:187-198` 只设置 `ReadHeaderTimeout`。这不能限制慢速 body、超大 JSON 字符串或长时间占用连接，且未认证的 login/bootstrap/join 同样暴露。

整改见 API 报告：按 route 限大小，检查 content type 与 EOF，为普通请求设置 body read deadline/timeout；SSE 和上传走明确例外。还应配置合理 `IdleTimeout`、`MaxHeaderBytes` 和连接/并发上限。

### 2.6 初始化挑战没有进入服务端安全决策

严重度：**P1**

Setup 页面计算挑战但只提交 password；`bootstrapAdmin` 只解析 PasswordBody，也不调用 limiter。该问题同时造成：

- 初始化接口未获得设计文档承诺的成本限制；
- 用户必须完成一段对服务端没有意义的验证码；
- 挑战接口的 CPU 风险被初始化页额外放大。

应与产品设计一起重新选择 bootstrap 防护机制，不应只把 challenge 字段机械加进请求。

### 2.7 `node add` 与 Join 生命周期断裂

严重度：**P1**

`node add` 创建 pending Node，但 Join 在 `internal/mechd/join.go:215-223` 拒绝任何已存在名称。因此“先登记/批量预置，再让节点 Join”的自然流程无法完成。项目自己已在 `docs/design/25-roadmap.md:271` 记录这一缺口。

整改：定义明确状态转换 `reserved/pending → joined/online`。绑名 token 可以 claim 同 Site、同名、尚无证书身份/首次上报的 reserved node；不能 claim active、revoked 或已有 identity 的 node。节点 Create 与 Upsert 语义应分开。

### 2.8 内部错误原样返回给客户端

严重度：**P1**

`writeErr` 在 `internal/mechd/httpapi.go:698-699` 无论状态码都发送 `err.Error()`。数据库、文件系统、Pack 目录、证书和配置错误可能泄漏内部路径与实现细节。

`statusFor` 又在 `httpapi.go:717-743` 依赖中文子串判断用户错误，文案变化可能把内部错误标成 400，或把输入错误标成 500。

整改：使用 typed domain errors 和稳定 code；500 只向客户端返回通用消息，请求 ID 对应服务端结构化日志。不要用本地化文案参与控制流。

## 3. 中风险安全与可靠性发现

### 3.1 Web 安全头缺失

严重度：**P2**

未设置 CSP/frame-ancestors、nosniff、Referrer-Policy、Permissions-Policy 和 HSTS。管理 UI 应至少禁止 framing，降低 clickjacking；HSTS 只在确认全站 HTTPS 和证书运维流程后启用。

### 3.2 审计不是可靠账本

严重度：**P2**

`internal/mechd/resolve.go:588-602` 的审计写入失败仅 warning，不影响动作；事件同样 best effort。Bearer token actor 固定为 `token`，browser 固定为 `admin`，无法区分自动化主体。没有 tamper evidence、保留策略、导出或 UI 查询。

对早期单管理员系统这可以叫“操作记录”，不应称合规级审计。若将来审计是硬要求，应采用同事务 outbox/审计行，至少保证状态改变与审计记录同成败；为 token 命名并记录 token ID/subject。

### 3.3 API 没有乐观并发与幂等语义

严重度：**P2**

并行 SetParams、Group、Deploy/Upgrade 可以相互覆盖；客户端超时后重试长操作也没有 idempotency key。当前单 admin 减少了概率，但 UI 轮询和 CLI 自动化已足以产生并发。

### 3.4 列表和历史增长边界不完整

严重度：**P2**

事件与 Rollout history 有 limit，主要资源列表没有分页；limit 也缺最大值钳制的统一契约。SQLite 数据库、事件、审计、状态与 blob 的保留/清理策略需在发布前文档化。

### 3.5 Pack 是 root 级代码，但信任边界尚未完成

严重度：**P1**

资源和 hook 被设计为可管理任意绝对路径与系统配置，这是产品能力，不是漏洞；因此 Pack 作者等价于节点 root 代码发布者。hermetic lint 只能减少无意外部依赖，安全 archive 校验只能保护解包边界，二者都不能证明发布者身份或阻止恶意 hook。

ADR-0016 和安全设计已正确得出“签名必需”，但代码注释明确 `sign` 未做。公开接收第三方 Pack 前必须完成签名/信任策略，或明确只允许管理员审核后的本地 Pack，不能把 lint 描述为沙箱。

### 3.6 反向代理身份语义未定义

严重度：**P2**

`remoteIP` 不信任 X-Forwarded-For 能避免直接伪造，是安全默认；但部署到反向代理后，登录限流与 bootstrap 审计都会看到代理 IP。应明确“不支持代理”或配置可信代理网段与标准 Forwarded 解析，不能无条件信任 header。

## 4. 代码结构与可维护性发现

### 4.1 超长文件与控制面集中度

最长生产文件包括：

| 文件 | 约行数 |
|---|---:|
| `internal/mechd/service.go` | 1,349 |
| `internal/mechd/httpapi.go` | 1,142 |
| `internal/reconcile/reconcile.go` | 1,000 |
| `internal/cli/ctlcmd/component.go` | 912 |
| `internal/store/repo.go` | 870 |
| `internal/render/render.go` | 845 |

行数本身不是缺陷，问题是修改一个业务动作往往同时触及 service、HTTP、CLI、TS type 和设计文档。优先按用例边界拆分 mechd 和 API contract；reconcile/render 只在出现清晰子领域时拆，避免碎片化。

### 4.2 字符串状态与 ad-hoc map

状态值虽然不少已有常量，但在 DB、JSON、Vue 与错误分类中仍存在字符串复制；handler 也常用 `map[string]any` 组织响应。应逐步替换为具名 DTO 和类型化枚举，为 API 兼容检查提供抓手。

### 4.3 注释陈旧

入口注释包含已完成/未完成列表并已出现事实错误；设计文档章节号也大量写进代码。建议：

- 代码注释只保留本地不变式和非显然原因；
- 实现状态由 roadmap/issue 维护；
- 文档引用尽量指向 ADR 编号，不引用容易变化的行号和里程碑步骤；
- CI 加一个最小的 CLI help snapshot/文档生成流程，减少人工列表。

### 4.4 错误类型迁移未完成

`faults.Error` 是正确方向，但 status mapping 仍保留字符串搜索。应枚举 `invalid_argument/not_found/conflict/failed_precondition/unavailable/internal` 等领域类别，并分别映射 CLI exit、HTTP status 和 gRPC code。

## 5. 已做得好的安全设计

以下内容值得保留，不能因本报告列出风险而被忽略：

- 默认 HTTPS，TLS 最低版本明确；insecure flag 会警告；
- agent 私钥在节点生成，Join 发送 CSR，mTLS 以证书身份为准；
- token/hash 比较使用固定时间思路，明文 Join/Admin token 只返回一次；
- browser cookie HttpOnly、SameSite Strict，并用自定义 header 做第二层 CSRF 防护；
- 口令使用 Argon2 体系，secret 采用 envelope encryption，主密钥与数据库分离；
- secret 不进入普通 CLI 参数、响应和 hook 环境变量，而用 0600 临时文件；
- archive/mpack 有路径、链接、大小和内容相关安全校验；
- remove 默认保留数据，缩容和清理必须显式表达；
- Pack upload 已有总大小限制和流式处理；
- 远程地址 header 默认不受信，避免简单来源伪造。

## 6. 静态检查缺口清单

### 6.1 本次通过

- `gofmt -l`
- `go vet ./...`
- `go mod tidy -diff`
- proto 生成物一致性检查
- Vue TypeScript + Vite production build
- `git diff --check`

### 6.2 当前仓库未配置

| 类别 | 建议工具/检查 | 优先级 |
|---|---|---:|
| Go 深度静态分析 | `staticcheck` 或小而明确的 `golangci-lint` 集合 | P1 |
| 漏洞依赖 | `govulncheck ./...`，npm audit/OSV scanner | P1 |
| 安全模式 | gosec 作为提示源，人工确认，不盲目全开 | P2 |
| 前端 lint | ESLint + Vue/TypeScript 规则 | P1 |
| 无障碍 | eslint-plugin-vuejs-accessibility + 组件 axe 测试 | P1 |
| Markdown | markdownlint + 本地链接/anchor 检查 | P2 |
| Secret 扫描 | gitleaks 或等价工具 | P1 |
| 许可证 | Go/npm 依赖许可证清单与不兼容策略 | P2 |
| 供应链 | SBOM、checksum、制品签名、provenance | P2 |
| 复杂度 | 只监控新增高复杂函数，不对历史代码一刀切 | P3 |
| Fuzz | Pack/YAML、archive/mpack、template、JSON/protocol 解码器 | P2 |

仓库没有生产代码 TODO/FIXME 堆积，这是优点；同时未发现 fuzz target 或 benchmark。高风险输入解析器非常适合先补 fuzz，而性能 benchmark 可等容量目标确定后针对 render、placement、digest 和大 Pack 解包建立。
