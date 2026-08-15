#!/bin/sh
# 把本地已有的 edits 日志推送到 JournalNode 仲裁组，初始化共享编辑日志。
#
# scope: once + when: profile == "ha"
# 只在 HA 形态、且整个 Component 只执行一次。
#
# 这是 distributed → ha 形态迁移的关键一步：已有的非 HA NameNode 元数据
# 通过本步骤进入 JournalNode 共享存储，之后备 NameNode 才能 bootstrap。
set -eu

HDFS="${MECHARION_PATHS_CURRENT}/bin/hdfs"

# JournalNode 必须先就绪——由 roles[namenode].requires: [journalnode] 保证启动顺序，
# 但仲裁组达成多数派仍需要时间。
echo "等待 JournalNode 仲裁组就绪…"
sleep 10

if "${HDFS}" namenode -initializeSharedEdits -force -nonInteractive; then
    echo "共享编辑日志初始化完成"
else
    rc=$?
    # 已初始化过时 Hadoop 返回非零；由引擎的 once 标记兜底，此处仅告警
    echo "initializeSharedEdits 返回 ${rc}——若共享目录已初始化属正常" >&2
    exit ${rc}
fi
