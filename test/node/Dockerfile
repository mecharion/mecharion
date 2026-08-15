# Mecharion 测试节点 —— 一台能跑 systemd 的最小 Linux
#
# 这个镜像是**刻意贫瘠**的：只装 systemd 与 dbus，不装 curl / wget / git /
# make / gcc / 语言工具链，并清空 apt 索引。
#
# 这样一来，Pack 若违反 hermetic 约束（docs/adr/0015-offline-first-hermetic.md），
# 测试会**直接失败**，而不是在一台什么都有的机器上意外成功。
#
#   测试环境本身成为约束的执行者——这比 lint 的静态扫描更彻底：
#   静态扫描能被变量拼接绕过（C=cur"l"; $C …），缺失的二进制不能。
#
# Mecharion 的二进制由 -v 挂载进来，不打进镜像——改代码不需要重建。

FROM debian:12-slim

ENV container=docker
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/* \
 # 关掉容器里无意义、且会拖慢启动或直接失败的 unit
 && find /etc/systemd/system /usr/lib/systemd/system -path '*.wants/*' \
      \( -name '*getty*'     -o -name '*udev*'      -o -name '*networkd*' \
      -o -name '*resolved*'  -o -name '*timesyncd*' -o -name '*firstboot*' \
      -o -name '*machine-id*' \) -delete \
 && rm -f /usr/lib/systemd/system/sockets.target.wants/*udev* \
 && rm -f /usr/lib/systemd/system/sysinit.target.wants/systemd-tmpfiles-setup-dev.service

# 断言镜像的"贫瘠"性质。若某次基础镜像变更引入了这些工具，构建立即失败——
# 否则 hermetic 违规会在测试中被悄悄放过。
RUN for c in curl wget git make gcc g++ javac mvn gradle npm yarn pip pip3 rsync; do \
      if command -v "$c" >/dev/null 2>&1; then \
        echo "构建失败：测试镜像中不应存在 $c —— 它会让 hermetic 违规意外通过" >&2; \
        exit 1; \
      fi; \
    done \
 # apt 仍在，但索引已清空：apt-get install 会以「无法定位软件包」失败，
 # 这正是我们希望 Pack 作者看到的结果
 && ! apt-get install -y --no-install-recommends hello 2>/dev/null

# Mecharion 的标准路径（docs/design/04-paths-and-storage.md）
#
# 二进制由 -v 挂进 current/bin，/usr/bin 下是四条软链——与真实安装完全一致。
# **刻意不设 ENV PATH**：那会把「零 PATH 配置是否真的成立」这个问题绕过去，
# 而它正是布局设计要保证的性质。软链在挂载前是悬空的，挂上即生效。
RUN mkdir -p /usr/local/lib/mecharion/current/bin /opt/mecharion/apps \
             /etc/mecharion /var/lib/mecharion /var/log/mecharion /run/mecharion \
 && for b in mechctl mechpack mechd mechlet; do \
      ln -s /usr/local/lib/mecharion/current/bin/$b /usr/bin/$b; \
    done

STOPSIGNAL SIGRTMIN+3
CMD ["/usr/lib/systemd/systemd"]
