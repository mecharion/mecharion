package render

import (
	"fmt"
	"sort"
	"strings"
	"text/template"

	"github.com/mecharion/mecharion/internal/pack"
)

// Ctx 是模板可见的全部变量（spec §9.2）。
//
// 字段全部导出且用 map 承载动态部分——`text/template` 对结构体按字段名
// 反射，对 map 按键查找，两者混用是最贴近 spec 变量表的形状。
type Ctx struct {
	Pack      PackCtx
	Profile   string
	Site      SiteCtx
	Component string
	Role      string

	// Params 是已解析的参数值。逐阶段填充：静态值 → defaultFrom →
	// generate → from，因此同一个 Ctx 在不同阶段看到的内容不同。
	Params map[string]any
	// Paths 中单值路径是 string，多值路径是 []string。
	Paths map[string]any

	Node       NodeCtx
	Requires   map[string]any
	Topology   *TopologyCtx
	Generation GenerationCtx

	// Address 与 Port 只在**求值 exports 时**有值（spec §5.3/§5.4 的
	// `{{ .Address }}` / `{{ .Port }}`）。
	//
	// 放在顶层而不是 .Params 下，是因为 spec 里印的就是这个写法；
	// 而它们在普通模板里为空，正好对应「导出之外没有这个概念」。
	Address string
	Port    string
}

// PackCtx 是 `.Pack`。
type PackCtx struct {
	Name     string
	Version  string
	Revision int
}

// SiteCtx 是 `.Site`。
type SiteCtx struct {
	Name   string
	Kind   string
	Labels map[string]string
}

// NodeCtx 是 `.Node`。
type NodeCtx struct {
	Name    string
	Address string
	Labels  map[string]string
	Roots   map[string]string
	Volumes map[string]any
	// Facts 是节点事实的**放置时快照**，不是实时值。
	// 实时跟随会把「运维觉察不到的事实变动」变成生产环境的配置变更（spec §9.4.1）。
	Facts map[string]any
}

// GenerationCtx 是 `.Generation`。
type GenerationCtx struct {
	// Seq 由 mechlet 本地分配，mechd 无从得知——因此这里恒为 0，
	// 需要它的地方一律用 `{{ .Paths.Generation }}` 占位符。
	Seq      int
	Previous string
}

// TopologyCtx 是 `.Topology`，放置结果的快照。
type TopologyCtx struct {
	// Ordinal 是**当前实例**在同角色中的序号。
	Ordinal int
	Self    *PeerCtx

	roles map[string][]*PeerCtx
}

// Role 实现 `{{ .Topology.Role "datanode" }}`。
//
// 未知角色返回空列表而非报错：`{{ range }}` 空列表是合理的「该角色没部署」，
// 而 missingkey=error 在这里帮不上忙——它管的是 map 索引，不是方法调用。
func (t *TopologyCtx) Role(name string) []*PeerCtx {
	if t == nil {
		return nil
	}
	return t.roles[name]
}

// Roles 返回全部角色名，按字典序。
func (t *TopologyCtx) Roles() []string {
	if t == nil {
		return nil
	}
	out := make([]string, 0, len(t.roles))
	for k := range t.roles {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PeerCtx 是拓扑中的一个对等实例。
type PeerCtx struct {
	Node    string
	Name    string
	Address string
	Ordinal int
	Role    string
	Labels  map[string]string
	// Paths 是该实例**在它自己那台节点上**解析出的路径。
	//
	// 不能用本机的 `.Paths` 去推断对端：节点间挂载点可以不同，
	// MinIO 的端点列表正是靠这个才写得出来（spec §9.3）。
	Paths map[string]any
}

// String 让 `{{ join "," (.Topology.Role "server") }}` 这类写法有意义——
// 对等实例在文本里的自然形态是它的地址。
func (p *PeerCtx) String() string { return p.Address }

// ── 依赖 ────────────────────────────────────────────────────────────────

// DepCtx 是 `.Requires.<name>`（spec §9.5）。
type DepCtx struct {
	Component string
	Version   string
	Scope     string
	// Paths 仅 scope:node 时非空。scope:site 的依赖在别的机器上，
	// 引用它的路径必然是 bug（lint 规则 40）。
	Paths map[string]any
	// Exports 是已求值的导出：导出名 → 字段名 → 值，或导出名 → 字符串。
	Exports map[string]any

	topology *TopologyCtx
}

// Topology 实现 `{{ .Requires.zookeeper.Topology.Role "server" }}`。
func (d *DepCtx) Topology() *TopologyCtx { return d.topology }

// ── 大小写宽容 ──────────────────────────────────────────────────────────

// alias 为 map 补上首字母大写的别名。
//
// spec 的变量表写 `.Paths.Home`，而 pack.yaml 里声明的键是 `home`；
// `.Node.Facts.Memory.Total` 对应的事实字段是 `memory.total`。
// text/template 的 map 索引是精确匹配，不补别名就会让 spec 里印着的写法
// 全部失败。
//
// 只在别名不存在时补，绝不覆盖真实键——否则一个同时含 `rack` 与 `Rack`
// 的 label 集合会静默丢掉一个。
func alias(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m)*2)
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			v = alias(sub)
		}
		out[k] = v
	}
	for k := range m {
		c := capitalize(k)
		if c == k {
			continue
		}
		if _, taken := out[c]; !taken {
			out[c] = out[k]
		}
	}
	return out
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ── 求值 ────────────────────────────────────────────────────────────────

