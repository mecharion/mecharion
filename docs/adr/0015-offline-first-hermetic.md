# ADR-0015: 离线优先与 hermetic 约束

- **状态**：已接受
- **日期**：2026-08-01
- **相关**：[ADR-0005](0005-pack-logic-payload-split.md)、[ADR-0016](0016-mandatory-pack-signing.md)

## 背景

产品要运行在从中心化数据中心到**边缘完全离线单机**的全部环境。边缘环境的现实：

- 无互联网，甚至无内网出口
- 无法访问 apt/yum 源、npm/PyPI/Maven、容器 registry
- 交付靠 U 盘或跨网闸摆渡
- 现场无工程师

若部署过程有任何外部依赖，这些环境就直接不可用。

## 候选方案与调研

### 方案 A：默认在线，提供「离线模式」

多数工具的做法（Ansible + 私有源、Helm + 私有 registry）。

- ❌ 离线是特例分支，测试覆盖弱、长期腐化
- ❌ 用户需自建镜像源/registry——把复杂度转嫁给最没有能力承担的边缘用户
- ❌ Pack 作者无约束，随手写 `apt-get install` 就破坏了离线可用性

### 方案 B：离线为唯一路径，构建期解决全部依赖 ⭐

调研对照：

| 系统 | 做法 | 借鉴 |
|---|---|---|
| **Cloudera Parcel** | 自包含分发单元，不依赖 OS 包管理器。**这是 CDH 能在隔离环境部署的根本原因** | 直接借鉴 |
| **Bazel / Nix** | hermetic build——构建期声明全部依赖，执行期无网络 | 「hermetic」概念与静态校验思路 |
| **Docker 镜像** | 构建期解决依赖，运行期只需镜像 | 阶段划分 |
| **Ansible** | 无此约束，role 里随处可见 `package:` 与 `get_url:` | 反面：因此 Ansible 在隔离环境需要大量改造 |

## 决策

**部署阶段不允许任何外部服务依赖。** 全部依赖在**组件开发阶段**由开发者用自己的构建工具解决，产物随 Pack 分发。

### 阶段划分与职责边界

```
【开发阶段】开发者自己的工具链
   make / mvn package / go build / 下载上游 tarball / docker build
        ↓ 产物
【打包阶段】mechpack
   assemble → lint → sign → bundle/push
        ↓ Pack
【部署阶段】mechlet —— 零外部依赖
   校验签名 → 校验 blob 摘要 → 物化 → 启动
```

**`mechpack` 不构建软件，只组装 Pack。** 命令刻意叫 `assemble` 而非 `build`，从命名上杜绝误解。这条边界必须清晰，否则 mechpack 会不断被要求增加构建能力，最终变成一个糟糕的构建系统。

### 机器可校验，而非口头约定

```
$ mechpack lint --hermetic
✗ hooks/pre-install.sh:12   外部依赖调用: apt-get
✗ hooks/post-install.sh:4   外部依赖调用: curl https://…
✗ pack.yaml:34              资源类型 `package` 需要 repo，非 hermetic
```

拦截清单：`apt-get` `yum` `dnf` `apk` `zypper` `curl` `wget` `git clone` `npm` `pip` `mvn` `gradle` `go build` `make` `docker pull` 等。

官方 Pack 仓库 CI 强制此检查通过。

`package`（OS 包）资源类型仍然提供——总有用户在有 repo 的环境需要它——但被明确标记为 non-hermetic 并被 lint 拦截。

## 理由

**离线若是「模式」，它就会腐化；离线若是「唯一路径」，它就必然可靠。**

方案 A 的根本问题在于：在线路径是开发者日常走的路径，离线路径只在客户现场走。前者每天被测试，后者每年被发现坏了一次。把两者合一，边缘用户就自动获得了与中心用户相同的质量保证。

**静态校验是这条约束能落地的关键。** 没有 `lint --hermetic`，第一个第三方 Pack 里就会出现 `apt-get install -y libssl-dev`，离线承诺当场破产——而且是在客户现场才发现。

Cloudera Parcel 是最直接的正面证据：CDH 能在完全隔离的政企环境部署，根本原因就是 parcel 自包含、不依赖 OS 包管理器。

## 后果

### 收益

- 边缘与中心走完全相同的代码路径，质量一致
- 用户无需自建镜像源或 registry
- 部署过程无网络抖动、源不可用、版本漂移等故障类别
- 离线约束反向简化了引擎：不需要依赖求解器、包管理器适配层、网络重试策略

### 代价

- **Pack 体积大**：JDK + PostgreSQL 的 thick pack 轻松上 GB。以内容寻址去重与增量传输缓解（[ADR-0005](0005-pack-logic-payload-split.md)）
- **Pack 作者负担转移**：依赖解析从部署期转移到开发期，作者需要自己处理。这是有意的——**这件事本来就该在开发期做**，只是多数工具允许拖到部署期
- **OS 层依赖是理想主义的破功点**：PostgreSQL 需要 libicu、某些组件需要 libaio、glibc 版本差异会直接崩溃。三条务实应对：
  1. 官方 Pack 优先静态链接构建
  2. 必须动态链接时，把 `.so` 一起打进 generation 目录，用 `LD_LIBRARY_PATH` 解决
  3. `requires.os` 在安装前**校验并快速失败**，给出人类可读错误，而非运行时段错误

  **绝不尝试自动修复宿主机依赖**——那就退回到 repo 依赖了
- **`lint --hermetic` 会有误报与绕过**：脚本可以用变量拼接命令名规避静态检查。当前接受——它的目标是防止无意违反，而非对抗恶意作者（后者由签名与可信发布者列表处理）

## 参考

- Cloudera Parcel 自包含分发模型
- Bazel hermetic build
- Nix 的封闭构建环境
