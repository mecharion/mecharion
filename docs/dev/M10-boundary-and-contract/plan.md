# M10:契约与边界收口

> 对应 [design/25-roadmap.md](../../design/25-roadmap.md#里程碑) 的 M10 行(当前该行内容仍是旧的"打磨、文档、v0.1.0 公开发布",在本阶段任务与顺序正式定稿后需要回写更新,见文末回写清单)。
> 来源:[review/20260809/06-defect-register-and-roadmap.md](../../review/20260809/06-defect-register-and-roadmap.md)(下称"缺陷台账")。

## 目标

06 文档给出的四句话,是这个里程碑要达成的:

1. 一个名字不会让 root agent 操作错误路径;
2. 一次 Join 或 Deploy 要么完整成功,要么不改变权威状态;
3. README、CLI、API 和 UI 描述的是同一个当前产品;
4. 外部用户能从受信发布制品完成一条真实、可恢复的生命周期。

## 任务

来源列的 REV 编号见[缺陷台账](../../review/20260809/06-defect-register-and-roadmap.md)。阶段划分抄自该文档 §5;标 `*` 的行是本表整理时按该文档 §4 缺陷表的领域描述补充归位,并非 §5 原文逐条列出的顺序,实际排期以本表为准,可随时调整。

### 阶段 A:保护主机、身份与状态(P0,不加新功能)

| # | 来源 | 摘要 | 级别 | 状态 | 备注 |
|---|---|---|:---:|---|---|
| A1 | REV-001 | 用户可控标识符(Component/Node/Site/ConfigGroup)统一校验 + path containment | P0 | 已完成 | 详见 log.md；SQL CHECK 明确不做（代价写在 09-naming-conventions.md §7） |
| A2 | REV-002 | Join 读后写竞态修复:token 消耗与 node claim 原子化 | P0 | 已完成 | 详见 log.md |
| A3 | REV-003 | Deploy 改为 Unit of Work,失败不留半份期望状态 | P1 | 已完成 | 详见 log.md;过程中修了一个真死锁(Vault 与主库共享写连接) |
| A4 | REV-004 | 未认证 HTTP 请求体大小/读取时间加边界 | P1 | 已完成 | 详见 log.md |
| A5 | REV-005 | Challenge 签发限流真正生效 | P1 | 已完成 | 详见 log.md |
| A6 | REV-006 | 首次初始化认证流程:一次性 admin token 门禁(ADR-0039),撤掉 PoW/滑块 | P1 | 已完成 | 详见 log.md |
| A7 | REV-007 | 预登记 Node(pending)可以 Join:reserved→joined 状态机 | P1 | 已完成 | 详见 log.md;三节点容器验收里顺带发现两个与本项无关的既有 rollout 测试失败,已记录待查 |
| A8 | REV-008 | HTTP 500 不泄漏内部错误;typed error + 稳定 code + request ID | P1 | 已完成 | 详见 log.md;`internal/mechd`+`internal/placement`（+ 撞出真回归后补的 `internal/spec/drift.go`）已全部显式打类型标记；`internal/render`/`internal/pack`/`internal/spec` 其余部分未逐条转换，记为已知欠账，见 08-security.md |

**阶段完成标志**:故障注入与并发测试证明关键状态全成/全败;所有未认证入口有大小、速率、时间边界;任何名称不能改变根目录或身份命名空间。

### 阶段 B:统一产品契约

| # | 来源 | 摘要 | 级别 | 状态 | 备注 |
|---|---|---|:---:|---|---|
| B1 | REV-009 | 单机模型 / mechd / `--local` 三方叙述统一,以 ADR-0026 为准 | P1 | 已完成 | 详见 log.md;顺带实现了只读 `--local`(mechlet 新增本机 unix socket 只读诊断入口);未做 `client.yaml` 的 `fallback: local` 自动降级,记为已知欠账;2026-08-14 用户真机测试撞出 `mechctl --local component ...`(flag 在名词前,文档规范写法)无法解析的回归,已按 B2 同一模式修复,详见 log.md 当日条目 |
| B2 | REV-011 | mechctl 输出 flag 统一(根 flag 与子命令不再各持一份) | P1 | 已完成 | 详见 log.md;2026-08-14 把同一处理跟进到 `--local`/`--server`/`--site` 等其余 6 个 ClientFlags,详见 log.md 当日条目 |
| B3 | REV-014 * | 建 API contract 真源(OpenAPI/schema)+ 稳定 error code | P2 | 暂缓 | 规模独立于其余各项(全部端点 schema + codegen 工具链选型 + CI breaking-change 检查),征询用户后先做完 B5→B7 这些 P1 项,再回头单独立项 |
| B4 | REV-015 * | path/query 统一走 builder,禁止字符串拼接 | P2 | 已完成 | 详见 log.md;`internal/mechd` 路由侧本来就干净(标准库 ServeMux),缺口全在 mechctl 客户端与 Web UI 两侧的调用方 |
| B5 | REV-012 | SliderCaptcha 键盘/ARIA 可访问性 | P1 | 暂缓 | 用户明确表示现阶段不做无障碍支持;调研发现真正的非视觉替代方案与"暴露滑块目标值"等价,会破坏 23-web-ui.md §6.12.1 明确写下的"不开后门"原则(三条 e2e 判据届时可被脚本绕过),留待用户后续决定 |
| B6 | REV-013 | 管理 UI 补基础安全响应头(frame-ancestors、nosniff、Referrer-Policy、HSTS 评估) | P1 | 已完成 | 详见 log.md;真实浏览器验证时撞出并修复了一个纸面推导漏掉的 CSP 问题(WASM PoW 求解需要 script-src 'wasm-unsafe-eval') |
| B7 | REV-010 | 确定 alpha 的 Pack 信任策略:补全签名闭环,或新 ADR 限定"本地受信 Pack" | P1 | 已完成 | 详见 log.md;用户明确要求不做强校验,写了 [ADR-0040](../../adr/0040-pack-trust-is-operator-responsibility.md) 取代 ADR-0016,撤销签名/可信发布者计划,sha256 内容寻址完整性校验保留 |
| B8 | REV-021 | 修复 5 处失效 ADR 链接、examples lint 缺口、CLI/入口注释漂移 | P2 | 已完成 | 详见 log.md;CI 链接检查本身归 C3(REV-020),这里只修实际缺陷 |
| B9 | REV-026 * | pack/v1 "已冻结"与"v0.1 前可调整"两种表述统一为 draft-stable | P2 | 已完成 | 详见 log.md;`v0.1.0` 后用兼容 fixture 真正冻结留给发布阶段(C 段) |
| B10 | REV-031 * | 清理源码里已陈旧的里程碑/实现状态注释 | P3 | 已完成 | 详见 log.md;历史性"M几做了什么"说明予以保留,只清理了变成假承诺的前瞻性里程碑标注 |

**阶段完成标志**:README、`--help`、OpenAPI/schema、UI 调用与实现对同一能力给出相同答案;不存在"文档已实现、代码没有"的主流程。

### 阶段 C:做出可验证的公开 alpha

| # | 来源 | 摘要 | 级别 | 状态 | 备注 |
|---|---|---|:---:|---|---|
| C1 | REV-022 | README quickstart 换成可 assemble 的真实 `.mpack`,进 CI | P2 | 已完成 | 详见 log.md;`examples/packs/` 真实组件的包仍按原计划留给用户后续制作;这次新建的是一个独立的、零外部依赖的最小 quickstart 示例(`examples/quickstart/`),连带补了 `mechctl pack upload`(此前命令行完全没有上传路径) |
| C2 | REV-024 | 补齐 CONTRIBUTING、SECURITY、CoC、Issue/PR 模板、CODEOWNERS | P2 | 已完成 | 详见 log.md;GOVERNANCE/MAINTAINERS/发布政策留给出现第二位维护者或 C5 时再补;需要用户在仓库 Settings 手动开启 Private vulnerability reporting |
| C3 | REV-020 | CI 加 staticcheck / govulncheck / gosec / secret 扫描 / ESLint / 链接检查 | P2 | 已完成 | 详见 log.md;许可证清单与 fuzz corpus 不在 plan.md 这行的范围内,留待后续单独立项 |
| C4 | REV-025 * | CI 矩阵与工具版本纪律:最低+当前 Go 双跑、Actions pin SHA、`sqlc` 固定版本、覆盖率趋势 | P2 | 已完成 | 详见 log.md;覆盖率趋势征询用户后明确不做(需要外部 SaaS,与离线优先方向冲突);Go 双跑只在 ubuntu 加一条,不搞满矩阵 |
| C5 | REV-023 | 版本、CHANGELOG、release workflow、checksum、签名、SBOM、provenance | P2 | 已完成 | 详见 log.md;征询用户后选了 GoReleaser(而非手写 workflow)+ tar.gz/zip 归档(不做 .deb/.rpm);"安装包"范围明确为归档 + 现有 `mechlet install --standalone`;`make release-dry-run` 本地验证过完整流程(构建/打包/SBOM),签名步骤因需要真实 GitHub Actions OIDC 身份未能本地验证 |
| C6 | — | 备份/恢复、证书、升级、故障排查运维文档(REV-034 的文档部分) | P2 | 已完成 | 详见 log.md;新增 `docs/ops/` 四份文档;过程中撞出一个真缺口——`Store.Backup` 已实现但没有任何入口能触发,征询用户后新增 `mechctl backup create`(认证 HTTP 端点)与配套的 `internal/mechd` 错误分类修正;顺带修了 `10-cli.md` 两处文档漂移(`mechd backup`/`mechd migrate` 从未真正实现) |
| C7 | — | mecharion.dev 官网骨架(用户已注册域名) | P2 | 进行中 | 详见 log.md;仓库(`mecharion/website`)已建好、框架+首页+运维手册四篇(改写自 C6 的 `docs/ops/`)已推到 `origin/main`;剩两件事未完成:Pack 仓库页仍是占位(等 pack/v1 冻结)、还没连 Cloudflare Pages 实际上线(用户自己在控制台操作,不在本仓改动范围) |

**阶段完成标志**:一个未参与开发的人只看发布文档,可以从制品完成安装、部署、升级、回滚、备份恢复与卸载,并知道如何报告安全问题。

### 阶段 D:beta 前的规模与多人协作(排在 M10 之后,先记在这里)

| # | 来源 | 摘要 | 级别 | 状态 | 备注 |
|---|---|---|:---:|---|---|
| D1 | REV-016 | 主要列表分页、更新 revision/ETag、关键 POST 幂等键 | P2 | 未开始 | |
| D2 | REV-017 | 审计从 best-effort 升级为事务审计,或明确降级称呼为"日志" | P2 | 已完成 | 提前于 M10 做;详见 log.md;选了便宜路径(改措辞,不做事务审计);顺带在 08-security.md §6 写清楚现在真正没有的三件事(查询入口/保留策略/actor 粒度) |
| D3 | REV-018 | 拆 `internal/mechd`(9k 行),抽 control API contract | P2 | 未开始 | |
| D4 | REV-019 | 前端测试补齐(Login/Setup/Deploy/Remove)+ a11y/视觉基线 | P2 | 部分完成 | 提前于 M10 做;详见 log.md;只做了 Login/Setup/Deploy/移除四个组件的行为测试(19 例),不含 a11y/视觉基线(与 B5 同一边界);`ComponentActions.vue` 里升级/回滚/启停三类动作未测,记为已知欠账 |
| D5 | REV-029 | Site 上下文、事件/审计视图、搜索分页、404、i18n/品牌系统 | P3 | 未开始 | |
| D6 | REV-030 | 容量模型、压测基线、备份恢复演练、增长告警 | P3 | 未开始 | |
| D7 | REV-033 * | `/healthz` 拆分 live/ready、关键指标、request ID、诊断包、默认零外发遥测 | P2 | 部分完成 | 提前于 M10 做;详见 log.md;只做了用户选定的两项(`/healthz` 拆分 live/ready + 确认零外发遥测);关键指标(Prometheus)、诊断包不在这次范围内,仍是未开始;request ID 排查后发现 A8 的 `withRequestID` 中间件已经覆盖,无需额外动作 |
| D8 | REV-034 * | 备份/迁移的产品化机制:release acceptance 从旧库升级、升级前自动备份、失败恢复演练 | P2 | 未开始 | |
| D9 | REV-028 | 提交粒度:后续按可独立验证能力提交,不再用超大提交 | P3 | 未开始 | 工作方式约定,非代码改动 |
| D10 | REV-032 | 拆 `make check` 为 `check-fast` / `check-web` / `check-all` | P3 | 已完成 | 提前于 M10 做;详见 log.md;`check` 保留指向 check-fast,不打断既有肌肉记忆 |

RBAC 与控制面 HA 不进本表——只有出现真实用户/采购条件后再立项,见缺陷台账 §5 阶段 D 说明。

## 验收标准

抄自缺陷台账 §6,作为阶段 A–C 完成后的发布门槛:

### `v0.1.0-alpha.1`

- REV-001~013 全部关闭,或由新 ADR 明确缩小范围(P0 不允许豁免);
- 静态检查、单元、三平台、systemd、三节点和 Web 关键流程均通过;
- 真实 quickstart 从发布制品通过;
- 没有 README/帮助宣称的未实现主流程;
- 有 SECURITY、贡献说明、已知限制和升级/卸载路径;
- 发布制品有 checksum、SBOM 和来源签名。

### `v0.1.0-beta.1`

- API contract、稳定 error code、分页和乐观并发落地;
- 备份恢复与控制面/agent 升级演练通过;
- 前端认证、危险操作、无障碍关键路径自动化;
- Pack 签名与信任模型完成,或正式限定只支持本地受信 Pack;
- 有可复现的小规模容量基线和资源上限。

## 回写清单

里程碑收尾时逐项落实到 `adr/design`:

- [ ] `design/` 正文更新(至少涉及 01/08/09/10/11/13/24 等文档,视实际改动而定)
- [ ] 新增/修订 ADR(已知至少需要:A6 的 bootstrap 认证模型、B7 的 Pack 信任策略)
- [ ] `decision-log.md` 追加条目(从当前最大编号 342 续,不重号)
- [ ] `design/25-roadmap.md` 的 M10 行更新为本文件的目标描述并标记完成状态
