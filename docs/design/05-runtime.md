# Runtime 抽象

## 1. 它解决的问题

一个 Pack 描述两类东西：

- **「什么东西应该存在」**——文件、目录、用户、渲染好的配置。这部分**与用什么技术拉起进程完全无关**。
- **「什么东西应该在跑」**——这部分完全取决于监管技术。

第二类在不同技术下差异巨大：

| | 拉起 | 状态 | 日志 | 停止 |
|---|---|---|---|---|
| systemd | 写 unit + `systemctl start` | `systemctl show` | journald | `systemctl stop` |
| docker | `docker load` + `docker run` | `docker inspect` | `docker logs` | `docker stop` |
| compose | 渲染 compose.yaml + `up -d` | `compose ps` | `compose logs` | `compose down` |

若不抽象，`if runtime == "systemd" {…} else if runtime == "docker" {…}` 这个分支会散落在**安装、升级、状态探测、漂移检测、日志、Rollout 编排**六个地方。每加一个 Runtime 就要在六处各改一次，每处都可能漏。

`Runtime` 接口把这个分支收敛到一个地方。

## 2. 接口

```go
type Runtime interface {
    Name() string

    // 这台机器支持我吗？版本多少？结果作为 Node capability 上报
    Probe(ctx context.Context) (Capability, error)

    // 把工作负载所需的一切落到节点上，但不启动
    //   systemd: 写 unit 文件 + daemon-reload
    //   docker:  docker load 离线镜像 tar + 创建容器（不 start）
    //   compose: 渲染 compose.yaml + 加载全部镜像
    Materialize(ctx context.Context, spec WorkloadSpec) (GenerationRef, error)

    Start(ctx context.Context, ref GenerationRef) error
    Stop(ctx context.Context, ref GenerationRef, opts StopOpts) error
    Reload(ctx context.Context, ref GenerationRef) error

    // 观测：漂移检测与 UI 的唯一输入
    Observe(ctx context.Context, ref GenerationRef) (WorkloadStatus, error)

    Logs(ctx context.Context, ref GenerationRef, opts LogOpts) (io.ReadCloser, error)
    Remove(ctx context.Context, ref GenerationRef) error

    // 在工作负载的上下文里执行一条命令（M4 加入，见 ADR-0032）
    //   systemd: 就在宿主机上
    //   docker:  docker exec
    ExecIn(ctx context.Context, ref GenerationRef, cmd []string) (command.Result, error)
}
```

## 3. 核心原理：接缝划在真正不同的地方

这是整个设计中最容易做错的一步。常见错误是**把接口划得太大**——把健康检查、配置渲染、升级编排都塞进去，结果每个实现都要重写一遍相同逻辑，抽象反而制造重复。

```
┌─ Rollout 编排（分批 / 健康门禁 / 暂停 / 回滚）       mechd    ┐
├─ 放置、参数解析、拓扑渲染                            mechd    │
├─ 资源引擎（file/template/user/dir/archive/…）        mechlet  │ 跨 Runtime
├─ generation 管理 + 原子切换 + 回滚                   mechlet  │ 完全共享
├─ 健康检查（http / tcp / exec）                       mechlet  ┘
└─ Runtime 接口 ──────────────────────────────────────────────
      systemd  │  docker  │  compose  │  (podman)
```

三条划界规则：

**① 健康检查的编排不进接口，但「在哪里执行」进。** 探针的重试、阈值、超时、结果归一化在任何 Runtime 下行为完全一致，写一次即可。Runtime 原生的健康信息（docker HEALTHCHECK、systemd watchdog）通过 `Observe()` 的返回值带出，不单开方法。

> 这条规则**原本写的是「健康检查不进接口」**，M4 做 docker runtime 时发现它不成立：`exec` 探针要跑的 `pg_isready` 只存在于容器镜像里，宿主机上没有。三种探针里只有 `exec` 有这个问题——`http` / `tcp` 打的是已发布端口，宿主机上照常可达。
>
> 因此接口多了一个 `ExecIn(ctx, ref, cmd)`：systemd 就在宿主机执行，docker 走 `docker exec`。判据是「**如果一件事在不同 Runtime 下的答案不同，它属于接口**」——「`pg_isready` 在哪跑」的答案不同，「连续失败三次算不健康」的答案相同。
>
> 这正是本 ADR 说的「接缝正确性只有第二个实现才能验证」的一个实例：第一个实现下这个洞不可见。见 [ADR-0032](../adr/0032-runtime-exec-seam.md)。

**② 升级与回滚不进接口。** generation 切换的编排逻辑是共享的；Runtime 只提供 `Materialize` / `Stop` / `Start` 三个原语，如何组合成一次安全升级由上层决定。

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

