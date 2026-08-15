# Mecharion 多节点测试节点 —— 能跑 systemd，且**有 sshd**
#
# 与 test/node 是两个镜像，刻意不合并。
#
# test/node 是**刻意贫瘠**的：不装 curl / wget / 包管理器索引，让违反
# hermetic 约束的 Pack 直接失败（docs/adr/0015-offline-first-hermetic.md）。
# 那条性质由**单节点套件**强制执行，至今有效。
#
# 这台不一样：它要验的是 `mechctl node bootstrap ssh://…`，而那条路径的
# 前提就是目标机器上有 sshd。真实用户的机器本来也不是贫瘠的——
# 强求它反而让 bootstrap 这条路测不了。
#
#   贫瘠 → test/node，跑单节点套件，强制 hermetic
#   带 ssh → test/node-ssh，跑多节点集群，验编排与加入
#
# 两处约束各在该在的地方，而不是靠一条写在错误位置的断言假装还在保。

FROM debian:12-slim

ENV container=docker
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      systemd systemd-sysv dbus openssh-server \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/* \
 # 关掉容器里无意义、且会拖慢启动或直接失败的 unit
 && find /etc/systemd/system /usr/lib/systemd/system -path '*.wants/*' \
      \( -name '*getty*'     -o -name '*udev*'      -o -name '*networkd*' \
      -o -name '*resolved*'  -o -name '*timesyncd*' -o -name '*firstboot*' \
      -o -name '*machine-id*' \) -delete \
 && rm -f /usr/lib/systemd/system/sockets.target.wants/*udev* \
 && rm -f /usr/lib/systemd/system/sysinit.target.wants/systemd-tmpfiles-setup-dev.service

# sshd：**只认公钥**。
#
# 测试环境里放一个固定口令看起来更省事，但那会让「bootstrap 用的是密钥
# 还是口令」这件事在验收里变得含糊——而 Mecharion 只实现了密钥这一条路。
RUN systemctl enable ssh \
 && mkdir -p /root/.ssh && chmod 700 /root/.ssh \
 && sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin prohibit-password/' \
      /etc/ssh/sshd_config \
 && sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication no/' \
      /etc/ssh/sshd_config

# 与 test/node 相同的标准路径与软链：二进制由 -v 挂进来再复制到本地。
# **刻意不设 ENV PATH**——零 PATH 配置这条承诺在每个镜像上都要成立。
RUN mkdir -p /usr/local/lib/mecharion/current/bin /opt/mecharion/apps \
             /etc/mecharion /var/lib/mecharion /var/log/mecharion /run/mecharion \
 && for b in mechctl mechpack mechd mechlet; do \
      ln -s /usr/local/lib/mecharion/current/bin/$b /usr/bin/$b; \
    done

STOPSIGNAL SIGRTMIN+3
CMD ["/usr/lib/systemd/systemd"]
