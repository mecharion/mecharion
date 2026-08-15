package pack

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"text/template/parse"
)

// PartialPrefix 标记模板片段：以此开头的文件不会被单独渲染（规范 §9.1）。
const PartialPrefix = "_"

// FuncNames 是模板中可用的受限函数集（规范 §9.1）。
// 刻意**不提供** env / exec / 文件读取 / 网络访问——任何可绕过 hermetic
// 约束的函数都不给。
var FuncNames = []string{
	"default", "quote", "squote", "upper", "lower", "trim", "replace", "join", "split",
	"indent", "nindent", "b64enc", "b64dec", "toYaml", "toJson",
	"add", "sub", "mul", "div", "min", "max",
	// text/template 内建
	"and", "or", "not", "len", "index", "slice", "printf", "print", "println",
	"eq", "ne", "lt", "le", "gt", "ge", "call", "html", "js", "urlquery",
}

// stubFuncs 构造一组仅用于**解析**的函数占位，使模板可被 Parse。
func stubFuncs() template.FuncMap {
	fm := template.FuncMap{}
	for _, n := range FuncNames {
		fm[n] = func(_ ...any) any { return nil }
	}
	return fm
}

// TemplateSet 是 templates/ 下全部文件解析出的模板集合。
// pack.yaml 中的字段表达式与之共享同一个 set（规范 §9.1）。
type TemplateSet struct {
	tmpl *template.Template
	// Files 是 templates/ 下的全部文件名（相对该目录）。
	Files []string
	// Defined 是被 {{ define }} 定义的模板名集合。
	Defined map[string]bool
}

// ParseTemplates 解析 Pack 的 templates/ 目录。
// 目录不存在时返回一个空集合而非错误。
func ParseTemplates(p *Pack) (*TemplateSet, error) {
	files, err := p.ListDir(DirTemplates)
	if err != nil {
		return nil, err
	}
	sort.Strings(files)

	ts := &TemplateSet{Files: files, Defined: map[string]bool{}}
	root := template.New("__pack__").Funcs(stubFuncs())

	for _, rel := range files {
		abs := filepath.Join(p.Dir, DirTemplates, filepath.FromSlash(rel))
		b, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("reading template %s: %w", rel, err)
		}
		if _, err := root.New(rel).Parse(string(b)); err != nil {
			return nil, fmt.Errorf("parsing template %s failed: %w", rel, cleanTmplErr(err))
		}
	}
	for _, t := range root.Templates() {
		if t.Tree != nil {
			ts.Defined[t.Name()] = true
		}
	}
	ts.tmpl = root
	return ts, nil
}

// ParseExpr 把 pack.yaml 中的一个字段表达式并入模板集合并解析。
// name 仅用于错误定位。
func (ts *TemplateSet) ParseExpr(name, expr string) error {
	if ts.tmpl == nil {
		ts.tmpl = template.New("__pack__").Funcs(stubFuncs())
	}
	if _, err := ts.tmpl.New(name).Parse(expr); err != nil {
		return cleanTmplErr(err)
	}
	return nil
}

func cleanTmplErr(err error) error {
	// text/template 的错误前缀含内部模板名，去掉噪音
	s := err.Error()
	if i := strings.Index(s, ": "); i > 0 && strings.HasPrefix(s, "template: ") {
		s = s[i+2:]
	}
	return fmt.Errorf("%s", s)
}

// ── 引用收集 ────────────────────────────────────────────────────────────

// Guard 描述一段模板内容在哪些形态下会生效。
//
// 这是让 R21 可用的关键：`{{ if eq .Profile "ha" }}` 里的
// `.Params.nameservice_id` 只需在 ha 形态下存在，不该在 standalone 下报错。
type Guard struct {
	// Only 非空时，表示仅在这些形态下生效。
	Only map[string]bool
	// Not 中的形态被排除。
	Not map[string]bool
}

// Admits 报告该守卫是否允许某个形态。
func (g Guard) Admits(profile string) bool {
	if g.Not[profile] {
		return false
	}
	if len(g.Only) > 0 {
		return g.Only[profile]
	}
	return true
}