`RuntimeRef` 不可省略。出问题时运维需要知道去 `journalctl -u xxx` 还是 `docker logs yyy`——这是[原则七（现场可诊断）](00-overview.md#原则七现场可诊断)的具体体现。

> 见 [ADR-0010](../adr/0010-runtime-abstraction.md)

## 4. 对离线约束的意义

`Materialize` 的输入是 **blob**：

| Runtime | blob 是什么 | Materialize 做什么 |
|---|---|---|
| systemd | tarball | 解压到 generation 目录 |
| docker | `docker save` 输出的 tar | `docker load` 进本地镜像库 |
| compose | 同上（多个镜像 tar） | 逐个 `docker load` |

**同一个内容寻址 blob store、同一条分发链路、Pack 格式一个字节不改。**

也就是说容器支持**完全不损害**「离线、免 repo」的核心约束——不需要容器 registry，`.mpack` 文件里就带着镜像。这是 Mecharion 相对于绝大多数容器部署方案的实质差异。

## 5. v1 的三个 Runtime

### 5.1 systemd

主路径。generation 是目录，unit 文件写入 `/etc/systemd/system/mecharion-<component>-<role>.service`。

### 5.2 docker 与 compose 是两个 Runtime

- `runtime: docker` → 一个容器 = 一个 workload
- `runtime: compose` → **整个 compose project 作为一个不透明的 workload**

**关键决策：不把 compose 里的 service 映射成 Role。** compose 有自己的 `depends_on`、networks、volumes 语义，硬映射会产生两套冲突的编排概念。把 compose project 当作一个受监管的整体，`Observe()` 聚合 `docker compose ps` 的结果、逐 service 放进 `Raw` 供 UI 展开。

代价：状态粒度粗一档。收益：模型不打架，且能直接消费用户既有的 compose 文件。

### 5.3 不隐式安装 docker

Pack 声明需求：

```yaml
roles:
  - name: web
    workload:
      runtime: docker
      requires: { capability: { docker: ">=20.10" } }
```

mechd 在放置时校验 Node 上报的 capability，不满足则拒绝并给出可执行错误：

```
node-7 缺少 docker（要求 >=20.10）
  · 部署官方 Pack "docker" 以离线安装，或
  · 在 /etc/mecharion/mechlet.yaml 中指定已有的 docker socket
```

**绝不在部署过程中偷偷安装 docker。** 隐式安装 root 级运行时是运维事故的来源（[原则六](00-overview.md#原则六显式优于隐式)）。

### 5.4 与用户既有 docker 共存

```yaml
# /etc/mecharion/mechlet.yaml
runtimes:
  docker:
    enabled: true
    socket:  auto        # auto | /path/to/sock，auto 依次探测标准位置与 rootless 位置
    managed: false       # ★ false = 用户自己的 docker，Mecharion 绝不重启/升级/卸载它
```

配套一条**硬规则**：mechlet 只操作带 Mecharion 标签的容器。

```
dev.mecharion.site       dev.mecharion.component
dev.mecharion.role       dev.mecharion.generation
dev.mecharion.managed-by
```

漂移检测、`Observe`、清理全部按标签过滤。没有这条，一次误清理就能删掉用户的生产容器。

### 5.5 官方 `docker` Pack

这个 Pack 本身使用 **`runtime: systemd`**——自洽得很干净：**用 systemd runtime 安装 docker runtime**。不存在鸡生蛋问题（mechlet 由 bootstrap 装，docker 由 systemd-runtime Pack 装，docker-runtime 的 Pack 最后才来）。

离线安装路径现成：Docker 官方发布**静态二进制 tarball**（内含 dockerd / docker / containerd / runc / ctr），Compose v2 是单个二进制插件。两者都是普通 blob，完全不需要 repo。

`requires.os` 需校验：cgroup v2（或 v1 的相应挂载）、iptables/nftables 可用、内核版本下限。失败快速报错，不尝试自动修复。

> 见 [ADR-0011](../adr/0011-docker-compose-in-v1.md)

## 6. 数据用 bind mount，不用 named volume

```yaml
workload:
  runtime: docker
  mounts:
    - { from: "{{ .Paths.Data }}",   to: /var/lib/postgresql/data }
    - { from: "{{ .Paths.Config }}", to: /etc/postgresql, readOnly: true }
```

named volume 的存储位置由 dockerd 决定，会同时打破：

- **多盘绑定设计**（[04-paths-and-storage.md §4](04-paths-and-storage.md#4-机制二node-volumes--多磁盘与按服务分盘)）
- **「数据目录升级永不触碰」不变式**
- **备份与现场排查的一致性**

bind mount 让容器化组件与裸机组件在数据管理上行为完全一致——这是能给用户的实打实的好处。named volume 作为 opt-in 保留。

> **挂的路径不能是 `{{ .Paths.Current }}`。** Docker 在**创建容器时**解析
> bind mount 的路径，`current` 是软链，解析之后绑的是当时那个 generation
> 目录；之后切换 generation，容器里看到的仍是旧的，而 `ls -l` 看软链一切正常。
> 配置挂 `.Paths.Config`、数据挂 `.Paths.Data`、要 generation 内的东西就
> 显式写 `.Paths.Generation`。详见 [19-container-runtime §3](19-container-runtime.md#3-一个会咬人的陷阱bind-mount-不跟随软链)。

## 7. Kubernetes：为什么不在这个接口里

Kubernetes 与前四个 Runtime **不同类**：

| | systemd/docker/compose/podman | Kubernetes |
|---|---|---|
| 作用域 | 节点本地 | 集群 |
| 执行者 | 目标节点上的 mechlet | 能连到 API Server 的某个执行者 |
| 有「启停进程」动作吗 | 有 | 没有，只有「提交 manifest」 |
| 谁负责调和 | mechlet | k8s 自己的控制器 |

强行塞进同一接口会污染所有实现。v1 为 Kubernetes 预留的只有三件事，全在模型层，代码量接近零：

1. **`Role.scope: node | cluster`** —— schema 中存在，v1 校验时只接受 `node`
2. **`RoleInstance` 拆分 `Target` / `Executor`** —— v1 中两者相同，k8s 场景下 Target 是集群、Executor 是某个 mechlet。**这是唯一真正重要的预留**
3. **`Ref.Kind` 枚举预留 `Endpoint`** —— 代表外部受管目标

`ClusterRuntime` 接口的具体形态、manifest 如何渲染、状态如何归一化，等真正实现时再设计，不提前猜测。

> 见 [ADR-0017](../adr/0017-k8s-extension-reserve.md)
