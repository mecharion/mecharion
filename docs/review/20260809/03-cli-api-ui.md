# CLI、HTTP API 与 Web UI 审阅

## 1. CLI

### 1.1 整体评价

CLI 的基本结构是优雅的：对象多、同名动词多、删除风险高，采用 `mechctl <noun> <verb>` 能让命令自解释，也避免顶层 `remove/status/list` 歧义。`mechpack assemble` 刻意不用 `build`，准确表达“不构建软件，只组装交付物”。错误信息普遍给出下一步动作，交互式危险操作有影响预览和名称确认，这些都优于普通早期 CLI。

主要问题不是单条命令难用，而是存在两套 CLI 契约：设计文档中的全局连接/输出模型，和代码中每个名词各自绑定的简化模型。

### 1.2 当前实际命令面

- `mechctl`：`apply`、`component`、`config`、`node`、`orphans`、`rollout`、`user`、`version`，以及 Cobra 静态 `completion`。
- `component`：`deploy/list/status/diff/render/start/stop/restart/remove/upgrade/rollback/ack-drift/set-drift-policy/set-rollout`。
- `config`：`get/set/explain/group`；group 下有 `list/create/set/move/remove/diff`。
- `node`：`list/show/add/remove/bootstrap/token`，以及动态构造的节点动作。
- `rollout`：状态、历史和过程动作。
- `mechpack`：`init/assemble/bundle/lint/inspect`。
- `mechd`：`serve/ca export/ca issue`。
- `mechlet`：`agent/apply/install`。

这已经足以支撑主要用户旅程，但与 `docs/design/10-cli.md` 和部分二进制注释不一致。

### 1.3 命令与文档漂移

| 文档/帮助承诺 | 实际情况 | 影响 |
|---|---|---|
| `mechctl --local` 可直连 mechlet | 没有 flag、客户端或服务端入口 | 核心故障恢复叙事不成立 |
| 全局 `--server/--context/-s/--site/-o` | 连接 flags 重复绑定在各名词命令；无 context，`--site` 无 `-s` | 使用方式不一致，配置无法复用 |
| 环境变量、用户 config、系统 config、unix socket 的连接解析链 | 只实现 flag、默认本机 HTTPS、默认 token/CA 文件 | CI、远程多环境使用体验不完整 |
| `table/json/yaml` 输出 | ctlcmd 实际只区分 `text/json` | 自动化契约不可信 |
| 顶层 `deploy` 别名 | 根命令未注册 | README/CLI 设计失真 |
| 动态 completion 查询 Pack/Component/Node | 只有 Cobra 的静态 completion | 设计目标未实现 |
| `mechd migrate/backup`、`mechlet status/probe` | 未实现，部分文档未清楚标识 | 用户会把路线图当参考手册 |

`cmd/mechctl/main.go` 还把已存在的 `orphans` 写在“还没做的”列表；`cmd/mechd/main.go` 把已实现的 `ca export` 写成未完成。源码注释已经开始失去可信度。

### 1.4 输出参数的实际缺陷

公共根命令在 `internal/cli/root.go:72-77` 定义 `-o table|json|yaml`，而 `ClientFlags.Bind` 在 `internal/cli/ctlcmd/component.go:28-38` 等名词层再次定义 `-o text|json`。子命令 flag 会遮蔽根 flag：根 `PersistentPreRunE` 验证的是另一份 `gf.Output`，客户端命令只判断是否等于 `json`。

结果是：

- `-o yaml` 在很多 mechctl 命令中不会输出 YAML，而会静默退回文本；
- `-o typo` 也可能静默退回文本，而不是失败；
- 同一个二进制的 `version -o yaml` 与 `component list -o yaml` 行为不同。

这对脚本尤其危险。建议根命令持有一份共享 `ClientFlags`，输出层统一支持 `table/json/yaml`，未知值必须在执行前失败。稳定 JSON 应测试字段、排序和空集合形状。

### 1.5 CLI 其他建议

- 用 context 文件解决多控制面、多 Site、token/CA 路径组合，不建议继续增加环境变量特例。
- 使用 `url.PathEscape` 和 `url.Values` 构造所有路径/查询，不拼接 Component、Node、Group、Site 名。
- 为 `--yes` 明确语义：只跳过交互确认，不能跳过服务端 confirm、缩容或数据清理保护。
- 增加 `--request-timeout`；当前固定 60 秒不适合上传、等待 Rollout 或慢速边缘链路。
- shell completion 只在连接快速且有短超时时查询远端，失败时静默退回静态补全。
- 文档中的命令漂移测试应验证“根命令真实存在”，不能把 `deploy` 放进硬编码的 `alwaysReal` 后直接视为实现。

## 2. HTTP API

### 2.1 优点