// Engine 持有 Pack 的模板集合与函数表。
type Engine struct {
	root *template.Template
	// files 是 templates/ 下会被单独渲染的文件（不含 `_` 前缀的片段）。
	files []string
}

// NewEngine 解析 Pack 的 templates/ 目录，并接上真实函数实现。
//
// 与 pack.ParseTemplates 的区别只在函数表：那边用占位实现，够解析不够求值。
// 目录结构与 `{{ define }}` 的可见性两边完全一致。
func NewEngine(p *pack.Pack) (*Engine, error) {
	names, err := p.ListDir(pack.DirTemplates)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)

	root := template.New("__pack__").Funcs(FuncMap()).Option("missingkey=error")
	var files []string
	for _, rel := range names {
		body, err := p.ReadFile(pack.DirTemplates, rel)
		if err != nil {
			return nil, fmt.Errorf("reading template %s: %w", rel, err)
		}
		if _, err := root.New(rel).Parse(string(body)); err != nil {
			return nil, fmt.Errorf("parsing template %s failed: %w", rel, err)
		}
		if !strings.HasPrefix(baseName(rel), pack.PartialPrefix) {
			files = append(files, rel)
		}
	}
	return &Engine{root: root, files: files}, nil
}

// Files 返回可单独渲染的模板文件名。
func (e *Engine) Files() []string { return e.files }

// Has 报告模板集合中是否有该名字。
func (e *Engine) Has(name string) bool {
	return e.root.Lookup(name) != nil
}

// Render 渲染一个已在集合中的模板。
func (e *Engine) Render(name string, ctx *Ctx) (string, error) {
	t := e.root.Lookup(name)
	if t == nil {
		return "", fmt.Errorf("template %q does not exist", name)
	}
	var sb strings.Builder
	if err := t.Execute(&sb, ctx); err != nil {
		return "", fmt.Errorf("rendering %s: %w", name, cleanErr(err))
	}
	return sb.String(), nil
}

// Expr 求值一个 pack.yaml 里的字段表达式。
//
// 表达式与 templates/ **共享同一个 set**（spec §9.1），因此可以调用片段：
// `from: '{{ template "minio-endpoints" . }}'`。这让跨节点、跨磁盘的枚举
// 能从 systemd.exec 这类单行字段里抽出来，写进有换行与注释的片段。
func (e *Engine) Expr(where, expr string, ctx *Ctx) (string, error) {
	if !strings.Contains(expr, "{{") {
		return expr, nil // 字面量，省一次解析
	}
	t, err := e.root.Clone()
	if err != nil {
		return "", err
	}
	if _, err := t.New(where).Parse(expr); err != nil {
		return "", fmt.Errorf("%s: expression parse failed: %w", where, cleanErr(err))
	}
	var sb strings.Builder
	if err := t.Lookup(where).Execute(&sb, ctx); err != nil {
		return "", fmt.Errorf("%s: %w", where, cleanErr(err))
	}
	return sb.String(), nil
}

// cleanErr 去掉 text/template 错误里的内部模板名噪音。
func cleanErr(err error) error {
	s := err.Error()
	s = strings.TrimPrefix(s, "template: ")
	if i := strings.Index(s, "executing "); i >= 0 {
		if j := strings.Index(s[i:], ": "); j > 0 {
			s = s[:i] + s[i+j+2:]
		}
	}
	return fmt.Errorf("%s", strings.TrimSpace(s))
}

func baseName(rel string) string {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[i+1:]
	}
	return rel
}
