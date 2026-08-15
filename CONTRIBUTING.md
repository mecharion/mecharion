# 贡献指南

Mecharion 目前处于早期实现阶段（见 [README「状态」](README.md#状态)）：内部接口仍在快速变化，`pack/v1` 规范是 draft-stable。欢迎在 Issue 中讨论设计，提交代码前建议先开一个 Issue 对齐方向——尤其是涉及对象模型、CLI 命令面、Pack 格式这类跨越多处的改动，避免大改动写完了才发现方向不对。

## 开发环境

需要 **Go 1.25+**。

```bash
git clone https://github.com/mecharion/mecharion.git
cd mecharion
make build        # 编译全部二进制 → bin/
make check        # = check-fast：fmt-check + vet + test，日常开发时跑这个
```

`check` 分三层（REV-032），按"要不要装 Node""要不要联网装工具"分，不是按"重不重要"分：

| 目标 | 内容 | 需要 |
|---|---|---|
| `make check` / `make check-fast` | fmt-check + vet + test | 只需要 Go |
| `make check-web` | Web UI 的 lint + 测试 | Node |
| `make check-all` | 以上全部 + tidy/proto/sqlc 一致性 + staticcheck/govulncheck/gosec/secret 扫描/链接检查 | Go + Node + 联网 + 本地装好 lychee |

日常改 Go 代码用 `make check` 就够；推送前想全面确认一遍用 `make check-all`。

改动涉及 Web UI 时，额外需要 Node：

```bash
make webui        # 构建 Web UI 并拷进 internal/webui/dist（ADR-0036：产物不进版本库）
make check-web    # lint + 测试
```

改动涉及 `internal/store/queries`（SQL）或 `proto/`（gRPC）时：

```bash
make sqlc          # 需先 make tools
make proto
```

`make help` 列出全部可用目标。

## 静态分析与安全扫描

`make check-all` 包含这一组（20260809 全面审阅 REV-020），也可以单独跑其中任意一项：

```bash
make staticcheck    # 深度静态分析（阻断 CI）
make govulncheck    # 已知漏洞依赖扫描（阻断 CI）
make gosec          # 安全模式扫描——建议性，不阻断；195 处基线发现见 ci.yml 的说明
make gitleaks       # secret 扫描，含 git 历史（阻断 CI）；已知误报见 .gitleaksignore
make lychee         # Markdown 链接检查（阻断 CI）；需要本地已装 lychee，见该目标的说明
```

`gosec` 的发现刻意不阻断 CI——**不要**因为它报了就一股脑加 `#nosec` 注释或"修复"：绝大多数发现是已知的误报模式（清理路径里的 `Close`/`Remove` 错误没检查、这个项目里每处权限位都有过真实威胁建模的选择），把它当人工复核的信号源，不是必须清零的门槛。

## 测试

单元测试（`go test ./...`）在任意平台都能跑，但一部分行为——软链原子切换、`chmod`/`chown`、`useradd`/`getent` 的真实参数、systemd 是否真的认一份生成的 unit——只有在真实 Linux + systemd 环境下才验证得出来；这类用例在开发机上会自行跳过。完整验收需要一台能跑 Docker 的 Linux（或 WSL2）：

```bash
make e2e            # 交叉编译 + 起 systemd 容器 + 跑全部真机测试
```

细节见 [test/README.md](test/README.md)。

## Pack 示例

`examples/packs/` 是 pack/v1 规范的验证夹具，`sha256` 是占位符，**不能真实部署**——它们的作用是压测格式与 lint 规则，不是教程。真实可部署的示例在 [`examples/quickstart/`](examples/quickstart/README.md)。改动 pack/v1 规范时，示例目录下的 lint（`mechpack lint --hermetic --strict examples/packs`）必须保持通过。

## 设计决策记录（ADR）

改变一个已有决策，或做出一个非显然的架构选择时，请写一份 ADR（见 [`docs/adr/README.md`](docs/adr/README.md) 的模板与约定）。ADR 只追加不修改——推翻旧决策的方式是新写一份并把旧的状态改成「已被 ADR-XXXX 取代」。**没有代价的决策通常意味着没想清楚**：调研过的候选方案、承担的代价，是 ADR 里跟结论同样重要的部分。

## 提交与 PR

- 提交信息说清楚"为什么"，不只是"改了什么"——commit message 本身就是给后来者的第一手材料。
- 一个 PR 对应一件可独立验证的事；不要把生成物/机械改动和语义改动混进同一次提交（见 `docs/design/decision-log.md` 里踩过的教训）。
- PR 会跑 [CI](.github/workflows/ci.yml)：格式、`vet`、静态分析（staticcheck/govulncheck/gosec/secret 扫描）、单元测试（三平台）、Web UI 构建/lint/测试、交叉编译、容器化 E2E、三节点里程碑验收、示例 Pack lint、Markdown 链接检查、README quickstart 端到端重放。全绿是合入的前提，不是唯一门槛——不改代码只改文档时同样欢迎，但涉及行为的改动请一并补测试。

## 版本号与发布

版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)。**0.y.z 阶段不承诺兼容性**——这是 SemVer 的标准约定，不是这个项目额外加的限制。`v0.1.0-alpha.N`/`v0.1.0-beta.N` 这类预发布标识符不是版本号仪式：alpha 与 beta 各自对应一组具体、可核对的完成条件，见 [`docs/dev/M10-boundary-and-contract/plan.md`](docs/dev/M10-boundary-and-contract/plan.md#验收标准) 的验收标准。

发布制品（`mechd`/`mechlet`/`mechctl`/`mechpack` 的跨平台归档，见 [`.goreleaser.yaml`](.goreleaser.yaml)）附带 checksum、SBOM（每个二进制一份 CycloneDX JSON）与 cosign keyless 签名——签的是"这份制品确实由本仓库的 CI 构建、没被篡改"，不是"里面部署的 Pack 内容可信"，两者是不同的问题（[ADR-0040](docs/adr/0040-pack-trust-is-operator-responsibility.md)）。

给维护者的操作步骤：

```bash
# 1. 更新 CHANGELOG.md 的"未发布"章节为本次版本号，另起一个空的"未发布"
# 2. 提交
# 3. 打 tag 并推送——推送即触发 release workflow，之后的构建/打包/
#    签名/发布全部自动完成
git tag v0.1.0-alpha.1
git push origin v0.1.0-alpha.1
```

本地想在打 tag 之前先看一遍构建产物长什么样：

```bash
make release-tools     # 只需要跑一次
make release-dry-run    # 构建+打包+生成 SBOM，不签名、不发布、不需要 tag
```

## 行为准则

参与本项目的所有互动（Issue、PR、讨论）适用 [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)。

## 安全问题

不要通过公开 Issue 报告安全漏洞，见 [SECURITY.md](SECURITY.md)。
