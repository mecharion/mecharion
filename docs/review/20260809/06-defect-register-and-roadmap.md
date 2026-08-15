# 缺陷台账与整改路线图

## 1. 分级规则

| 级别 | 定义 |
|---|---|
| **P0** | 可能破坏主机文件/身份边界或核心状态可信度；任何公开发布前必须关闭 |
| **P1** | 会让主要流程、安全承诺或自动化契约失真；公开 alpha 前应关闭 |
| **P2** | 影响规模化、可维护性、社区采用或 beta 质量；应进入明确里程碑 |
| **P3** | 优化项或远期能力；在不打断主线的前提下治理 |

本台账来自静态分析，没有运行 exploit、服务或测试。P0/P1 表示代码路径和不变式风险足够明确，不等于本次已动态复现事故。

## 2. P0 缺陷

### REV-001：用户标识符未校验，进入高权限文件与受管路径

- **领域**：安全、资源引擎、命名
- **证据**：`internal/mechd/service.go:197-203`、`:823-828`；`internal/render/paths.go:16-21`；`internal/spec/spec.go:325-326`；`internal/agent/desired.go:214-224`；`internal/pki/pki.go:333-372`。
- **问题**：Component/Node/Site/ConfigGroup 没有统一合法字符和长度约束。Component 可进入 desired 文件名和 `/.../apps/<component>` 默认路径；Node 可进入离线证书文件名。注释假设 InstanceKey 无斜杠，但入口没有保证。
- **风险**：目录逃逸、错误位置的 root 文件写入/调和/移除、不可寻址资源、unit/container identity 冲突。
- **修复**：建立领域 Identifier 类型；所有入口校验；危险文件 sink 做 containment check；数据库增加 CHECK/迁移清理策略。
- **验收**：针对空白、`..`、正反斜杠、绝对路径、URL 保留字符、Unicode 混淆、超长输入做表驱动和 API 测试；任何非法名字在落库/签证/渲染前返回稳定 `invalid_identifier`；文件路径测试证明结果位于预期根目录内。

### REV-002：Join 读后写竞态可签出重复节点身份，token 使用上限可突破

- **领域**：身份、并发、数据库
- **证据**：`internal/mechd/join.go:184-242`；`internal/store/queries/expected.sql:219-220`。
- **问题**：token 可用性、节点存在性、证书签发、Node Upsert、used++ 分离；Use 没有条件，失败仅 warning。
- **风险**：并发请求取得同 CN 的多张有效证书；同 token 超额使用；计数失败后 token 继续可用。
- **修复**：条件消耗 token + INSERT/claim node 的原子事务；禁止身份 Upsert；签发使用可补偿 reservation 状态。
- **验收**：`-race` 下并发 50 次同 token/同 node 只有一次成功；maxUses 在不同 node 并发下严格不超限；数据库写故障不返回成功证书；重复证书身份不可出现。

## 3. P1 缺陷

### REV-003：Deploy 不是业务原子操作

- **证据**：`internal/mechd/service.go:239-265`、`internal/mechd/resolve.go:268-288`、`:394-401`、`internal/store/store.go:220-224`。
- **问题**：Component、实例、事实、binding 在渲染完成前分步提交。
- **修复/验收**：预计算无副作用；一个 Unit of Work 提交；在每个写入/渲染阶段注入失败，数据库快照必须与调用前完全一致。

### REV-004：未认证 JSON 请求体大小和读取时间无界

- **证据**：`internal/mechd/httpapi.go:680-687`；`internal/cli/mechdcmd/serve.go:187-198`。
- **问题**：除 Pack upload 外无 body limit；只有 header timeout；不验证 content type/EOF。
- **修复/验收**：每类 route 有明确上限和 413；普通 body 有 deadline；第二个 JSON 值与错误 content type 被拒；慢体测试不会长期占用 handler。

### REV-005：Challenge 签发限流无效

- **证据**：`internal/mechd/authapi.go:33-45`；`internal/authn/ratelimit.go:63-102`。
- **问题**：Issue 只 Check，不记录请求；无限成功 GET 不进入 limiter bucket。
- **修复/验收**：独立请求速率/并发 limiter，含单 IP 和全局限额；突发请求稳定返回 429，不继续执行 Argon2/图片生成；内存中的 outstanding challenge 有界。

