#!/bin/sh
# 初始化主库数据目录。
# 幂等性由资源的 creates 守卫保证（PG_VERSION 存在即跳过），脚本本身不重复判断。
set -eu

PGDATA="${MECHARION_PATHS_CONFIG}"
BIN="${MECHARION_PATHS_CURRENT}/bin"

# admin_password 标记为 sensitive，通过临时文件传入而非环境变量
PWFILE="${MECHARION_PARAM_FILE_ADMIN_PASSWORD}"

"${BIN}/initdb" \
    --pgdata="${PGDATA}" \
    --encoding="${MECHARION_PARAM_ENCODING}" \
    --locale=C \
    --username=postgres \
    --auth-local=peer \
    --auth-host=scram-sha-256 \
    --pwfile="${PWFILE}"

echo "initdb 完成: ${PGDATA}"
