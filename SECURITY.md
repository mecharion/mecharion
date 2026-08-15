# 安全策略

## 支持的版本

Mecharion 目前处于早期实现阶段，还没有发布过任何 tag（见 [README「状态」](README.md#状态)）。安全报告请针对 `main` 分支的最新代码。

## 报告一个漏洞

**请不要通过公开 Issue 报告安全漏洞。** 公开 Issue 在漏洞修复并发布之前，等于把利用方法告诉所有人。

请通过 GitHub 的私密漏洞报告功能提交：

> **[Report a vulnerability](https://github.com/mecharion/mecharion/security/advisories/new)**（仓库 Security 标签页 → "Report a vulnerability"）

报告中请尽量包含：

- 漏洞的类型与位置（文件/函数，或触发它的具体操作）
- 复现步骤或可运行的 PoC
- 影响面：能做到什么（读取任意文件？绕过认证？在目标节点执行代码？）
- 是否已经有公开利用代码

我们会在 **3 个工作日**内确认收到，并在评估后告知后续处理节奏。修复发布前，请不要公开细节。

## 这个项目的信任边界

在判断某个行为是不是"漏洞"之前，先看 [08-security.md](docs/design/08-security.md) 与相关 ADR 里已经写明的设计边界，这些是**有意的**，不是需要报告的问题：

- **部署一个 Pack 等价于在目标机器执行 root 代码**（[08-security §1](docs/design/08-security.md#1-信任模型先说清楚边界)）。Mecharion 不做 Pack 来源的身份认证——信任来自哪个 Pack、装不装，是运维方自己的判断（[ADR-0040](docs/adr/0040-pack-trust-is-operator-responsibility.md)）。"我能用一个自己制作的恶意 Pack 让 mechlet 执行任意代码"不是一个需要报告的漏洞，这是设计如此。
- **单机形态下 `mechlet`/`mechctl --local` 需要本机 root/对应 socket 的访问权限**——socket 文件权限就是这条通道的认证（ADR-0026）。"本机 root 能读到 mechlet 的本地状态"同样不是漏洞。
- 一张被吊销的证书**握手仍会成功**，但随后的每个 RPC 都会被拒绝——这是应用层吊销而非 CRL 的既知代价（ADR-0034），不是握手校验没生效。

如果不确定某个发现算不算这个范畴，请照样按上面的流程报告，我们会一起判断。