### REV-006：首次初始化挑战是无效仪式

- **证据**：`webui/src/views/Setup.vue:18-38`、`:75-99`；`internal/mechd/userapi.go:45-61`。
- **问题**：UI 要求挑战和 PoW，POST 只含 password，服务端不验证也不限流。
- **修复/验收**：用新 ADR 选择“共享 challenge/limiter”或“loopback/一次性 bootstrap token”；UI 与服务端使用同一模型；绕过 UI 不能绕过防护。

### REV-007：预登记 Node 无法 Join

- **证据**：`internal/mechd/service.go:823-843`；`internal/mechd/join.go:215-223`；`docs/design/25-roadmap.md:271`。
- **问题**：`node add` 产生 pending 行，Join 拒绝所有已存在节点。
- **修复/验收**：定义 reserved→joined 状态机；绑名 token 只能 claim 相同 pending node；active/revoked/已有 identity 仍拒绝；预置批量节点 E2E 成功。

### REV-008：HTTP 500 原样泄漏内部错误，状态分类依赖中文文案

- **证据**：`internal/mechd/httpapi.go:698-743`。
- **问题**：内部路径/SQL/配置可能返回给客户端，文案子串参与控制流。
- **修复/验收**：typed error + stable code + request ID；500 响应不含内部 error，日志保留完整 cause；改变中文消息不改变 HTTP status。

### REV-009：单机模型、mechd 和 `--local` 的产品契约互相冲突

- **证据**：`README.md:79-84`、`cmd/mechctl/main.go:1-24`、`cmd/mechd/main.go:1-24`、`cmd/mechlet/main.go:24` 对比 ADR-0026、`docs/design/01-architecture.md:30`；代码无 `--local`。
- **问题**：用户被告知可在不存在的通路上操作，且不知道单机是否依赖 mechd。
- **修复/验收**：以 ADR-0026 为当前真相；未实现前从 README/help 删除能力宣称；若实现，只读命令和故障模式有验收测试；CLI help 与 docs 自动比对。

### REV-010：Pack 签名被定义为强制信任锚，但产品没有实现

- **证据**：ADR-0016、`docs/design/08-security.md:13`；`cmd/mechpack/main.go:37` 明确 sign 未完成。
- **问题**：Pack 可执行 root 级 hook/资源，lint 不是来源认证；文档却描述强制签名模型。
- **修复/验收**：实现 sign/verify、可信 key/publisher、吊销/轮换和服务端强制策略；或写新 ADR 把 alpha 限定为“管理员本地审核 Pack”，删除来源可信声明。首发不能处于“设计说必需、代码全接受”的中间态。

### REV-011：mechctl 输出 flag 冲突，错误格式静默退回文本

- **证据**：`internal/cli/root.go:64-77`；`internal/cli/ctlcmd/component.go:18-38`。
- **问题**：根 `table/json/yaml` 与名词 `text/json` 各持一份值，子 flag 遮蔽根验证。
- **修复/验收**：单一全局输出 flag；table/json/yaml 都真实实现；未知值非零退出；所有只读命令的 JSON/YAML snapshot 测试通过。

### REV-012：SliderCaptcha 阻断键盘和屏幕阅读器用户

- **证据**：`webui/src/components/SliderCaptcha.vue:63-80`。
- **问题**：普通 div、pointer-only，无焦点、键盘、ARIA 或替代方式；认证入口不可访问。
- **修复/验收**：可聚焦 slider，方向键/Home/End，完整 ARIA，非视觉替代；组件级键盘测试与 axe/等价检查无严重违规。

### REV-013：管理 UI 缺少基础安全响应头

- **证据**：`internal/webui/webui.go` 和 `internal/mechd/httpapi.go` 只设置内容/缓存头。
- **问题**：无 anti-framing、nosniff、referrer policy 等；管理界面可被 iframe 利用。
- **修复/验收**：统一 security-header middleware；至少 `frame-ancestors 'none'`、nosniff、Referrer-Policy；CSP 在生产构建上验证；HTTPS 模式评估并启用 HSTS。

## 4. P2/P3 台账

