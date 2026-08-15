# ADR-0001: 采用常驻 Agent 架构

- **状态**：已接受
- **日期**：2026-08-01
- **相关**：[ADR-0002](0002-mechlet-as-sole-engine.md)

## 背景

部署与配置管理工具在「如何触达目标机器」上有两条成熟路线：

- **agentless（推模式）**：控制机通过 SSH 连接目标，推送并执行。代表：Ansible
- **常驻 agent（拉模式）**：目标机运行长期进程，周期性获取期望状态并调和。代表：Puppet、Chef、Salt、Cloudera Manager、Kubernetes

Mecharion 的产品目标包含**状态管理**（漂移检测、自愈）与**可靠升级**，且场景横跨中心化数据中心到边缘离线单机。

## 候选方案与调研

| | agentless（Ansible 式） | 常驻 agent（Puppet/CM 式） |
|---|---|---|
| 首次使用门槛 | ✅ 零安装 | ❌ 需先装 agent |
| 持续漂移检测 | ❌ **结构性做不到**——只能在每次执行时 pull 式探测 | ✅ |
| 断连自治与自愈 | ❌ | ✅ |
| 实时状态 / 日志流 | ❌ 每次需建 SSH 连接 | ✅ 复用长连接 |
| NAT / 防火墙后的边缘节点 | ❌ 要求控制机能入站访问目标 | ✅ agent 主动拨出 |
| 节点暴露面 | ❌ SSH 需长期开放 | ✅ 零入站端口 |
| agent 自身的生命周期 | 无此问题 | ❌ 引导、版本兼容、自升级、断连重试 |
| 大规模并发 | ❌ 控制机成为瓶颈 | ✅ 计算分散到节点 |

调研的具体实践：

- **Ansible** — 无持久化期望状态，因此 `--check` 只能回答「如果现在执行会改什么」，无法回答「谁在什么时候改坏了什么」。这是它在「状态管理」这一诉求上的天花板。
- **Cloudera Manager** — `cloudera-scm-agent` 常驻，正是它能提供服务健康、自动重启、滚动升级的基础。
- **Salt** — minion 主动连 master 的 ZeroMQ 长连，验证了「agent 拨出」在大规模与 NAT 场景的可行性。
- **Balena / Azure IoT Edge** — 边缘领域几乎全部采用常驻 agent，因为边缘设备提供入站访问不现实。

## 决策

**采用常驻 Agent 架构。** 节点代理为 `mechlet`，连接方向为 mechlet 主动拨出到 mechd（长连 gRPC 双向流）。

同时提供两条缓解路径：

1. **`mechctl node bootstrap ssh://…`** — 一次性 SSH 推送单个静态二进制 + systemd unit + join token。SSH 只用于引导，之后不再使用。
2. **one-shot 模式** — `mechlet` 支持「推过去 → 执行一次调和 → 自删」，复用完全相同的执行引擎，零额外代码换来一个 agentless 逃生舱，供不允许安装常驻代理的主机使用。

## 理由

决定性的是**产品定位**而非技术偏好：目标里写着「状态管理」和「升级」，这两件事只有在系统持有期望状态、且能持续观测实际状态时才可能做对。agentless 在结构上无法提供后者。

放弃这一点，产品就退化成一个更差的 Ansible——没有理由存在。

用户方基于大量 Ansible 实战经验明确指出「没有 agent 存在很多能力无法实现」，与上述分析一致。

## 后果

### 收益

- 持续调和、漂移检测与自愈成为一等特性
- 边缘 NAT/防火墙场景天然可行
- 节点零入站端口，长期暴露面小于「SSH 常开」
- 断连期间节点自治，控制面故障不影响业务

### 代价

- **引导成本**：必须解决「agent 怎么装上去」。通过 bootstrap 命令 + 离线本地安装 + 镜像预置三条路径覆盖。
- **agent 自升级风险**：升级正在运行的自己是有风险的操作。必须在节点规模上量前完成 watchdog + 自动回退机制（见 [01-architecture.md §4](../design/01-architecture.md#4-mechlet-自升级)），否则一次坏版本推送就是全网手工修复。
- **仍需内置 SSH 客户端**：虽然只服务于 bootstrap 一个命令。
- **首次试用门槛高于 Ansible**：以 one-shot 模式和一条命令的 bootstrap 缓解。

## 参考

- Ansible Architecture — https://docs.ansible.com/
- Cloudera Manager Agent — Cloudera Manager 架构文档
- Salt Architecture (minion/master ZeroMQ)
- Balena Supervisor — 边缘设备常驻 agent 实践
