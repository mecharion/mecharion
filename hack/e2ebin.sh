#!/bin/sh
# 交叉编译端到端测试所需的一切到 bin/。
#
# 只依赖 go，不依赖 make —— Windows 开发机上通常有 Go 但没有 make，
# 而 WSL 侧有 make 和 docker 却没有 Go。于是流程分两步：
#
#   宿主机（有 Go）:  ./hack/e2ebin.sh
#   WSL（有 docker）: ./hack/testenv.sh up && ./hack/testenv.sh test
#
# 两边都齐全时直接 `make e2e` 即可。
set -eu

cd "$(dirname "$0")/.."

BIN=bin
mkdir -p "$BIN"

export GOOS=linux GOARCH=amd64 CGO_ENABLED=0

MODULE=github.com/mecharion/mecharion
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS="-s -w \
 -X $MODULE/internal/version.Version=$VERSION \
 -X $MODULE/internal/version.Commit=$COMMIT \
 -X $MODULE/internal/version.Date=$DATE"

for b in mechd mechlet mechctl mechpack; do
    echo "  bin      $b"
    go build -trimpath -ldflags "$LDFLAGS" -o "$BIN/$b" "./cmd/$b"
done

echo "  bin      webapp（端到端夹具）"
go build -trimpath -o "$BIN/webapp" ./test/webapp

# tlsprobe：拿一个不带客户端证书的连接去撞 mTLS 端口（22-multi-node §5 第 5 行）。
# 被测镜像里没有 openssl，集群端口也不对宿主发布——验收需要的能力用夹具补，
# 不往被测机器上装工具。
echo "  bin      tlsprobe（mTLS 验收夹具）"
go build -trimpath -o "$BIN/tlsprobe" ./test/tlsprobe

# uiprobe：走完整条 HTTP 取 Web UI 的页面（23-web-ui §6）。tlsprobe 只判
# 连接接不接受，判不了「回来的是不是那个页面」。
echo "  bin      uiprobe（Web UI 验收夹具）"
go build -trimpath -o "$BIN/uiprobe" ./test/uiprobe

# ssereader：验收表第 14 条是**时间维度**的判据（「第 N/M 批自动更新」），
# 一次取页面验不了它。
echo "  bin      ssereader（SSE 验收夹具）"
go build -trimpath -o "$BIN/ssereader" ./test/ssereader

# uploadprobe：Pack 上传的验收（23-web-ui §2.7）。节点镜像里没有 curl。
echo "  bin      uploadprobe（Pack 上传验收夹具）"
go build -trimpath -o "$BIN/uploadprobe" ./test/uploadprobe

# 这些包的测试必须在真 Linux 上跑：软链、属主、systemd、getent
# 在开发机上要么不可用要么语义不同。
for p in internal/faults internal/command internal/spec internal/state \
         internal/pack \
         internal/health internal/resource internal/runtime \
         internal/runtime/systemd internal/reconcile \
         internal/store internal/vault internal/packindex internal/placement \
         internal/render internal/cli/ctlcmd internal/protocol internal/hook \
         internal/mechd internal/runtime/docker; do
    name=$(basename "$p")
    echo "  test     $name"
    go test -c -o "$BIN/test-$name" "./$p"
done

echo "  test     e2e（M2 验收）"
go test -c -o "$BIN/test-e2e" ./test/e2e

# M8 验收：23-web-ui §5 那张表。**跑在集群内部**——判据几乎全是 HTTP
# 层面的（cookie 属性、CSRF 头、Content-Encoding、SSE 帧），而宿主到
# 容器网络不通。
echo "  test     webui（M8 验收，集群内运行）"
go test -c -o "$BIN/test-webui" ./test/webui

# 多节点验收的驱动**跑在宿主侧**，不进容器：它要在三台机器上分别执行命令
# 并比对三份状态，而节点镜像里既没有 docker 也没有 ssh（test/multinode 的
# 包注释说明了为什么不能给它们装上）。
echo "  test     multinode（M7 验收，宿主侧运行）"
go test -c -o "$BIN/test-multinode" ./test/multinode

echo
echo "已就绪。接下来在能访问 docker 的环境里："
echo "  ./hack/testenv.sh up && ./hack/testenv.sh test"
