# ADR-0036: Web UI 用 Vue + Vite，产物由 `go generate` 构建而不进仓库

- **状态**：已接受
- **日期**：2026-08-07
- **相关**：[ADR-0007](0007-params-custom-subset.md)、[ADR-0015](0015-offline-first-hermetic.md)、[ADR-0026](0026-standalone-runs-mechd.md)

## 背景

M8 要做 Web UI，核心目标是**由 params schema 自动生成配置表单**——这是当初
选自定义 params 类型子集而不是 JSON Schema 的第二个理由（[ADR-0007](0007-params-custom-subset.md)）。

两个约束先摆着：

1. UI 产物要 `go:embed` 进 mechd（[00-overview](../design/00-overview.md)），
   因为单机形态下 mechd 就是那台机器上唯一的东西
2. **部署时零外部依赖**（[ADR-0015](0015-offline-first-hermetic.md)）——
   不允许 CDN、不允许运行时拉包

约束 2 只管**部署时**。构建时要不要允许依赖 npm，是这个 ADR 要定的。

## 候选与调研

### 框架

| 项目 | 做法 | 观察 |
|---|---|---|
| **Cockpit**（Red Hat） | 原生 JS + PatternFly 起家 | 场景最像（Web 管 Linux 机器）；后期仍引入了构建链 |
| **Syncthing** | 单页 + 老 Angular，产物随二进制发布 | 「零构建依赖」曾是卖点，后来框架老化成了负债 |
| **Prometheus** | 原生 JS → React（npm） | 迁移后贡献门槛明显上升 |
| **Grafana** | React + 自建构建链 | 前端体量超过后端 |
| **Traefik** / **Portainer** | Vue / Angular + npm，产物 embed | 同上 |
| **Gitea** | 服务端模板为主，交互处增量加 JS | 贡献门槛低；复杂交互写起来笨 |

共同点：**所有引入 npm 的项目，前端最终都长成了另一个项目。**
分歧点是「这个界面有多少客户端状态」。

表单生成这一块是**数据驱动的动态渲染**：12 种参数类型 × 分组 × 折叠 ×
`from` 只读 × `immutable` 禁用 × `restartRequired` 提示 × ConfigGroup 层级。
原生 JS 写它会很快变成手搓的 DOM 拼接，而这恰恰是 M8 的核心价值所在。

### 产物进不进仓库

| 做法 | 谁在用 | 代价 |
|---|---|---|
| **产物提交进仓库** | 本项目的 `sqlcgen` / `agentpb` | 拿到源码就能 `go build`；但 `dist/` 让 diff 变脏、merge 冲突变多 |
| **`go generate` 现场构建，产物不入库** | 多数带前端的 Go 项目 | 仓库干净；但**没有 node 就构建不出带 UI 的二进制** |

## 决策

**Vue 3 + Element Plus + UnoCSS，Vite 构建；产物由 `go generate` 生成，
不提交进仓库。**

### 组件库与样式

- **Element Plus**：现成的表单控件、表格、抽屉、消息提示。表单生成要的
  控件它全都有，自己写一套没有意义
- **UnoCSS**（Tailwind 语法）：布局与间距用原子类，几乎不写 CSS 文件

### 目录与构建

```
webui/                      ← Vue 源码（npm 项目）
  package.json  src/  vite.config.ts
internal/webui/
  embed.go                  ← //go:embed all:dist
  gen.go                    ← //go:generate（跑 npm build 并拷进 dist/）
  dist/
    .gitkeep                ← **仓库里只有这一个文件**
```

- `.gitignore` 忽略 `internal/webui/dist/*`，只留 `.gitkeep`
- `//go:embed all:dist`——`all:` 前缀让 embed 收下点号开头的文件，
  因此**空仓库也编译得过**
- 开发期 `npm run dev`，Vite 开发服务器反代到 mechd，不必每次全量构建

### 没构建时要**吵**，不能静默

一个没跑过 `go generate` 的 mechd，UI 目录里只有 `.gitkeep`。此时
**不能返回 404**——那看起来像路由坏了。它必须给出一页明确的说明：

> Web UI 尚未构建。在有 Node 的机器上执行 `make webui` 后重新构建 mechd。

## 后果

**得到的：**

- 表单生成写起来是声明式的，而它是 M8 的主要价值
- Element Plus 省掉大量控件代码，UnoCSS 省掉大量 CSS
- 仓库里没有 `dist/`：diff 干净，不会有生成物的 merge 冲突
- 开发期热更新，不必等全量构建

**代价（照实写）：**

- **「拿到源码就能 `go build` 出完整产物」这条性质到此为止。** 没有 node 的
  机器构建出的 mechd **没有 UI**——CLI 全部功能仍然可用，但浏览器打开只有
  那页说明。这与 `sqlcgen`/`agentpb` 的既有纪律**不一致**，是一次刻意的例外：
  前端产物的体量与变更频率跟那两者不是一个量级，入库的长期噪声更贵
- **发布必须经过带 node 的环境**（CI 或开发机）。发布流程因此多一个依赖，
  且「构建可复现」要靠 `package-lock.json` 锁死
- **mechd 会变大。** Element Plus 会让产物明显变大，而它装在每一台机器上。
  对策是按需引入、embed 前预压缩、`Content-Encoding: gzip` 直出——**把该做的
  都做到位，但不设体积硬指标**：为了凑一个数字去砍功能，换来的是更差的
  界面，而这个项目的界面本来就是为「不写 YAML 的人」做的
- **非前端贡献者进不去 `webui/src/`**。所有引入 npm 的项目的共同结局
- **框架会老化。** Syncthing 的 Angular 是前车之鉴。五年后这套前端多半要
  重写一次

**重新评估的条件：**

- 若「没有 node 就没有 UI」在实际协作中造成了反复的困扰（比如贡献者拿到的
  二进制总是没界面），改回产物入库，接受 diff 噪声
- 若 mechd 体积在边缘设备上成为**真实**问题（而不是理论问题），
  考虑把 UI 拆成独立可选二进制——但那会破坏「单机装完就有界面」
