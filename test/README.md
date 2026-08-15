# 测试环境

集成测试需要一台**能跑 systemd 的 Linux**——资源引擎要装 unit、建系统用户、切软链，这些在普通容器或宿主机上都验证不了。

```bash
make e2e                 # 交叉编译 + 起容器 + 跑全部真机测试（含 M2 验收）

make testenv-up          # 只起容器
./hack/testenv.sh exec mechpack lint --hermetic /examples/packs/go-webapp
./hack/testenv.sh shell
make testenv-down
```

## 两个节点镜像

| | `test/node` | `test/node-docker` |
|---|---|---|
| 用途 | hermetic + systemd runtime | docker / compose runtime（M4） |
| 内容 | **刻意贫瘠**：只有 systemd 与 dbus | systemd + dockerd + compose 插件 |
| 权限 | `--cap-add SYS_ADMIN` | `--privileged` + 两个具名卷 |
| 命令 | `./hack/testenv.sh up` | `./hack/testenv.sh --docker up` |

**两者刻意不合并。** 一台装了 docker 的机器无法执行 hermetic 约束——
`docker pull` 本身就是联网取内容，「Pack 偷偷拉镜像」这类违规在那里会
意外成功。因此 M4 的 Pack **仍须单独过 `mechpack lint --hermetic`**，
不能靠环境兜底。

两个容器可以同时存在，互不影响。

> **dind 需要两个卷**：容器内的 overlayfs 上再叠一层 overlay 挂不起来
> （`invalid argument`）。镜像层必须落在真正的文件系统上，而 dockerd 与
> containerd **各存各的**（`/var/lib/docker` 与 `/var/lib/containerd`）——
> 只给前者会在 `docker build` 的最后一步失败，报的是一句 containerd 的
> mount 错误，看起来与 docker 完全无关。
>
> `test/node-docker` 的构建要从 Docker 官方仓库装包，**比另一个镜像慢得多**
> （本机实测约 27 分钟）。它只在改动那个 Dockerfile 时才需要重建。

## 哪些测试必须进容器

单元测试在开发机上照常跑，但**这几类用例会自行跳过**，因为它们在开发机上
要么不可用、要么语义不同：

| 用例 | 为什么开发机上测不了 |
|---|---|
| 软链（`current` 原子切换、`linkInto`） | Windows 默认不允许非管理员建软链 |
| 属主与权限 | Windows 的 ACL 与 uid/gid 不对应，`chmod` 读回来永远是 0666/0777 |
| `useradd` / `getent` 的真实 flag | 命令替身对任何参数都答成功，拼错的 flag 发现不了 |
| systemd 是否**认**生成的 unit | 只有 systemd 自己能回答；替身照单全收 |
| `Environment=` 的引用是否正确 | 带空格的值被拆开与否，只有 systemd 说了算 |

因此 `go test ./...` 通过**不等于**功能正确，容器里那一遍才是判据。
`hack/testenv.sh test` 会把 `bin/test-*` 全部跑一遍。

## 镜像刻意是"贫瘠"的

`test/node/Dockerfile` 只装 systemd 与 dbus，**不装** curl / wget / git / make / gcc / 语言工具链，并清空 apt 索引。

这不是为了省体积，而是为了让 **hermetic 违规直接导致测试失败**（[ADR-0015](../docs/adr/0015-offline-first-hermetic.md)）：

```
$ curl https://example.com
sh: 1: curl: not found

$ apt-get install -y hello
E: Unable to locate package hello
```

> **测试环境本身成为约束的执行者**——这比 `mechpack lint --hermetic` 的静态扫描更彻底：静态扫描能被变量拼接绕过（`C=cur"l"; $C …`），缺失的二进制不能。

Dockerfile 里有一条构建期断言，若某次基础镜像变更引入了这些工具，**构建立即失败**——否则 hermetic 违规会被悄悄放过。

## 二进制是挂载进去的，不打进镜像

```
-v "$ROOT/bin:/usr/local/lib/mecharion/current/bin:ro"
-v "$ROOT/examples:/examples:ro"
```

改代码后只需 `make testbin`，不必重建镜像。

## Windows + WSL2 上的用法

Go 装在 Windows 侧、Docker 在 WSL2 侧，两边工具不重合（Windows 常常没有
`make`，WSL 里常常没有 Go），因此分两步：

