#!/bin/sh
# 装之前确认这台机器上没有别人在管同一份 docker 资源。
#
# 两个 daemon 抢同一个 socket 或同一个 data-root，症状是随机的
# 「容器莫名消失」「镜像层损坏」——那种现场几乎查不出来，因此宁可
# 在这里直接拦住，让人先决定要留哪一个。
#
# **判据是「会不会撞」，不是「机器上有没有别的 docker」**：
# 明确换了 socket 与 data-root 的共存是合法部署方式，拦掉它等于
# 禁止一台机器上跑两个用途不同的 daemon。
#
# 只用 POSIX sh 与核心工具（ADR-0015：hermetic，不依赖任何仓库）。
set -eu

DEFAULT_SOCK=/var/run/docker.sock

sock="${MECHARION_PARAM_SOCKET:-$DEFAULT_SOCK}"
data_root="${MECHARION_PARAM_DATA_ROOT:-/var/lib/docker}"
pidfile="${MECHARION_PARAM_PIDFILE:-/var/run/docker.pid}"

fail() {
    echo "$*" >&2
    exit 1
}

# ① 已经有一个 dockerd 在跑，而且用的就是我们要的 socket
#
# 直接读 /proc 而不是 ping socket：**贫瘠的机器上没有 curl 也没有 nc**
# （那正是本 Pack 要服务的机器），而 /proc 在任何 Linux 上都在。
if [ -S "$sock" ]; then
    for p in /proc/[0-9]*; do
        [ -r "$p/comm" ] || continue
        if [ "$(cat "$p/comm" 2>/dev/null)" = "dockerd" ]; then
            fail "已有 dockerd 在跑（pid ${p#/proc/}），而 $sock 已存在。
  两个 daemon 共用一个 socket 会导致容器随机消失。
  请先停掉既有的 docker（systemctl stop docker），或把 socket 参数改到别处。"
        fi
    done
fi

# ② 发行版装的 docker.service
#
# 检查 unit **文件**而不是运行状态：一个当前停着、但开机自启的
# docker.service 会在下次重启后与我们抢起来，那时没人会想到是它。
#
# 只在**我们要用默认 socket**时才拦——那种情况下必定撞。换了 socket 的
# 共存是合法的，那时只提醒。
if command -v systemctl >/dev/null 2>&1 &&
    systemctl list-unit-files docker.service 2>/dev/null | grep -q '^docker\.service'; then
    if [ "$sock" = "$DEFAULT_SOCK" ]; then
        fail "本机已存在发行版安装的 docker.service，而本组件也要用默认 socket $sock。
  即使它现在停着，开机自启也会让两者抢同一份数据。
  请先 systemctl disable --now docker，或把 socket 参数改到别处。"
    fi
    echo "提示：本机有发行版的 docker.service，本组件用的是 $sock，两者可以共存。" >&2
fi

# ③ pid 文件被另一个还活着的 dockerd 占着
#
# 这条单列，是因为它的失败最难懂：dockerd 报的是
#
#   failed to start daemon, ensure docker is not running or
#   delete /var/run/docker.pid: process with PID 79 is still running
#
# 那句话完全没提「pidfile 是可配置的、而你只改了 socket 和 data-root」。
# 表现是 unit 无限重启，而 daemon.json 看上去一切正常。
if [ -f "$pidfile" ]; then
    pid=$(cat "$pidfile" 2>/dev/null || true)
    if [ -n "$pid" ] && [ -r "/proc/$pid/comm" ] &&
        [ "$(cat "/proc/$pid/comm" 2>/dev/null)" = "dockerd" ]; then
        fail "pid 文件 $pidfile 被一个还在跑的 dockerd（pid $pid）占着。
  socket、data_root、pidfile 三者共同决定「哪一台 daemon」——
  与既有 docker 共存时三者都要改，只改前两个就会撞在这里。"
    fi
fi

# ④ data-root 里已经有别人的东西
#
# 不拦的话两个 daemon 会各自维护自己的元数据视图，写坏对方的镜像层。
if [ -d "$data_root" ] && [ -f "$data_root/engine-id" ]; then
    echo "警告：$data_root 里已有 docker 的数据（engine-id 存在）。" >&2
    echo "  本组件将接管它。若那是别的 docker 的数据，请先换一个 data_root。" >&2
fi

echo "预检通过：socket=$sock data-root=$data_root pidfile=$pidfile"
