package mechd

import (
	"fmt"
	"sort"

	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/store"
)

// 分批的默认旋钮（22-multi-node §2.4）。
const (
	// DefaultMaxUnavailable 是同时能动几个实例。
	DefaultMaxUnavailable = 1
	// DefaultCanary 是首批大小。
	//
	// **默认 1 是刻意的**：第一批只动一台，它失败时受影响的面最小，
	// 而绝大多数「新版本根本起不来」的问题在第一台上就暴露了。
	DefaultCanary = 1
)

// batchInput 是分批要用到的一切。
type batchInput struct {
	// Roles 是角色名 → 该角色的实例（未排序）。
	Roles map[string][]store.BatchTarget
	// Order 是角色的执行顺序，按 requires 拓扑序。
	Order []string
	// Quorum 标记哪些角色声明了仲裁。
	Quorum map[string]bool
	// Cordoned 是被暂停调和的节点名。
	Cordoned map[string]bool

	MaxUnavailable int
	Canary         int
}

// planBatches 把一次变更切成阶段与批次。
//
// 两层结构（22-multi-node §2.4）：
//
//	阶段  按角色的 requires 拓扑序，一个角色一个阶段——被依赖者先升
//	批次  阶段内按 maxUnavailable 切；首批大小是 canary
//
// **批内顺序按 ordinal 升序**：稳定顺序让「上次停在第几台」这个问题有
// 答案，而随机顺序会让同一次故障每次复现在不同机器上。
//
// 被 cordon 的节点**不进任何批次**：那是人明确说过「别动这台」
// （§2.7）。它们单独返回，供 status 显式列出——不列的话，
// 「为什么这台还是旧版」会变成一次排查。
func planBatches(in batchInput) ([]store.RolloutBatch, []store.BatchTarget, error) {
	maxUnavail := in.MaxUnavailable
	if maxUnavail <= 0 {
		maxUnavail = DefaultMaxUnavailable
	}
	canary := in.Canary
	if canary < 0 {
		canary = DefaultCanary
	}

	var batches []store.RolloutBatch
	var skipped []store.BatchTarget
	seq := 0

	for stage, role := range in.Order {
		all := append([]store.BatchTarget(nil), in.Roles[role]...)
		// 稳定顺序：ordinal 升序，同 ordinal 再按节点名
		sort.Slice(all, func(i, j int) bool {
			if all[i].Ordinal != all[j].Ordinal {
				return all[i].Ordinal < all[j].Ordinal
			}
			return all[i].Node < all[j].Node
		})

		var targets []store.BatchTarget
		for _, t := range all {
			if in.Cordoned[t.Node] {
				skipped = append(skipped, t)
				continue
			}
			targets = append(targets, t)
		}
		if len(targets) == 0 {
			continue
		}

		// **quorum 角色强制 maxUnavailable ≤ (N-1)/2**（14-placement §5）。
		//
		// 仲裁语义只有 Pack 作者知道——用户设 maxUnavailable 时无从判断
		// 某个组件能同时下线几个。没有这条，一次「并发度 2」的滚动重启
		// 就能让 3 节点的 ZK 失去多数派。
		//
		// 按**参与分批的实例数**算，不是按声明的总数：被 cordon 的那几台
		// 本来就不会动，把它们计进去只会让并发度虚高。
		limit := maxUnavail
		if in.Quorum[role] {
			q := (len(targets) - 1) / 2
			if q < 1 {
				q = 1
			}
			if limit > q {
				limit = q
			}
		}

		for i := 0; i < len(targets); {
			size := limit
			// 首批用 canary（canary=0 表示关掉）
			if i == 0 && canary > 0 && canary < size {
				size = canary
			}
			if size <= 0 {
				return nil, nil, fmt.Errorf("batch size computed as %d -- this is a bug", size)
			}
			end := i + size
			if end > len(targets) {
				end = len(targets)
			}
			seq++
			batches = append(batches, store.RolloutBatch{
				Seq: seq, Stage: stage + 1, Role: role,
				Targets: targets[i:end], State: store.BatchPending,
			})
			i = end
		}
	}
	return batches, skipped, nil
}

// roleOrder 返回角色的执行顺序：按 requires 拓扑序，被依赖者在前。
//
// 与解析管线用的是**同一个顺序**（15-render-pipeline §5）：启动顺序、
// 停止顺序（反向）、滚动升级顺序本来就是同一件事，分成两套迟早会
// 出现「装的时候 A 先，升级的时候 B 先」这类没人能解释的差异。
// **用 RolesForProfile 而不是 Pack.Roles**：形态在放置阶段就被解析掉了
// （ADR-0022），一个在本形态下 enabled=false 的角色不该出现在批次里。
func roleOrder(roles []pack.EffectiveRole) []string {
	byName := map[string]pack.EffectiveRole{}
	names := make([]string, 0, len(roles))
	for _, r := range roles {
		if !r.Enabled {
			continue
		}
		byName[r.Name] = r
		names = append(names, r.Name)
	}
	sort.Strings(names) // 稳定的起点，让同层角色的顺序可复现

	visited := map[string]int{} // 0 未访问 1 访问中 2 已完成
	var out []string
	var visit func(string)
	visit = func(n string) {
		if visited[n] != 0 {
			// 环由 lint 拦（spec §7），这里不再报错——多节点升级
			// 不该因为一个本该在打包时就被拒的 Pack 而崩掉
			return
		}
		visited[n] = 1
		r := byName[n]
		deps := append([]string(nil), r.Requires...)
		sort.Strings(deps)
		for _, d := range deps {
			if _, ok := byName[d]; ok {
				visit(d)
			}
		}
		visited[n] = 2
		out = append(out, n)
	}
	for _, n := range names {
		visit(n)
	}
	return out
}