// And 把两个守卫合取。
func (g Guard) And(o Guard) Guard {
	out := Guard{Only: map[string]bool{}, Not: map[string]bool{}}
	switch {
	case len(g.Only) == 0:
		mergeSet(out.Only, o.Only)
	case len(o.Only) == 0:
		mergeSet(out.Only, g.Only)
	default:
		for k := range g.Only {
			if o.Only[k] {
				out.Only[k] = true
			}
		}
	}
	mergeSet(out.Not, g.Not)
	mergeSet(out.Not, o.Not)
	return out
}

// invert 返回守卫的否定，用于 else 分支。
func (g Guard) invert() Guard {
	return Guard{Only: g.Not, Not: g.Only}
}

// ParamRef 是一次带守卫的参数引用。
type ParamRef struct {
	Name  string
	Guard Guard
}

// Refs 是从模板 AST 中静态收集到的引用。
//
// 采用 AST 静态分析而非「用假值渲染一遍」：前者能精确定位到具体字段名，
// 给出的错误远好于 missingkey=error 的运行时报错，且不需要构造完整上下文；
// 更重要的是它能识别 profile 守卫，从而让逐形态校验不产生误报。
type Refs struct {
	Params    []ParamRef      // .Params.X（带守卫）
	Paths     map[string]bool // .Paths.X
	Facts     map[string]bool // .Node.Facts.X
	Requires  map[string]bool // .Requires.X
	Roles     map[string]bool // .Topology.Role "x"
	Profiles  map[string]bool // 与 .Profile 比较的字面量
	Templates map[string]bool // {{ template "x" }}

	// UsesProfile 报告是否引用过 .Profile。
	UsesProfile bool
}

func newRefs() *Refs {
	return &Refs{
		Paths: map[string]bool{},
		Facts: map[string]bool{}, Requires: map[string]bool{},
		Roles: map[string]bool{}, Profiles: map[string]bool{},
		Templates: map[string]bool{},
	}
}

func (r *Refs) merge(o *Refs) {
	r.Params = append(r.Params, o.Params...)
	mergeSet(r.Paths, o.Paths)
	mergeSet(r.Facts, o.Facts)
	mergeSet(r.Requires, o.Requires)
	mergeSet(r.Roles, o.Roles)
	mergeSet(r.Profiles, o.Profiles)
	mergeSet(r.Templates, o.Templates)
	r.UsesProfile = r.UsesProfile || o.UsesProfile
}

func mergeSet(dst, src map[string]bool) {
	for k := range src {
		dst[k] = true
	}
}

// CollectRefs 静态收集某个模板中的全部引用。
func (ts *TemplateSet) CollectRefs(name string) *Refs {
	refs := newRefs()
	if ts.tmpl == nil {
		return refs
	}
	t := ts.tmpl.Lookup(name)
	if t == nil || t.Tree == nil || t.Tree.Root == nil {
		return refs
	}
	walkNode(t.Tree.Root, refs, Guard{})
	return refs
}

// ProfileGuardOf 从一个条件表达式中解析出它允许的形态集合。
// 用于资源的 `when: '{{ eq .Profile "zookeeper" }}'`。
func ProfileGuardOf(expr string) Guard {
	g := Guard{}
	if expr == "" || !strings.Contains(expr, "{{") {
		return g
	}
	t, err := template.New("__guard__").Funcs(stubFuncs()).Parse(expr)
	if err != nil || t.Tree == nil || t.Tree.Root == nil {
		return g
	}
	for _, n := range t.Tree.Root.Nodes {
		act, ok := n.(*parse.ActionNode)
		if !ok {
			continue
		}
		if pg, ok := profileGuardFromPipe(act.Pipe); ok {
			g = g.And(pg)
		}
	}
	return g
}

