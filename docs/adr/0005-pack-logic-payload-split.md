# ADR-0005: Pack 逻辑与载荷分离，blob 内容寻址

- **状态**：已接受
- **日期**：2026-08-01
- **相关**：[ADR-0015](0015-offline-first-hermetic.md)、[ADR-0016](0016-mandatory-pack-signing.md)

## 背景

Pack 需要同时满足两个方向相反的需求：

- **离线自包含**：边缘环境靠 U 盘交付，Pack 必须能带上全部字节
- **高效分发**：中心化环境下，改一行模板不应触发 200MB 的重新分发

## 候选方案与调研

### 方案 A：单体归档（逻辑与载荷打在一起）

类似 RPM / DEB / Cloudera Parcel。

- ✅ 简单，天然自包含
- ❌ 改一行配置模板要重新分发整包
- ❌ 同一个 JDK 被三个 Pack 各自打包一份，无法去重
- ❌ 升级时全量传输，无法只传差异

### 方案 B：完全外置载荷（只引用远程地址）

类似 Helm Chart 引用容器镜像。

- ✅ 逻辑极小
- ❌ **直接违反离线约束**——部署时必须能访问 registry 或下载源

### 方案 C：内容寻址 blob + 双形态分发 ⭐

逻辑引用 blob 的 sha256；blob 可以内嵌（thick）或外部解析（thin）。

调研对照：

| 系统 | 做法 | 借鉴 |
|---|---|---|
| **OCI / Docker** | manifest 引用按 digest 寻址的 layer；`docker save/load` 可导出为 tar | 内容寻址 + 离线导出的成熟范式 |
| **Git** | 全部对象按 SHA 寻址，天然去重 | 内容寻址的可靠性验证 |
| **Nix** | 按内容哈希的 store path，多版本共存 | 去重与多版本并存 |
| **Cloudera Parcel** | 单体 tar.gz，无内容寻址 | 反面：无法去重、无法增量 |
| **Helm** | Chart 小、镜像外置 | 反面：无法离线 |

## 决策

**采用方案 C。**

```yaml
blobs:
  main:
    linux/amd64: { sha256: "ab12ef…", size: 31457280 }
    linux/arm64: { sha256: "cd34ab…", size: 30214656 }
```

两种分发形态，**逻辑完全相同**：

| 形态 | 内容 | 用途 |
|---|---|---|
| **thin pack** | 逻辑 + blob 摘要引用 | 中心化。blob 从 mechd blob store 按需拉取 |
| **thick pack**（`.mpack`） | 逻辑 + 全部 blob | 离线。单文件，U 盘可携带 |

```
mechpack bundle   # thin → thick
mechpack push     # thick → thin，blob 入库
```

blob 物理存储始终在文件系统，**绝不进数据库**：`/var/lib/mecharion/blobs/sha256/ab/ab12ef…`

## 理由

内容寻址是**同时满足两个相反需求的唯一方式**。thick/thin 只是同一份逻辑的两种打包，不是两种格式——这意味着离线场景不是特例分支，而是同一条代码路径的另一种输入。

副产品同样重要：

- 升级只传差异 blob（未变化的 blob 按摘要跳过）
- 多版本共存自动去重（同一个 JDK blob 被多个 Pack 共享）
- 节点侧可按引用计数 GC
- **摘要天然是完整性校验**，与签名机制（[ADR-0016](0016-mandatory-pack-signing.md)）无缝衔接
- **对容器 Runtime 免费适用**：`docker save` 的 tar 就是一个 blob，容器支持完全不损害离线约束（[ADR-0011](0011-docker-compose-in-v1.md)）

## 后果

### 收益

- 离线与高效分发同时满足，无需二选一
- 自动去重、增量传输、多版本共存
- 完整性校验内建
- 容器镜像与二进制 tarball 走同一条分发链路

### 代价

- **实现复杂度高于单体归档**：需要 blob store、引用计数、GC、缺失 blob 解析逻辑
- **thick pack 体积大**：JDK + PostgreSQL 的 thick pack 轻松上 GB。需要断点续传、传输并发限流。这些不难但必须在设计中占位，否则到 50 节点规模时会很难受
- **两种形态需要用户理解**：文档必须清楚说明何时用哪种。缓解：默认行为足够聪明（`mechpack bundle` 给离线场景、`push` 给中心化场景），用户不需要主动思考

## 参考

- OCI Image Specification — content-addressable layers
- `docker save` / `docker load` 离线镜像分发
- Nix store paths — 内容哈希与多版本共存
- Cloudera Parcel（反面参照：单体归档的局限）
