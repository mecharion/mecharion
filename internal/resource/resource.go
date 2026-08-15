// Package resource 实现资源引擎：把一条已渲染的期望状态变成机器上的实际状态。
//
// 设计见 docs/design/11-resource-engine.md 与 ADR-0027。边界刻意很窄：
//
//   - 引擎**不管** generation、不管 Runtime、不管 notify 的执行时机
//     ——那些都是调和器的事
//   - 每种类型只负责「让这一条资源变成期望的样子」
//   - Apply **必须幂等**：调和每 60 秒跑一次，引擎不保证只在有差异时调用
package resource

import (
	"context"
	"fmt"

	"github.com/mecharion/mecharion/internal/faults"
)

// Resource 是一条已渲染的期望状态 ＋ 它的行为。
//
// 实例本身承载期望值——因此各方法都不需要再接收 desired 参数。
type Resource interface {
	// ID 是资源在一次调和中的稳定身份，用于状态追踪、notify 去重，
	// 以及「已不再声明但仍存在」的比对。
	ID() string

	// Type 返回资源类型名，如 "template"。
	Type() string

	// Read 探测当前实际状态。
	//
	// 返回 error 表示「本环境不适用」等应当中止调和的情况；
	// 「本该能读但这次读不到」应返回 Observed{State: StateUnknown}。
	Read(ctx context.Context) (Observed, error)

	// Diff 比较期望与实际，返回字段级差异。纯描述性，不参与 Apply。
	//
	// 约定：Diff 总是在同一次调和的 Read 之后调用。部分类型（如 file）
	// 会在 Read 时缓存期望侧的摘要以供此处比对。
	Diff(observed Observed) []Change

	// Apply 让资源变成期望的样子。**必须幂等**。
	Apply(ctx context.Context) error

	// Remove 在 component remove 时被调用。可以是 no-op。
	Remove(ctx context.Context) error
}

// ── 观测状态 ────────────────────────────────────────────────────────────

// State 是资源的三态观测结果。
type State int

const (
	// StateAbsent 表示资源不存在：文件缺失、用户没建、unit 未安装。
	StateAbsent State = iota
	// StatePresent 表示存在，且字段已读出。
	StatePresent
	// StateUnknown 表示无法确定：NFS 挂死、systemd 未运行、getent 超时。
	//
	// 引擎**跳过**这类资源，在 status 中单独归类，不计为「漂移」。
	// 「本环境不适用」不属于 Unknown，那是 error——没有这条判据，
	// Unknown 会变成所有疑难情况的垃圾桶，从而失去信息量。
	StateUnknown
)

