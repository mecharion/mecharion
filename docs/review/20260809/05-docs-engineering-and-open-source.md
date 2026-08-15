# 文档、测试、工程化与开源成熟度审阅

## 1. 文档体系

### 1.1 优点

当前文档的最大优点是保留了推理过程，而不是只描述最后代码。`design/`、`adr/`、`spec/` 三分法合理：

- design 解释系统形态和模块关系；
- ADR 解释为何选择、比较过什么、承担什么代价；
- spec 定义 Pack 外部契约。

38 份 ADR 对单机控制面、SQLite、Runtime 接缝、Pack 签名、放置、配置组、secret、协议、Web UI 和登录模型都有明确决策。对于主要由 AI Agent 快速实现的项目，这种记录能显著降低“下一轮生成把上一轮理由覆盖掉”的风险。

其他优点包括：

- `docs/README.md` 有阅读路线和完整索引；
- README 坦率说明早期状态、未发布、不可生产使用；
- 规范通过真实组件示例反推，而非只凭抽象设计；
- 多数文档会写已知代价、非目标和失败语义；
- 测试环境文档很好地解释了为何需要 systemd 容器、贫瘠镜像和真实运行时。

### 1.2 结构性问题：事实、历史与计划混写

`design/` 声称描述“当前系统形态”，但实际混入大量：

- 已接受但未实现的设计；
- 里程碑实施日记；
- 某次收尾审查记录；
- 未来多团队/RBAC/HA 愿景；
- 代码中后来已经改变的命令面。

例如 `10-cli.md` 同时是规范、实现状态表、未实现理由和未来 completion 设计，达到 771 行；`23-web-ui.md` 达到 1,626 行；`pack-v1.md` 超过 1,800 行。信息很丰富，但用户很难迅速得到“今天该怎么用”的答案。

建议重构为四层：

1. **User docs**：安装、quickstart、常用任务、运维、故障恢复；只写当前可用能力。
2. **Reference**：CLI help、配置、HTTP API、Pack spec；尽量由 schema/代码生成和验证。
3. **Architecture**：稳定的当前结构和关键不变式。
4. **History/decisions**：ADR、decision log、里程碑复盘。

M0–M10 的实现日记可以移到 `docs/development/history/`，不要让它占据用户导航主路径。

### 1.3 已确认的文档缺陷

#### 失效本地链接

静态链接扫描发现以下 5 处引用不存在的 `0006-mechd-renders-mechlet-applies.md`：

- `docs/adr/0033-mechlet-local-desired-state.md:5`
- `docs/design/12-spec-and-state.md:369`
- `docs/design/20-continuous-reconcile.md:195`
- `docs/design/24-lifecycle-completion.md:32`
- `docs/design/24-lifecycle-completion.md:776`

应找到当前承载相同决定的 ADR/设计文档后修正，不能简单指向现有 ADR-0006，因为当前 `0006-multi-role-pack.md` 主题完全不同。

#### 示例数量过期

README、Pack spec、roadmap 和 Web UI 设计多处写“10 个示例”，当前 `examples/packs` 实际有 12 个。数量本身不重要，但它说明手工维护的“规范已由多少案例验证”会持续漂移。建议写成“示例矩阵”并由脚本生成数量。

#### “冻结”语义互相矛盾

- README `:19` 与 `:128` 说 pack/v1 已冻结，外部作者据此写 Pack 是安全的。
- `docs/spec/pack-v1.md:8` 又说明首个公开 v0.1 前仍可调整，发布后才严格冻结。
- 当前没有 Tag、已发布兼容 fixture 或升级政策。

建议在 v0.1 前统一称 **release candidate / draft stable**，仅承诺会记录变更；`v0.1.0` 发布后再冻结 `pack/v1`。

#### quickstart 不是可执行教程

README 展示 `mechlet install --standalone` 后直接部署 `go-webapp`，但示例 Pack 明确只是规范验证物，blob hash 是 `0000…` 占位值，无法 assemble/sign，也没有真实 payload。新用户照抄不能完成首次部署。

应提供至少一个随仓库可构建的真实 `.mpack` 或生成脚本，并用 CI 从空环境执行完整 quickstart。规范夹具与教程样例应物理/命名区分。

#### CLI 与架构描述过期

- README 和四个二进制帮助仍宣称 mechd 可选、`--local` 可驱动单机；已被 ADR-0026 修订且代码没有实现。
- CLI 文档承诺 context、环境解析、全局 site、YAML 输出和顶层 deploy，代码不具备。
- `10-cli.md` 有重复章节号 `1.5`，命令参考和设计讨论没有分离。
- 入口源码的“还没做”注释已经错误标记 orphans、ca export。

