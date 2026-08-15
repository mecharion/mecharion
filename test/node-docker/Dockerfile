# Mecharion 测试节点（带 docker）—— 用于 docker / compose runtime 的验收
#
# 与 test/node 是**两个镜像，刻意不合并**。
#
# test/node 是刻意贫瘠的：不装 curl / wget / 包管理器索引，让违反 hermetic
# 约束的 Pack 直接失败（docs/adr/0015-offline-first-hermetic.md）。
# 一台装了 docker 的机器做不到那件事——`docker pull` 本身就是联网取内容，
# 「Pack 偷偷拉镜像」这类违规在这里会意外成功。
#
#   因此：hermetic 与 systemd runtime 的测试留在 test/node；
#         docker / compose runtime 的测试在这里。
#         M4 的 Pack 仍须单独过 mechpack lint --hermetic，不能靠环境兜底。
#
# 这台机器要以 --privileged 运行：dockerd 需要挂载、iptables 与 cgroup 写权限。

FROM debian:12-slim

ENV container=docker
ENV DEBIAN_FRONTEND=noninteractive

# systemd 与 docker 一起装。docker 走官方仓库而不是 Debian 的 docker.io：
# 后者没有 compose v2 插件，而 compose 是本镜像存在的一半理由。
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      systemd systemd-sysv dbus ca-certificates curl gnupg \
 && install -m 0755 -d /etc/apt/keyrings \
 && curl -fsSL https://download.docker.com/linux/debian/gpg \
      -o /etc/apt/keyrings/docker.asc \
 && chmod a+r /etc/apt/keyrings/docker.asc \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] \
https://download.docker.com/linux/debian bookworm stable" \
      > /etc/apt/sources.list.d/docker.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
      docker-ce docker-ce-cli containerd.io docker-compose-plugin \
 # curl / gnupg 只是为了配仓库，装完就撤。
 # 这不能让本镜像变得 hermetic（docker 自己就能联网），但少两个工具
 # 就少两条让测试意外通过的路。
 && apt-get purge -y curl gnupg \
 && apt-get autoremove -y \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/* \
 # 关掉容器里无意义、会拖慢启动或直接失败的 unit
 && find /etc/systemd/system /usr/lib/systemd/system -path '*.wants/*' \
      \( -name '*getty*'     -o -name '*udev*'      -o -name '*networkd*' \
      -o -name '*resolved*'  -o -name '*timesyncd*' -o -name '*firstboot*' \
      -o -name '*machine-id*' \) -delete \
 && rm -f /usr/lib/systemd/system/sockets.target.wants/*udev* \
 && rm -f /usr/lib/systemd/system/sysinit.target.wants/systemd-tmpfiles-setup-dev.service

# dockerd 由 systemd 拉起——这与生产形态一致，也顺带验证了「用 systemd
# runtime 安装 docker runtime」这条路径的落点是对的（ADR-0011）。
RUN systemctl enable docker.service containerd.service

# 容器内的 overlayfs 上再叠一层 overlay **挂不起来**（`invalid argument`），
# 因此镜像层的存放位置必须落在真正的文件系统上。
#
# **两个目录都要**：dockerd 的镜像在 /var/lib/docker，而 containerd 的
# snapshotter 在 /var/lib/containerd——只声明前者会在 `docker build` 的
# 最后一步失败，报的是一句 containerd 的 mount 错误，与 docker 看起来无关。
VOLUME /var/lib/docker
VOLUME /var/lib/containerd

# 与 test/node 相同的标准路径与软链：二进制由 -v 挂进 current/bin。
# **刻意不设 ENV PATH**——零 PATH 配置这条承诺在两个镜像上都要成立。
RUN mkdir -p /usr/local/lib/mecharion/current/bin /opt/mecharion/apps \
             /etc/mecharion /var/lib/mecharion /var/log/mecharion /run/mecharion \
 && for b in mechctl mechpack mechd mechlet; do \
      ln -s /usr/local/lib/mecharion/current/bin/$b /usr/bin/$b; \
    done

STOPSIGNAL SIGRTMIN+3
CMD ["/usr/lib/systemd/systemd"]
