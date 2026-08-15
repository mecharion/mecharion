#!/bin/sh
# 格式化 NameNode 元数据目录。
#
# scope: once —— 整个 Component 生命周期内只执行一次，在 namenode 角色
# ordinal 最小的实例上。引擎记录执行标记，重复 apply 不会重跑，
# 因此本脚本无需（也不应）自行判断「是否已格式化」。
#
# 但仍保留一层防御：若元数据目录已有内容，直接退出而非覆盖——
# 格式化一个有数据的 NameNode 会丢失整个集群的元数据。
set -eu

NN_DIR=$(echo "${MECHARION_PATHS_NNDIRS}" | cut -d',' -f1)

if [ -f "${NN_DIR}/current/VERSION" ]; then
    echo "NameNode 已格式化，跳过: ${NN_DIR}"
    exit 0
fi

"${MECHARION_PATHS_CURRENT}/bin/hdfs" namenode -format \
    -force -nonInteractive \
    -clusterId "${MECHARION_COMPONENT}"

echo "NameNode 格式化完成: ${NN_DIR}"
