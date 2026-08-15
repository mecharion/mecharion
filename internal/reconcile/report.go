package reconcile

import (
	"fmt"
	"strings"
	"time"

	"github.com/mecharion/mecharion/internal/resource"
	"github.com/mecharion/mecharion/internal/runtime"
)

// Result 是一次调和的总体结论。
type Result string

const (
	// ResultOK 表示实际状态已与期望一致。
	ResultOK Result = "ok"
	// ResultChanged 表示做了改动，且改完之后一致。
	ResultChanged Result = "changed"
	// ResultDrift 表示检出了漂移但按策略未处理（driftPolicy: report）。
	ResultDrift Result = "drift"
	// ResultFailed 表示调和失败。
	ResultFailed Result = "failed"
)

// Report 是一次调和的完整结果，也是上报给 mechd 的内容。
type Report struct {
	Component   string `json:"component"`
	Role        string `json:"role"`
	ConfigGroup string `json:"configGroup,omitempty"`
	Node        string `json:"node,omitempty"`

	Result Result `json:"result"`
	// Error 在 Result==failed 时说明原因。
	Error string `json:"error,omitempty"`

	Digest        string `json:"digest"`
	Generation    int    `json:"generation"`
	GenerationDir string `json:"generationDir,omitempty"`
	// Switched 表示切换了 current 软链。
	Switched bool `json:"switched,omitempty"`
	// Rollback 表示本轮命中了一个已保留的历史 generation。
	Rollback bool `json:"rollback,omitempty"`
	// Blocked 表示这个版本上次失败过，本轮没有尝试。
	Blocked bool `json:"blocked,omitempty"`
	// RolledBackFrom 是被回滚掉的那个 digest。
	//
	// 报告本身描述的是**回滚之后的状态**（跑的是旧版），因此 Digest 是旧的——
	// 那是收敛判据，必须如实。这个字段回答「为什么它不是期望的那个」。
	RolledBackFrom string `json:"rolledBackFrom,omitempty"`
	// Pruned 是被回收的 generation 序号。
	Pruned []string `json:"pruned,omitempty"`

	// Images 是本轮物化装进本地镜像库的镜像引用。
	//
	// **节点本地用，不上报**：中心不需要知道某台机器的镜像库里有什么，
	// 它只是从 Materialize 传到台账的一段路——镜像引用只有物化那一刻
	// 才存在（22-upgrade §2.5 ①）。
	Images []string `json:"-"`

	// Resources 逐条记录每个资源的观测与动作。
	Resources []ResourceReport `json:"resources,omitempty"`
	// Notified 是实际执行的 notify 动作。
	Notified []string `json:"notified,omitempty"`
	// Hooks 逐条记录执行过的 hook。
	Hooks []HookReport `json:"hooks,omitempty"`
	// Absorbed 是被 restart 吸收掉的动作。
	Absorbed []string `json:"absorbed,omitempty"`

	// Removed 在本轮执行了卸载（runState: removed）时非 nil。
	//
	// **它是 mechd 判定「这个实例已经拆干净了」的唯一依据**——Result==changed
	// 只说明本轮做了事，不说明做的是拆。少了它，一个正在被删的组件永远
	// 分不清「还在装着」与「已经卸完」。
	Removed *RemovalReport `json:"removed,omitempty"`

	// WorkloadAction 记录本轮**对工作负载做了什么**。
	//
	// 少了它，一次「服务被人停了、调和把它拉起来」在报告里不留任何痕迹：
	// 没有资源被 Apply、没有切软链，结果是 ok。**一个每分钟崩一次又被
	// 拉起的服务，从中心看完全健康**——那正是这个项目一直在消灭的那种沉默。
	WorkloadAction WorkloadAction `json:"workloadAction,omitempty"`
	// LastWorkloadAction 是**最近一次**纠正动作，含时间。
	//
	// 与 WorkloadAction 的区别是状态与事件之分：上面那个描述本轮，
	// 这个是持续可查的事实。上报走的是这个——周期快照丢不起事件。
	LastWorkloadAction string `json:"lastWorkloadAction,omitempty"`
	LastWorkloadAt     string `json:"lastWorkloadAt,omitempty"`
	// Workload 是 Runtime 的观测结果；无 workload 的角色为 nil。
	Workload *runtime.Status `json:"workload,omitempty"`
	// Health 是健康探针的结论。
	Health *HealthReport `json:"health,omitempty"`

	StartedAt time.Time     `json:"startedAt"`
	Duration  time.Duration `json:"durationMs"`
}

// ResourceAction 是引擎对一个资源做了什么。
type ResourceAction string

const (
	// ActionNone 表示无差异。
	ActionNone ResourceAction = "none"
	// ActionApplied 表示有差异且已收敛。
	ActionApplied ResourceAction = "applied"
	// ActionReported 表示有差异但按 driftPolicy 只上报。
	ActionReported ResourceAction = "reported"
	// ActionSkipped 表示读不到实际状态（Unknown），本轮跳过。
	ActionSkipped ResourceAction = "skipped"
	// ActionIgnored 表示 driftPolicy: ignore，根本没比对。
	ActionIgnored ResourceAction = "ignored"
	// ActionSuppressed 表示检出了漂移，但已被运维显式确认，本轮不告警。
	//
	// 与 ActionIgnored 的区别很重要：ignore 是 Pack 作者说「这文件本来就
	// 会被应用改」，suppressed 是运维说「我知道，是我改的，先别管」——
	// 后者有期限、有理由、进审计。
	ActionSuppressed ResourceAction = "suppressed"
	// ActionFailed 表示 Apply 失败。
	ActionFailed ResourceAction = "failed"
)

