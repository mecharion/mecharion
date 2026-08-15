#!/bin/sh
# 造 README quickstart 用的真实 Pack，产出 dist/hello-0.1.0-1.mpack。
#
# 与 hack/realpack.sh 是同一种手法（现场编译、真 sha256），但服务不同的
# 消费方：realpack.sh 的产物只喂给 M8 验收测试；这个脚本的产物是给
# **新用户跟着 README 走一遍**用的。
#
# **产出的是 .mpack（bundle 过的厚包），不是 assemble 的目录形态**：
# 单机 quickstart 走的是 `mechctl pack upload` 这条路——本地 pack-dir
# 扫描只能让 mechd 看到 Pack 的元数据（参数、角色声明），节点按 sha256
# 取的 blob 内容必须经 `POST /packs` 的入库步骤才会真的存在；直接把
# assemble 的目录扔进 pack-dir 会让部署"能发出去、永远不收敛"——这是
# 用真集群踩出来的一个真实的坑（见 internal/mechd/upload.go 的注释）。
#
# 三步，都是真实工具链：
#
#   ① go build          产出真二进制（复用 test/webapp 那个夹具）
#   ② mechpack assemble 按 sources 算**真** sha256，sources → blobs
#   ③ mechpack bundle   打成单文件 .mpack（tar + zstd，可复现）
set -eu

cd "$(dirname "$0")/.."
BIN=dist
SRC=examples/quickstart/hello
# 工作目录不放在 dist/ 或 bin/ 里——道理与 .realpack-work/ 一样：中间
# 产物混进最终目录，同步/清理时容易把半成品当成成品。
WORK=".quickstart-work"

command -v go >/dev/null || { echo "需要 go（本脚本跑在有 Go 的那一侧）"; exit 1; }

# 用 go run 而不是 bin/mechpack：后者可能是交叉编译出来的目标平台
# 二进制，在开发机上跑不了；走源码还顺带保证工具与代码同版本。
MECHPACK="go run ./cmd/mechpack"

rm -rf "$WORK"
mkdir -p "$WORK/dist" "$BIN"
cp "$SRC/pack.yaml" "$WORK/"

echo "[1/3] 编译载荷"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
    go build -trimpath -o "$WORK/dist/tree/hello-0.1.0/bin/webapp" ./test/webapp

# 归档要与 `strip: 1` 对得上：顶层套一个版本目录，剥掉之后才是 bin/。
( cd "$WORK/dist/tree" && tar -czf ../hello-linux-amd64.tar.gz hello-0.1.0 )
rm -rf "$WORK/dist/tree"

echo "[2/3] assemble（算真 sha256）"
$MECHPACK assemble "$WORK" --out "$WORK/out" >/dev/null

echo "[3/3] bundle（打成 .mpack）"
$MECHPACK bundle "$WORK/out" --out "$BIN/"

rm -rf "$WORK"

echo
echo "产物："
ls -l "$BIN"/hello-*.mpack
