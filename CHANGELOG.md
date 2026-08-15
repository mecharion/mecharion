# Changelog

格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

**0.y.z 阶段不承诺兼容性**——语义化版本的标准约定：主版本号为 0 时，任何次版本号变化都可能包含破坏性改动。`v0.1.0-alpha.N`/`v0.1.0-beta.N` 这类预发布标识符的含义见 [`docs/dev/M10-boundary-and-contract/plan.md`](docs/dev/M10-boundary-and-contract/plan.md#验收标准) 的验收标准——不是纯粹的版本号仪式，标着 alpha 还是 beta 对应着具体的、可核对的完成条件。

## [未发布]

从这里往后的条目，随每次发布补充。

## [v0.1.0-alpha.1] - 2026-08-15

首个公开预发布。单机与多节点的完整链路都已跑通：资源引擎与三个
runtime（systemd / docker / compose）、控制面 `mechd`、持续调和与
漂移检测、升级与自动回滚、多节点 mTLS 加入与分批滚动升级、Web UI、
组件生命周期（`remove` / `orphans` / `apply` / `restart`）。

- [pack/v1 规范](docs/spec/pack-v1.md) 是 draft-stable：格式已经稳定到可以据此写 Pack，实现过程中发现的问题仍可能导致调整，变更会记录。
- 发布制品（`mechd`/`mechlet`/`mechctl`/`mechpack`）附带 checksum、SBOM 与 cosign keyless 签名，见 [CONTRIBUTING.md](CONTRIBUTING.md#版本号与发布)。
- **尚不可用于任何生产场景**——接口与磁盘布局都可能再变。

完整能力边界见 [README](README.md)，里程碑历史见 [docs/design/25-roadmap.md](docs/design/25-roadmap.md)。