| ID | 级别 | 领域 | 缺陷 | 建议完成条件 |
|---|:---:|---|---|---|
| REV-014 | P2 | API | 无 OpenAPI/schema、稳定 error code、兼容策略；Go/TS/CLI 三份手写 DTO | 建立 contract 真源和 breaking-change check，CLI/TS 至少一侧生成 |
| REV-015 | P2 | CLI/API/UI | path/query 使用字符串拼接，缺少统一 URL 转义 | 所有调用经 path segment/query builder，特殊字符测试覆盖 |
| REV-016 | P2 | API | 主要列表无分页；更新无 revision/ETag；长操作无幂等键 | 游标/上限、If-Match/resourceVersion、关键 POST 幂等策略 |
| REV-017 | P2 | 审计 | 写失败 best effort；actor 只有 token/admin；无查询/保留/防篡改 | 明确称“日志”或升级为事务审计；token subject 可区分；有导出/保留策略 |
| REV-018 | P2 | 架构 | `internal/mechd` 9k 行；ctlcmd 直接依赖 mechd 实现类型 | 抽 control API contract；按 Component/Join/Rollout 用例拆分；依赖方向测试 |
| REV-019 | P2 | 前端 | 仅 4 个逻辑测试文件，无页面交互、lint、无障碍和 visual regression | Login/Setup/Deploy/Remove 组件测试；ESLint/a11y lint；关键视觉基线 |
| REV-020 | P2 | 静态质量 | 无 staticcheck、漏洞/secret/许可证扫描、fuzz | CI 加最小可维护工具集；解析器和 archive 有 fuzz corpus |
| REV-021 | P2 | 文档 | 5 个失效 ADR 链接、10/12 示例、CLI/入口注释漂移、CLI 章节重复 | 链接/文档 lint 进入 CI；当前能力清单与 help 自动核对 |
| REV-022 | P2 | 上手体验 | README quickstart 使用不可 assemble 的占位示例 | 从干净环境执行真实 `.mpack` 的 15 分钟 quickstart 并进 CI |
| REV-023 | P2 | 发布 | 0 tag；无 release workflow、checksum、签名、SBOM、provenance、安装包 | alpha 发布制品可复现、可校验、可安装/卸载/升级 |
| REV-024 | P2 | 开源 | 缺 CONTRIBUTING、SECURITY、CoC、模板、CODEOWNERS、版本政策 | 最小社区文件齐全，安全披露地址有效，PR 流程可执行 |
| REV-025 | P2 | CI | 只跑 Go 1.26；Actions 未 pin SHA；`sqlc@latest`；覆盖无阈值/趋势 | 最低 Go + 当前 Go 矩阵；工具 pin；依赖安全更新；覆盖趋势可见 |
| REV-026 | P2 | 文档契约 | pack/v1 同时称“已冻结”和“v0.1 前可调整” | alpha 前统一 draft-stable；v0.1 后用兼容 fixture 真正冻结 |
| REV-033 | P2 | 可观测性 | `/healthz` 无条件 ok；无 readiness、metrics、request ID、诊断包 | 分离 live/ready；关键控制面与存储指标；脱敏离线诊断包；默认零外发遥测 |
| REV-034 | P2 | 升级/迁移 | 启动自动 Up migration，但缺少产品化备份、Schema 兼容和降级演练 | release acceptance 从旧 DB 升级；升级前备份；失败恢复/不可回退规则写入文档 |
| REV-027 | P3 | 文档 | 超长 design 混入实施日记和未来愿景 | User/Reference/Architecture/History 分层，当前与 planned 标签化 |
| REV-028 | P3 | Git | 4 个超大提交，不利 bisect 和外部审阅 | 以后按可独立验证能力提交，生成/机械修改与语义修改分开 |
| REV-029 | P3 | UI | 无 Site 上下文、事件/审计视图、搜索分页、404、title、i18n/品牌系统 | 先完成高密度运维 IA，再定品牌视觉和英文路径 |
| REV-030 | P3 | 容量 | “数百节点”无负载基线、DB/事件/blob 保留模型、RTO/RPO | 形成容量模型、压测基线、备份恢复演练与增长告警 |
| REV-031 | P3 | 代码 | 大量里程碑/实现状态注释已陈旧 | 源码只保留不变式，状态转 issue/roadmap；入口注释复核 |
| REV-032 | P3 | 开发命令 | `make check` 不包含 tidy/generated/web/pack，却称全套 | 拆 `check-fast/check-web/check-all`，CI 与本地复用 |