// CollectRefsDeep 收集某个模板的引用，**并递归跟进它 include 的片段**。
//
// CollectRefs 只看单个模板自身的 AST：`{{ template "_common" . }}` 只会
// 被记为一条模板引用，片段内部引用了什么它并不知道。对 R21 那种「逐形态
// 校验参数可见性」够用（片段随主模板一并遍历），但对**安全类规则不够**——
// 一个把口令藏在片段里的模板会整个逃掉检查。
func (ts *TemplateSet) CollectRefsDeep(name string) *Refs {
	out := newRefs()
	seen := map[string]bool{}

	var walk func(string)
	walk = func(n string) {
		if seen[n] {
			return // 片段互相 include 时不至于打转
		}
		seen[n] = true
		refs := ts.CollectRefs(n)
		out.merge(refs)
		for _, sub := range sortedKeys(refs.Templates) {
			walk(sub)
		}
	}
	walk(name)
	return out
}

// CollectAllRefs 收集模板集合中全部模板的引用（含 pack.yaml 字段表达式）。
func (ts *TemplateSet) CollectAllRefs() *Refs {
	all := newRefs()
	if ts.tmpl == nil {
		return all
	}
	for _, t := range ts.tmpl.Templates() {
		if t.Tree == nil {
			continue
		}
		all.merge(ts.CollectRefs(t.Name()))
	}
	return all
}

func walkNode(n parse.Node, refs *Refs, g Guard) {
	if n == nil {
		return
	}
	switch x := n.(type) {
	case *parse.ListNode:
		if x == nil {
			return
		}
		for _, c := range x.Nodes {
			walkNode(c, refs, g)
		}
	case *parse.ActionNode:
		walkPipe(x.Pipe, refs, g)
	case *parse.IfNode:
		walkPipe(x.Pipe, refs, g)
		// `{{ if eq .Profile "ha" }}` 让 then 分支只在 ha 下生效，
		// else 分支则排除 ha
		if pg, ok := profileGuardFromPipe(x.Pipe); ok {
			walkNode(x.List, refs, g.And(pg))
			walkNode(x.ElseList, refs, g.And(pg.invert()))
			return
		}
		walkNode(x.List, refs, g)
		walkNode(x.ElseList, refs, g)
	case *parse.RangeNode:
		walkPipe(x.Pipe, refs, g)
		walkNode(x.List, refs, g)
		walkNode(x.ElseList, refs, g)
	case *parse.WithNode:
		walkPipe(x.Pipe, refs, g)
		walkNode(x.List, refs, g)
		walkNode(x.ElseList, refs, g)
	case *parse.TemplateNode:
		refs.Templates[x.Name] = true
		walkPipe(x.Pipe, refs, g)
	}
}

// profileGuardFromPipe 识别 `eq .Profile "X"` / `ne .Profile "X"`，
// 返回它所限定的形态集合。
func profileGuardFromPipe(p *parse.PipeNode) (Guard, bool) {
	if p == nil || len(p.Cmds) != 1 {
		return Guard{}, false
	}
	cmd := p.Cmds[0]
	if len(cmd.Args) < 3 {
		return Guard{}, false
	}
	id, ok := cmd.Args[0].(*parse.IdentifierNode)
	if !ok || (id.Ident != "eq" && id.Ident != "ne") {
		return Guard{}, false
	}
	if !pipeMentionsProfile(cmd) {
		return Guard{}, false
	}
	names := map[string]bool{}
	for _, a := range cmd.Args[1:] {
		if s, ok := a.(*parse.StringNode); ok {
			names[s.Text] = true
		}
	}
	if len(names) == 0 {
		return Guard{}, false
	}
	if id.Ident == "eq" {
		return Guard{Only: names}, true
	}
	return Guard{Not: names}, true
}

func walkPipe(p *parse.PipeNode, refs *Refs, g Guard) {
	if p == nil {
		return
	}
	for _, cmd := range p.Cmds {
		var lastFieldChain []string
		for i, arg := range cmd.Args {
			switch a := arg.(type) {
			case *parse.FieldNode:
				recordFieldChain(a.Ident, refs, g)
				lastFieldChain = a.Ident
			case *parse.ChainNode:
				if f, ok := a.Node.(*parse.FieldNode); ok {
					recordFieldChain(append(append([]string{}, f.Ident...), a.Field...), refs, g)
				}
			case *parse.PipeNode:
				walkPipe(a, refs, g)
			case *parse.StringNode:
				// `.Topology.Role "datanode"` —— 上一个字段链是 Topology.Role 时，
				// 本字符串即角色名
				if isTopologyRole(lastFieldChain) {
					refs.Roles[a.Text] = true
				}
				// `eq .Profile "ha"` —— 记录被比较的 profile 名
				if i > 0 {
					if id, ok := cmd.Args[0].(*parse.IdentifierNode); ok &&
						(id.Ident == "eq" || id.Ident == "ne") && pipeMentionsProfile(cmd) {
						refs.Profiles[a.Text] = true
					}
				}
			}
		}
	}
}