// ResourceReport 是单个资源的调和结果。
type ResourceReport struct {
	ID     string         `json:"id"`
	Type   string         `json:"type"`
	Action ResourceAction `json:"action"`
	// State 是观测到的三态。
	State string `json:"state"`
	// Reason 在 State==unknown 时说明为什么读不到。
	Reason string `json:"reason,omitempty"`
	// Changes 是字段级差异，供 CLI 渲染。
	Changes []ChangeReport `json:"changes,omitempty"`
	Error   string         `json:"error,omitempty"`
}

// ChangeReport 是一处字段差异。
type ChangeReport struct {
	Field string `json:"field"`
	Want  string `json:"want"`
	Got   string `json:"got,omitempty"`
	Kind  string `json:"kind"`
}

func changeReports(cs []resource.Change) []ChangeReport {
	if len(cs) == 0 {
		return nil
	}
	out := make([]ChangeReport, len(cs))
	for i, c := range cs {
		out[i] = ChangeReport{
			Field: c.Field, Want: c.Want, Got: c.Got, Kind: c.Kind.String(),
		}
	}
	return out
}

// HealthReport 是健康探针的结论。
type HealthReport struct {
	// Probe 是探针描述，如 "HTTP 探针 http://127.0.0.1:8080/healthz"。
	Probe   string `json:"probe"`
	Healthy bool   `json:"healthy"`
	Error   string `json:"error,omitempty"`
}

// Changed 报告本轮是否实际改动了机器。
func (r *Report) Changed() bool {
	if r.Switched || r.WorkloadAction != "" {
		return true
	}
	for _, rr := range r.Resources {
		if rr.Action == ActionApplied {
			return true
		}
	}
	return len(r.Notified) > 0
}

// Summary 返回一行人类可读的结论。
func (r *Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s/%s %s", r.Component, r.Role, r.Result)
	if r.Generation > 0 {
		fmt.Fprintf(&b, " generation=%04d", r.Generation)
	}
	if r.Rollback {
		b.WriteString(" (rollback)")
	} else if r.Switched {
		b.WriteString(" (switched)")
	}

	applied, reported, skipped := 0, 0, 0
	for _, rr := range r.Resources {
		switch rr.Action {
		case ActionApplied:
			applied++
		case ActionReported:
			reported++
		case ActionSkipped:
			skipped++
		}
	}
	if r.WorkloadAction != "" {
		fmt.Fprintf(&b, " %s", workloadActionText(r.WorkloadAction))
	}
	fmt.Fprintf(&b, " resources %d", len(r.Resources))
	if applied > 0 {
		fmt.Fprintf(&b, ", applied %d", applied)
	}
	if reported > 0 {
		fmt.Fprintf(&b, ", drift %d", reported)
	}
	if skipped > 0 {
		fmt.Fprintf(&b, ", skipped %d", skipped)
	}
	if len(r.Notified) > 0 {
		fmt.Fprintf(&b, ", notify %s", strings.Join(r.Notified, "+"))
	}
	if r.Workload != nil {
		fmt.Fprintf(&b, ", workload %s", r.Workload.State)
	}
	fmt.Fprintf(&b, ", took %s", r.Duration.Round(time.Millisecond))
	return b.String()
}

// RemovalReport 是一次卸载的回执（24-lifecycle §2.3）。
//
// 它同时是两样东西：给 mechd 的「拆完了」信号，与给人的「拆掉了什么、
// 留下了什么」的账。后者不能省——数据目录默认保留，而一次不说明留了
// 什么的卸载，等于在 N 台机器上悄悄堆下 N 堆没人知道来历的目录。
type RemovalReport struct {
	// Runtime / Native 是被卸掉的 unit 名或容器名，供事后核对。
	Runtime string `json:"runtime,omitempty"`
	Native  string `json:"native,omitempty"`

	// RetainedPaths 是**故意留下**的目录，按字典序。它们会被登记为孤儿。
	RetainedPaths []string `json:"retainedPaths,omitempty"`
	// PurgedPaths 是删掉的目录。
	PurgedPaths []string `json:"purgedPaths,omitempty"`
	// PurgedIdentities 是删掉的系统用户与组（--purge-user）。
	PurgedIdentities []string `json:"purgedIdentities,omitempty"`

	// Warnings 是「没做成但不致命」的事，主要是 userdel/groupdel 被拒。
	//
	// **不能只写日志**：那条日志在被卸载的那台机器上，而敲 remove 的人
	// 在另一头。一次「说是 --purge-user 了、用户其实还在」必须回到他面前。
	Warnings []string `json:"warnings,omitempty"`
}

// WorkloadAction 是调和器本轮对工作负载做的事。
type WorkloadAction string

const (
	// WorkloadRestored 表示它本该在跑却没在跑，被拉了起来。
	//
	// 这是**漂移被纠正**，不是例行操作——它值得出现在报告与状态里。
	WorkloadRestored WorkloadAction = "restored"
	// WorkloadStopped 表示期望是 stopped 而它在跑，被停了回去。
	WorkloadStopped WorkloadAction = "stopped"
)

func workloadActionText(a WorkloadAction) string {
	switch a {
	case WorkloadRestored:
		return "workload restored"
	case WorkloadStopped:
		return "workload stopped"
	}
	return string(a)
}