## 5. 推荐整改顺序

### 阶段 A：保护主机、身份与状态

只处理边界，不增加新产品功能：

1. REV-001 Identifier 与 path containment；
2. REV-002 Join 原子 claim/consume；
3. REV-003 Deploy Unit of Work；
4. REV-004/005/006 HTTP、Challenge、Bootstrap 资源边界；
5. REV-007 pending Node claim；
6. REV-008 typed errors 与 500 隐藏。

阶段完成标志：故障注入和并发测试证明关键状态全成/全败；所有未认证入口有大小、速率和时间边界；任何名称不能改变根目录或身份命名空间。

### 阶段 B：统一产品契约

1. REV-009 统一单机/mechd/`--local` 事实；
2. REV-011 统一 CLI flags/output；
3. REV-014/015 建 API contract、path builder 和稳定错误码；
4. REV-012/013 修认证可访问性和安全头；
5. REV-010 决定并实现 alpha 的 Pack 信任策略；
6. 修所有文档事实与陈旧注释。

阶段完成标志：README、`--help`、OpenAPI/schema、UI 调用和实现对同一能力给出相同答案；不存在“文档已实现、代码没有”的主流程。

### 阶段 C：做出可验证的公开 alpha

1. 真实 quickstart Pack 与 clean-machine acceptance；
2. CONTRIBUTING、SECURITY、CoC、Issue/PR 模板、CODEOWNERS；
3. staticcheck、漏洞/secret/link/frontend lint；
4. 版本、CHANGELOG、release notes、checksum、签名、SBOM/provenance；
5. 备份/恢复、证书、升级和故障排查文档。

阶段完成标志：一个未参与开发的人只看发布文档，可以从制品完成安装、部署、升级、回滚、备份恢复与卸载，并知道如何报告安全问题。

### 阶段 D：beta 前的规模与多人协作

分页、revision/幂等、审计主体、事件保留、容量基线、代理部署、前端高密度信息架构应在这一阶段完成。RBAC 和 HA 只有在出现真实用户/采购条件后再立项；不要让它们抢占当前边界收口工作。

## 6. 建议发布门槛

### `v0.1.0-alpha.1`

- REV-001～013 全部关闭或由新 ADR 明确缩小范围，P0 不允许豁免；
- 静态检查、单元、三平台、systemd、三节点和 Web 关键流程均通过；
- 真实 quickstart 从发布制品通过；
- 没有 README/帮助宣称的未实现主流程；
- 有 SECURITY、贡献说明、已知限制和升级/卸载路径；
- 发布制品有 checksum、SBOM 和来源签名。

### `v0.1.0-beta.1`

- API contract、稳定 error code、分页和乐观并发落地；
- 备份恢复与控制面/agent 升级演练通过；
- 前端认证、危险操作、无障碍关键路径自动化；
- Pack 签名与信任模型完成，或正式限定只支持本地受信 Pack；
- 有可复现的小规模容量基线和资源上限。

### 首次“可用于生产”声明

不能只以功能完成为判据。至少还需要独立安全评审、持续版本支持政策、故障与恢复演练、真实长稳运行、依赖漏洞响应流程、可观测性和清晰支持边界。控制面 HA 未必是所有边缘生产场景的硬条件，但可验证的备份、恢复时间和控制面中断行为是硬条件。

## 7. 最终建议

Mecharion 已有足够强的概念和实现基础进入收口阶段。下一阶段最有价值的产出不是第 33 个命令，而是让以下四句话同时为真：

1. 一个名字不会让 root agent 操作错误路径；
2. 一次 Join 或 Deploy 要么完整成功，要么不改变权威状态；
3. README、CLI、API 和 UI 描述的是同一个当前产品；
4. 外部用户能从受信发布制品完成一条真实、可恢复的生命周期。

达到这四点后，项目才真正从“完成一轮核心功能开发”跨入“可由社区验证的产品”。
