# ADR-0010: Runtime 抽象及其接缝位置

- **状态**：已接受
- **日期**：2026-08-01
- **相关**：[ADR-0011](0011-docker-compose-in-v1.md)、[ADR-0017](0017-k8s-extension-reserve.md)

## 背景

产品需支持多种部署形态：裸机二进制 + systemd（优先）、docker、docker compose、podman，未来还有 Kubernetes。

问题不是「要不要抽象」，而是**接缝划在哪里**。

## 候选方案与调研

### 方案 A：不抽象，条件分支

`if runtime == "systemd" {…} else if runtime == "docker" {…}` 会散落在**安装、升级、状态探测、漂移检测、日志、Rollout 编排**六个地方。每加一个 Runtime 要在六处各改一次，每处都可能漏。

否决。

### 方案 B：大接口 — 把生命周期全部纳入

接口包含 `Install / Upgrade / Rollback / HealthCheck / RenderConfig / Start / Stop / …`

调研这类设计的实际结果：

| 案例 | 结果 |
|---|---|
| **早期 Docker 的 graphdriver** | 接口过大，每个 driver 重复实现相同逻辑，后期大幅收窄 |
| **Kubernetes CRI 的演进** | 从大而全逐步收敛为「容器与镜像的最小操作集」，编排逻辑全部留在 kubelet |
| **Terraform Provider** | 接口极小（CRUD + schema），编排在核心 |

**共同规律：成功的抽象都在收窄，失败的抽象都在膨胀。**

大接口的具体伤害：健康检查、配置渲染、升级编排在 systemd 与 docker 之间**完全相同**，放进接口意味着每个实现重写一遍——抽象反而制造重复。

### 方案 C：小接口 — 只封装「进程如何被监管」 ⭐

## 决策

**采用方案 C。**

```go
type Runtime interface {
    Name() string
    Probe(ctx) (Capability, error)                        // 这台机器支持我吗？版本多少？
    Materialize(ctx, WorkloadSpec) (GenerationRef, error) // 落地但不启动
    Start(ctx, GenerationRef) error
    Stop(ctx, GenerationRef, StopOpts) error
    Reload(ctx, GenerationRef) error
    Observe(ctx, GenerationRef) (WorkloadStatus, error)   // 漂移检测与 UI 的唯一输入
    Logs(ctx, GenerationRef, LogOpts) (io.ReadCloser, error)
    Remove(ctx, GenerationRef) error
}
```

### 分层

```
┌─ Rollout 编排（分批 / 健康门禁 / 暂停 / 回滚）      mechd    ┐
├─ 放置、参数解析、拓扑渲染                           mechd    │
├─ 资源引擎（file/template/user/dir/archive/…）       mechlet  │ 跨 Runtime
├─ generation 管理 + 原子切换 + 回滚                  mechlet  │ 完全共享
├─ 健康检查（http / tcp / exec）                      mechlet  ┘
└─ Runtime 接口 ─────────────────────────────────────────────
      systemd  │  docker  │  compose  │  (podman)
```

### 三条划界规则

**① 健康检查不进接口。** `http` / `tcp` / `exec` 探针在任何 Runtime 下行为完全一致，写一次即可。Runtime 原生的健康信息（docker HEALTHCHECK、systemd watchdog）通过 `Observe()` 返回值带出，不单开方法。

**② 升级与回滚不进接口。** generation 切换的编排逻辑共享；Runtime 只提供 `Materialize` / `Stop` / `Start` 三个原语，如何组合成一次安全升级由上层决定。

**③ `Observe()` 必须返回归一化状态**，否则漂移检测与 UI 就得认识每种 Runtime：

```go
type WorkloadStatus struct {
    State      State          // Absent|Stopped|Starting|Running|Failed|Degraded
    Since      time.Time
    Restarts   int
    ExitCode   *int
    Health     HealthState
    RuntimeRef string         // systemd unit 名 / 容器 ID —— 排障时给人看
    Raw        map[string]any // 原始信息，UI 可展开
}
```

`RuntimeRef` 不可省略——出问题时运维需要知道去 `journalctl -u xxx` 还是 `docker logs yyy`。

## 理由

**接缝必须划在技术之间确实存在差异的位置，而不是划在概念边界上。**

「安装一个组件」概念上是一件事，但它的绝大部分——建用户、建目录、渲染配置、解压载荷、原子切换——在所有 Runtime 下完全相同。只有最后一步「让进程跑起来并被监管」才真正不同。

把接口划在概念边界（"安装"）会导致每个实现重复 90% 的逻辑；划在技术边界（"进程监管"）则每个实现只需几百行。

`Materialize` 输入为 blob 的设计还带来一个免费收益：**容器支持完全不损害离线约束**——`docker save` 的 tar 就是一个 blob，走完全相同的内容寻址分发链路，Pack 格式一个字节不改（[ADR-0005](0005-pack-logic-payload-split.md)）。

## 后果

### 收益

- 每加一个 Runtime 只需实现 8 个方法，上层零改动
- 漂移检测、升级编排、健康检查天生 Runtime 无关
- 离线分发机制跨 Runtime 复用
- 归一化状态让 UI 与告警不必认识底层技术

### 代价

- **`Observe()` 的归一化是有损的**：不同 Runtime 的状态语义并非一一对应（systemd 的 `activating` vs docker 的 `created`+`starting`）。以 `Raw` 字段兜底，但 UI 若只看归一化状态会丢失细节。
- **接缝正确性只有第二个实现才能验证。** 这是本 ADR 的直接推论，也是把 docker/compose 提前到 v1 的核心理由（[ADR-0011](0011-docker-compose-in-v1.md)）——若等到 v1.0 之后再补第二个实现，retrofit 成本会高一个量级。
- **Kubernetes 不适用此接口**：它是集群作用域、无「启停进程」动作、调和由自身控制器完成。强行塞入会污染所有实现。为此另行预留（[ADR-0017](0017-k8s-extension-reserve.md)）。
- **`Reload` 并非所有 Runtime 都支持**：需要能力查询或默认降级为「重启」。

## 参考

- Kubernetes CRI 的收窄演进
- Docker graphdriver 接口膨胀的教训
- Terraform Provider 接口（CRUD + schema 的极小接口）
