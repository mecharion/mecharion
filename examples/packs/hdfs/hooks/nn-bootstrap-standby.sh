#!/bin/sh
# 备 NameNode 从主 NameNode 拉取元数据。
#
# scope: perInstance（默认）+ when: profile == "ha" && ordinal != 0
# 每个非首个 NameNode 实例各执行一次——这正是 scope: once 无法表达、
# 而 perInstance + when 恰好覆盖的模式。
set -eu

HDFS="${MECHARION_PATHS_CURRENT}/bin/hdfs"
NN_DIR=$(echo "${MECHARION_PATHS_NNDIRS}" | cut -d',' -f1)

if [ -f "${NN_DIR}/current/VERSION" ]; then
    echo "备 NameNode 元数据已存在，跳过 bootstrap: ${NN_DIR}"
    exit 0
fi

echo "从主 NameNode 引导备节点元数据（ordinal=${MECHARION_ORDINAL}）…"
"${HDFS}" namenode -bootstrapStandby -force -nonInteractive

echo "备 NameNode 引导完成"
