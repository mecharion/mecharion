#!/bin/sh
# 创建流复制用户与供消费方使用的应用账号。
#
# 声明为 scope: once —— 整个 Component 生命周期内只执行一次，
# 在 primary 角色 ordinal 最小的实例上。引擎记录执行标记，重复 apply 不重跑。
#
# **密码经由文件传入，不经命令行也不经环境变量**：命令行会出现在同机任何
# 用户的 ps 输出里，环境变量会出现在 /proc/<pid>/environ 与崩溃转储里。
# 引擎把值落到 0600 的临时文件，hook 结束即删。
set -eu

BIN="${MECHARION_PATHS_CURRENT}/bin"
PORT="${MECHARION_PARAM_PORT}"
PWFILE="${MECHARION_PARAM_FILE_ADMIN_PASSWORD}"

APP_DB="${MECHARION_PARAM_APP_DB}"
APP_USER="${MECHARION_PARAM_APP_USER}"
APP_PWFILE="${MECHARION_PARAM_FILE_APP_PASSWORD}"

# 等待主库可接受连接（postStart 之后仍可能处于恢复中）
for _ in $(seq 1 60); do
    "${BIN}/pg_isready" -p "${PORT}" -q && break
    sleep 1
done

"${BIN}/psql" -p "${PORT}" -U postgres -v ON_ERROR_STOP=1 <<SQL
DO \$\$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'replicator') THEN
    CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD '$(cat "${PWFILE}")';
  END IF;
END
\$\$;
SQL

echo "流复制用户已就绪"

# ── 供消费方使用的应用账号 ────────────────────────────────────────────
# 消费方拿到的是这个账号，**不是 superuser**。给下游应用 superuser
# 口令是提权：一个被攻破的 web 应用会直接拥有整个实例。
#
# 幂等：角色已存在时改密码而非报错——引擎轮换 app_password 后重跑本 hook
# 即可让数据库侧跟上。
"${BIN}/psql" -p "${PORT}" -U postgres -v ON_ERROR_STOP=1 <<SQL
DO \$\$
BEGIN
  IF EXISTS (SELECT FROM pg_roles WHERE rolname = '${APP_USER}') THEN
    ALTER ROLE ${APP_USER} WITH LOGIN PASSWORD '$(cat "${APP_PWFILE}")';
  ELSE
    CREATE ROLE ${APP_USER} WITH LOGIN PASSWORD '$(cat "${APP_PWFILE}")';
  END IF;
END
\$\$;
SQL

# CREATE DATABASE 不能出现在 DO 块里，单独判一次
if ! "${BIN}/psql" -p "${PORT}" -U postgres -tAc \
        "SELECT 1 FROM pg_database WHERE datname = '${APP_DB}'" | grep -q 1; then
    "${BIN}/psql" -p "${PORT}" -U postgres -v ON_ERROR_STOP=1 \
        -c "CREATE DATABASE ${APP_DB} OWNER ${APP_USER}"
fi

echo "应用账号 ${APP_USER} 与数据库 ${APP_DB} 已就绪"
