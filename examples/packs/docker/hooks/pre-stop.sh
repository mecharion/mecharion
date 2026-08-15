#!/bin/sh
# 停 dockerd 之前，把当时还在跑的容器记下来。
#
# 关了 live-restore 时停 daemon 会带走全部容器；开着时容器留着但失去
# 监管。两种情况事后都会有人问「那会儿到底影响了什么」——**这个答案
# 只有停之前才拿得到**，事后无从查起。
#
# 这个 hook **不该失败**：它只是留档，拦住停机没有任何好处。
set -u

docker_bin="${MECHARION_GENERATION:-}/docker"
sock="${MECHARION_PARAM_SOCKET:-/var/run/docker.sock}"

if [ ! -x "$docker_bin" ]; then
    echo "取不到 docker CLI（$docker_bin），跳过留档"
    exit 0
fi

running=$("$docker_bin" -H "unix://$sock" ps --format '{{.Names}}' 2>/dev/null || true)
if [ -z "$running" ]; then
    echo "停止前没有在跑的容器"
    exit 0
fi

count=$(printf '%s\n' "$running" | grep -c . || true)
echo "停止前有 $count 个容器在跑："
printf '%s\n' "$running" | sed 's/^/  /'
exit 0
