# 总体架构

## 1. 分层

```
┌──────────────────────────────────────────────────────────────┐
│  mechctl (CLI)          webui (go:embed 进 mechd)             │
├──────────────────────────────────────────────────────────────┤
│  mechd  — 协调层                                              │
│    · 期望状态存储（Site/Component/Role/Node）  SQLite         │
│    · blob 存储与 Pack 注册                                     │
│    · 放置解析、参数解析、拓扑渲染                               │
│    · Rollout 编排（分批 / 健康门禁 / 暂停 / 回滚）              │
│    · HTTP API / RBAC / 审计 / WebUI                           │
├──────────────────────────────────────────────────────────────┤
│  mechlet — 唯一执行引擎                                        │
│    · 资源引擎（file/template/user/dir/archive/…）              │
│    · generation 管理 + 原子切换 + 回滚                         │
│    · 健康检查（http / tcp / exec）                             │
│    · 持续调和与漂移检测                                        │
│    ├─ Runtime 接口 ─────────────────────────────────────────  │
│    │    systemd  │  docker  │  compose  │  (podman)          │
└──────────────────────────────────────────────────────────────┘
```

关键性质：**横线以上的所有逻辑跨 Runtime 共享，只写一次**。见[原则五](00-overview.md#原则五抽象只在真正不同处划界)。

## 2. 两种部署形态

**mechd 不是「可选」，而是「可以与 mechlet 同机」**（[ADR-0026](../adr/0026-standalone-runs-mechd.md)）。两种形态共用同一份存储、同一套 API、同一个 WebUI——**唯一的差异是 mechd 与 mechlet 之间的传输方式**。

### 2.1 边缘 / 单机（基准形态）

```
                 ┌──────────── 一台机器 ────────────┐
浏览器 ──HTTPS──> │  mechd                          │
mechctl ────────> │    ├ HTTP  0.0.0.0:8443 (TLS)   │
                  │    └ gRPC  unix socket ─────┐   │
                  │                             ↓   │
                  │  mechlet ──> systemd / docker    │
                  │    SQLite + blobs 在本机         │
                  └─────────────────────────────────┘
```

```bash
mechlet install --standalone     # 一条命令，装 mechd + mechlet 两个 unit
```

- **功能与多节点完全一致**：完整的查询、事件、审计、WebUI
- 不需要网络——`mechd` 与 `mechlet` 经本机 unix socket 通信
- Pack 来自本地 `.mpack` 文件（U 盘交付）

### 2.2 中心化

```
mechctl ──HTTPS──> mechd <──gRPC/mTLS── mechlet(node-1)
浏览器  ──HTTPS──>   │     (agent 主动拨出)  mechlet(node-2)
              SQLite + blobs                mechlet(node-N)
```

### 2.3 本机通信为什么走 unix socket 且不用 mTLS

| | unix socket | localhost TCP |
|---|---|---|
| 认证 | **内核保证的对端身份**（`0600 root:root`） | 裸 TCP 则本机任何进程可连；要安全就得上 TLS |
| 证书 | 不需要 | 单机上生成/轮换/信任一套证书纯属负担 |
| 端口 | 不占 | 可能与用户组件冲突 |

**mTLS 叠在 unix socket 上是负收益**——socket 权限是内核强制、无法伪造的保证，比证书更强。

这不构成代码分叉：gRPC 的 service 定义、handler、序列化全部共用，差异只是一行传输凭据。

### 2.4 为什么 mechlet 主动拨出

连接方向是 **mechlet → mechd**，长连 gRPC 双向流。mechd 从不需要反向连接任何节点。

| 收益 | 说明 |
|---|---|
| 穿透 NAT 与防火墙 | 边缘节点（门店、产线、基站）几乎不可能提供入站端口 |
| 节点侧零暴露面 | 节点不监听任何对外端口，攻击面小于 Ansible 要求的 SSH 常开 |
| 天然的存活感知 | 连接断开即是节点失联信号，不需要额外探活 |
| 断连自治 | 见 §5 |

代价：mechd 无法「立即推送」到已断连的节点，命令需要排队等待重连。对 ALM 场景可接受。

## 3. 引导（bootstrap）

Agent 模式唯一的真实成本是「代理本身怎么装上去」。三条路径：

| 场景 | 方式 |
|---|---|
| 有 SSH 可达 | `mechctl node bootstrap ssh://root@host --token …` — 一次性推送单个静态二进制 + unit + token |
| 完全离线 | 拷贝二进制后本机执行 `mechlet install --token …` |
| 批量预置 | 打进系统镜像 / cloud-init / kickstart |

**SSH 只用于 bootstrap，之后再不使用。** 因此 Mecharion 仍需内置一个 SSH 客户端，但它只服务于一个命令。

### 3.1 one-shot 模式（agentless 逃生舱）

某些主机不允许安装常驻代理。`mechlet` 支持一次性模式：推送 → 执行一次调和 → 自删。

```
mechctl node exec --one-shot ssh://host -f site.yaml
```

这**复用完全相同的执行引擎**，零额外代码，换来一个 agentless 后备路径。代价是该主机上没有持续调和与漂移检测——这正是常驻模式的价值所在。

## 4. mechlet 自升级

mechlet 需要升级正在运行的自己。做法与组件升级一致，复用 generation 机制：

```
/usr/local/lib/mecharion/generations/0003-0.4.1/   ← 新版本写入
/usr/local/lib/mecharion/current -> generations/0003-0.4.1
/usr/bin/mechlet -> /usr/local/lib/mecharion/current/bin/mechlet   ← 不动
```

**PATH 上的软链指向 `current`，因此升级只切一次中间那条软链**，
`/usr/bin` 下的四条自始至终不变。这也是实体二进制不能直接放 `/usr/bin`
的原因：覆盖一个正在执行的文件既不原子也无法回滚
（[04-paths-and-storage](04-paths-and-storage.md#三条布局判据)）。

1. 下载/接收新版本 blob，校验签名与 sha256
2. 物化到新 generation 目录
3. 原子切换 `current` 软链
4. `systemctl restart mechlet`
5. **watchdog**：新进程若在 N 秒内未成功回连 mechd（或本地自检失败），由 systemd `OnFailure` 触发的回滚单元切回上一 generation 并重启

第 5 步不可省略。在有 100 个节点之前必须完成——否则一次坏版本推送就是全网手工修复。

> 单机形态下 mechd 在同一台机器上，watchdog 的回连检查照常进行——只是走 unix socket。

### 4.1 单机形态下的升级顺序

mechd 与 mechlet **始终同步升级**，由 `mechlet install` 一并处理。

跨版本时先 mechd 后 mechlet——控制面向后兼容 agent 是既定的兼容方向（新 mechd 能服务旧 mechlet，反之不保证）。

### 4.2 启动依赖用重试，不用 systemd `Requires=`

mechlet 启动时 mechd 可能尚未就绪。**用连接重试处理，不在 unit 里声明依赖。**

理由：多节点形态下本机根本没有 mechd，mechlet 本来就必须处理「mechd 不可达」。用同一套重试逻辑覆盖两种形态，避免 unit 文件出现单机/多节点两个版本。

## 5. 断连自治

mechlet 与 mechd 失联时（单机形态下即 mechd 进程异常）：

- **继续按最后已知的期望状态调和与自愈**——业务不受控制面故障影响
- 事件写入本地有上限的 JSONL 缓冲，重连后补报给 mechd
- 不接受新的期望状态变更（没有可信来源）
- `mechctl --local` 提供本机**只读**诊断视图，供现场运维介入

这是「控制面不在数据面上」的直接体现，也是 [ADR-0014](../adr/0014-no-ha-in-v1.md)（v1 不做 HA）成立的前提：mechd 短暂不可用不会造成业务中断。

**单机形态同样成立**：mechd 挂了，本机组件继续运行并自愈，只是暂时不能下发变更、看不到 UI。

## 6. 一次 apply 的完整链路

以中心化形态、部署一个多角色 Component 为例：

```
1. mechctl apply -f site.yaml
       │
2. mechd 校验 → 解析 Pack 依赖 → 确定 Role 放置（RoleInstance 列表）
       │        ★ 放置必须先于渲染，因为 topology.* 引用依赖放置结果
3. mechd 解析参数（Pack 默认 → Component → Role → Node 覆盖）
       │
4. mechd 渲染模板，生成每个 RoleInstance 的「已解析规格」
       │        ★ mechlet 收到的是拓扑快照，不需要自己去查别的节点
5. mechd 创建 Rollout，按 Role 的 requires 依赖排序、分批下发
       │
6. mechlet 收到规格 → 拉取缺失 blob（按 sha256，已有则跳过）
       │
7. mechlet 物化新 generation → 应用资源 → Runtime.Materialize
       │
8. mechlet 原子切换 → Runtime.Start → 健康检查
       │
9. 成功：上报 → Rollout 推进下一批
   失败：切回上一 generation → 上报 → Rollout 暂停
```

第 2–4 步在 mechd 完成是刻意的：**跨角色拓扑引用（`topology.role('primary').nodes[0]`）只有在放置确定后才能解析**，且让 mechlet 保持无状态、不需要相互查询，是整个系统能够简单的关键。

**单机形态走的是完全相同的九步**——只是 mechd 与 mechlet 在同一台机器上，第 5→6 步之间的传输从 gRPC/mTLS 换成 gRPC/unix socket。放置结果平凡（全部落在本机），但解析逻辑一行都不变。

> 这正是 [ADR-0026](../adr/0026-standalone-runs-mechd.md) 的核心收益：单机不是「简化流程」，而是「同一流程的同机部署」。

## 7. 单机形态的资源要求

mechd（Go + SQLite + 嵌入的 WebUI）与 mechlet 同机运行：

| 项 | 约 |
|---|---|
| 磁盘（两个二进制） | 40 MB |
| 常驻内存（RSS，两进程合计） | 60–100 MB |
| 额外 | SQLite 数据文件、blob 目录随组件数量增长 |

**内存 ≤256MB 的极小工控设备不适用。** 这是同机运行 mechd 换来功能完整性的代价（[ADR-0026](../adr/0026-standalone-runs-mechd.md)）。

## 8. 相关决策

- [ADR-0001 采用常驻 Agent 架构](../adr/0001-agent-based.md)
- [ADR-0002 mechlet 为唯一执行引擎，mechd 为协调层](../adr/0002-mechlet-as-sole-engine.md)
- [ADR-0026 单机形态同机运行 mechd，存储与 API 不分叉](../adr/0026-standalone-runs-mechd.md)