func isTopologyRole(chain []string) bool {
	return len(chain) >= 2 &&
		strings.EqualFold(chain[len(chain)-2], "Topology") &&
		strings.EqualFold(chain[len(chain)-1], "Role")
}

func pipeMentionsProfile(cmd *parse.CommandNode) bool {
	for _, a := range cmd.Args {
		if f, ok := a.(*parse.FieldNode); ok {
			if len(f.Ident) == 1 && strings.EqualFold(f.Ident[0], "Profile") {
				return true
			}
		}
	}
	return false
}

func recordFieldChain(chain []string, refs *Refs, g Guard) {
	if len(chain) == 0 {
		return
	}
	switch {
	case strings.EqualFold(chain[0], "Params") && len(chain) >= 2:
		refs.Params = append(refs.Params, ParamRef{Name: chain[1], Guard: g})
	case strings.EqualFold(chain[0], "Paths") && len(chain) >= 2:
		refs.Paths[chain[1]] = true
	case strings.EqualFold(chain[0], "Requires") && len(chain) >= 2:
		refs.Requires[chain[1]] = true
	case strings.EqualFold(chain[0], "Profile"):
		refs.UsesProfile = true
	case strings.EqualFold(chain[0], "Node") && len(chain) >= 3 &&
		strings.EqualFold(chain[1], "Facts"):
		refs.Facts[chain[2]] = true
	}
}

// ── from / defaultFrom 表达式 ───────────────────────────────────────────

// exprRefRe 匹配 from 表达式中的引用，如
// `topology.role('primary').nodes[0].address` 与 `requires.zookeeper.exports.client`。
var (
	topoRoleRe = mustRe(`topology\.role\(\s*['"]([^'"]+)['"]\s*\)`)
	// 大小写不敏感：字段表达式有两种写法，`from: "requires.zk.exports.client"`
	// 与 `from: "{{ .Requires.zk.Exports.client }}"`。只认前者会让 R38 / R40
	// 对后者完全失明——而那正是带凭据的引用最常用的写法。
	reqRefRe  = mustRe(`(?i)requires\.([A-Za-z0-9_-]+)\.`)
	factRefRe = mustRe(`facts\.([A-Za-z0-9_]+)`)
)

// CollectExprRefs 从 from / defaultFrom 表达式中收集引用。
// 表达式既可能是模板语法（`{{ … }}`），也可能是点式路径语法。
func CollectExprRefs(expr string) *Refs {
	refs := newRefs()
	for _, m := range topoRoleRe.FindAllStringSubmatch(expr, -1) {
		refs.Roles[m[1]] = true
	}
	for _, m := range reqRefRe.FindAllStringSubmatch(expr, -1) {
		refs.Requires[m[1]] = true
	}
	for _, m := range factRefRe.FindAllStringSubmatch(expr, -1) {
		refs.Facts[m[1]] = true
	}
	return refs
}

// ExprMentionsFacts 报告表达式是否引用了节点事实。
// 用于规则 41：facts 不得出现在 from 中。
func ExprMentionsFacts(expr string) bool {
	return factRefRe.MatchString(expr) ||
		strings.Contains(expr, ".Node.Facts") ||
		strings.Contains(expr, ".node.facts")
}

// ExprReferencesPaths 报告表达式是否访问了 `.Paths`。
// 用于规则 40：scope: site 的依赖不得被引用 .Paths。
func ExprReferencesPaths(expr, dep string) bool {
	return strings.Contains(strings.ToLower(expr),
		strings.ToLower("requires."+dep+".paths"))
}

