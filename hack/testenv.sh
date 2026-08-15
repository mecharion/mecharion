#!/bin/sh
# Mecharion 测试环境 —— 一台能跑 systemd 的容器。
#
#   ./hack/testenv.sh up      构建镜像并启动节点
#   ./hack/testenv.sh shell   进入节点
#   ./hack/testenv.sh exec …  在节点上执行命令
#   ./hack/testenv.sh test    跑 bin/ 下全部测试二进制（含 M2 验收）
#   ./hack/testenv.sh logs    看 systemd 日志
#   ./hack/testenv.sh status  节点与 systemd 状态
#   ./hack/testenv.sh down    停止并删除
#
#   加 --docker 切到带 dockerd 的节点（M4 的 docker / compose runtime）：
#     ./hack/testenv.sh --docker up
#     ./hack/testenv.sh --docker test
#
#   多节点集群（M7）—— 三台同镜像容器接在一个用户自定义网络上：
#     ./hack/testenv.sh cluster up|down|status
#     ./hack/testenv.sh cluster exec m7n-n2 <命令…>
#     ./hack/testenv.sh cluster test
#
# 二进制先只读挂到 /mnt/hostbin，再由 sync_bins 复制到容器本地的
# /usr/local/lib/mecharion/current/bin（/usr/bin 下是软链，与真实安装一致）。
# 每次 up / exec / shell / test 都会同步一遍，因此改代码后只需 `make build`，
# 不必重建镜像。**不直接从挂载点执行**的原因见 sync_bins。
set -eu

# 变量一律加 M7N_ 前缀：`NAME` / `IMAGE` 这类通用名会被环境里已有的同名变量
# 顶掉——WSL 从 Windows 继承的 $NAME 就是计算机名，曾让容器叫成了主机名。
M7N_IMAGE=${M7N_IMAGE:-mecharion/testnode}
M7N_NODE=${M7N_NODE:-m7n-node}
ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)

# 宿主 bin/ 的挂载点，与运行位置**刻意分开**（见 sync_bins）。
M7N_HOSTBIN=/mnt/hostbin
M7N_BINDIR=/usr/local/lib/mecharion/current/bin

# ── 多节点集群（M7）────────────────────────────────────────────────────
#
# 三台**同一个贫瘠镜像**的容器，接在一个用户自定义网络上。
# 用户自定义网络（不是默认 bridge）才有 docker 的内嵌 DNS：容器之间
# 用**容器名**互相解析。这不是省事——mechd 的自签服务端证书的 SAN 里
# 有主机名，用 IP 连会验不过；而「用名字连得上」正是要验的东西之一。
M7N_NET=${M7N_NET:-m7n-net}
M7N_CLUSTER_PREFIX=${M7N_CLUSTER_PREFIX:-m7n-n}
M7N_CLUSTER_SIZE=${M7N_CLUSTER_SIZE:-3}
# 集群用**带 sshd** 的镜像，不是那个贫瘠的。
#
# `mechctl node bootstrap ssh://…` 的前提就是目标机器上有 sshd，而真实
# 用户的机器本来也不是贫瘠的。hermetic 约束由**单节点套件**强制执行
# （test/node 不变）——两处约束各在该在的地方。
M7N_CLUSTER_IMAGE=${M7N_CLUSTER_IMAGE:-mecharion/testnode-ssh}
M7N_CLUSTER_CONTEXT=node-ssh

# ── 节点形态 ────────────────────────────────────────────────────────────
#
# 两个镜像**刻意不合并**：test/node 是贫瘠的，靠「没有 curl」强制执行
# hermetic 约束；一台装了 docker 的机器做不到那件事（test/node-docker/Dockerfile）。
#
# 因此这里是两套名字、两个容器，可以同时存在——跑 docker 的测试不必
# 拆掉另一台。
M7N_CONTEXT=node
M7N_EXTRA=""
if [ "${1:-}" = "--docker" ]; then
    shift
    M7N_CONTEXT=node-docker
    M7N_IMAGE=${M7N_DOCKER_IMAGE:-mecharion/testnode-docker}
    M7N_NODE=${M7N_DOCKER_NODE:-m7n-node-docker}
    # --privileged: dockerd 需要挂载、iptables 与 cgroup 的写权限。
    #
    # 两个具名卷: 容器内的 overlayfs 上**再叠一层 overlay 挂不起来**
    # （invalid argument）。镜像层必须落在真正的文件系统上，而 dockerd 与
    # containerd 各存各的——只给前者会在 docker build 的最后一步失败，
    # 报的是一句 containerd 的 mount 错误，看起来与 docker 无关。
    M7N_EXTRA="--privileged -v m7n-dind-docker:/var/lib/docker -v m7n-dind-containerd:/var/lib/containerd"
