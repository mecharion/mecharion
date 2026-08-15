# 架构、模块与命名审阅

## 1. 总体架构评价

总体架构方向正确，最重要的边界也划在了正确位置：

- mechd 持有期望状态、Pack/Blob、放置、拓扑、Rollout、HTTP API 和 UI；
- mechlet 持有本地期望状态副本，执行资源调和、运行时操作、健康与漂移检测；
- mechctl 是控制面客户端；
- mechpack 负责 Pack 的本地创作与组装，不承担软件构建；
- systemd、Docker、Compose 位于统一 Runtime/资源接口之下，而不是形成三条业务链路。

这种结构让“中心化是协调层、节点才是执行面”成为代码事实，而不只是文档口号。控制面中断时业务进程与节点调和仍可持续，是 v1 不做 mechd HA 能够成立的前提。

### 1.1 当前结构的主要优点

1. **数据面与控制面分离。** mechd 不直接在目标机执行资源动作，多节点身份和通信方向明确。
2. **解析与执行分离。** mechd 输出已解析规格，mechlet 不需要 Pack 上下文和中心数据库即可重复调和。
3. **运行时抽象层级正确。** 上层生命周期逻辑不感知 systemd/Docker 细节，Runtime 实现也没有承载领域决策。
4. **状态存储与观测存储有意识地区分。** SQLite 负责中心权威状态，节点 JSON/JSONL 用于本地运行和断连缓冲。
5. **纯函数比例较高。** placement、render、digest、lint 等核心逻辑容易静态推理和单元测试。
6. **失败和安全默认值进入模型。** drift policy、run state、removal、rollout state 没有完全依赖临时命令语义。

### 1.2 主要架构风险

- `internal/mechd` 已达到约 9,023 行，既包含用例编排、HTTP DTO、认证、Join、Rollout、上传、表单、事件和后台状态机，也暴露类型给 CLI。它正在成为控制面“万物包”。
- `Service.Deploy` 在渲染成功前分步持久化 Component、RoleInstance、事实和依赖绑定，事务边界与业务动作边界不一致。
- Store 声明“写路径一律走 `InTx`”，实际全仓只有 `InstanceRepo.Ensure` 调用它。Repository 抽象存在，但缺少可组合的 Unit of Work。
- CLI 直接导入 `internal/mechd` 的路径常量、Body 和 View/Result 类型，客户端与服务端实现耦合，未来抽 SDK 或兼容多版本会困难。
- HTTP API、CLI 类型、Vue TypeScript 类型是三份手写契约，没有 schema/codegen 作为共同真源。
- 当前 SQLite/单进程方案适合 v0.x，但“数百节点”目标还没有容量预算、背压策略、数据库增长模型和恢复时间目标支撑。

## 2. 模块分工

### 2.1 当前模块评价

