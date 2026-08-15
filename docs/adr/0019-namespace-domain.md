# ADR-0019: 命名空间与域名约定

- **状态**：已接受
- **日期**：2026-08-01
- **相关**：[ADR-0018](0018-project-naming.md)、[ADR-0011](0011-docker-compose-in-v1.md)

## 背景

项目将注册 **`mecharion.dev`** 作为官网域名（源码托管与版本发布仍在 GitHub）。

域名可用后需要决定：**哪些地方使用反向 DNS 命名空间，哪些地方不用。**

这个决定影响 Pack 格式、容器标签、systemd unit 命名、Go module path 等多处，且部分位置一旦定下就难以更改。

## 决策

### 使用反向 DNS `dev.mecharion.*` 的地方

| 场景 | 形式 | 例 |
|---|---|---|
| **容器标签** | `dev.mecharion.<key>` | `dev.mecharion.site` `dev.mecharion.component` `dev.mecharion.role` `dev.mecharion.generation` `dev.mecharion.managed-by` |
| **OCI artifact media type** | `application/vnd.mecharion.pack.v1+json` | 遵循 OCI 惯例 |
| **对象注解/标签 key**（未来） | `dev.mecharion/<key>` | 对齐 Kubernetes 惯例 |

### 不使用反向 DNS 的地方

| 场景 | 采用形式 | 原因 |
|---|---|---|
| **Pack 格式版本** | `schema: pack/v1` | 见下文详述 |
| **systemd unit 名** | `mecharion-<component>-<role>.service` | systemd 生态惯例是短前缀，反向 DNS 会让 `systemctl status` 输出难以阅读 |
| **Go module path** | `github.com/mecharion/mecharion` | 与代码托管一致，不引入 vanity import 的额外基础设施与故障点 |
| **文件系统路径** | `/etc/mecharion/` `/opt/mecharion/` | FHS 惯例 |

## 理由

### 容器标签为什么用反向 DNS

Docker 生态的既定惯例（对比 `org.opencontainers.image.*`），且这些标签必须与用户自有容器的标签空间**绝对无冲突**——mechlet 的隔离规则「只操作带 `dev.mecharion.*` 标签的容器」（[ADR-0011](0011-docker-compose-in-v1.md)）的安全性直接依赖于此。

一次标签命名冲突可能导致误删用户的生产容器，这里的谨慎是值得的。

### Pack 格式为什么**不**用域名 —— 本 ADR 的核心

早期决策记为「不用域名做 apiVersion group」，理由是「尚未注册域名」。域名注册后该理由失效，需要重新论证。**重新论证的结论是：仍然不用。**

新的理由：**Pack 是一个文件格式，不是 API 组。**

| 对照 | 做法 | 为什么 |
|---|---|---|
| **Helm Chart** | `apiVersion: v2` | 文件格式，只有一个定义者 |
| **Docker Compose** | `version: "3.8"` | 同上 |
| **GitHub Actions** | 无版本字段 | 同上 |
| **Kubernetes CRD** | `apiVersion: <group>/<version>` | **需要域名**，因为多个第三方 API 组共存于同一个 API Server，必须靠域名区分归属 |

Kubernetes 使用域名分组是为了解决一个 Mecharion 不存在的问题：**多方定义的资源类型共存于同一命名空间**。

Mecharion 不存在「第三方定义自己的 Pack 格式组并与官方格式共存」的场景——Pack 格式只有一个，由项目定义。套上域名只会给每个 Pack 文件增加噪音，不带来任何区分能力。

因此保持：

```yaml
schema: pack/v1
```

**若将来 mechd 的 REST API 需要支持第三方扩展资源类型**，届时再为 **API 对象**引入 `dev.mecharion/v1` 形式的组。API 对象与 Pack 文件格式是两套独立的版本空间，互不影响。

### Go module path 为什么不用 vanity import

`mecharion.dev/mecharion` 形式的 vanity import 需要域名持续托管一个 meta 标签页面。收益是「品牌一致」，代价是：

- 多一个必须永久维护的基础设施
- 域名或托管故障会导致**全球所有用户无法 `go get`**
- 增加离线/内网环境的代理配置复杂度

对一个以「零外部依赖」为核心卖点的项目，为品牌美观引入一个新的外部依赖是自相矛盾的。

## 后果

### 收益

- 容器标签全局唯一，隔离规则可靠
- Pack 文件保持简洁，无冗余噪音
- `go get` 不依赖域名可用性
- systemd unit 名可读
- API 对象的域名分组空间保留待用

### 代价

- **命名空间不统一**：项目在不同场景使用不同形式（`dev.mecharion.*` / `mecharion-*` / `pack/v1` / `github.com/mecharion/...`）。需要本 ADR 说明各自理由，否则会被认为是随意的
- **Pack 格式若将来需要第三方扩展，会有一次迁移**：当前判断该场景不会出现；若出现，`schema` 字段可扩展为接受 `<domain>/<name>/<version>` 形式，向后兼容
- **放弃了 vanity import 的品牌一致性**：`github.com/mecharion/mecharion` 略长。已接受

## 参考

- OCI 标签惯例 `org.opencontainers.image.*`
- Helm Chart `apiVersion: v2`
- Kubernetes CRD API group 命名规则
- Go vanity import paths 的运维代价