func (s State) String() string {
	switch s {
	case StateAbsent:
		return "absent"
	case StatePresent:
		return "present"
	case StateUnknown:
		return "unknown"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// Observed 是一次探测的结果。
type Observed struct {
	State State
	// Fields 是 State==StatePresent 时读到的实际字段值。
	Fields map[string]any
	// Reason 说明 State==StateUnknown 时为什么读不到。
	Reason string
}

// Field 取出一个字段的字符串值；不存在时返回空串。
func (o Observed) Field(name string) string {
	v, ok := o.Fields[name]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// present 构造一个 StatePresent 观测。
func present(fields map[string]any) Observed {
	return Observed{State: StatePresent, Fields: fields}
}

// unknown 构造一个 StateUnknown 观测。
func unknown(format string, a ...any) Observed {
	return Observed{State: StateUnknown, Reason: fmt.Sprintf(format, a...)}
}

// ── 差异 ────────────────────────────────────────────────────────────────

// ChangeKind 决定 CLI / Web UI 的呈现方式，不影响语义。
type ChangeKind int

const (
	// KindScalar 是短值：mode、uid、port。呈现为 `mode: 0644 → 0640`。
	KindScalar ChangeKind = iota
	// KindText 是长文本：文件内容、unit 内容。呈现为 unified diff。
	KindText
	// KindList 是列表：groups、addresses。逐项增删标记。
	KindList
)

func (k ChangeKind) String() string {
	switch k {
	case KindScalar:
		return "scalar"
	case KindText:
		return "text"
	case KindList:
		return "list"
	default:
		return fmt.Sprintf("ChangeKind(%d)", int(k))
	}
}

// Change 是一处字段级差异。
//
// **行级 diff 的计算与呈现在 CLI 层**，资源引擎不碰 diff 算法。
type Change struct {
	Field string
	Want  string
	// Got 在 Observed.State==StateAbsent 时为空。
	Got  string
	Kind ChangeKind
}

func (c Change) String() string {
	if c.Got == "" {
		return fmt.Sprintf("%s: (无) → %s", c.Field, c.Want)
	}
	return fmt.Sprintf("%s: %s → %s", c.Field, c.Got, c.Want)
}

// diffBuilder 收集差异。空值语义统一：want 为空串表示「Pack 未声明该字段」，
// 未声明的字段不参与比对——否则每个没写 owner 的资源都会报一次漂移。
type diffBuilder struct {
	changes []Change
}

func (d *diffBuilder) add(field, want, got string, kind ChangeKind) {
	if want == "" || want == got {
		return
	}
	d.changes = append(d.changes, Change{Field: field, Want: want, Got: got, Kind: kind})
}

func (d *diffBuilder) scalar(field, want, got string) { d.add(field, want, got, KindScalar) }
func (d *diffBuilder) text(field, want, got string)   { d.add(field, want, got, KindText) }

// list 比较两个列表，顺序无关。
func (d *diffBuilder) list(field string, want, got []string) {
	if len(want) == 0 {
		return
	}
	set := make(map[string]bool, len(got))
	for _, g := range got {
		set[g] = true
	}
	same := len(want) == len(got)
	if same {
		for _, w := range want {
			if !set[w] {
				same = false
				break
			}
		}
	}
	if !same {
		d.changes = append(d.changes, Change{
			Field: field, Want: joinList(want), Got: joinList(got), Kind: KindList,
		})
	}
}

// absent 记录「整个资源都不存在」。
func (d *diffBuilder) absent() {
	d.changes = append(d.changes, Change{
		Field: "exists", Want: "yes", Got: "no", Kind: KindScalar,
	})
}

func joinList(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}

// ── 错误分类 ────────────────────────────────────────────────────────────

// 分类本身定义在 internal/faults —— 它是资源引擎与 Runtime 共用的概念，
// Rollout 要据此决定「重试本批」还是「直接暂停」。这里保留同名入口，
// 因为本包内部用得非常密集。
type (
	// ErrorClass 是失败的可重试性。
	ErrorClass = faults.Class
	// Error 是带分类的错误。
	Error = faults.Error
)

const (
	// ErrTransient 可重试：超时、临时 IO 错误、载荷还没下载完。
	ErrTransient = faults.Transient
	// ErrPermanent 不可重试：权限不足、路径非法、参数写错。
	ErrPermanent = faults.Permanent
)

// Transient 标记一个可重试的错误。
func Transient(op string, err error) error { return faults.Wrap(faults.Transient, op, err) }

// Permanent 标记一个不可重试的错误。
func Permanent(op string, err error) error { return faults.Wrap(faults.Permanent, op, err) }

// Permanentf 是 Permanent 的格式化版本。
func Permanentf(op, format string, a ...any) error { return faults.Permanentf(op, format, a...) }

// ClassOf 返回错误的分类；未分类的按 ErrPermanent 处理。
func ClassOf(err error) ErrorClass { return faults.ClassOf(err) }

// IsTransient 报告错误是否可重试。
func IsTransient(err error) bool { return faults.IsTransient(err) }

// ── 公共基座 ────────────────────────────────────────────────────────────

// base 提供 ID 与 Type，供各类型嵌入。
type base struct {
	id  string
	typ string
}

func (b base) ID() string   { return b.id }
func (b base) Type() string { return b.typ }

// DeriveID 是 id 的生成约定：`<type>:<关键参数>`，如
// `template:/etc/mecharion/apps/pg-main/postgresql.conf`。
//
// mechd 在下发前填好 id，引擎侧不做派生；此函数存在是为了让两侧
// 共用同一套约定（状态文件里的 appliedResources 依赖它稳定）。
func DeriveID(typ, key string) string { return typ + ":" + key }
