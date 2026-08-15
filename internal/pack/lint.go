package pack

import (
	"fmt"
	"sort"
	"strings"
)

// Severity 是 lint 结果的严重级别。
type Severity string

const (
	// SevError 会让 lint 失败。
	SevError Severity = "error"
	// SevWarn 只提示，不影响退出码。
	SevWarn Severity = "warning"
)

// Finding 是一条 lint 结果。
//
// 字段名一律 lowerCamelCase——`-o json|yaml` 的输出结构对脚本保证向后兼容
// （只增字段，不改语义），见 docs/design/10-cli.md §7。
type Finding struct {
	// Rule 是规则编号，对应规范 §19，如 "R13"。
	Rule     string   `json:"rule"     yaml:"rule"`
	Severity Severity `json:"severity" yaml:"severity"`
	// Path 是出问题的字段路径，如 "roles[1].cardinality"。
	Path string `json:"path"    yaml:"path"`
	// Line 是 pack.yaml 中的行号，0 表示未知。
	Line    int    `json:"line"    yaml:"line"`
	Message string `json:"message" yaml:"message"`
	// Hint 是可行动的修复建议。
	Hint string `json:"hint,omitempty" yaml:"hint,omitempty"`
}

func (f Finding) String() string {
	loc := f.Path
	if f.Line > 0 {
		loc = fmt.Sprintf("%s:%d", f.Path, f.Line)
	}
	s := fmt.Sprintf("%s [%s] %s", f.Severity, f.Rule, f.Message)
	if loc != "" {
		s = fmt.Sprintf("%-40s %s", loc, s)
	}
	if f.Hint != "" {
		s += "\n" + strings.Repeat(" ", 4) + "hint: " + f.Hint
	}
	return s
}

// Result 是一次 lint 的全部结果。
type Result struct {
	Findings []Finding
}

// Errors 返回错误级结果。
func (r *Result) Errors() []Finding { return r.filter(SevError) }

// Warnings 返回警告级结果。
func (r *Result) Warnings() []Finding { return r.filter(SevWarn) }

func (r *Result) filter(s Severity) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Severity == s {
			out = append(out, f)
		}
	}
	return out
}

// OK 报告是否没有错误级结果。
func (r *Result) OK() bool { return len(r.Errors()) == 0 }

// Sort 按行号、规则号排序，让输出稳定。
func (r *Result) Sort() {
	sort.SliceStable(r.Findings, func(i, j int) bool {
		a, b := r.Findings[i], r.Findings[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Rule < b.Rule
	})
}

// ── lint 上下文 ─────────────────────────────────────────────────────────

// Options 控制 lint 的行为。
type Options struct {
	// Hermetic 开启离线约束检查（规范 §17）。
	Hermetic bool
	// Resolver 解析跨 Pack 引用（规则 43）。
	//
	// **为 nil 时相关检查整个跳过，不告警**：lint 一次只看得见一个 Pack，
	// 而依赖方完全可能来自别处、单独发布。对每个带依赖的 Pack 都唠叨一句
	// 「无法核对」，会让 --strict 对正常的单包校验失效。
	//
	// 给了解析器但依赖不在其中，那才值得一句警告。
	Resolver DepResolver
}

// DepResolver 回答「被依赖的 Pack 导出了什么」。
//
// 定义在这里而非直接依赖 internal/packindex，是因为后者要 import 本包
// 来解析 Pack——反过来引用会成环。
type DepResolver interface {
	// Exports 返回满足约束的依赖 Pack 的导出名。
	// ok 为 false 表示本地没有它，**无法核对**。
	Exports(name, constraint string) (exports []string, ok bool)
}

type linter struct {
	p    *Pack
	opts Options
	ts   *TemplateSet
	res  *Result

	roleSet map[string]bool
	depSet  map[string]PackRequire // 依赖名 → 声明（含 profile 追加的）
}

func (l *linter) add(rule string, sev Severity, path string, line int, msg, hint string) {
	l.res.Findings = append(l.res.Findings, Finding{
		Rule: rule, Severity: sev, Path: path, Line: line, Message: msg, Hint: hint,
	})
}

func (l *linter) err(rule, path string, line int, msg, hint string) {
	l.add(rule, SevError, path, line, msg, hint)
}

func (l *linter) warn(rule, path string, line int, msg, hint string) {
	l.add(rule, SevWarn, path, line, msg, hint)
}

// Lint 对 Pack 执行全部校验规则。
func Lint(p *Pack, opts Options) *Result {
	l := &linter{p: p, opts: opts, res: &Result{}, roleSet: map[string]bool{}, depSet: map[string]PackRequire{}}

	for _, n := range p.RoleNames() {
		l.roleSet[n] = true
	}
	collectDeps := func(r *Requires) {
		if r == nil {
			return
		}
		for _, d := range r.Packs {
			l.depSet[d.Name] = d
		}
	}
	collectDeps(p.Requires)
	for _, pr := range p.Profiles {
		collectDeps(pr.Requires)
	}
	for _, role := range p.Roles {
		if role.Workload != nil {
			collectDeps(role.Workload.Requires)
		}
	}

	// 模板必须先解析——后续多条规则依赖它
	ts, err := ParseTemplates(p)
	if err != nil {
		l.err("R15", DirTemplates, 0, err.Error(), "")
		ts = &TemplateSet{Defined: map[string]bool{}}
	}
	l.ts = ts

	l.checkStructure()    // R01–R08
	l.checkRoles()        // R09–R12
	l.checkPlacement()    // R13–R15
	l.checkProfiles()     // R16–R21
	l.checkParams()       // R22–R26
	l.checkPaths()        // R27–R30
	l.checkResources()    // R06b–R06c, R31
	l.checkHooks()        // R32
	l.checkDependencies() // R34–R45
	l.checkTemplates()    // R15, R21
	if opts.Hermetic {
		l.checkHermetic() // R33
	}

	l.res.Sort()
	return l.res
}

// ── 输出辅助 ────────────────────────────────────────────────────────────

func quoteList(ss []string) string {
	if len(ss) == 0 {
		return "(none)"
	}
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strconv_Quote(s)
	}
	return strings.Join(out, ", ")
}

func strconv_Quote(s string) string { return `"` + s + `"` }

// containsString 报告 ss 中是否含有 s。
func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
