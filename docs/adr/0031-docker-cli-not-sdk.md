# ADR-0031: docker / compose runtime 走 CLI，不用 Docker SDK

- **状态**：已接受
- **日期**：2026-08-04
- **相关**：[ADR-0011](0011-docker-compose-in-v1.md)、[ADR-0015](0015-offline-first-hermetic.md)

## 背景

实现 docker runtime 有两条路：

| | Docker Engine API（Go SDK 或自写 HTTP over unix socket） | CLI |
|---|---|---|
| 结构化 | 是，直接拿到结构体 | 需要解析输出 |
| 依赖 | SDK 很重；自写则要处理 API 版本协商 | 无 |
| 需要 `docker` 二进制 | 否 | 是 |
| 覆盖 compose | **否** | 是 |

## 决策

**两个 runtime 都走命令行**，用 `internal/command` 的 `Runner`——与 systemd
runtime fork `systemctl` 是同一条路径、同一套替身、同一个「退出码不是错误」
的约定。

输出**一律用 `--format '{{json .}}'`** 取，不解析人读的表格。

## 理由

### ① compose 根本没有 API（决定性）

Docker Compose v2 是一个 **CLI 插件**，唯一的接口就是命令行。没有
`/compose/...` 端点，也没有官方的 Go 库来做等价的事。

于是走 SDK 的方案实际上是「docker 用 SDK、compose 用 CLI」——两套错误处理、
两套超时语义、两套测试替身。而这两个 runtime 在 Mecharion 里是并列的，
不该有一个是二等公民。

### ② 与 systemd runtime 一致

systemd runtime 已经是 fork `systemctl`。三个 runtime 用同一个
`command.Runner` 意味着：

- 同一套测试替身（`command.Fake`），M4 不需要发明第二种打桩方式
- 同一套超时与进程组语义（第 7 步为 hook 加的进程组整组 kill 同样受益）
- 排障时**日志里看到的就是运维自己能敲的命令**——这条在现场很值钱

### ③ 零依赖

Docker 的 Go SDK 会拖进 containerd、runc、opencontainers 的一大串间接
依赖。`CGO_ENABLED=0` 的纯静态二进制是一条已经付过代价的约束
（[路线图 M2 支持范围](../design/25-roadmap.md#libc没有下限)：它换来的是
「无 glibc 下限」），不值得为一个 runtime 让整个依赖树膨胀。

自写 HTTP-over-unix-socket 客户端能避开依赖，但要自己处理 API 版本协商
（`/v1.43/containers/...`），而那正是 CLI 已经替我们做好的事。

### ④ 「需要 docker 二进制」不是新增约束

`workload.requires.capability.docker` 本来就要求这台机器有 docker
（[ADR-0011](0011-docker-compose-in-v1.md)：**绝不隐式安装**）。
官方 `docker` Pack 装的静态发行包里，`dockerd` 与 `docker` 是一起的。

也就是说：**任何能用 docker runtime 的机器上，`docker` 命令必然存在。**
这条"代价"是空的。

## 后果

### 收益

- 一套执行路径、一套替身、一套错误约定覆盖三个 runtime
- 依赖树不变，静态二进制的承诺不受影响
- 日志里的命令可以直接复制到终端里重现

### 代价

- **要解析输出**。用 `--format '{{json .}}'` 缓解：那是稳定契约，
  而人读的表格列宽与措辞随版本变
- **多一次 fork**。相对容器启动本身的开销可以忽略；`Observe` 是最高频的
  调用（每个上报周期一次），必要时可以合并成一次 `docker ps --filter`
  批量取回，而不是逐实例 `inspect`
- **`docker load` 的输出格式要认两种**：`Loaded image: repo:tag` 与
  `Loaded image ID: sha256:…`（无标签镜像）。这条要有测试钉住
- **版本差异**：`docker compose ps --format json` 在 v2 的早期版本里输出的是
  逐行 JSON（JSONL）而非数组。`Probe` 报的 compose 版本要参与判断，
  或者两种都认——**倾向两种都认**，因为版本判断本身也会出错

### 什么情况下应当重新考虑

- 若将来需要**流式**地拿容器事件（`docker events`）来做实时漂移检测，
  CLI 的长流在进程管理上会比 API 麻烦。届时可以只把那一条换成 API，
  而不必整体切换
- 若 rootless / 远程 docker（`DOCKER_HOST=ssh://…`）成为主要场景，
  CLI 反而更省事——它原生支持这些，而自写客户端要逐一实现

## 参考

- Docker Compose v2 的插件形态（`~/.docker/cli-plugins/docker-compose`）
- `docker inspect --format` / `docker ps --format` 的 Go template 输出
- Docker Engine API 的版本协商机制（`/v1.43/...` 路径前缀）
