#!/bin/sh
# 造一个**真的能装**的 Pack，产出 bin/realapp-1.0.0-1.mpack。
#
# 为什么要它：examples/packs/ 里那些的 sha256 是占位符——它们是给人看
# 格式的，装不了。而 M8 第 10 步的验收表第 17 条要的是「上传合法 Pack →
# 进入 Pack 集合」，靠一个装不了的包验不出来：它能通过 lint，却在部署
# 那一刻才露馅。
#
# 三步，每一步都是真实工具链：
#
#   ① go build          产出真二进制（复用 test/webapp 那个夹具）
#   ② mechpack assemble 按 sources 算**真** sha256，sources → blobs
#   ③ mechpack bundle   打成单文件 .mpack（tar + zstd，可复现）
set -eu

cd "$(dirname "$0")/.."
BIN=bin
SRC=test/realpack
# **工作目录不放在 bin/**：那个目录会被整个同步进测试容器，而同步用的
# 是 `cp -f`（不带 -r）——一个子目录会让整条同步链断掉，报出来的却是
# 「bin 是空的」。踩过一次。
WORK=".realpack-work"

command -v go >/dev/null || { echo "需要 go（本脚本跑在有 Go 的那一侧）"; exit 1; }

# **用 go run 而不是 bin/mechpack**：后者是交叉编译出来的 Linux 二进制，
# 在开发机（Windows）上跑不了。走源码还顺带保证工具与代码同版本。
MECHPACK="go run ./cmd/mechpack"

rm -rf "$WORK"
mkdir -p "$WORK/dist"
cp "$SRC/pack.yaml" "$WORK/"

echo "[1/3] 编译载荷"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
    go build -trimpath -o "$WORK/dist/tree/realapp-1.0.0/bin/webapp" ./test/webapp

# 归档要与 `strip: 1` 对得上：顶层套一个版本目录，剥掉之后才是 bin/。
# 这是真实上游产物的形状。
( cd "$WORK/dist/tree" && tar -czf ../realapp-linux-amd64.tar.gz realapp-1.0.0 )
rm -rf "$WORK/dist/tree"

echo "[2/3] assemble（算真 sha256）"
$MECHPACK assemble "$WORK" --out "$WORK/out" >/dev/null

echo "[3/3] bundle（打成 .mpack）"
$MECHPACK bundle "$WORK/out" --out "$BIN/"

echo
# 只把成品留在 bin/，中间产物清掉
rm -rf "$WORK"

echo "产物："
ls -l "$BIN"/realapp-*.mpack
