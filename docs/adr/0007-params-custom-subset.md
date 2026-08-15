# ADR-0007: params 采用自定义类型子集而非 JSON Schema

- **状态**：已接受
- **日期**：2026-08-01
- **相关**：[ADR-0006](0006-multi-role-pack.md)

## 背景

Pack 需要声明参数：类型、默认值、校验规则。这套 schema 有三个消费者：

1. **CLI/API** — 校验用户输入
2. **Web UI** — 自动生成配置表单（这是 Cloudera Manager 可用性的核心来源）
3. **引擎** — 决定参数变更后要做什么（重启？reload？拒绝？）

第三个消费者是关键，也是本决策的分水岭。

## 候选方案与调研

| 方案 | 生态 | 表达力 | 编写负担 |
|---|---|---|---|
| **完整 JSON Schema** | ✅ 成熟，UI 生成器现成（react-jsonschema-form 等） | ✅ 强 | ❌ 重 |
| **自定义类型子集** | ❌ 需自建 | ⚠️ 有限但可控 | ✅ 轻 |
| **无 schema（自由 map）** | — | ❌ 无校验、UI 无法生成表单 | ✅ 最轻 |

调研对照：

| 系统 | 做法 | 观察 |
|---|---|---|
| **Helm** | `values.schema.json`（完整 JSON Schema），可选 | 大多数 Chart 不写；UI 生态没真正建立起来 |
| **Cloudera Manager** | **自定义参数描述符**，含 type / unit / 是否需重启 / 是否敏感 / 配置分组 | 其配置界面的可用性直接来自这套元数据 |
| **Ambari** | 自定义 XML config schema，含 `restart_required` | 同上 |
| **Terraform** | 自定义 schema（HCL 类型系统） | 未采用 JSON Schema |
| **Kubernetes CRD** | OpenAPI v3（JSON Schema 子集）+ **大量 `x-kubernetes-*` 扩展** | **完整 schema 不够用，必须挂扩展** |

Kubernetes 的做法是最有价值的信号：即使采用 OpenAPI，仍不得不引入 `x-kubernetes-list-type`、`x-kubernetes-preserve-unknown-fields` 等一系列扩展来表达标准 schema 无法表达的语义。

## 决策

**采用自定义类型子集，且不提供 JSON Schema 逃生舱。** 类型集是完整且封闭的。

### 12 个类型

```
string  int  float  bool  enum
path  port  duration  size  cidr
secret  list<T>
```

其中 `path` / `port` / `duration`（`30s`）/ `size`（`4GB`）/ `cidr` 是**运维语义类型**——它们让 UI 渲染合适的控件、让引擎做单位换算与校验。

### 字段集

```yaml
type / default / required / description
min / max / pattern / enum          # 校验
unit                                # 显示单位
group / advanced                    # UI 分组与折叠
sensitive: true                     # 日志与 UI 中脱敏
immutable: true                     # 创建后不可改
restartRequired: true               # 变更需重启 workload
reloadRequired: true                # 变更只需 reload
from: "topology.…"                  # 由拓扑推导，不由用户填写
```

### 类型集的演进方式

类型不够用时，**为 `pack/v1` 增补新类型**——这是向后兼容的变更，Pack 通过 `requires.mecharion: ">=x.y.z"` 声明引擎版本下限即可。

**不引入第二套 schema 体系。** 曾考虑提供 `paramsSchema: jsonschema` 逃生舱，评估后放弃，理由见下文「为什么不留逃生舱」。

## 理由

**决定性理由：`immutable` / `restartRequired` / `reloadRequired` / `sensitive` / `from` 这五个字段 JSON Schema 表达不了，而它们对运维工具是刚需。**

其中 `restartRequired` **不是 UI 装饰，它直接决定引擎行为**——Rollout 据此判断参数变更后要不要重启 workload：

| 字段 | Rollout 行为 |
|---|---|
| `restartRequired: true` | 变更后重启 |
| `reloadRequired: true` | 变更后 `Runtime.Reload`，不中断服务 |
| `immutable: true` | 拒绝变更，提示需重建 Component |
| 都未设置 | 仅更新配置文件，不动进程 |

若采用完整 JSON Schema，就必须在旁边再挂一套 `x-mecharion-*` 扩展——那时既承担了 JSON Schema 的编写负担，又没有获得它的生态收益（因为通用 UI 生成器不认识你的扩展）。**两头落空。**

Cloudera Manager 与 Ambari 都选择了自定义描述符，而它们的配置界面恰是这两个产品最被认可的部分。这是最直接的正面证据。

### 为什么不留逃生舱

初版设计曾保留 `paramsSchema: jsonschema` 作为「极少数复杂场景」的出路。取消它的三个理由：

**① 逃生舱会使引擎行为退化，且退化是静默的。** JSON Schema 无法表达 `restartRequired` / `immutable` / `sensitive` / `from`，因此走该路径的 Pack，其参数变更会被引擎当作「仅更新配置文件、不动进程」处理。用户看不出区别，直到某次改了需要重启的参数而服务没重启。**一个会让核心机制悄悄失效的选项，比没有这个选项更糟。**

**② 两条路径意味着两套校验实现、两套 UI 渲染、两套测试。** 而第二条路径的预期使用率极低——Helm 的 `values.schema.json` 是现成的对照：它存在多年，绝大多数 Chart 从不使用。为一条几乎无人走的路付双倍维护成本不划算。

**③ 增补类型是更好的应对方式。** 若真出现 12 种类型无法表达的场景，那说明**类型集本身需要演进**，而这是向后兼容的、对所有 Pack 都有益的改进。留逃生舱反而会掩盖这个信号——个别 Pack 绕过去了，类型集的缺陷就永远不会被暴露和修复。

## 后果

### 收益

- 运维语义可表达且驱动引擎行为
- Pack 作者编写负担轻（`{ type: port, default: 5432 }` 一行）
- 运维语义类型让 UI 能渲染合适控件、引擎能做单位换算
- `from` 支持拓扑推导参数，用户无需手填

### 代价

- **需自建校验器与 UI 表单生成器**，无法复用 JSON Schema 生态的现成组件
- **表达力有上限且无逃生舱**：嵌套对象、条件依赖（「若 A=x 则 B 必填」）、复杂联合类型难以表达。当前判断这些在组件配置场景中罕见；真出现时必须**增补类型**，而不能绕过。这是有意承担的约束——它保证了运维语义字段在所有 Pack 上一致有效
- **新增类型需要版本协商**：旧版 mechlet 无法解析使用新类型的 Pack。通过 `requires.mecharion` 声明引擎版本下限处理

## 参考

- Cloudera Manager 参数描述符（type / unit / restart / sensitive / config group）
- Apache Ambari config schema `restart_required`
- Kubernetes CRD OpenAPI v3 + `x-kubernetes-*` 扩展（反面证据）
- Helm `values.schema.json` 的实际采用率
