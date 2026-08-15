#!/bin/sh
# 从主库拉取基础备份，初始化副本数据目录。
# 幂等性由资源的 creates 守卫保证（PG_VERSION 存在即跳过）。
set -eu

PGDATA="${MECHARION_PATHS_CONFIG}"
BIN="${MECHARION_PATHS_CURRENT}/bin"
PRIMARY="${MECHARION_PARAM_PRIMARY_HOST}"
PORT="${MECHARION_PARAM_PORT}"

# 复制用户口令由 bootstrap-roles.sh 在主库上创建，此处通过 .pgpass 提供
export PGPASSFILE="${MECHARION_PATHS_DATA}/.pgpass"

"${BIN}/pg_basebackup" \
    --host="${PRIMARY}" \
    --port="${PORT}" \
    --username=replicator \
    --pgdata="${PGDATA}" \
    --wal-method=stream \
    --write-recovery-conf \
    --progress \
    --no-password

echo "基础备份完成，副本已就绪跟随 ${PRIMARY}:${PORT}"
