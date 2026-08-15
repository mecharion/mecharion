# mechd ↔ mechlet 协议

gRPC。单机走 unix socket（无 mTLS），多机走 mTLS
（[01-architecture §2.3](01-architecture.md#23-本机通信为什么走-unix-socket-且不用-mtls)）。

**mechlet 主动拨出**，节点上零入站端口（[§2.4](01-architecture.md#24-为什么-mechlet-主动拨出)）。

---

## 1. 形态：服务端流下发 + 一元上报

```protobuf
service Agent {
  // 注册：交换版本与节点能力，换取会话
  rpc Register(RegisterRequest) returns (RegisterResponse);

  // ★ 服务端流：mechlet 拨出后挂住，mechd 按需推送规格
  rpc Subscribe(SubscribeRequest) returns (stream Assignment);

  // 一元：上报观测状态（高频小包）
  rpc Report(ReportRequest) returns (ReportResponse);

  // 一元：补报断连期间缓冲的事件
  rpc PushEvents(PushEventsRequest) returns (PushEventsResponse);

  // 一元：按 sha256 取 blob（分块）
  rpc FetchBlob(FetchBlobRequest) returns (stream BlobChunk);

  // 一元：用**当前仍然有效的证书**换一张新的（M7）
  //
  // 不需要 token——现有证书就是身份证明。剩余期限由 mechlet 侧判断：
  // 证书在谁手上谁最清楚，而且这让「断连三个月后回来」这件事有明确
  // 行为——它一连上就发现自己过期了，走重新加入而不是悄悄用一张废证书。
  rpc RenewCert(RenewCertRequest) returns (RenewCertResponse);
}
```

**为什么不是双向流**：上报与下发的失败模式不同。一条双向流上，一次上报失败会
连带撕掉下发通道；分开之后，心跳丢几个包不影响正在进行的部署。

**为什么不是纯轮询**：用户点了 deploy 希望机器立刻动起来。轮询要么延迟大，
要么把间隔压到很小而变成变相的长连接。

详见 [ADR-0029](../adr/0029-push-over-server-stream.md)。

## 2. 版本协商

```protobuf
message RegisterRequest {
  string node_name = 1;
  string agent_version = 2;        // mechlet 的版本
  uint32 max_spec_schema = 3;      // 它能理解的最高 ResolvedSpec 版本
  repeated Capability caps = 4;    // systemd 252 / docker 24 / …
}
message RegisterResponse {
  uint32 spec_schema = 1;          // mechd 决定用哪个版本下发
  uint32 reconcile_interval_s = 2;
  uint32 report_interval_s = 3;
}
```

一条规则：**控制面向后兼容 agent**。

```
下发版本 = min(mechd 支持的最高版本, mechlet 上报的 max_spec_schema)
```

低于 mechd 支持下限的 mechlet 被拒绝，并明确告知需要升级到哪个版本。
反过来——mechlet 比 mechd 新——是允许的：它按旧结构工作。

> 这条方向是刻意的。多节点升级时先升控制面还是先升 agent，是运维必须能自己
> 决定的事；要求两者同版本会让升级变成一次全停。
> 单机形态下[始终同步升级二者](01-architecture.md#41-单机形态下的升级顺序)，
> 但那是简化，不是协议要求。

gRPC package 名带大版本：`mecharion.agent.v1`。不兼容变更换包名，两版可并存。

## 3. 下发

```protobuf
message Assignment {
  repeated ResolvedSpecEnvelope specs = 1;   // 该节点上应有的全部实例
  bool full_sync = 2;                        // true = 这是全量，未列出的应移除
  uint64 revision = 3;                       // 用于诊断，不参与判定
  bool cordoned = 4;                         // ★ 这台机器现在被暂停调和（M7）
}
message ResolvedSpecEnvelope {
  bytes spec_json = 1;                       // ResolvedSpec，含 @@m7n:secret:<id>@@
  map<string, bytes> secrets = 2;            // ★ id → 明文，mechlet 不落盘
}
```

### 全量重推，不做增量

重连、mechd 重启、期望状态变化——一律推全量并置 `full_sync`。

**不做增量同步、不做确认序号、不做差异计算。** 下发本身幂等：digest 相同的
规格在 mechlet 侧是无操作。一个内容寻址的期望状态让整类同步协议问题消失。

代价是重连瞬间一次全量传输。1000 节点 × 每节点几 KB = 几 MB，且只在 mechd
重启时发生。用协议复杂度换这点带宽不划算。

### `cordoned` 是状态，不是动词

协议层有一条守卫测试（`TestAssignmentHasNoVerbs`）拦住 `Assignment` 的
每一个新字段并要求说明理由——`cordoned` 通过判据是因为它说的是
**「这台机器现在是什么情况」**，随全量一起重推。

写成 `pause_reconcile` 那种动词就不行：断连期间的语义得由节点自己猜，
而「那条暂停指令我到底收到没有」是没法从状态重新推导的。节点重连拿到
全量之后，`cordoned` 自然还是对的（[ADR-0029](../adr/0029-push-over-server-stream.md)）。

### secrets 字段的纪律

`secrets` 是**唯一**承载明文的地方，且：

- mechlet **只在内存持有**，不写 spec 归档、不写日志
- 不参与 digest（digest 覆盖的是 `secretRefs` 的 id 与 version）
- mechlet 重启后重新 Register + Subscribe 拿一次

## 4. 上报

```protobuf
message ReportRequest {
  string node_name = 1;
  repeated InstanceStatus instances = 2;
  NodeFacts facts = 3;               // 周期性刷新
}
message InstanceStatus {
  string component = 1;  string role = 2;
  string digest = 3;                 // 当前生效的 generation 的 digest
  int32  generation = 4;
  string result = 5;                 // ok | changed | drift | failed
  WorkloadStatus workload = 6;
  HealthStatus   health = 7;
  repeated ResourceStatus resources = 8;
  repeated SuppressedDrift suppressed = 9;
}
```

上报间隔默认 15 秒，与调和间隔（60 秒）**独立可配**
（[11-resource-engine §3.2](11-resource-engine.md#32-三个独立的周期)）。

**`digest` 是收敛判据**：Rollout 靠「该实例上报的 digest == 期望的 digest
且健康」来判定这一批完成，而不是靠 mechlet 说「我成功了」。前者是状态，
后者是事件——状态可以重复确认，事件丢一次就永远丢了。

## 5. 断连

| 时段 | mechlet 行为 |
|---|---|
| 断连中 | 按最后已知期望状态**继续调和**；事件写本地 JSONL 缓冲（按天轮转、有上限） |
| 重连 | Register → Subscribe（收到全量）→ PushEvents 补报缓冲 |

重连用指数退避 + 抖动，上限 60 秒。**不使用 systemd `Requires=`**
把 mechlet 绑到 mechd 上（[01-architecture §4.2](01-architecture.md#42-启动依赖用重试不用-systemd-requires)）——
那会让 mechd 的一次重启把所有 mechlet 拖下水。

## 6. blob 传输

```
mechlet 发现规格引用的 blob 本地没有 → FetchBlob(sha256) → 分块流式写入
```

内容寻址意味着**幂等且可断点续传**：已有则跳过，传坏了 sha256 对不上就重传。
不做增量差分（延后成本为零，见[路线图](25-roadmap.md#已知需在实现阶段确定的议题)）。

多机形态下 blob 可能很大（200MB 的 JDK）。约定：

- 分块 1MB，流式落盘到临时文件，校验通过后 rename 进内容寻址目录
- 同一 blob 的并发请求在 mechlet 侧合并，避免 N 个实例各拉一份

## 7. 不做的事

| | 为什么 |
|---|---|
| mechd 主动连 mechlet | 节点要开入站端口，与「零入站」直接冲突 |
| 双向流 | 上报失败会撕掉下发通道 |
| 增量同步 / ACK 序号 | 幂等下发让它们成为纯粹的复杂度 |
| 往 `Assignment` 里塞业务语义（如「执行这个 hook」） | 下发的永远是**期望状态**，不是指令。指令式接口会让断连自治不可能 |

最后一条是根本性的：`Assignment` 里没有动词。mechlet 收到的是「这台机器上
应该有什么」，怎么达成是它自己的事——这正是它在断连时仍能工作的原因。
有一条 `TestAssignmentHasNoVerbs` 守着它。

> **这条禁的是 `Assignment`，不是「一切命令」。** M9 之后另有一条独立的
> `Tasks` 流承载 ad-hoc 命令（restart / exec / logs），见
> [ADR-0038](../adr/0038-adhoc-task-channel.md)。两者的分界是
> **丢一次会怎样**：
>
>     期望状态  丢了无所谓，下一轮重新确认；断连三天回来照做仍然正确
>     命令      丢了就是没执行，必须告诉人；断连三天回来**不该补做**
>
> 把它们放在同一条流上，第一次「顺手加个字段」就会开始侵蚀断连自治。

## 8. 代码生成不依赖 protoc

`proto/` 下的 `.proto` 由 `make proto` 生成到 `internal/protocol/agentpb/`，
**生成物提交进仓库**（与 `sqlcgen` 同一条纪律：离线环境下构建不能依赖再跑
一次生成）。

生成器是 `hack/protogen`，用 [protocompile](https://github.com/bufbuild/protocompile)
（纯 Go 的 proto 编译器）解析描述符，再把 `CodeGeneratorRequest` 喂给
`protoc-gen-go` 与 `protoc-gen-go-grpc`——后两者是 Go 程序，用 `go tool` 跑。
**整条链路只依赖 Go 工具链，不需要 protoc 二进制。**

常规做法要装一个 C++ 写的 protoc 加两个插件。对本项目那是三份平台相关的
构建期依赖：开发机是 Windows、CI 是 Linux、贡献者什么都有可能——每加一个
非 Go 的构建期依赖，「clone 下来就能构建」这条就弱一分。

CI 跑 `make proto-check` 确认生成物与 `.proto` 一致：改了 proto 却忘了重新
生成，两端会按不同的消息格式工作，而那多半要到跨版本部署时才暴露。

## 9. 实现说明

| | |
|---|---|
| 包 | `internal/protocol`（两侧同包：协议的两半必须一起改） |
| 服务端 | `NewServer(ServerOptions{Backend})`——`Backend` 是接口，因此协议层可以脱离数据库单独测 |
| 客户端 | `Dial` → `Run(ctx, Handler)`，`Handler.Apply(specs, fullSync)` |
| 唤醒 | mechd 侧调 `Notify(node)` / `NotifyAll()`。信号通道容量 1 且非阻塞写——**连续多次变更合并成一次推送是安全的**，因为推的是全量 |

两处在实现时才浮现的约束：

- **`sha256` 的形状要在客户端就校验。** 它会被拼进临时文件名与目标路径，
  一个空串或畸形值要么让文件名切片越界，要么在目录之外落盘。服务端也查
  一遍，但那太晚了。
- **校验通过后才 `rename` 进内容寻址目录。** 否则一次中断的传输会留下一个
  名字正确、内容残缺的文件，而下一次调和会认为它已经在了。

> 二进制的接线（`mechd serve` / `mechlet agent`）在[第 8–9 步](25-roadmap.md#m3-的设计与实施顺序)。
> 本步交付的是协议库本身，验收由一条走真实 gRPC 服务端与客户端的
> 端到端测试完成：连上 → 拿到规格 → 还原密钥 → 上报 digest。

## 10. 相关决策

- [ADR-0029 下发用服务端流，上报用一元 RPC](../adr/0029-push-over-server-stream.md)
- [ADR-0001 常驻 Agent](../adr/0001-agent-based.md) · [ADR-0002 mechlet 为唯一执行引擎](../adr/0002-mechlet-as-sole-engine.md)
