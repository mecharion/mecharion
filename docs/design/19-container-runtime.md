# 容器 Runtime（docker / compose）

M4 的设计。规范见 [spec §15.2](../spec/pack-v1.md#152-runtime-docker) 与
[§15.3](../spec/pack-v1.md#153-runtime-compose)，决策依据见
[ADR-0011](../adr/0011-docker-compose-in-v1.md)。

> **这个里程碑的真正目的不是「支持容器」。**
>
> 是在漂移检测（M5）与升级编排（M6）写出来之前，**让第二个 Runtime 存在**——
> 那样这两块逻辑天生就是 Runtime 无关的，而不是先写死在 systemd 上再回头
> 重构（[ADR-0011 §理由①](../adr/0011-docker-compose-in-v1.md)）。
>
> 因此本文里最重要的部分不是 docker 怎么调，而是 **§2 接缝在哪里错了**。

---

## 1. 与 systemd 的形态差异

三个 Runtime 提供同一组原语，但它们背后的东西差异很大：

| | systemd | docker | compose |
|---|---|---|---|
| 被监管的东西 | 一个进程 | 一个容器 | 一个 project（多个容器） |
| 载荷 | tarball，解压进 generation 目录 | `docker save` 的 tar，`docker load` 进镜像库 | 同左，多个 |
| 「配置变了」怎么办 | 改文件 + reload/restart | **必须重建容器** | `compose up -d` 自行差异化重建 |
| 状态来源 | `systemctl show` | `docker inspect` | `docker compose ps` |
| 状态粒度 | 实例 | 实例 | **project 级**（service 明细进 Raw） |
| 原生 reload | `ExecReload=` | 无 | 无 |

**「必须重建容器」是 docker 最本质的差异。** 容器的配置（env、mount、port、
command）在创建时固化，`docker update` 只能改极少数资源限制。因此
docker runtime 的 `Materialize` 不是「写个文件」，而是「必要时删掉重建」。

## 2. 接缝错了一处：缺一个 `Exec`

[05-runtime §3](05-runtime.md#3-核心原理接缝划在真正不同的地方) 定了三条
划界规则，其中第①条是「健康检查不进接口」——`http` / `tcp` / `exec` 三种
探针在任何 Runtime 下行为一致，写一次即可。

**前两种成立，第三种不成立。**

```yaml
health:
  exec: { command: ["pg_isready", "-p", "5432"] }
```

裸机上 `pg_isready` 在 `{{ .Paths.Current }}/bin` 下；容器化之后它**只存在于
容器里**，宿主机上根本没有。同一份 Pack 换个 runtime 就探不动了，而
「健康检查跨 Runtime 行为一致」这条承诺当场作废。

这正是 [ADR-0010](../adr/0010-runtime-abstraction.md) 说的那种错误：
**接缝是否划对，只有第二个实现才能验证。** 第一个实现下这个洞不可见——
systemd 的「工作负载上下文」恰好就是宿主机。

### 修正

给接口加一个原语：

```go
// ExecIn 在工作负载的上下文里执行一条命令。
//
//	systemd: 就在宿主机上执行（工作负载的上下文就是这台机器）
//	docker:  docker exec 进容器
//	compose: docker compose exec <service>
ExecIn(ctx context.Context, ref Ref, cmd []string) (command.Result, error)
```

规则①因此改写为：

> **健康检查的编排不进接口，但「在哪里执行」进。**
> 探针的重试、阈值、超时、结果归一化仍然写一次；只有 `exec` 探针把
> 「在哪执行」这一步交给 Runtime。

这不是给接口开口子：`mechctl component exec <target> -- <cmd>` 本来就在
[CLI 动词表](10-cli.md#43-component)里，它需要的是同一个原语。**一个动词
两个用途，而不是为健康检查特开一条路。**

> 见 [ADR-0032](../adr/0032-runtime-exec-seam.md)

## 3. 一个会咬人的陷阱：bind mount 不跟随软链

```yaml
mounts:
  - { from: "{{ .Paths.Current }}/conf", to: /etc/app }   # ✗ 危险
```

Docker 在**创建容器时**解析 bind mount 的路径。`current` 是一条软链，
解析之后容器绑的是**当时那个 generation 目录**。之后 generation 切换、
软链改指向，容器里看到的仍然是旧的那份——而 `ls -l` 看软链一切正常。

这类「文件明明改了、进程就是读不到」的现场极难查。

因此对 `runtime: docker`：

| 要挂的东西 | 用什么路径 |
|---|---|
| 配置 | `{{ .Paths.Config }}`（稳定，不随 generation 变） |
| 数据 | `{{ .Paths.Data }}` |
| generation 内的东西 | `{{ .Paths.Generation }}`（**显式**指向本次那个） |

`{{ .Paths.Current }}` 在 docker 的 `mounts` 里**由 lint 拒绝**（新增规则
R52）。它在 systemd 的 `exec` 里仍然是正确写法——那是进程启动时才解析的路径。

> 顺带解释了为什么配置变更必须重建容器：即使挂的是稳定路径，
> env / command / port 的变化也只能靠重建生效。两条理由指向同一个做法。

## 4. 身份、标签与「绝不误伤」

### 4.1 命名

```
容器名        mecharion-<component>-<role>
compose 项目  mecharion-<component>
```

与 systemd 的 unit 名同构（`mecharion-<component>-<role>.service`），
排障时三个 Runtime 的现场标识形状一致。

### 4.2 标签

```
dev.mecharion.managed-by   = mecharion
dev.mecharion.site         = <site>
dev.mecharion.component    = <component>
dev.mecharion.role         = <role>
dev.mecharion.generation   = <seq>
dev.mecharion.spec-digest  = <digest>     ★ 判定「要不要重建」
```

`spec-digest` 是新增的一条：容器不可变，所以「现有容器是不是我要的那个」
只能靠比对一个摘要。它取自已解析规格的 digest——**同一个 digest 意味着
同一份期望状态**，这与 generation 的身份判定是同一条规则
（[12-spec-and-state §1.5](12-spec-and-state.md)）。

### 4.3 硬规则：只操作带标签的容器

ADR-0011 把这条列为**高风险点**：漏一处就可能删掉用户与 Mecharion 无关的
生产容器。落到实现上是一条可检查的纪律：

> **凡是会改变或删除容器的调用，命令行里必须带
> `--filter label=dev.mecharion.managed-by=mecharion`；
> 凡是按名字直接操作的（`docker stop <name>`），
> 必须先 `inspect` 确认标签。**

`managed: false`（用户自己的 docker，默认值）下这条尤其重要——那台
dockerd 上跑着的多半是别人的东西。

这条要有**专门的负向测试**：造一个同名但没有标签的容器，确认每一个
会动它的操作都拒绝。「examples 能过 ≠ 检查有效」这条教训在这里同样适用。

## 5. 用 CLI，不用 SDK

`docker` 与 `docker compose` 都走命令行，不引入 Docker 的 Go SDK。

三条理由，其中第一条是决定性的：

1. **compose 根本没有 API。** 它是 CLI 插件，唯一的接口就是命令行。
   为 docker 用 SDK、为 compose 用 CLI，等于维护两套错误处理与两套超时语义。
2. **与 systemd runtime 一致**：那边也是 fork `systemctl`，共用
   `internal/command` 的替身与「退出码不是错误」的约定。
3. **零依赖**：Docker SDK 会拖进 containerd、runc 的一大串间接依赖，
   而 `CGO_ENABLED=0` 的纯静态二进制是一条已经付过代价的约束。

代价是要解析输出。**一律用 `--format '{{json .}}'`**，不解析人读的表格——
后者的列宽与措辞随版本变，前者是稳定契约。

> 见 [ADR-0031](../adr/0031-docker-cli-not-sdk.md)

## 6. 各原语的落地

### 6.1 Probe

```
docker version --format '{{json .}}'      → Server.Version
docker compose version --short            → compose 版本
```

探不到不是错误，是 `Capability{Available: false, Reason: …}`——
mechd 放置时把这句话原样给用户看。**绝不在部署过程中偷偷装 docker**
（[原则六](00-overview.md#原则六显式优于隐式)）。

socket 位置：`mechlet.yaml` 的 `runtimes.docker.socket`，`auto` 时依次探测
`/var/run/docker.sock` 与 rootless 的 `$XDG_RUNTIME_DIR/docker.sock`。

### 6.2 Materialize

```
① docker load -i <blob>          幂等：同一份 tar 反复 load 是无操作
② 记下镜像引用                    从 load 的输出取；随 generation 一起入账
③ inspect 现有容器
     不存在              → create
     存在且 digest 相同  → 无操作
     存在但 digest 不同  → stop + rm + create      ★ 容器不可变
④ 不 start
```

第②步要留意：`docker load` 的输出是 `Loaded image: repo:tag` 或
`Loaded image ID: sha256:…`（无标签的镜像）。两种都要认。取到的引用**写进
本地状态**——下次调和不必再 load 一遍就知道该用哪个镜像，回滚时也知道
旧 generation 用的是哪个。

### 6.3 Start / Stop / Reload

```
Start   docker start <name>
Stop    docker stop --timeout <timeoutStop> <name>
Reload  → ErrReloadUnsupported
```

**docker 不实现 reload。** 它没有原生的热加载，而 `docker kill -s HUP` 要求
Pack 声明信号——`workload.docker` 里没有这个字段，而 pack/v1 现在是 draft-stable，不为这类目前用不上的边缘情况新增字段。
返回 `ErrReloadUnsupported` 让上层降级为重启，那条路径已经实现并测过
（systemd 上「声明了 reload 但 unit 不支持」走的是同一条）。

> 想清楚了再加字段，比现在塞一个 `reloadSignal` 好：容器化应用的热加载
> 惯例并不统一（有的读 SIGHUP，有的要 exec 进去调 CLI），而 `ExecIn`
> 已经让后者可行。

### 6.4 Observe

docker 的状态字段映射到归一化枚举：

| docker | Mecharion |
|---|---|
| 容器不存在 | `Absent` |
| `created` | `Stopped` |
| `running` + health `starting` | `Starting` |
| `running` | `Running` |
| `restarting` | `Starting` |
| `paused` | `Degraded` |
| `exited` / `dead` | `Failed`（带 ExitCode） |

容器原生 HEALTHCHECK 的结果进 `Status.Health`——**不是** Pack 声明的探针，
那些在 Runtime 之上跑（[05-runtime §3 规则①](05-runtime.md#3-核心原理接缝划在真正不同的地方)）。

compose 的 `Observe` 聚合整个 project 的容器：

```
全部 running                → Running
有一个 exited/dead          → Failed
有一个还在 starting         → Starting
project 不存在              → Absent
```

**逐 service 的明细放进 `Raw`**，供 UI 展开与排障。粒度粗一档是
「不把 compose 的 service 映射成 Role」这个决策的已知代价
（[ADR-0011](../adr/0011-docker-compose-in-v1.md)）。

> 实现上**不走 `docker compose ps`**，理由见 [§6.6.3](#663-observe-不用-docker-compose-ps)。

### 6.5 Remove

```
docker stop + docker rm
```

**不删镜像。** 镜像是内容寻址的、可能被别的组件共用，而一次误删要靠重新
分发几百 MB 来补。镜像回收单独做，随 M6 的 generation 回收一起
（那时才知道哪些 generation 已经不可回滚）。

### 6.6 compose 落地时才浮现的四个问题

前五节写的时候只有 docker 一个实现。真去实现 compose 时有四件事没有答案，
这里逐个定下来。**四个问题有一个共同的根源**：compose 的接口是「一个
project 文件 + 一组子命令」，而 Mecharion 需要的是「按容器判定归属、按
摘要判定重建、按 service 定位 exec」——粒度对不上。

#### 6.6.1 compose 文件怎么上机

Pack 里 `workload.compose.file` 写的是 `templates/` 下的模板名。它需要被
渲染成真文件，`docker compose -f` 才读得到。三种做法：

| 做法 | 代价 |
|---|---|
| Pack 自己再声明一条 `template` 资源 | 两处必须一致，而两处都是模板表达式，**lint 检查不了**；写错的表现是 `compose up` 读到一份过期文件 |
| Runtime 自己渲染 | 违反 ADR-0006：mechlet 不做模板求值。且 Runtime 拿不到参数上下文 |
| **渲染流水线自动产出**（采用） | `file` 在管道两侧含义不同，必须写清楚 |

采用第三种：`renderWorkload` 遇到 `runtime: compose` 时，把
`templates/<file>` 当作一条 `template` 资源渲染出来，落在
`{{ .Paths.Config }}/compose.yaml`，并把已解析规格里的 `compose.file`
改写成**那个绝对路径**。

于是：

```
pack.yaml      workload.compose.file = "compose.yaml.tmpl"   模板名
已解析规格      workload.compose.file = "/etc/…/compose.yaml"  绝对路径
```

**这个双重含义是本决策的代价**，写在这里是为了让下一个读代码的人不必
去猜。它可以接受，因为管道两侧本来就有别的字段这么做（`template` 资源的
`src` 在规格里变成 `content`），而落盘路径不该由 Pack 作者挑——它是实现
产物，不是用户配置。

产出的资源 id 是 `template:<dest>`，与 `autoID` 同构。Pack 若自己也往
同一个路径写，会撞上已有的「资源 id 重复」检查——那正是该报错的情形。

#### 6.6.2 标签怎么进 compose 建的容器

「只操作带标签的容器」是 ADR-0011 列的高风险点，compose 下同样要守。
但 `docker compose up` **没有 `--label`**：容器标签只能写在 compose 文件的
`services.<name>.labels` 里。

做法：Materialize 时先 `docker compose config --services` 取出 service 列表，
再生成一份**只含标签的 override 文件**，与主文件一起传给 compose：

```
docker compose -p <proj> -f <compose.yaml> -f <labels.yaml> up --no-start
```

override 文件落在 **generation 目录**里。选它而不是配置目录，是因为标签里
含 `spec-digest`——它本来就是一次性的、随 generation 生灭的东西，放进
generation 目录能顺带被 M6 的回收带走。

不改主文件而是叠一层 override，是为了让 `compose config` 的输出仍然是
Pack 作者写的那份。用户 `cat` 一眼能看懂的文件在排障时值这点复杂度。

#### 6.6.3 Observe 不用 `docker compose ps`

§6.4 原本写的是聚合 `docker compose ps --format json`。实现时换成
`docker ps --filter label=com.docker.compose.project=<proj>` 拿到容器名，
再 `docker inspect` 它们。两条理由：

1. **`compose ps` 的输出里没有标签**，因此判不了归属——而归属判定是这里
   风险最高的一条规则。为了它还要再 `inspect` 一次，那不如一开始就 inspect。
2. `compose ps --format json` 在 v2 的小版本之间**改过形状**（JSON 数组 ↔
   JSON-lines）。`docker inspect` 的形状是稳定契约。

顺带的好处是 compose 与 docker 共用同一个状态映射函数——「两个 Runtime
对同一个容器状态给出不同结论」这类 bug 从结构上不可能出现。

#### 6.6.4 ExecIn 用 `docker exec` 而不是 `docker compose exec`

ADR-0032 写的是 `docker compose exec <service>`，并要求 Pack 用
`execService` 指定 service（多 service 且未指定时报错，不猜）。

`execService` 这条保留。但**执行时不走 compose**：Materialize 时把选中的
service 记进它自己容器的一条标签

```
dev.mecharion.exec = true
```

ExecIn 按这条标签找容器，然后 `docker exec`。理由是 `ExecIn` 只拿得到
`Ref`，而 `docker compose exec` 需要 project 模型——没有 `-f` 时 compose 会
从容器标签里读回**配置文件路径**再解析一遍，那个文件在 generation 被回收
之后就没了。把决策在 Materialize 时做完并记在标签上，`ExecIn` 就不依赖
任何文件是否还在。

行为上两者等价：`compose exec` 内部也是 service → 容器的解析。唯一的差别
在 `scale > 1` 时它取第 1 个副本，这里取容器名排序的第一个——同样确定。

#### 小结：compose 的原语落到什么命令

```
Probe         docker compose version --short
Materialize   docker load ×N → compose config --services → 写 override
              → docker compose -p P -f F -f L up --no-start
Start         docker compose -p P start
Stop          docker compose -p P stop --timeout N
Reload        ErrReloadUnsupported（与 docker 同）
Observe       docker ps --filter label=…project=P → docker inspect
Remove        docker compose -p P down --remove-orphans
ExecIn        docker exec <带 exec 标签的容器>
```

`Ref.Native` 对 compose 是 **project 名**。

## 7. generation 在容器下是什么

systemd 下 generation 是**目录**：切换 = 切软链 + 重启。
docker 下 generation 是**容器 + 镜像的组合**：切换 = 重建容器。

两者共用的部分没有变：

| | 仍然共享 |
|---|---|
| 渲染出的配置 | 落在 generation 目录里，bind mount 进容器 |
| digest | 仍是 generation 的身份 |
| 台账、回收、回滚判定 | `internal/state` 一行不改 |

因此**回滚在两个 Runtime 下是同一件事**：找到目标 digest 对应的
generation → systemd 切软链重启 / docker 用记录的镜像与配置重建容器。
M6 的编排不需要知道自己在跟谁打交道。

> 这一节是 M4 存在的意义的直接兑现。若它写不出来，说明接缝还是错的。

## 8. 测试环境：需要第二个节点镜像

现有的 `test/node` 是**刻意贫瘠**的——不装 curl / wget / 包管理器索引，
让 hermetic 违规直接失败（[test/README](../../test/README.md)）。

docker runtime 测不了：它需要一个跑着的 dockerd。

因此加**第二个**镜像 `test/node-docker`：

| | `test/node` | `test/node-docker` |
|---|---|---|
| 用途 | hermetic + systemd runtime | docker / compose runtime |
| 内容 | 贫瘠 | systemd + dockerd + compose 插件 |
| 权限 | `--cap-add SYS_ADMIN` | `--privileged`（dind 需要） |

**两个镜像不合并**：把 docker 装进贫瘠镜像会让 hermetic 那条约束的执行者
失效——一台有 docker 的机器上，「Pack 偷偷拉镜像」这类违规会意外成功。

> 这条本身也是一次权衡的记录：M4 的测试环境**不能**是贫瘠的，
> 因此 M4 的 Pack 仍须单独过 `mechpack lint --hermetic`，不能靠环境兜底。

## 9. 实施顺序

| # | 内容 | 可验证的成果 |
|---|---|---|
| 1 | `ExecIn` 接缝：接口 + systemd 实现 + exec 探针改造 | systemd 上行为不变，测试全绿 |
| 2 | `test/node-docker` 镜像与 testenv 支持 | 容器里 `docker version` 可用 |
| 3 | docker runtime：Probe / Materialize / Start / Stop / Observe | 单容器 webapp 跑起来 |
| 4 | 标签纪律 + 负向测试 | 同名无标签容器一律不被碰 |
| 5 | `ExecIn` 的 docker 实现 + exec 探针在容器上跑通 | `pg_isready` 那类探针可用 |
| 6 | compose runtime | 一个双服务 project 跑起来 |
| 7 | `docker` 官方 Pack（用 systemd runtime 装 docker） | 离线装出 dockerd |
| 8 | lint 规则 R52（docker mounts 不得引用 `.Paths.Current`） | 负向用例被拒 |

第 1 步先行是刻意的：**接缝的修正要在有第二个实现之前完成**，否则会写出
一个「为了让 docker 能用」而临时加的方法，而不是一个想清楚的原语。

第 7 步排在最后而不是最前：它需要真实的 Docker 静态二进制（几百 MB），
而前六步用发行版自带的 dockerd 就能测。**验证链路不需要等发布物料。**

## 10. 相关决策

- [ADR-0010 Runtime 抽象](../adr/0010-runtime-abstraction.md) ·
  [ADR-0011 v1 纳入 docker 与 compose](../adr/0011-docker-compose-in-v1.md)
- [ADR-0031 用 CLI 而非 Docker SDK](../adr/0031-docker-cli-not-sdk.md)
- [ADR-0032 `ExecIn` 进 Runtime 接口](../adr/0032-runtime-exec-seam.md)
- [05-runtime](05-runtime.md) · [spec §15.2 / §15.3](../spec/pack-v1.md#152-runtime-docker)
