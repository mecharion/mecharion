package spec

import "github.com/mecharion/mecharion/internal/faults"

// 漂移策略的取值（spec §14.1）。
const (
	// DriftReconcile 自动改回期望状态。**最严**。
	DriftReconcile = "reconcile"
	// DriftReport 发现漂移则上报并告警，不修改。默认。
	DriftReport = "report"
	// DriftIgnore 不比对。**最松**。
	DriftIgnore = "ignore"
)

// driftLooseness 给三种策略排一个「有多松」的序。
//
// 严 → 松：reconcile > report > ignore。
var driftLooseness = map[string]int{
	DriftReconcile: 0,
	DriftReport:    1,
	DriftIgnore:    2,
}

// ValidDriftPolicy 报告取值是否合法。空串表示「未声明」。
func ValidDriftPolicy(p string) bool {
	if p == "" {
		return true
	}
	_, ok := driftLooseness[p]
	return ok
}

// EffectiveDriftPolicy 把 Pack 声明与站点覆盖合成最终策略。
//
// **取更松的那个**（06-state-and-drift §4.2）。这条方向不是对称的：
//
//	Pack: reconcile + 覆盖: report  → report   放松，生效
//	Pack: ignore    + 覆盖: report  → ignore   已经更松，覆盖不起作用
//
// 「更松的赢」而不是「覆盖直接赢」，是因为一个 Component 级的单值要同时
// 作用于几十条资源：Pack 作者把某个文件标成 `ignore`（应用自己会改写它），
// 一个 `report` 覆盖不该把它拽回来报警——那不是运维想表达的意思，
// 而是单值粒度的副作用。
func EffectiveDriftPolicy(declared, override string) string {
	if declared == "" {
		declared = DriftReport // 规范的默认
	}
	if override == "" {
		return declared
	}
	if driftLooseness[override] > driftLooseness[declared] {
		return override
	}
	return declared
}

// CheckDriftOverride 校验一个站点侧覆盖是否可接受。
//
// **只能放松不能收紧。** `reconcile` 是最严的一档，作为覆盖它要么无意义
// （目标已经是 reconcile），要么就是在收紧——因此直接拒绝，而不是悄悄
// 当成空操作。
//
// 拒绝的理由不是洁癖：`driftPolicy` 写在 Pack 里，等于**Pack 作者决定了
// 运维现场的临时修改能不能活下来**，这个权责关系本来就是反的。允许站点
// 反向收紧，等于让一个不在现场的人否决现场的判断——而 `reconcile` 最坏的
// 后果是「运维只是想试个参数，服务却被改回去并重启了」。
func CheckDriftOverride(p string) error {
	switch p {
	case "", DriftReport, DriftIgnore:
		return nil
	case DriftReconcile:
		return faults.Permanentf("",
			"an override can only relax, never tighten; %s is the strictest level\n"+
				"  a Pack author marking something report usually means that file is meant to be "+
				"editable, and a site policy has no reason to be stricter than that (06-state-and-drift §4.2)\n"+
				"  available overrides: report (report only) | ignore (don't compare) | empty (clear the override)",
			DriftReconcile)
	default:
		return faults.Permanentf("", "invalid driftPolicy %q, available: %s | %s | empty (clear the override)",
			p, DriftReport, DriftIgnore)
	}
}