```bash
# Windows（git bash / PowerShell 均可，只需要 Go）
sh hack/e2ebin.sh
```
```bash
# WSL2（有 docker）
./hack/testenv.sh up && ./hack/testenv.sh test
```

两边工具齐全时（Linux / macOS / CI）直接 `make e2e`。

## cgroup

`testenv.sh` 探测宿主的 cgroup 版本而非假设：

| 宿主 | 参数 |
|---|---|
| cgroup v2 | `--cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw` |
| v1 / hybrid（**WSL2 默认**） | `-v /sys/fs/cgroup:/sys/fs/cgroup:rw` |

WSL2 是 hybrid 布局（v1 控制器 ＋ `/sys/fs/cgroup/unified` 的 v2）。实测 Debian 12 的 systemd 252 在此布局下能正常启动到 `running`，零失败 unit。

用 `--cap-add SYS_ADMIN` 而非 `--privileged`——systemd 需要的就是它，全特权是不必要的宿主暴露。

## 已验证的能力

| 能力 | 验证方式 |
|---|---|
| **M2 验收**：`mechlet apply -f` 跑起 go-webapp | `test/e2e`：unit `active` ＋ `/healthz` 返回 `ok` |
| 重复 apply 不惊动进程 | 连续 apply 三次，`MainPID` 不变 |
| 配置变更 → 新 generation → 回滚 | `current` 软链改指，旧 generation 目录完整保留 |
| 安装并启动 systemd unit | 写 unit → `daemon-reload` → `enable` → `is-active` 为 `active` |
| 创建系统用户 | 真实 `useradd` / `usermod`，验证 flag 拼写 |
| **原子软链切换**（generation 机制） | 建临时名 + `rename`，`readlink` 确认指向新目标 |
| `Environment=` 引用正确 | 读进程的 `/proc/self/environ`，不经过 shell |
| hermetic 违规必然失败 | `curl` not found；`apt-get install` 无法定位软件包 |

### 夹具应用

`test/webapp` 是 M2 的端到端夹具：单静态二进制、无外部依赖、暴露 `/healthz`、
支持 SIGHUP 热加载。选它而非 nginx 是刻意的——nginx 会把开发拖进发行版差异与
包版本可用性的泥潭（[25-roadmap](../docs/design/25-roadmap.md)）。

## 三节点里程碑验收（本地跑）

M7（多节点）、M8（Web UI）、M9（生命周期）的验收判据装在 `test/multinode`
与 `test/webui`，起三台同镜像容器接在一个用户自定义网络上。这套比单机套件
慢得多（真实的滚动升级要等健康门禁），**不在 CI 里自动跑**，改动多节点/
Web UI 相关代码后请在本地走一遍：

```bash
make webui                    # mechd 要带着构建好的 UI，否则验不出真实界面
./hack/e2ebin.sh               # 交叉编译测试二进制
./hack/realpack.sh             # 造一个真包（部分判据要用到）

./hack/testenv.sh cluster up
./hack/testenv.sh cluster test    # M7 + M9：test/multinode 整个包
./hack/testenv.sh cluster webui   # M8：依赖上一步建立的集群状态，顺序不能反

./hack/testenv.sh cluster status  # 排障：节点与 systemd 状态
./hack/testenv.sh cluster down    # 用完清理
```

## 容器与虚拟机的分界

| 场景 | 容器 | 需要虚拟机 |
|---|---|---|
| systemd unit 生命周期、generation 切换、回滚 | ✅ | |
| 多节点 mechd ↔ mechlet（M7） | ✅ compose 网络够用 | |
| 健康检查、漂移检测 | ✅ | |
| docker runtime（M4，docker-in-docker） | ⚠️ 可行但别扭 | 推荐 |
| **`sysctl` / `limits` / THP** | ❌ **改的是宿主内核** | ✅ |
| **多磁盘绑定**（minio / hdfs `dnDirs`） | ❌ 假挂载无意义 | ✅ |
| **SELinux、防火墙、swap** | ❌ | ✅ |

M2–M6 主线用容器；`host-tuning` 与多磁盘类 Pack 的验证留给虚拟机。

## 目录

```
test/
├── README.md
├── node/Dockerfile      测试节点镜像
├── webapp/main.go       端到端夹具应用
└── e2e/                 M2 验收测试（驱动真正的 mechlet 二进制）
hack/testenv.sh          up / down / shell / exec / test / logs / status
hack/e2ebin.sh           交叉编译全部测试二进制（只需 Go）
```

`compose.yaml`（多节点）等 M7 再加。
