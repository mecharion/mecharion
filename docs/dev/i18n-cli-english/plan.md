# CLI/日志英文化

> 来源：用户真机使用后反馈——作为开源社区项目，命令行输出、`--help`、日志应当是英文，
> 代码注释与设计文档继续保留中文。

## 范围决定（已与用户确认）

1. **结构化日志字段名也改成英文**（不是只改 msg 文本）——日志整体变成纯英文 JSON。
2. **不含 Web UI**（`webui/` 是独立前端代码库，这次只做后端四个二进制：mechd/mechlet/mechctl/mechpack）。
3. **分阶段推进**，每阶段独立验证（build/test/容器验收），不到最后才一次性发现问题。

## 边界：什么算"要改"，什么不算

- **要改**：cobra 的 `Use`/`Short`/`Long`/`Example`、flag 说明文字；`fmt.Fprint*` 打到
  stdout/stderr 的提示文本；`fmt.Errorf`/`errors.New` 构造的、会被用户看到的错误信息
  （无论是 CLI 自己校验产生的，还是从 `internal/mechd` 等包经 API 冒泡上来的）；
  `log/slog` 的 msg 与字段名。
- **不改**：`//` 代码注释、doc comment；`docs/`、`ADR`、设计文档正文；变量名/函数名等
  标识符；测试里描述测试意图的注释。
- **连带**：凡是测试用 `strings.Contains(out, "中文串")` 断言 CLI/日志输出的地方，
  文案改了断言必须同步改，否则会成片报红——这不是范围扩大，是同一处改动的另一半。

## 阶段划分

| 阶段 | 内容 | 范围 |
|---|---|---|
| 1 | CLI 帮助文本 + 标准输出 | `cmd/*/main.go`、`internal/cli/{root,version}.go`、`internal/cli/ctlcmd/**`、`internal/cli/mechdcmd/**`、`internal/cli/mechletcmd/**`、`internal/cli/packcmd/**`，含各自的 `_test.go` |
| 2 | 服务端错误信息 | `internal/mechd`、`internal/spec`、`internal/pack`、`internal/render`、`internal/placement`、`internal/store`、`internal/authn`、`internal/pki`、`internal/vault` 等——这些包的 `fmt.Errorf` 经 HTTP API 冒泡到 CLI/UI，是用户会看到的文本 |
| 3 | 结构化日志（slog） | 全仓库 `slog.Info/Warn/Error` 的 msg 与字段名（`internal/agent`、`internal/mechd`、`internal/reconcile`、`internal/cli/mechletcmd` 等） |

当前阶段：**三个阶段全部完成**（阶段 1：2026-08-14；阶段 2：2026-08-14 开工、
2026-08-15 容器验收通过；阶段 3：2026-08-15 开工并通过容器验收）。详情见 log.md。

阶段 3 范围严格限定为 `slog.Info/Warn/Error/Debug`（含 `.log().Info/...` 这类包了
一层的调用）的 msg 与字段名——不含 `internal/resource`/`internal/runtime`/
`internal/hook` 等 mechlet agent 侧执行引擎包里经 `faults.Permanentf` 等构造的
错误信息、`protocol.TaskResult.Message` 这类"函数返回值直接是文本"的写法、以及
`internal/protocol` 的 gRPC `status.Error(...)`——这三类在阶段 3 验收过程中被
明确识别为"看着像但不是当前三阶段计划范围内"的文本，需要用户决定是否值得
开一个新阶段，见 log.md 阶段 3 完成条目的"已知未尽事宜"。

## 验证方式

沿用既有纪律：`go build/vet/test ./...`（Windows），关键路径跑一次容器化验收
（`./hack/testenv.sh up && test`），变异验证只在改了行为判定逻辑（不是纯文案）时才做——
这次绝大多数改动是文案替换，risk 集中在"改文案漏改对应断言"而不是逻辑错误，
因此验证重点是**编译 + 测试全绿**，而不是逐处变异。

## 术语表（保持全仓库一致，避免同一个词在不同文件里译法不同）

| 中文 | 英文 |
|---|---|
| 组件 / Component | Component |
| 节点 / Node | Node |
| 站点 / Site | Site |
| 已收敛 / 未收敛 | Converged / Not converged |
| 漂移 | Drift |
| 干跑 | Dry run |
| 确认 | Confirm / Confirmation |
| 实例 | Instance |
| 部署 | Deploy |
| 下发 | Dispatch |
| 收到下发 | Received dispatch |
| 调和 | Reconcile |
| 调和完成 | Reconcile complete |
| 已分批 | Batched |
| 放行批次 | Batch released |
| 批次已过门禁 | Batch passed gate |
| 孤儿 | Orphan |
| 回滚 | Rollback |
| 升级 | Upgrade |
| 移除 | Remove |
| 保留 | Retained |
| 删掉 | Deleted |
| 只读 | Read-only |
