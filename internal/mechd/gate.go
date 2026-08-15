package mechd

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mecharion/mecharion/internal/store"
)

// 健康门禁的两个时间常数（22-multi-node §2.5）。
const (
	// StableFor 是「这批得稳住多久才算过」。
	//
	// **它不能省。** 一个刚起来还没崩的进程，在起来后的那一瞬间收敛与健康
	// 都成立——没有稳定窗口，Rollout 会用一台正在崩溃循环里的机器去批准
	// 下一批，故障因此被逐批放大。
	StableFor = 30 * time.Second

	// BatchTimeout 是一批放行之后多久还没过门禁就判失败。
	//
	// 从**批次放行**起算，不是从 Rollout 开始起算：一次 4 批的升级合法地
	// 要花 4×（物化 + 稳定窗口），拿一个全局超时去卡它必然误判。
	BatchTimeout = 10 * time.Minute
)

// 这两个都**不是旋钮**，与 M6 的 RolloutTimeout 同一条理由：它们是判据，
// 不是配置。慢启动的组件靠 Pack 的 `health.startupGrace` 表达——那是节点侧
// 的事，节点在 grace 期内不报 unhealthy，因此 StableFor 不需要跟着变。
// 加一个 stableFor 旋钮等于把同一件事表达两遍，而两处迟早会打架。

// gateVerdict 是一批的门禁结论。
type gateVerdict struct {
	// Passed 为 true 表示这批过了，可以放行下一批。
	Passed bool
	// Since 是本轮算出来的窗口起点；零值表示窗口该被清掉。
	Since time.Time
	// Baseline 是窗口起点上各实例的重启次数。
	Baseline map[int64]int
	// Blockers 说明是**哪几台**卡在**哪一条**上。
	//
	// 「10 分钟内未收敛」说不出该去看哪台机器——这是超时报错与一句废话
	// 之间的全部区别。
	Blockers []string
}

// checkGate 判一批过没过（22-multi-node §2.5）。
//
// 四条全部成立才算过：
//
//	① 收敛：上报的 digest == 期望 digest
//	② 健康：调和结果不是 failed，且健康探针没有说 unhealthy
//	③ 没崩：窗口期间工作负载的重启次数没有增加
//	④ 稳定：以上状态持续了 StableFor
//
// 第 ③ 条是对崩溃循环最直接的判据：④ 是个有限窗口，一个每 40 秒崩一次的
// 进程能从 30 秒窗口里干干净净地溜过去，而重启计数涨了就是崩过，与观察
// 时机无关。
func checkGate(
	b store.RolloutBatch, byInstance map[int64]instanceGateState, now time.Time,
) gateVerdict {
	v := gateVerdict{Baseline: map[int64]int{}}

	for _, t := range b.Targets {
		st, ok := byInstance[t.InstanceID]
		if !ok {
			v.Blockers = append(v.Blockers, t.Node+": has not reported in yet")
			continue
		}
		if !st.Converged {
			v.Blockers = append(v.Blockers, t.Node+": "+st.Why)
			continue
		}
		// 收敛了，但窗口期间崩过——**这一条比稳定窗口更早发现问题**
		if base, seen := b.RestartBaseline[t.InstanceID]; seen && st.Restarts > base {
			v.Blockers = append(v.Blockers, fmt.Sprintf(
				"%s: restarted %d times within the stability window", t.Node, st.Restarts-base))
			continue
		}
		v.Baseline[t.InstanceID] = st.Restarts
	}
	sort.Strings(v.Blockers)

	if len(v.Blockers) > 0 {
		// 有人掉出判据 → **窗口清零重来**。
		//
		// 这就是门禁挡住崩溃循环的全部机制：机器每崩一次就把窗口清掉，
		// 于是永远攒不满 StableFor，最后撞上 BatchTimeout 被指名。
		return gateVerdict{Blockers: v.Blockers}
	}

	if b.HealthySince.IsZero() {
		// 窗口刚开始。这一轮不算过——**哪怕 StableFor 是 0**：
		// 「第一次看到就通过」正是这道门禁要防的东西。
		v.Since = now
		return v
	}
	v.Since = b.HealthySince
	if now.Sub(b.HealthySince) >= StableFor {
		v.Passed = true
	} else {
		v.Blockers = append(v.Blockers, fmt.Sprintf(
			"stability window needs %s more", (StableFor-now.Sub(b.HealthySince)).Round(time.Second)))
	}
	// **基线不在这里维护。**
	//
	// 曾经这里有一句「沿用 b.RestartBaseline」，怕的是每轮刷新把刚发生的
	// 重启吸收进基线。那句话是多余的，而且方向还错了：
	//
	//   多余  走到这里说明本轮没有实例的重启数超过基线，两者本就相等；
	//         而 saveGate 只在窗口起点变化时才落盘，窗口期间根本不写。
	//   错了  真正会不相等的只有一种情况——`systemctl reset-failed` 把
	//         计数清了。那时守着一个偏高的旧基线，反而会让之后真实的
	//         崩溃（1 < 7）被掩盖掉。
	//
	// 崩溃不会被「吸收」：它会让上面那个 Blockers 分支清掉窗口，
	// 下一轮以当前计数重新开窗——每崩一次就赔掉一整个 StableFor。
	return v
}

// instanceGateState 是门禁要看的那几个字段。
type instanceGateState struct {
	Converged bool
	Restarts  int
	// Why 说明它为什么不算收敛，用于超时时指名。
	Why string
}

// gateStateOf 把一份实例视图压成门禁要看的东西。
//
// **自己判，不搭 iv.Converged 的便车。** 那个字段服务的是
// `component status`，它的定义会为了显示需要而调整（第 7 步就调过一次）；
// 门禁跟着它走的话，下一次调整会**悄悄改掉滚动升级的判据**。
//
// 顺序即优先级：先说最要紧的那一条。一台既没上报又不健康的机器，
// 运维要先知道它没上报。
func gateStateOf(iv InstanceView) instanceGateState {
	st := instanceGateState{Restarts: iv.Restarts}
	switch {
	case iv.Got == "":
		st.Why = "has not reported in yet"
	case iv.PendingVersion != "":
		st.Why = "not its turn yet (queued)"
	case iv.Want == "" || iv.Got != iv.Want:
		st.Why = "has not switched to the new version yet (digest mismatch)"
	case iv.Health == "unhealthy":
		st.Why = "health check failed"
	case iv.Result == "failed":
		st.Why = "reconciliation failed"
	default:
		st.Converged = true
	}
	return st
}

// timedOut 报告一批是不是已经超时。
//
// 没记住放行时刻的批次**不判超时**：那说明放行时写坏了，判它超时等于把
// 一个记账问题变成一次生产事故。它会一直等到有人来看，而 `rollout status`
// 里那一批的「第 N/M」会停着不动——那是看得见的。
func timedOut(b store.RolloutBatch, now time.Time) bool {
	if b.ReleasedAt.IsZero() {
		return false
	}
	return now.Sub(b.ReleasedAt) > BatchTimeout
}

// blockerText 把卡住的原因拼成一句话。
func blockerText(b store.RolloutBatch, blockers []string) string {
	if len(blockers) == 0 {
		return fmt.Sprintf("batch %d (%s) timed out", b.Seq, batchNodes(b))
	}
	return fmt.Sprintf("batch %d timed out: %s", b.Seq, strings.Join(blockers, "; "))
}
