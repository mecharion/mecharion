# minio — L2+

**这是第二波里最有价值的示例：它一口气打出了三个规范缺口。**

MinIO 的 `server` 参数需要列出集群中**每个节点的每块盘**：

```
minio server http://n1:9000/data1 http://n1:9000/data2 http://n2:9000/data1 …
```

这一条命令行同时压测了拓扑渲染、多盘绑定、以及二者的**组合**——第一波的示例都只单独触碰其中一项。

## 三个缺口（均已修入规范）

### ① 无法放置裸二进制载荷

MinIO 发布的是**单个可执行文件**，不是 tarball。而资源类型里：

- `archive.blob` 要求归档
- `file.source` 只能引用 `files/` 下随 Pack 分发的静态文件，**不能引用 blob**

于是一个单文件组件竟然无处安放。绕法是把二进制打进一个多余的 tar，但那是为了迁就格式而扭曲载荷。

**修正**：`file` 增加 `blob` 字段，与 `content` / `source` 互斥。

```yaml
- file: { blob: main, path: "{{ .Paths.Generation }}/bin/minio", mode: "0755" }
```

配合 blob 的 `mediaType: raw`。lint 规则 6c 禁止对 `raw` 使用 `archive`。

### ② pack.yaml 字段无法调用模板片段

MinIO 的端点表达式是一个双层嵌套循环。把它内联进 `systemd.exec` 这种单行字段里，可读性为零：

```yaml
exec: "…/minio server {{ range .Topology.Role \"server\" }}{{ $n := . }}{{ range .Paths.DataDirs }} http://{{ $n.Address }}…"
```

**修正**：pack.yaml 中的字段表达式与 `templates/` **共享同一个 template set**，解析顺序为「先 `templates/`，后 pack.yaml 字段」。复杂表达式因此可以抽进带换行与注释的片段：

```yaml
params:
  endpoints:
    from: '{{ template "minio-endpoints" . }}'
```

### ③ 拓扑条目不携带已解析的路径

最重要的一个。渲染端点需要**每个对等节点各自的盘路径**，而节点之间的挂载点可以不同——`/data1..12` 与 `/mnt/disk1..4` 在同一集群中共存是常态。因此**不能用本机的 `.Paths.DataDirs` 推断对端**。

**修正**：拓扑中每个 RoleInstance 携带 `.Paths.*`，即该实例在它自己那台机器上解析出的路径。

**这与 `scope: site` 依赖不暴露 `.Paths` 并不矛盾**，边界很清楚：

| | 引用对象 | 是否暴露 `.Paths` |
|---|---|---|
| `.Topology.Role(…)` | **同一 Component** 内的对等实例 | ✅ 作者本就该知道自己组件的布局 |
| `.Requires.<pack>`（`scope: site`） | **别的 Component** 在别的机器上 | ❌ 跨越封装边界 |

## 验证了什么（新增部分）

| 规范条目 | 验证点 |
|---|---|
| [§14.2 `file.blob`](../../../docs/spec/pack-v1.md#142-文件系统) | ✅ 裸二进制落地 |
| [§9.1 共享 template set](../../../docs/spec/pack-v1.md#91-引擎) | ✅ `from: '{{ template "minio-endpoints" . }}'` |
| [§9.3 拓扑携带 `.Paths`](../../../docs/spec/pack-v1.md#93-topology-引用) | ✅ 跨节点、跨磁盘的双层枚举 |
| [§8.6 多盘](../../../docs/spec/pack-v1.md#86-多盘) | ✅ `kind: multi` + ConfigGroup 统一绑定 |
| [§13 profiles](../../../docs/spec/pack-v1.md#13-profiles--部署形态) | ✅ `standalone` / `distributed`，后者 `cardinality: "4-N"` |

## 一个部署时的约束，规范暂时无法表达

MinIO 纠删码要求**每个节点的盘数一致**，且总盘位数 ≥ 4。

- 「总数 ≥ 4」可以用 `cardinality: "4-N"` 近似（4 节点各 1 盘），但 **2 节点各 4 盘也合法**，而这表达不了
- 「每节点盘数一致」完全无法表达——它是对 `paths.dataDirs` 解析结果的跨实例约束

当前处理：由 ConfigGroup 统一绑定盘（同一组内的节点自然一致），并在 `postInstall` hook 里做一次运行时校验。

**是否值得引入「路径级放置约束」？当前判断：不值得。** 它只服务于纠删码这一类场景，而 `placement` 现有的角色级约束已覆盖绝大多数需求。若第三波出现第二个同类需求再议——记在这里以免被遗忘。
