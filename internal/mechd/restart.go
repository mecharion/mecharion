package mechd

import (
	"context"
	"fmt"
	"time"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/protocol"
)

// Tasker 由能发 ad-hoc 命令的传输层实现（ADR-0038）。
//
// 与 Notifier / Presence 同一个模式：服务层不认识 gRPC，只要一个
// 「把这批命令发出去并把结果拿回来」的能力。
type Tasker interface {
	RunTasks(ctx context.Context, reqs []protocol.TaskRequest,
		timeout time.Duration) []protocol.TaskOutcome
}

// RestartRequest 是一次 `component restart`。
type RestartRequest struct {
	Site      string
	Component string
	// Role / Node 缩小范围；都为空时是整个组件。
	Role string
	Node string
	// Timeout 覆盖等结果的上限。
	Timeout time.Duration
	Actor   string
}

// RestartResult 是一次重启的逐节点结果。
type RestartResult struct {
	Component string           `json:"component"`
	Instances []RestartOutcome `json:"instances"`
}

// RestartOutcome 是一个实例的重启结果。
type RestartOutcome struct {
	Role string `json:"role"`
	Node string `json:"node"`
	// State 取 ok | failed | unreachable | timeout。
	//
	// **unreachable 与 failed 必须分开**：前者是「那台机器没连着，命令
	// 根本没发出去」，后者是「发出去了但没成功」。运维的下一步完全不同
	// ——一个去查网络，一个去看日志。
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
	Millis  int64  `json:"millis,omitempty"`
}

// Failed 报告这次重启有没有任何一个实例没成功。
func (r RestartResult) Failed() bool {
	for _, o := range r.Instances {
		if o.State != "ok" {
			return true
		}
	}
	return false
}

// Restart 重启一个组件的实例（06-state-and-drift）。
//
// **它不改期望状态**：期望状态没有变，变的只是「现在把它踢一下」。因此
// 这里不碰 run_state、不碰 generation、不触发 Rollout。
//
// 命令走独立的 Tasks 流（ADR-0038），因此有三件事是**状态式下发给不了**
// 的：逐节点的结果、明确的超时、以及「这台不可达、未执行」这句确定的话。
func (s *Service) Restart(ctx context.Context, req RestartRequest) (*RestartResult, error) {
	if s.Tasks == nil {
		return nil, faults.Permanentf("", "this mechd has no command channel connected, restart is unavailable")
	}
	site, err := s.resolveSite(ctx, req.Site)
	if err != nil {
		return nil, err
	}
	comp, err := s.componentForWrite(ctx, site.ID, req.Component)
	if err != nil {
		return nil, err
	}

	insts, byID, err := s.existingInstances(ctx, comp.ID)
	if err != nil {
		return nil, err
	}

	var reqs []protocol.TaskRequest
	type target struct{ role, node string }
	var targets []target
	for _, ri := range insts {
		node := byID[ri.NodeID].Name
		if req.Role != "" && ri.Role != req.Role {
			continue
		}
		if req.Node != "" && node != req.Node {
			continue
		}
		targets = append(targets, target{role: ri.Role, node: node})
		reqs = append(reqs, protocol.TaskRequest{
			Node: node, Kind: protocol.TaskRestart,
			Component: comp.Name, Role: ri.Role,
		})
	}
	if len(reqs) == 0 {
		// **0 个匹配要报错而不是静默成功**：一句「已重启」而实际什么都
		// 没重启，会让人以为问题已经处理过了。
		return nil, faults.Permanentf("", "no matching instances (component=%s role=%q node=%q)",
			req.Component, req.Role, req.Node)
	}

	outs := s.Tasks.RunTasks(ctx, reqs, req.Timeout)

	// **按下标对齐**：同一台机器上可以有同一个组件的两个角色，按节点名
	// 对齐会让后一条覆盖前一条。
	res := &RestartResult{Component: comp.Name}
	for i, t := range targets {
		var o protocol.TaskOutcome
		if i < len(outs) {
			o = outs[i]
		}
		res.Instances = append(res.Instances, RestartOutcome{
			Role: t.role, Node: t.node,
			State: outcomeState(o), Message: o.Message,
			Millis: o.Duration.Milliseconds(),
		})
	}

	s.audit(ctx, req.Actor, "restart", comp.Name, nil,
		fmt.Sprintf("%d instance(s)", len(reqs)))
	return res, nil
}

func outcomeState(o protocol.TaskOutcome) string {
	switch {
	case o.Unreachable:
		return "unreachable"
	case o.TimedOut:
		return "timeout"
	case o.OK:
		return "ok"
	default:
		return "failed"
	}
}
