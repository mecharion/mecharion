#!/bin/sh
# 关闭透明大页。由 mecharion-disable-thp.service 在每次开机时执行。
# THP 设置位于 sysfs，不持久化，因此必须靠 oneshot 单元而非 sysctl。
set -eu

for f in /sys/kernel/mm/transparent_hugepage/enabled \
         /sys/kernel/mm/transparent_hugepage/defrag; do
    [ -w "$f" ] || continue
    echo never > "$f"
done

exit 0