fi

die() { printf '%s\n' "错误: $*" >&2; exit 1; }
info() { printf '  %s\n' "$*"; }

need_docker() {
    command -v docker >/dev/null 2>&1 || die "未找到 docker。在 WSL2 中运行本脚本。"
}

# systemd 对 cgroup 的要求随宿主的 cgroup 版本而不同。
# WSL2 常见的是 hybrid 布局（v1 控制器 + /sys/fs/cgroup/unified 的 v2），
# 因此这里探测而非假设。
cgroup_args() {
    ver=$(docker info --format '{{.CgroupVersion}}' 2>/dev/null || echo 1)
    if [ "$ver" = "2" ]; then
        # 纯 v2：私有 namespace + 可写挂载即可
        printf '%s' "--cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw"
    else
        # v1 / hybrid：systemd 需要写自己的层级
        printf '%s' "-v /sys/fs/cgroup:/sys/fs/cgroup:rw"
    fi
}

cmd_build() {
    need_docker
    info "构建 $M7N_IMAGE"
    docker build -t "$M7N_IMAGE" "$ROOT/test/$M7N_CONTEXT"
}

# sync_bins 把宿主的 bin/ 复制到容器本地磁盘再运行。
#
# **不能直接从挂载点执行**。宿主 bin/ 在 Windows 文件系统上，经 WSL2 的
# 9p/virtiofs 转发进容器；从那里 exec 出来的进程会稳定 SIGSEGV（exit 139），
# 而 `cp` 到容器本地后同一个二进制（md5 一致）必然跑通——验过 3/3 vs 3/3。
# 崩溃发生在 Go runtime 起来之前，没有任何输出，看起来就像「测试偶发地
# 什么都不打印就挂了」，为此排查了两个里程碑。
#
# 复制而不是打进镜像：改代码后仍然只需 `make build`，不必重建镜像。
sync_bins() {
    node=${1:-$M7N_NODE}
    docker exec "$node" sh -c "
        mkdir -p '$M7N_BINDIR' &&
        rm -f '$M7N_BINDIR'/* &&
        cp -f '$M7N_HOSTBIN'/* '$M7N_BINDIR'/ 2>/dev/null &&
        chmod +x '$M7N_BINDIR'/*
    " || die "同步二进制失败——$ROOT/bin 是空的？先运行 make build"
}

cmd_up() {
    need_docker
    docker image inspect "$M7N_IMAGE" >/dev/null 2>&1 || cmd_build

    if docker ps -a --format '{{.Names}}' | grep -qx "$M7N_NODE"; then
        info "$M7N_NODE 已存在，先移除"
        docker rm -f "$M7N_NODE" >/dev/null
    fi

    [ -d "$ROOT/bin" ] || die "未找到 $ROOT/bin —— 先运行 make build"

    info "启动 $M7N_NODE"
    # shellcheck disable=SC2046  # cgroup_args 需要按空格分词
    docker run -d --name "$M7N_NODE" \
        $(cgroup_args) $M7N_EXTRA \
        --tmpfs /run --tmpfs /run/lock --tmpfs /tmp \
        --cap-add SYS_ADMIN \
        --stop-signal SIGRTMIN+3 \
        -v "$ROOT/bin:$M7N_HOSTBIN:ro" \
        -v "$ROOT/examples:/examples:ro" \
        "$M7N_IMAGE" >/dev/null

    info "等待 systemd 就绪"
    i=0
    while [ "$i" -lt 30 ]; do
        if docker exec "$M7N_NODE" systemctl is-system-running --wait >/dev/null 2>&1; then
            break
        fi
        # degraded 也算起来了——容器里总有几个 unit 起不来
        state=$(docker exec "$M7N_NODE" systemctl is-system-running 2>/dev/null || true)
        case "$state" in
            running|degraded) break ;;
        esac
        i=$((i + 1))
        sleep 1
    done

    # is-system-running 对 degraded 也返回非零退出码——`|| echo unknown`
    # 会在这种情况下把 echo 的输出也一并接进 $()，state 变成
    # "degraded\nunknown" 这种两行的怪字符串，谁都匹配不上，白白 die。
    # 跟上面探测循环一样用 `|| true`，"unknown" 只在真拿不到状态时兜底。
    state=$(docker exec "$M7N_NODE" systemctl is-system-running 2>/dev/null || true)
    case "$state" in
        running|degraded)
            info "systemd 就绪（$state）"
            sync_bins
            docker exec "$M7N_NODE" sh -c 'mechlet version --short 2>/dev/null' \
                | sed 's/^/  mechlet /' || info "mechlet 尚不可用（先 make build）"
            ;;
        *)
            printf '\n' >&2
            docker logs --tail 40 "$M7N_NODE" >&2
            die "systemd 未能启动（状态: ${state:-unknown}）。上方是容器日志。"
            ;;
    esac

    [ "$M7N_CONTEXT" = "node-docker" ] && wait_dockerd
    return 0
}

# wait_dockerd 等 dockerd 真的能应答。
#
# systemd 报 running 只说明它把 unit 拉起来了；dockerd 在容器里要建网桥、
# 初始化存储驱动，那之后才接受连接。不等就会得到一堆
# 「Cannot connect to the Docker daemon」——而真正的原因只是还没起完。
wait_dockerd() {
    info "等待 dockerd 就绪"
    i=0
    while [ "$i" -lt 60 ]; do
        if docker exec "$M7N_NODE" docker version --format '{{.Server.Version}}' >/dev/null 2>&1; then
            ver=$(docker exec "$M7N_NODE" docker version --format '{{.Server.Version}}' 2>/dev/null)
            cver=$(docker exec "$M7N_NODE" docker compose version --short 2>/dev/null || echo '?')
            info "dockerd 就绪（docker $ver，compose $cver）"
            return 0
        fi
        i=$((i + 1))
        sleep 1
    done

    printf '\n' >&2
    docker exec "$M7N_NODE" journalctl -u docker --no-pager -n 40 >&2 || true
    die "dockerd 未能就绪。上方是它的日志。"
}

cmd_down() {
    need_docker
    docker rm -f "$M7N_NODE" >/dev/null 2>&1 && info "已移除 $M7N_NODE" || info "$M7N_NODE 未在运行"
}

# 只在 stdin 真是终端时加 -t，否则在 CI / 管道里会以
# "the input device is not a TTY" 失败。
tty_flag() { [ -t 0 ] && printf '%s' "-it" || printf '%s' "-i"; }

cmd_shell()  { need_docker; sync_bins; exec docker exec -it "$M7N_NODE" bash; }
cmd_exec()   {
    need_docker
    [ $# -gt 0 ] || die "exec 需要命令"
    sync_bins
    # shellcheck disable=SC2046
    exec docker exec $(tty_flag) "$M7N_NODE" "$@"
}
cmd_logs()   { need_docker; exec docker exec "$M7N_NODE" journalctl --no-pager "$@"; }

# cmd_test 在容器里跑全部交叉编译好的测试二进制。
#
# 测试**二进制**由宿主机编译后挂载进来，容器里没有 Go 工具链——
# 那是刻意的（test/node/Dockerfile）：容器越贫瘠，hermetic 违规越藏不住。
cmd_test() {
    need_docker
    workdir=/tmp/m7n-test
    docker exec "$M7N_NODE" sh -c "rm -rf $workdir && mkdir -p $workdir"
    sync_bins

    # test-multinode **刻意排除**：它的驱动跑在宿主侧（见 cluster_test）。
    # 在容器里跑它只会因为没有集群环境而 skip，然后打出一行 PASS——
    # 那比不跑更糟，「一直在跳过」会被当成「一直在通过」。
    bins=$(docker exec "$M7N_NODE" \
        sh -c "ls '$M7N_BINDIR'/test-* 2>/dev/null | grep -v test-multinode" || true)
    [ -n "$bins" ] || die "bin/ 里没有测试二进制，请先运行 make e2ebin"

    # 每个测试的输出留一份在宿主机上。
    #
    # 输出本来就会打到 stdout，但一次全量有上千行，失败详情滚在几百行之上——
    # 而失败往往是**偶发的**，等发现时那一屏已经找不回来了。留档让「下次再
    # 出现时能拿到现场」不依赖当时有没有想起来重定向。
    logdir=$(mktemp -d)
    failed=""
    for b in $bins; do
        name=$(basename "$b")
        printf '\n\033[1m── %s ────────────────────────────────\033[0m\n' "$name"
        # **状态要写文件**：管道的退出码是 tee 的，不是测试的。
        # 直接 `if cmd | tee` 会让每个失败都被当成通过——那比没有留档更糟。
        # POSIX sh 没有 PIPESTATUS，因此在同一个子 shell 里把 $? 落盘。
        { docker exec -w "$workdir" "$M7N_NODE" "$b" -test.count=1 2>&1
          echo $? > "$logdir/$name.status"
        } | tee "$logdir/$name.log"
        [ "$(cat "$logdir/$name.status")" = 0 ] || failed="$failed $name"
    done

    printf '\n'
    if [ -n "$failed" ]; then
        # 把失败行重新贴到最后：汇总要自带现场，否则「哪个用例挂了」
        # 还得回滚屏幕去找
        for name in $failed; do
            printf '\033[1m%s 的失败：\033[0m\n' "$name"
            grep -E '^(--- FAIL|    --- FAIL|panic:)' -A3 "$logdir/$name.log" |
                head -40 || true
            printf '  完整输出: %s\n\n' "$logdir/$name.log"
        done
        die "以下测试失败:$failed"
    fi
    rm -rf "$logdir"
    info "全部通过"
}

# ── 集群（M7 多节点）────────────────────────────────────────────────────
#
# 与单节点的 up/down **刻意共存**：M2–M6 的验收仍然只需要一台机器，
# 而那是这个项目最主要的形态。多节点是加出来的，不是取代。

cluster_nodes() {
    i=1
    while [ "$i" -le "$M7N_CLUSTER_SIZE" ]; do
        printf '%s%d\n' "$M7N_CLUSTER_PREFIX" "$i"
        i=$((i + 1))
    done
}

cmd_cluster() {
    need_docker
    sub=${1:-}
    [ $# -gt 0 ] && shift
    case "$sub" in
        up)     cluster_up ;;
        down)   cluster_down ;;
        status) cluster_status ;;
        exec)   cluster_exec "$@" ;;
        shell)  cluster_shell "$@" ;;
        test)   cluster_test ;;
        webui)  cluster_webui ;;
        *)      die "cluster 需要 up|down|status|exec|shell|test|webui" ;;
    esac
}

cluster_up() {
    if ! docker image inspect "$M7N_CLUSTER_IMAGE" >/dev/null 2>&1; then
        info "构建 $M7N_CLUSTER_IMAGE"
        docker build -t "$M7N_CLUSTER_IMAGE" "$ROOT/test/$M7N_CLUSTER_CONTEXT"
    fi
    [ -d "$ROOT/bin" ] || die "未找到 $ROOT/bin —— 先运行 make build"

    if ! docker network inspect "$M7N_NET" >/dev/null 2>&1; then
        info "创建网络 $M7N_NET"
        docker network create "$M7N_NET" >/dev/null
    fi

    for n in $(cluster_nodes); do
        if docker ps -a --format '{{.Names}}' | grep -qx "$n"; then
            docker rm -f "$n" >/dev/null
        fi
        info "启动 $n"
        # --hostname 与容器名一致：mechd 的自签证书把主机名写进 SAN，
        # 而集群里是**用容器名互相解析**的。不设的话主机名会是容器 ID，
        # 证书里没有 m7n-n1 这个名字，跨机连接会在 TLS 校验处失败——
        # 那种失败看起来像网络问题，实际是命名问题。
        # shellcheck disable=SC2046
        docker run -d --name "$n" --hostname "$n" \
            --network "$M7N_NET" \
            $(cgroup_args) \
            --tmpfs /run --tmpfs /run/lock --tmpfs /tmp \
            --cap-add SYS_ADMIN \
            --stop-signal SIGRTMIN+3 \
            -v "$ROOT/bin:$M7N_HOSTBIN:ro" \
            -v "$ROOT/examples:/examples:ro" \
            "$M7N_CLUSTER_IMAGE" >/dev/null
    done

    for n in $(cluster_nodes); do
        wait_systemd "$n"
        sync_bins "$n"
    done
    info "集群就绪：$(cluster_nodes | tr '\n' ' ')"
}

# wait_systemd 等一台容器里的 systemd 起来。
wait_systemd() {
    node=$1
    i=0
    while [ "$i" -lt 30 ]; do
        state=$(docker exec "$node" systemctl is-system-running 2>/dev/null || true)
        case "$state" in
            running|degraded) info "$node systemd 就绪（$state）"; return 0 ;;
        esac
        i=$((i + 1))
        sleep 1
    done
    printf '\n' >&2
    docker logs --tail 40 "$node" >&2
    die "$node 的 systemd 未能启动。上方是容器日志。"
}

cluster_down() {
    for n in $(cluster_nodes); do
        docker rm -f "$n" >/dev/null 2>&1 && info "已移除 $n" || true
    done
    docker network rm "$M7N_NET" >/dev/null 2>&1 && info "已移除网络 $M7N_NET" || true
    return 0
}

cluster_status() {
    docker ps --filter "network=$M7N_NET" \
        --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}'
    printf '\n'
    for n in $(cluster_nodes); do
        # is-system-running 对 degraded 也返回非零退出码——`|| echo` 会在
        # 节点其实可达、只是 degraded 时把两段输出一起接进 $()。
        state=$(docker exec "$n" systemctl is-system-running 2>/dev/null || true)
        printf '  %-10s %s\n' "$n" "${state:-不可达}"
    done
}

cluster_exec() {
    [ $# -gt 1 ] || die "cluster exec 需要 <节点> <命令…>"
    node=$1; shift
    sync_bins "$node"
    # shellcheck disable=SC2046
    exec docker exec $(tty_flag) "$node" "$@"
}

cluster_shell() {
    node=${1:-${M7N_CLUSTER_PREFIX}1}
    sync_bins "$node"
    exec docker exec -it "$node" bash
}

# cluster_test 跑多节点验收。
#
# **驱动程序在宿主侧跑，不在容器里** —— 与单节点的 test 不同，这是必须的：
# 它要在三台机器上分别执行命令、比对三份状态，而节点镜像里既没有 docker
# 也没有 ssh（那正是「贫瘠」要保住的性质）。
#
# 被测系统仍然完整地跑在容器里：mechd、mechlet、systemd 一个都没打桩。
# 挪到宿主的只有那双**观察的眼睛**——而它本来就不能站在任何一台被测机器上。
cluster_test() {
    bin="$ROOT/bin/test-multinode"
    [ -x "$bin" ] || die "未找到 $bin —— 先运行 hack/e2ebin.sh"

    # **每台都要同步，而且要重启已经在跑的服务。**
    #
    # 驱动程序在宿主侧跑，被测的 mechd / mechlet / mechctl 全在容器里。
    # 只换文件不够：mechd 是 systemd 拉起来的常驻进程，**已经在跑的那个
    # 仍在执行旧代码**——换掉磁盘上的文件不会让它重新 exec。
    #
    # 这一条踩过两次，两次都表现为「变异测试假绿」：改动明明生效了，
    # 跑出来却和没改一样。同步 + 重启一起做，这个坑就关上了。
    for n in $(cluster_nodes); do
        docker inspect "$n" >/dev/null 2>&1 ||
            die "$n 不在运行——先 ./hack/testenv.sh cluster up"
        sync_bins "$n"
        # try-restart：没在跑的不管，在跑的换成新二进制
        docker exec "$n" systemctl try-restart 'mecharion-*' >/dev/null 2>&1 || true
    done

    M7N_CLUSTER_PREFIX="$M7N_CLUSTER_PREFIX" \
    M7N_CLUSTER_SIZE="$M7N_CLUSTER_SIZE" \
    M7N_NET="$M7N_NET" \
        "$bin" -test.count=1 -test.v
}

# cluster_webui 跑 M8 的验收表（23-web-ui §5）。
#
# **在集群内部跑**，与 cluster_test 相反：那些判据几乎全是 HTTP 层面的，
# 而宿主到容器网络不通（节点端口不对外发布）。把驱动放进容器，用一个
# 真正的 http.Client 去打，比在宿主侧拼 docker exec 诚实得多。
cluster_webui() {
    node=$(cluster_nodes | head -1)
    docker inspect "$node" >/dev/null 2>&1 ||
        die "$node 不在运行——先 ./hack/testenv.sh cluster up"
    [ -f "$ROOT/bin/test-webui" ] || die "未找到 bin/test-webui —— 先运行 hack/e2ebin.sh"

    sync_bins "$node"
    docker exec "$node" systemctl try-restart 'mecharion-*' >/dev/null 2>&1 || true
    sleep 3

    info "M8 验收表（在 $node 内部跑）"
    docker exec -e M7N_WEBUI_HOST="$node" "$node"         "$M7N_BINDIR/test-webui" -test.count=1 -test.v
}

cmd_status() {
    need_docker
    docker ps --filter "name=$M7N_NODE" --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}'
    printf '\n'
    docker exec "$M7N_NODE" systemctl is-system-running || true
    docker exec "$M7N_NODE" systemctl list-units --failed --no-pager --no-legend \
        | sed 's/^/  失败: /' || true
}

usage() {
    sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
    exit 2
}

[ $# -gt 0 ] || usage
sub=$1; shift
case "$sub" in
    build)  cmd_build ;;
    up)     cmd_up ;;
    down)   cmd_down ;;
    shell)  cmd_shell ;;
    exec)   cmd_exec "$@" ;;
    logs)   cmd_logs "$@" ;;
    test)    cmd_test ;;
    status)  cmd_status ;;
    cluster) cmd_cluster "$@" ;;
    *)       usage ;;
esac