| 模块 | 当前职责 | 评价 |
|---|---|---|
| `cmd/*` | 四个二进制的装配与退出码 | 薄入口，良好；但帮助文案有明显陈旧内容 |
| `internal/cli` | Cobra 根命令、公共日志/output | 共享骨架合理；output 与 ctlcmd 重复 |
| `internal/cli/ctlcmd` | mechctl 命令、HTTP 客户端、渲染输出 | 功能完整，但 4,600+ 行且直接依赖 mechd 类型 |
| `internal/cli/mechdcmd` | mechd serve、CA | 合理；serve 同时组装 HTTP、gRPC、TLS、存储，后续可抽 bootstrap |
| `internal/cli/mechletcmd` | agent/apply/install | 安装逻辑较大，但仍属于 CLI 用例边界 |
| `internal/cli/packcmd` | init/assemble/bundle/lint/inspect | 分工清楚；描述中含 sign/push，实际未实现 |
| `internal/pack` | Pack 模型、解析、lint、模板辅助 | 核心资产之一；规则多但组织尚可 |
| `internal/packindex` | Pack 目录索引与解析选择 | 独立合理，避免 pack 包混入存储策略 |
| `internal/placement` | 角色到节点的确定性放置与约束 | 接口集中、纯逻辑，边界优秀 |
| `internal/render` | 参数、依赖、路径、资源、导出的规格渲染 | 核心复杂度高但职责一致；应保持无持久化副作用 |
| `internal/spec` | mechd 到 mechlet 的已解析契约 | 依赖扇入最高是合理的；应成为严格版本化边界 |
| `internal/protocol` | gRPC server/client、身份、任务与兼容 | 分工合理；协议版本策略需要对外化 |
| `internal/agent` | 下发处理、本地 desired、周期调和协调 | 合理；文件名依赖 InstanceKey 合法性这一不变式未被入口保证 |
| `internal/reconcile` | 七阶段资源调和、移除、漂移报告 | 领域集中；`reconcile.go` 过长但不建议按函数机械拆包 |
| `internal/resource` | 资源接口与各资源类型实现 | Registry 和窄接口设计良好；Pack 本质上拥有 root 级能力需写入安全模型 |
| `internal/runtime` | workload Runtime 接口 | 位置正确 |
| `internal/runtime/systemd` | systemd 实现 | Linux 平台边界清楚 |
| `internal/runtime/docker` | Docker 与 Compose CLI 实现 | 共用 Docker CLI 适合 v1；单包文件偏大 |
| `internal/state` | 节点实例状态与原子文件操作 | 职责合理；名称较泛，仅因是 internal 尚可接受 |
| `internal/store` | SQLite、迁移、sqlc、Repository | 基础扎实；事务组合能力不足 |
| `internal/vault` | envelope encryption 与节点密钥副本 | 安全边界清晰 |
| `internal/pki` | CA、证书签发、CSR、指纹 | 内聚；必须在这里防御性校验节点身份，而不能只信调用者 |
| `internal/authn` / `password` | 挑战、限流、token/session、口令 | 分包清楚；Challenge limiter 的接口语义不适合请求速率限制 |
| `internal/mechd` | 控制面全部用例与 HTTP | 当前最大结构风险，应按业务能力逐步拆分 |
| `internal/webui` | embed 与 SPA 静态文件服务 | 很薄，合理 |
| `webui/src` | Vue 页面、组件、API/composable | 早期规模适中；类型契约集中在单一大文件 |

### 2.2 建议的目标分层

不建议为了“整洁”一次性创建大量小包。应围绕真实问题做三步拆分：

1. 新建稳定的 `internal/controlapi`（若未来对外则迁为 `pkg/client`）：只放 API 路径、请求/响应 DTO、错误码和客户端；CLI 不再导入 `mechd`。
2. 为跨表业务动作提供 `store.UnitOfWork`，让 Deploy、Join、ConfigGroup、Remove 的状态变更可以在一次事务内完成。Repository 实例由事务创建，而不是每个方法自行取 writer。
3. 从 `mechd` 先拆出高内聚能力域，例如 `joinservice`、`rolloutservice`、`componentservice`；HTTP handler 只做协议适配。不要按 `helpers`、`utils` 这种技术分类拆。

目标依赖方向可概括为：

`cmd/cli/http` → `application use cases` → `pack/placement/render/domain` → `repository/runtime ports` → `sqlite/grpc/systemd/docker adapters`

## 3. 持久化与一致性

SQLite WAL、独立 reader pool、单 writer、嵌入 goose migrations 和 sqlc 适合当前产品形态。选择纯 Go SQLite 也支持静态二进制和跨平台开发。

问题不在数据库选型，而在事务没有覆盖业务不变式：

- `internal/store/store.go:222-224` 明确说 Component、RoleInstance、PackBinding 应在一次事务中改变；
- 实际 `Store.InTx` 只有 `internal/store/repo.go:578` 的 ordinal 分配在使用；
- `Service.Deploy` 在 `internal/mechd/service.go:239-265` 先写 Component 和实例、再冻结事实、最后渲染；
- `resolveBindings` 在渲染过程中于 `internal/mechd/resolve.go:394-401` 创建持久绑定；
- `ensureInstances` 对每个新增实例单独事务，并对移除逐条删除。

这会导致渲染或后续写入失败时留下已更新 Component、部分实例、部分事实或绑定。建议采用“预计算 → 开事务 → 条件校验/写入 → 提交 → 通知”的两阶段模式；大计算不必占着写事务，但提交时必须用 revision/条件更新确认预计算依据未变化。

## 4. 通信与协议

gRPC 用于 mechd↔mechlet，HTTP/JSON 用于人类客户端和 UI，职责选择合理。服务端流式 Assignment、节点主动拨出、mTLS CN 作为身份、完整快照而非增量补丁，都降低了断连恢复复杂度。

建议补充：

