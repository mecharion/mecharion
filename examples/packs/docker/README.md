# docker

**层级 L1｜压测：用 Mecharion 装出 Mecharion 自己要用的运行底座**

这个 Pack 与其它示例不同：它不是为了验证规范的某个角落，而是**闭环**——
`docker` / `compose` 两个 Runtime 需要一台 dockerd，而这台 dockerd 由
systemd Runtime 装出来。ADR-0011 与 05-runtime 里那句
「部署官方 Pack "docker" 以离线安装」在这里兑现。

## 压测了规范的哪些部分

| 部分 | 怎么压的 |
|---|---|
| `archive` + `strip` | 官方静态包解开是 `docker/<binaries>`，剥掉一层 |
| `systemd.env` | dockerd 运行期 exec containerd / runc，**PATH 必须显式给** |
| `systemd.extraUnit` | `Delegate` / `TasksMax` / `OOMScoreAdjust` 这些字段表里没有 |
| `health.exec` | 判据是 `docker version` 的退出码，不是端口通不通 |
| `preInstall` hook | **第一个用 preInstall 的示例 Pack**，一上来就打出两个 bug |
| hermetic | 预检脚本读 `/proc` 而不是 `curl` unix socket |

## 暴露的问题

### ① `preInstall` 的 cwd 指向一个还不存在的目录

hook 的执行环境约定 cwd 是 generation 目录（18-hooks §3），而 `preInstall`
是第一个 hook 点——那时目录还没建。Go 的 `fork/exec` 于是报

```
fork/exec /var/lib/.../hooks/pre-install.sh: no such file or directory
```

**这句话指向脚本，而脚本明明在那儿。** 排查会一直盯着 hook 文件看。

之前没暴露，是因为没有一个示例 Pack 用过 `preInstall`——postgresql 与 hdfs
用的都是 `postStart` 与 `scope: once`。

修法是把 generation 目录的创建挪到 `preInstall` 之前（空目录不算「物化」，
台账要到最后才写），并在 hook 执行器里对这种情况给一句能看懂的话。

### ② hook 路径的两种写法只有一种能用

规范允许 `hooks/x.sh` 与 `x.sh`（§16.3），lint 也确实两种都放行——但渲染侧
无条件再拼一次 `hooks/`。写全路径的 Pack **能过 lint，部署时才炸**：

```
执行 hook: hooks/hooks/pre-install.sh: no such file or directory
```

根源是同一条规则被写了两遍。现在两处共用 `pack.HookScriptPath`。

### ③ `mechctl component render` 渲染不出带载荷的 Pack

离线渲染命令从不填 `blobs`，于是任何带 `archive` 的 Pack 渲染出来都是残的，
装上去报「规格中没有名为 main 的 blob」。而这条命令的全部价值在于
「同样的输入必然得到同样的输出」——算漏一样东西就不成立了。
现在与 mechd 共用 `render.BlobsFor`。

### ④ `--out` 抢了 `-o` 简写

`mechctl component render` **一执行就 panic**：`-o` 在 root 与 component 上
已经是 `--output`（输出格式）。单独构造这个命令的单测看不到命令树合并那一步，
于是全绿而命令根本没法用。

## 三个参数共同决定「哪一台 daemon」

```
socket      监听在哪
data_root   镜像与容器存在哪
pidfile     谁占着这台机器上的 docker 身份
```

与既有 docker 共存时**三者都要改**。只改前两个的表现是 unit 无限重启，
而 dockerd 说的是

```
failed to start daemon, ensure docker is not running or
delete /var/run/docker.pid: process with PID 79 is still running
```

那句话完全没提 pidfile 是可配置的。因此 `preInstall` 专门检查这一项——
**把一个难懂的重启循环换成一句能照着做的话**，这比多一个参数值钱。

## 载荷

`blobs.main` 是 Docker 官方的静态包（`download.docker.com/linux/static/stable`）。
`sourceUrl` 只是溯源信息，**部署时不会去下载**（ADR-0015）——
真实产物由 `mechpack assemble` 填进去。

不用发行版仓库装：`package` 资源需要 repo，会被 `lint --hermetic` 拦掉。
一台连不上任何仓库的机器要装出 dockerd，只有静态包这一条路——
而那正是本 Pack 存在的理由。
