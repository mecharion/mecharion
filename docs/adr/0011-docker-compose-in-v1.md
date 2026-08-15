# ADR-0011: v1 纳入 docker 与 compose runtime

- **状态**：已接受
- **日期**：2026-08-01
- **相关**：[ADR-0010](0010-runtime-abstraction.md)、[ADR-0009](0009-node-volumes-multidisk.md)

## 背景

初始规划中，v1 只实现 systemd runtime，docker 作为 M7 的「验证接缝」的最小实现。用户要求将 docker 与 compose 提前至 v1 正式支持。

## 决策

**v1 实现三个 Runtime：systemd、docker、compose。** 并在里程碑中把 docker/compose（M4）**排在漂移检测（M5）与升级回滚（M6）之前**。

## 理由

### ① 里程碑顺序比功能本身更重要

**在漂移检测与升级编排写出来之前，就让第二个 Runtime 存在。**

这样那两块逻辑天生就是 Runtime 无关的，而不是先写死在 systemd 上、再回头重构。

[ADR-0010](0010-runtime-abstraction.md) 的核心论点是「接缝正确性只有第二个实现才能验证」。若 M3/M4 在只有 systemd 时完成，接缝错误会被固化进漂移检测与 Rollout 编排——那是全项目最难重构的两块代码。

**这是提前 docker 的最大收益，比「早点支持容器」本身重要得多。**

### ② 离线约束不受损害

`docker save` 输出的 tar 就是一个 blob，走完全相同的内容寻址分发链路（[ADR-0005](0005-pack-logic-payload-split.md)）。**不需要容器 registry，`.mpack` 文件里就带着镜像。**

这构成 Mecharion 相对绝大多数容器部署方案的实质差异：容器化部署也能完全离线。

## 关键设计

### docker 与 compose 是两个 Runtime

- `runtime: docker` → 一个容器 = 一个 workload
- `runtime: compose` → **整个 compose project 作为一个不透明的 workload**

**不把 compose 里的 service 映射成 Role。** compose 有自己的 `depends_on`、networks、volumes 语义，硬映射会产生两套冲突的编排概念。`Observe()` 聚合 `docker compose ps` 的结果，逐 service 放进 `Raw` 供 UI 展开。

代价是状态粒度粗一档；收益是模型不打架，且能直接消费用户既有的 compose 文件。

### 不隐式安装 docker

```yaml
roles:
  - name: web
    workload:
      runtime: docker
      requires: { capability: { docker: ">=20.10" } }
```

mechd 在放置时校验 Node 上报的 capability（来自 `Probe()`），不满足则拒绝并给出可执行的错误：

```
node-7 缺少 docker（要求 >=20.10）
  · 部署官方 Pack "docker" 以离线安装，或
  · 在 /etc/mecharion/mechlet.yaml 中指定已有的 docker socket
```

**隐式安装 root 级运行时是运维事故的来源**（[原则六](../design/00-overview.md#原则六显式优于隐式)）。

### 与用户既有 docker 共存

```yaml
runtimes:
  docker:
    enabled: true
    socket:  auto        # auto | /path/to/sock
    managed: false       # false = 用户自己的 docker，绝不重启/升级/卸载它
```

配套**硬规则**：mechlet 只操作带 `dev.mecharion.*` 标签的容器。

```
dev.mecharion.site        dev.mecharion.component
dev.mecharion.role        dev.mecharion.generation
dev.mecharion.managed-by
```

漂移检测、`Observe`、清理全部按标签过滤。**没有这条，一次误清理就能删掉用户与 Mecharion 无关的生产容器。**

### 官方 `docker` Pack

该 Pack 本身使用 **`runtime: systemd`**——**用 systemd runtime 安装 docker runtime**。不存在鸡生蛋问题：mechlet 由 bootstrap 装 → docker 由 systemd-runtime Pack 装 → docker-runtime 的 Pack 最后才来。

离线安装路径现成：Docker 官方发布静态二进制 tarball（内含 dockerd / docker / containerd / runc / ctr），Compose v2 是单个二进制插件。两者都是普通 blob。

`requires.os` 需校验 cgroup v2（或 v1 的相应挂载）、iptables/nftables 可用、内核版本下限，失败快速报错。

### 数据用 bind mount，不用 named volume

```yaml
mounts:
  - { from: "{{ .Paths.Data }}",   to: /var/lib/postgresql/data }
  - { from: "{{ .Paths.Config }}", to: /etc/postgresql, readOnly: true }
```

named volume 的存储位置由 dockerd 决定，会同时打破：

- **多盘绑定设计**（[ADR-0009](0009-node-volumes-multidisk.md)）
- **「数据目录升级永不触碰」不变式**（[ADR-0008](0008-immutable-generation-linkinto.md)）
- 备份与现场排查的一致性

bind mount 让容器化组件与裸机组件在数据管理上行为完全一致。named volume 作为 opt-in 保留。

## 后果

### 收益

- 漂移检测与升级编排天生 Runtime 无关
- Runtime 接缝在低成本阶段被验证
- 容器化部署同样完全离线
- 容器与裸机在数据管理、路径绑定、备份上行为一致
- 可直接消费用户既有的 compose 文件

### 代价

- **v1 工作量增加**：docker/compose 是一个独立里程碑，三个 Runtime 都要在后续的漂移与升级里程碑中回归验证
- **compose 状态粒度粗**：project 级而非 service 级。以 `Raw` 字段部分补偿
- **多一类环境依赖问题**：docker 版本差异、rootless 模式、cgroup v1/v2、socket 路径差异都会成为支持负担。以 `Probe()` 的 capability 上报与明确的 `requires` 声明控制
- **`managed: false` 的边界需严格执行**：标签过滤必须覆盖所有操作路径，遗漏一处就可能误伤用户容器。这是需要专门测试覆盖的高风险点

## 参考

- Docker 静态二进制发行包（离线安装路径）
- `docker save` / `docker load`
- OCI 标签惯例 `org.opencontainers.image.*`（反向 DNS 命名参照）
- Docker Compose v2 插件形态
