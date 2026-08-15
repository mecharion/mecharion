<p align="center">
  <img src="docs/brand/mecharion-logo.svg" alt="Mecharion" width="120" />
</p>

# Mecharion

> **Mecharion** — *MEK-uh-RY-un*, rhymes with *Orion*. From **mech**anism + **Arion**, the immortal horse of Greek myth. Community shorthand: **m7n**.

应用生命周期管理工具。用 Go 编写，同时提供 CLI 与可视化界面，管理 Linux 环境下组件的**打包、部署、配置、升级、状态监控与运维**全过程。

**离线优先**——部署阶段不访问任何外部服务：不用 apt/yum 源，不用容器 registry，不做编译。从中心化数据中心到完全离线的边缘单机，走的是同一条代码路径。

```bash
mechctl component deploy postgresql -c pg-main --profile replicated
mechctl component status pg-main
mechctl component rollback pg-main
```

---

## 状态

**早期实现阶段，目前处于快速迭代期。** [pack/v1 规范](docs/spec/pack-v1.md)是 draft-stable（M1 定型骨架）
——格式已经稳定到可以据此写 Pack，但实现过程中发现的表达缺失或别扭之处仍可能调整，变更会记录，不会悄悄发生。
单机与多节点的完整链路都已跑通（M2–M7）：资源引擎、systemd / docker /
compose 三个 runtime、控制面 `mechd`、持续调和与漂移检测、升级与自动回滚、
多节点 mTLS 与分批滚动升级。

**Web UI 已完成（M8）**：由 params schema 自动生成配置表单、单一 admin
账号、部署与改配置、ConfigGroup、Rollout 实时进度（SSE）、节点加入引导、
`.mpack` 上传。

**生命周期已完整（M9）**：`component remove`（默认保留数据、二档确认、
影响面预览）、`orphans` 发现与清理、`mechctl apply -f` 声明式入口、
`component restart` 与它带出来的
[ad-hoc 命令通道](docs/adr/0038-adhoc-task-channel.md)。

下一步是 M10（打磨、文档与首个公开版本）。

**关于「文档里写了但代码里没有」**：M8 收尾审查发现 10-cli 里有十几个
动词只存在于文档。M9 把该做的做了，其余**显式标注未实现并写明理由**——
不删掉，因为那会让设计连同理由一起消失。现在有一条自动化守卫钉住这件事
（`internal/cli/ctlcmd/docdrift_test.go`）：文档里每一条命令，要么真的
存在，要么标着未实现。

**尚不可用于任何生产场景**：还没有发布过版本，接口与磁盘布局都可能再变。

```bash
# 打包（自己的应用；下面的 myapp 只是示意，跑不了）
mechpack init myapp                      # 生成 Pack 骨架
mechpack assemble myapp                  # 组装（不构建你的软件，只组装）
mechpack lint --hermetic dist/myapp-0.1.0-1

# 单机：装完就能用，mechd 跑在同一台机器上
mechlet install --standalone
systemctl enable --now mecharion-mechd mecharion-mechlet

# 部署一个真实、可运行的示例（examples/quickstart/hello/，见该目录 README）
./hack/quickstartpack.sh                        # 现场打包：真实二进制 + 真实 sha256
mechctl pack upload dist/hello-0.1.0-1.mpack
mechctl component deploy hello -c web --nodes $(hostname)
mechctl component status web                    # 已收敛、healthy

# 多节点：token 带外交付，一次性 SSH 推送，之后不再用 SSH
mechctl node token create --ttl 30m
mechctl node bootstrap ssh://root@store-042 --token m7n_… --ca-hash sha256:…

# 滚动升级：按 canary → 其余分批，每批过健康门禁才放行下一批
mechctl component upgrade web --version 1.4.0
mechctl rollout status web               # 第 2/3 批（正在做 default store-042）
```

"单机"这几行是**逐字可执行**的：CI 在一台跑 systemd 的真实容器里从零重放这整段
（见 [`.github/workflows/ci.yml`](.github/workflows/ci.yml) 的 `quickstart` job），
跑不通会直接把 CI 标红——不会出现"文档说能跑、代码变了没人发现"的漂移
（20260809 全面审阅 REV-022）。