- 使用明确的 `/api/v1` 前缀，为兼容演进留出了空间。
- 基于 Go 1.22+ method/path 路由，路由表直观，受保护与未认证入口一眼可见。
- GET/POST/PATCH/PUT/DELETE 大体符合资源语义；危险删除的确认值放在 body，避免进入 URL 日志。
- JSON decoder 拒绝未知字段，能捕获拼写错误。
- Pack 上传直接流式读取二进制，并通过 `MaxBytesReader` 限制为 4 GiB，没有 multipart 的额外缓冲。
- SSE 发送完整快照，客户端可以覆盖本地状态，断线恢复比增量事件简单。
- Bearer token、browser session、CSRF 与 agent mTLS 三种身份通路分开，边界可读。

### 2.2 契约一致性

当前 API 更像产品内部接口，还不是可公开承诺的 v1：

- 没有 OpenAPI、JSON Schema、稳定错误码或兼容政策；
- CLI 直接复用 mechd 实现类型，Vue 又手写一份 TypeScript interface；
- list 有的直接返回数组、有的返回 envelope，时间字段有字符串也有 `time.Time` 编码；
- Site 有时来自 query、有时来自 body，客户端还会自动追加 `site`，容易产生重复或冲突；
- 更新策略混用 `POST /drift-policy`、`POST /rollout-policy`、`PATCH /params`、`PUT /groups/{group}`；
- `POST /nodes/{name}/{action}` 和 `POST /components/{name}/rollout/{action}` 方便实现，但 action 是未类型化路径值；
- 所有错误只有 `{"error":"中文文本"}`，调用方只能依赖 HTTP 状态或解析文案。

建议在 v0.1 前定义最小 problem response：

```json
{
  "code": "component_not_found",
  "message": "组件 web 不存在",
  "requestId": "...",
  "details": {}
}
```

中文 `message` 可继续对人友好，`code` 才是脚本契约。服务端内部错误只记录到日志，响应使用稳定的通用消息和 request ID。

### 2.3 输入与资源边界

普通 `decodeBody` 在 `internal/mechd/httpapi.go:680-687` 直接读取 `r.Body`：

- 没有 route-specific 大小上限；
- 没有要求 `Content-Type: application/json`；
- 读取一个 JSON 值后不检查第二个值或尾随垃圾；
- `/auth/login`、`/auth/bootstrap`、`/join` 等未认证入口同样受影响。

HTTP Server 只设置 10 秒 `ReadHeaderTimeout`，没有 body read timeout、`ReadTimeout` 或 `MaxHeaderBytes`。SSE 确实不适合全局 `WriteTimeout`，但这不妨碍为普通请求限制 body、在 handler 中设置读取 deadline，并为上传单独设置策略。

建议默认 JSON 上限 64 KiB，Apply 可按实际文档大小设 1–4 MiB，CSR/Join 设 64 KiB，login/bootstrap 设 16 KiB。解码后必须确认 EOF。

### 2.4 并发、幂等与规模

- 除事件和 rollout history 外，Component、Node、Pack、Orphan 列表无分页/游标。
- 没有 ETag、resourceVersion、If-Match 或其他乐观并发；两个页面/CLI 同时修改时是最后写入者获胜。
- 长操作没有 idempotency key；网络超时后客户端不易判断能否安全重试。
- HTTP 客户端和 Web UI 用字符串插路径，未统一转义。
- token/session 的 actor 分别退化为固定 `token`/`admin`，审计无法区分不同自动化调用方。

当前单 admin、小规模场景可容忍部分限制，但在宣称多团队或数百节点前必须建立分页、并发版本和身份可追踪性。

### 2.5 Web 安全响应头

静态 UI 和 API 只设置 Content-Type、缓存和压缩相关 header；未见：

- `Content-Security-Policy`（至少包含 `frame-ancestors 'none'`）；
- `X-Content-Type-Options: nosniff`；
- `Referrer-Policy`；
- `Permissions-Policy`；
- HTTPS 部署下的 `Strict-Transport-Security`。

管理界面尤其应禁止被 iframe 嵌入，以降低 clickjacking。CSP 需要结合 Vite 产物验证，不能直接复制一条过严模板。

## 3. Web UI

> 本节基于 Vue/TS/CSS 源码和生产构建结果做静态评审，按要求未启动服务或浏览器，因此不包含像素级、响应式实机或跨浏览器验证。

### 3.1 优点

