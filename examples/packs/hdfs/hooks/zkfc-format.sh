#!/bin/sh
# 在 ZooKeeper 中创建 HA 自动故障转移所需的 znode。
#
# scope: once —— 整个 Component 只做一次；两个 ZKFC 实例共用同一个 znode。
set -eu

HDFS="${MECHARION_PATHS_CURRENT}/bin/hdfs"

if "${HDFS}" zkfc -formatZK -force -nonInteractive; then
    echo "ZKFC znode 创建完成"
else
    rc=$?
    echo "zkfc -formatZK 返回 ${rc}——若 znode 已存在属正常" >&2
    exit ${rc}
fi