#### 当前能力与愿景边界不清

架构图写 HTTP API/RBAC/审计，总览写多团队、数百节点，但当前只有单一 admin，审计是 best effort，控制面不做 HA。应在图上标 `planned`，或者把当前图和目标图分开。

### 1.4 缺少的用户/运维文档

公开 v0.1 至少需要：

- 安装与卸载（支持平台、systemd unit、目录和端口）；
- 真实可执行的 15 分钟 quickstart；
- 单机与多节点部署拓扑；
- mechd/mechlet 配置参考与默认值；
- 证书生命周期、Join token、节点替换和吊销；
- 备份、恢复和灾难恢复演练；
- 控制面/节点升级与兼容矩阵；
- 日志、liveness/readiness、metrics、事件、状态和常见故障排查；
- 安全模型与 Pack=root 级信任说明；
- 稳定 CLI/API/Pack 版本政策；
- 从旧版本升级/迁移的说明。

中文作为当前唯一语言对早期国内用户没有问题；若以国际开源社区为目标，默认 README 和核心贡献文档最终需要英文，中文可保留在 `docs/zh/`。不建议在产品契约尚未稳定时先翻译 80 多份历史文档。

## 2. 测试设计

### 2.1 优点

- 117 个 Go 测试文件，测试代码约 37,629 行，相对约 46,624 行生产 Go 代码投入很高。
- 纯逻辑模块与 Repository、CLI wire、HTTP、认证、协议、运行时等均有测试痕迹。
- `go test -race -cover ./...` 在 Linux/macOS/Windows 三平台配置，能够发现平台和竞态问题。
- systemd 资源、软链、用户、权限等不能由 mock 证明的行为进入真实容器 E2E。
- “贫瘠”测试镜像让外部工具/联网依赖直接失败，是很好的约束性测试设计。
- 三节点验收覆盖 Join、mTLS、吊销、cordon、滚动升级和完整生命周期。
- 示例 Pack 以 `--hermetic --strict` 作为规范可执行夹具。
- 生成物一致性对 proto 有 CI guard。

这些测试形态比单纯追求行覆盖率更符合基础设施项目。

### 2.2 缺口

- `-cover` 只打印结果，没有上传、趋势或最低阈值；真机套件不产覆盖数据，roadmap 已承认。
- 没有 fuzz target；Pack/YAML、archive、mpack、模板、HTTP JSON、CSR/证书输入都是高价值对象。
- 没有 benchmark 或容量回归，无法支撑“数百节点”的性能叙事。
- 前端只有 `joinCommand`、`orphans`、`useLive`、`useParamEdits` 4 个逻辑测试文件；没有页面/组件级 Login、Setup、SliderCaptcha、Deploy、Remove 测试。
- Web UI 设计文档承认滑块阻挡了部分端到端验收，认证主流程没有完整自动化。
- roadmap 记录多节点全量套件对繁忙机器负载敏感、尚未解决。
- 关键新发现缺少测试：非法标识符、Deploy 回滚、Join 并发、challenge 签发速率、JSON body 上限、bootstrap challenge。
- 没有最低受支持 Go 版本测试。`go.mod` 是 `go 1.25.7`、README 是 Go 1.25+，CI 只跑 1.26。

### 2.3 建议的测试金字塔

1. 单元/属性测试：identifier、placement、render、state machine、path containment。
2. Repository 事务测试：真实 SQLite，故障注入验证全成或全败。
3. 并发测试：Join、ordinal、Deploy revision、Rollout action。
4. Handler 合约测试：body 上限、content type、错误 code、分页、认证/CSRF。
5. Vue 组件测试：认证、表单脏状态、危险操作、键盘可访问性。
6. 单机 systemd E2E。
7. 三节点关键 smoke；较慢完整矩阵可 nightly，但 PR 必须保留一条真实多节点路径。
8. Release acceptance：从发布制品而非源码构建目录完成 quickstart、升级和恢复。

本报告按要求没有运行上述测试；这里评价的是结构和缺口。

## 3. CI 与开发体验

### 3.1 当前 CI 评价

CI job 分为 check、webui、三平台 test、交叉 build、systemd E2E、三节点 acceptance、Pack lint，边界清楚；权限为 `contents: read`，并设置并发取消，基础安全和成本意识较好。

值得修正：

- GitHub Actions 使用 `actions/checkout@v4` 等浮动 major tag，没有 pin 到 commit SHA；
- 只测试 Go 1.26，未验证 README 宣称的 1.25 最低版本；
- 没有 CodeQL/漏洞/secret/许可证/SBOM 检查；
- 没有 release workflow、checksum、签名和 provenance；
- E2E 全跑在 `ubuntu-latest`，基础镜像与 runner 漂移可能造成不稳定，关键依赖应适度固定；
- CI 状态没有在 README badge 或贡献文档中解释。