其余验收也都在跑着 systemd 的真实容器里完成，见 [test/](test/README.md)——
单机一套、三节点一套，后者验的是 mTLS、加入、吊销、cordon 与分批滚动升级。

## 它解决什么

| 参照 | 吸收了什么 | 不同在哪 |
|---|---|---|
| **Ansible** | 主机配置、临时命令执行、幂等资源模型 | Ansible 不持久化期望状态，无法回答「现在偏离了吗」 |
| **Cloudera Manager** | 组件生命周期、多角色模型、滚动升级、参数化配置界面 | CM 深度绑定 Hadoop 且闭源；Mecharion 面向任意组件 |

一句话：**用 Cloudera Manager 的模型深度，管理 Ansible 那样广泛的目标。**

## 组件

| 二进制 | 角色 |
|---|---|
| `mechctl` | CLI，连 `mechd`。`--local` 直连本机 `mechlet` 的只读诊断视图，仅供 mechd 不可达时现场排障用 |
| `mechd` | 控制面。期望状态、blob 存储、Rollout 编排、API、Web UI |
| `mechlet` | 节点代理，**唯一的执行引擎** |
| `mechpack` | 组件包工具。**只组装 Pack，不构建你的软件** |

**mechd 不是"可选"，而是"可以与 mechlet 同机部署"**（[ADR-0026](docs/adr/0026-standalone-runs-mechd.md)）：单机形态下 `mechlet install --standalone` 一条命令同机装好 mechd 与 mechlet，功能与多节点完全一致——完整的审计、查询与 Web UI，不是阉割版。**边缘离线单机是基准形态，中心化是叠加在其上的一层**，不是反过来；控制面与数据面的分离仍然成立——mechd 不可用时 mechlet 继续按最后已知的期望状态自愈，只是暂时不能下发变更、看不到 UI。

## 从源码构建

需要 **Go 1.25+**。

```bash
git clone https://github.com/mecharion/mecharion.git
cd mecharion
make build        # → bin/
./bin/mechctl version
```

无 `make` 时直接用 go：

```bash
go build ./...
go run ./cmd/mechctl version
```

提交前：

```bash
make check        # fmt-check + vet + test
```

## 文档

| | |
|---|---|
| [设计总览与七条设计原则](docs/design/00-overview.md) | 从这里开始 |
| [总体架构](docs/design/01-architecture.md) | mechlet / mechd / mechctl 的职责与关系 |
| [对象模型](docs/design/02-object-model.md) | Site / Component / Role / RoleInstance |
| [**pack/v1 规范**](docs/spec/pack-v1.md) | 组件包格式契约 |
| [**Pack 示例**](examples/packs/README.md) | 12 个真实组件，用于验证规范（sha256 是占位符，装不了） |
| [**Quickstart 示例**](examples/quickstart/README.md) | 上面"单机"那几行用的真实可部署示例 |
| [架构决策记录](docs/adr/README.md) | 每个选择的调研、理由与**代价** |
| [CLI 参考](docs/design/10-cli.md) | 动词表与输出格式 |
| [路线图](docs/design/25-roadmap.md) | M0 → M10，含各阶段已定下的技术选型与支持范围 |

设计文档记录**为什么是这样**，不只是**是什么**——顶层对象为何叫 `Site` 而不是 `Cluster`、控制面为何用 SQLite 而不是 BoltDB，都在 ADR 里有调研对比与承担的代价。

## 参与

欢迎在 Issue 中讨论设计。内部接口仍在快速变化（`internal/` 下的包尚未稳定），
但 [pack/v1 规范](docs/spec/pack-v1.md)是 draft-stable——格式已经稳定到可以据此写 Pack，但 v0.1.0 发布前仍可能因发现的问题调整（变更会记录）。

提交代码前请看 [CONTRIBUTING.md](CONTRIBUTING.md)；参与本项目的所有互动适用 [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)；安全漏洞请勿走公开 Issue，见 [SECURITY.md](SECURITY.md)。

## License

[Apache-2.0](LICENSE)