// reqParamsRe 匹配对依赖参数的访问，两种写法都认：
//
//	requires.pg.params.admin_password        （from 的裸点号写法）
//	{{ .Requires.pg.Params.admin_password }} （模板写法）
//
// 不复用 reqRefRe 是因为它区分大小写、只认裸写法；而这条规则**必须
// 两种写法都堵住**——漏掉一种等于没有这条规则。
var reqParamsRe = mustRe(`(?i)requires\.([A-Za-z0-9_-]+)\.params\b`)

// paramRefRe 匹配对参数的访问，裸写法与模板写法都认。
//
// 第一组捕获可选的 `requires.<dep>.` 前缀：`.Params.x` 与
// `.Requires.pg.Params.x` 只差这一段，而前者的前置字符恰好也是点号，
// 靠「前面不是点」区分不开。把前缀显式捕获出来再跳过才是准确的。
var paramRefRe = mustRe(`(?i)(requires\.[A-Za-z0-9_-]+\.)?params\.([A-Za-z0-9_]+)`)

// ExprReferencedParams 返回表达式中引用的**本 Pack** 参数名。
//
// 依赖的参数（`requires.pg.params.x`）不计入——那种写法由规则 49 拦截。
func ExprReferencedParams(expr string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range paramRefRe.FindAllStringSubmatch(expr, -1) {
		if m[1] != "" {
			continue // 读的是依赖的参数，不是自己的
		}
		if !seen[m[2]] {
			seen[m[2]] = true
			out = append(out, m[2])
		}
	}
	sort.Strings(out)
	return out
}

// reqExportRe 匹配对依赖导出名的引用，两种写法都认：
//
//	requires.zookeeper.exports.client
//	{{ .Requires.zookeeper.Exports.client.host }}
//
// 第三段之后可能还有字段名（fields 形态），只取导出名本身。
var reqExportRe = mustRe(`(?i)requires\.([A-Za-z0-9_-]+)\.exports\.([A-Za-z0-9_]+)`)

// DepExportRef 是一次对依赖导出名的引用。
type DepExportRef struct {
	Dep    string
	Export string
}

// ExprReferencedExports 返回表达式中引用到的依赖导出名。
//
// 用于规则 43。**需要跨 Pack 索引才能核对**——这也是它此前一直没实现的原因
// （lint 一次只看得见一个 Pack）。
func ExprReferencedExports(expr string) []DepExportRef {
	seen := map[DepExportRef]bool{}
	var out []DepExportRef
	for _, m := range reqExportRe.FindAllStringSubmatch(expr, -1) {
		r := DepExportRef{Dep: m[1], Export: m[2]}
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dep != out[j].Dep {
			return out[i].Dep < out[j].Dep
		}
		return out[i].Export < out[j].Export
	})
	return out
}

// ExprReferencedDepParams 返回表达式中被读取了参数的依赖名。
//
// 用于规则 49。`.Requires.<pack>.Params` **不存在**，理由有两层：
// 提供方一改参数名所有消费方同时失效；而且提供方的超级用户口令
// 本就不该给消费方——要被消费的凭据应当走 exports 明确导出（规范 §5.4）。
func ExprReferencedDepParams(expr string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range reqParamsRe.FindAllStringSubmatch(expr, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

func mustRe(s string) *regexp.Regexp { return regexp.MustCompile(s) }

// currentPathRe 匹配对 `.Paths.Current` 的访问，裸写法与模板写法都认。
//
// 不用 strings.Contains：`.PathsCurrentFoo` 这种名字会被误伤，而漏报比
// 误报更该防——这条规则拦的是一个几乎查不出来的现场。
var currentPathRe = mustRe(`(?i)\bpaths\.current\b`)

// ExprReferencesCurrentPath 报告表达式是否引用了 `{{ .Paths.Current }}`。
//
// 用于规则 52。`current` 是一条软链，而 **Docker 在创建容器时就把 bind
// mount 的路径解析掉**——之后 generation 切换、软链改指向，容器里看到的
// 仍然是旧的那份，而 `ls -l` 看软链一切正常。
//
// 它在 systemd 的 `exec` 里是**正确**写法：那是进程启动时才解析的路径。
// 因此这条规则只能按 runtime 分别判断，不能一刀切。
func ExprReferencesCurrentPath(expr string) bool {
	return currentPathRe.MatchString(expr)
}