### 3.2 Makefile

Makefile 的 build/test/codegen/testenv 目标清楚，版本信息通过 ldflags 注入，静态构建和平台矩阵有明确策略。

问题是 `make check` 只包含 fmt、vet、Go test，不包含 tidy、proto/sqlc 一致性、Web build/test、Pack lint 或未来静态工具，名称“全套检查”会误导。建议：

- `check-fast`：fmt、vet/staticcheck、unit、tidy、generated；
- `check-web`：npm ci、lint、typecheck、test、build；
- `check-all`：fast + web + pack + 能在本机运行的集成测试；
- CI job 继续拆分，但共同调用这些目标，减少本地/CI 差异。

`tools` 使用 `sqlc@latest` 不可复现，应 pin 到已验证版本；开发工具版本可以进入 `go.mod tool` 或专用版本文件。

## 4. 发布与供应链

当前没有 Git tag、release workflow、GoReleaser/等价配置、包仓库、安装脚本、checksum、制品签名、SBOM 或 provenance。`make dist` 只生成裸二进制，未包含：

- 许可证/NOTICE 与版本说明；
- systemd unit、默认配置和目录初始化；
- shell completion；
- 校验和与签名；
- 升级/回滚说明；
- Linux 发行版包或 archive 布局。

建议首发产物最少包括：

1. `mechctl`/`mechpack` 多平台压缩包；
2. Linux amd64/arm64 的 mechd/mechlet 包，附 systemd units；
3. `SHA256SUMS`、签名、SBOM 与构建 provenance；
4. 版本化 release notes、已知问题、兼容矩阵；
5. 从干净 VM 验证安装、启动、卸载和升级的 release job。

不要把 Pack 签名与项目发布制品签名混为一件事：前者证明 Pack 发布者，后者证明 Mecharion 二进制发布者，两条链都需要。

控制面启动时会自动执行数据库 Up migration，虽然每份 migration 都提供 Down 且 Store 有回退接口，但没有面向操作者的“升级前备份、二进制/Schema 兼容、失败恢复”流程。发布流水线应从旧版本数据库副本执行升级，再验证旧版回退或明确声明不可回退；不能把 goose 的 Down 函数等同于经过验证的产品降级能力。

## 5. 开源社区成熟度

### 5.1 已有基础

- Apache-2.0 LICENSE 完整，README 有 License 入口；
- README 有定位、状态、构建、文档和参与说明；
- 设计决策透明，适合外部技术评审；
- 核心代码全部位于 `internal/`，诚实表达尚未承诺 Go library API；
- 示例和测试环境能帮助未来贡献者理解产品约束。

### 5.2 缺失文件与机制

当前未见：

- `CONTRIBUTING.md`
- `CODE_OF_CONDUCT.md`
- `SECURITY.md`
- `CHANGELOG.md`
- `GOVERNANCE.md`
- `MAINTAINERS.md`
- `SUPPORT.md`
- `.github/CODEOWNERS`
- Issue/PR 模板
- Dependabot/Renovate 配置
- release 配置与版本政策

在真正吸引外部贡献前，最小集合是 CONTRIBUTING、CODE_OF_CONDUCT、SECURITY、PR/Issue 模板、CODEOWNERS 和发布/兼容政策。Governance 可以等出现第二位长期 maintainer 后再细化，但安全披露方式不能等生产用户出现后补。

### 5.3 Git 历史

当前只有 4 个提交和 0 个 tag，主要能力被压在两个超大里程碑提交中。这不影响运行，却降低：

- `git bisect` 的价值；
- 外部 reviewer 理解决策的能力；
- 变更日志自动生成质量；
- 对 AI Agent 改动进行逐步审查和回退的能力。

建议今后以“一项可独立验证的不变式/能力”为提交单位，提交信息说明 why 和验证方式；大规模机械生成与业务变更分开提交。禁止为了美化历史重写已经公开的提交。

## 6. 文档与开源整改顺序

1. 先修当前事实：`--local`、mechd 可选、10/12 Pack、冻结语义、失效链接、陈旧入口注释。
2. 新增真实 quickstart 和最小运维手册，并由 release acceptance 执行。
3. 从代码/schema 生成 CLI/API 参考，设计讨论回到 ADR。
4. 加贡献、安全、行为准则、模板、CODEOWNERS。
5. 建立 alpha release、checksum、SBOM、签名和升级说明。
6. 产品契约稳定后，再做英文默认文档和完整网站。