- Vue 3、TypeScript strict、Vue Router、Element Plus 和 UnoCSS 的组合适合当前团队与迭代速度。
- 页面按路由懒加载，生产构建成功，早期 bundle 规模合理。
- 部署、参数、ConfigGroup、状态、Rollout、节点、Join、Orphan 等核心流程都有界面，而非只有展示型 Dashboard。
- 参数表单由服务端 schema 生成，secret 只显示“是否设置”而不回传内容，设计正确。
- 状态不仅依赖颜色，还显示文字；SSE 失败时回退轮询，编辑脏表单时停止刷新，能保护用户输入。
- 删除与清理区分风险级别，展示删除/保留项，并要求输入名称确认。
- 首次初始化明确提示抢注窗口，节点 Join 提供可复制命令，运维导向清楚。

### 3.2 首次初始化流程缺陷

`Setup.vue` 加载挑战、要求拖动并等待 PoW 100%，但提交请求仅包含：

```ts
await api.post('/auth/bootstrap', { password: password.value })
```

服务端 `bootstrapAdmin` 也只解析 `PasswordBody`，不校验挑战、不调用 limiter。于是初始化页的滑块与 Argon2 计算不会保护接口，只增加初始化耗时和失败点。这还直接违背 ADR-0037 和 Web UI 设计文档中“初始化使用同一限流”的决定。

建议二选一：

1. bootstrap 与 login 共用 challenge answer、限流和 body 上限；或
2. 明确首次初始化只允许 loopback/unix-socket/一次性启动 token，移除无效的人机验证。

对于基础设施管理器，第二种方案往往更简单、更可靠。

### 3.3 无障碍

`SliderCaptcha.vue` 的操作柄是普通 `div`，只监听 pointer 事件，没有 `tabindex`、键盘事件、`role=slider`、`aria-valuemin/max/now` 或可访问替代流程。键盘和屏幕阅读器用户无法登录；这不是装饰性组件缺陷，而是认证入口阻断。

其他静态可见问题：

- 图片 `alt` 为空且没有文本等价说明；
- 320px 固定宽度与 52px 拼图尺寸不随容器缩放；
- 错误与进度没有明确 live region；
- 复杂表格、状态更新和对话框尚无可访问性自动检查。

最低整改是让操作柄可聚焦，支持左右方向键/Home/End，提供 ARIA slider 属性，并保留不依赖视觉拼图的验证方式。建议在 CI 加 axe 类组件测试或等价静态规则。

### 3.4 信息架构与视觉系统

代码层面看，当前 UI 是清楚、克制的 Element Plus 管理界面，适合早期验证；但品牌与高密度运维体验尚未形成：

- 没有专用 logo/icon/色彩/token 系统，产品辨识度主要来自文字；
- 缺少全局 Site/context 显示，未来多 Site 时容易误操作；
- 没有事件/审计视图，用户无法从 UI 完成故障时间线复盘；
- 列表缺少搜索、筛选、排序和分页，规模增加后会快速失效；
- 没有全局 404 route、页面 title、面包屑或一致的返回路径；
- 固定宽度验证码、对话框和表格需要在窄屏/缩放场景验证；
- 中文硬编码遍布页面，没有 i18n 边界。

首发前无需做“炫”的 Dashboard，优先补 Site 上下文、搜索过滤、事件时间线、可访问性与危险操作一致性。品牌视觉可以在这些基础体验稳定后完成。

### 3.5 前端工程质量

- `vue-tsc` 和生产构建通过，类型声明总体完整。
- `api.ts` 把全部 DTO 手写在一个文件，已超过单一薄客户端的舒适规模；与 Go 类型漂移不可自动发现。
- API fetch 没有 AbortController、默认超时或统一 401 跳转；切换路由时旧请求仍在运行。
- 只发现 4 组前端逻辑测试，缺少 Login/Setup/Captcha、危险删除、部署表单和组件级交互测试。
- 没有 ESLint、Stylelint、无障碍 lint、视觉回归或 bundle budget。
- URL 中的 Component/Node/Group 名多为模板字符串直插，应统一由 path/query builder 编码。

## 4. 建议的界面契约

CLI 与 UI 应共享以下产品规则，并由服务端最终强制：

| 规则 | CLI | UI | 服务端 |
|---|---|---|---|
| 危险移除先预览 | 文本/结构化 impact | 对话框列出影响 | dry-run/impact endpoint |
| 确认目标名称 | 交互输入或显式 confirm | 输入名称 | 比较请求 confirm |
| 缩容显式允许 | `--allow-remove` | 单独开关与列表 | 默认拒绝 |
| Secret 不回显 | 只显示 set/unset | 只显示已设置 | 响应永不含值 |
| 并发修改保护 | revision/If-Match | 冲突提示并刷新 | 条件更新 |
| 错误可自动化 | 稳定 exit code + API code | 按 code 呈现 | problem response |
| 身份可追踪 | token 名/subject | 当前用户/会话 | audit principal |

目前前三项已经有不错基础；后四项是 API 成熟度提升的重点。