- 明确 gRPC 最大消息、并发流、keepalive 与资源配额；blob 已是流式传输，不应让普通控制消息无界增长。
- 给协议兼容性建立已发布 fixture，而不只检查当前 `.proto` 与生成物一致。
- Task channel 继续保持“明确枚举的临时动作”，不要演化为通用远程 shell；后者会破坏产品安全模型。
- HTTP 与 gRPC 请求都增加 request/correlation ID，贯穿事件、审计和节点日志。

### 4.1 可观测性与运行维护

项目已经使用结构化 `slog`、事件、审计记录、节点报告和 SSE 快照，这些是很好的基础；但控制面本身只有一个无条件返回 `ok` 的 `/api/v1/healthz`，没有区分进程存活与数据库/关键后台循环就绪。也未见 metrics、trace、请求 ID、队列/连接/数据库增长指标或诊断包导出。

离线优先并不排斥可观测性：metrics 可以本地暴露，trace 默认关闭，诊断包可以人工导出。建议至少提供 liveness/readiness 分离、结构化请求日志、关键错误/延迟/队列/DB/blob 指标和脱敏诊断包。所有遥测默认不向外发送，保持离线承诺。

## 5. 项目与领域命名

### 5.1 `Mecharion` / `m7n`

项目已经诚实记录了名称的硬伤：`Mecharion` 容易误读，词源需要解释；`m7n` 的首尾字形接近、口头传播也不自然。README 第一屏给发音和词源是有效补救。

优点是原创度高、品牌意象与机械/持续运行相关，且能形成统一 `mech*` 词根。现阶段不值得改名，但应避免在每份技术文档重复大段命名故事；首页和 naming ADR 足够。

### 5.2 二进制命名

| 名称 | 评价 |
|---|---|
| `mechctl` | 最清楚、符合基础设施工具惯例 |
| `mechd` | 简洁地表示 daemon/control plane；帮助中应统一称“控制面”，不要又称“可选协调层” |
| `mechlet` | 与 kubelet 类似，能表达小型节点代理；第一次接触不够自解释，但产品内一致性好 |
| `mechpack` | 动宾关系自然；`assemble` 明确表达只组装不构建，是优秀命名 |

### 5.3 CLI 名词与动词

名词优先结构适合对象多、危险操作多的系统，`component remove` 明显优于顶层 `remove`。当前可改进点：

- `orphans` 是唯一复数顶层名词，与 `component/node/config/rollout/user` 不一致。可保留用户熟悉的复数集合语义，但应在命名规范中明确例外。
- `ack-drift` 实际表达“暂时抑制某条 drift”，`suppress-drift` 或 `drift suppress` 更贴近行为；如果保留 ack，应解释持续时间和审计语义。
- `component set-rollout` 与独立 `rollout status/pause/resume` 把策略和过程拆在两个命名空间，逻辑成立但不够容易发现。建议策略统一为 `component rollout-policy get|set`，过程保留 `rollout`。
- `node bootstrap` 实际做 SSH 一次性安装并 Join，名称可接受；帮助必须突出 SSH 只用于初始推送。
- `mechpack bundle` 与 `assemble` 的产物差异应在一屏帮助中给出，否则两个“打包”动词难以区分。

## 6. 内部代码命名

整体 Go 命名简短、一致，包名多为单词；`Pack`, `ResolvedSpec`, `Assignment`, `RemovalImpact`, `RolloutHalted` 等能表达领域状态。值得改善的方面：

- `Service` 过于通用且承载全部用例，拆分后应出现 `ComponentService`、`JoinService` 等具体名称。
- `state`、`command`、`resource` 在全局搜索中较泛；保持 internal 包可接受，但新代码避免再增加 `common`、`model`、`util`。
- 状态字符串散布在存储、服务、协议和前端；应以类型常量与契约 schema 为真源，避免手写字符串漂移。
- `AddNode` 使用 Upsert 实现，方法名暗示“只能新建”而底层语义是“可覆盖”；应使数据库操作与领域意图一致，避免并发身份错误。

## 7. 架构整改优先级

1. 先修共享 Identifier 校验和 Unit of Work；这两项保护所有上层设计。
2. 抽出 HTTP 契约/客户端包，切断 ctlcmd→mechd 实现依赖。
3. 按 Join、Component、Rollout 三个用例域逐步减小 mechd 包。
4. 为 revision/乐观并发、分页和稳定错误码设计 API v1 的最小契约。
5. 等实际容量数据出现后再讨论控制面 HA，不要因当前问题改换数据库。
