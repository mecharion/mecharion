#!/bin/sh
# KRaft 存储格式化。
#
# 注意 scope 是 perInstance（默认）而非 once —— 与 HDFS 的 namenode -format 不同：
# KRaft 的每个节点都要用同一个 cluster-id 格式化自己的本地存储目录。
# 「集群唯一」体现在 cluster_id 参数上，而非「只执行一次」。
#
# 幂等性由 meta.properties 的存在判断。
set -eu

BIN="${MECHARION_PATHS_CURRENT}/bin"
CONF="${MECHARION_PATHS_CONFIG}/server.properties"
FIRST_DIR=$(echo "${MECHARION_PATHS_LOGDIRS}" | cut -d',' -f1)

if [ -f "${FIRST_DIR}/meta.properties" ]; then
    echo "存储已格式化，跳过: ${FIRST_DIR}"
    exit 0
fi

"${BIN}/kafka-storage.sh" format \
    --cluster-id "${MECHARION_PARAM_CLUSTER_ID}" \
    --config "${CONF}" \
    --ignore-formatted

echo "KRaft 存储格式化完成 (node.id 序号 ${MECHARION_ORDINAL})"
