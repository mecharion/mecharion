package protocol

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mecharion/mecharion/internal/protocol/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ── ad-hoc 命令（ADR-0038）──────────────────────────────────────────────
//
// 这条通道与期望状态那条**彻底分开**。分界是「丢一次会怎样」：
//
//	期望状态  丢了无所谓，下一轮重新确认；断连三天回来照做仍然正确
//	命令      丢了就是没执行，必须告诉人；断连三天回来**不该补做**
//
// 因此这里没有队列、没有重试、没有落库：**离线就是离线，如实报告**。
// 一个「等它上线再执行」的队列会让人以为命令一定会生效，而那是期望状态
// 的语义——真需要那种语义的东西，本来就该做成期望状态。

// Task 的类型。**显式枚举，加一种要过一次评审**（ADR-0038）：
// 这是一条真正的命令通道，「顺手加个 task 类型」比往 Assignment 里加
// 字段容易得多。
//
// 放在 protocol 而不是 agent：**两端都要用同一份**，而让控制面去 import
// 节点侧的包是反的——那条依赖迟早会带来一个真正的环。
const (
	// TaskRestart 重启一个角色实例的工作负载。
	TaskRestart = "restart"
)

// DefaultTaskTimeout 是一条命令等结果的默认上限。
//
// 30 秒：一次 systemd 重启加健康检查通常在几秒内，而敲命令的人正在终端
// 前等着。等太久与「挂住了」在体感上没有区别。
const DefaultTaskTimeout = 30 * time.Second

// TaskRequest 是要发给一个节点的一条命令。
type TaskRequest struct {
	Node      string
	Kind      string
	Component string
	Role      string
}

// TaskOutcome 是一条命令在一个节点上的结果。
type TaskOutcome struct {
	Node string
	// OK 为 true 表示节点报告执行成功。
	OK bool
	// Unreachable 表示那台机器**当时没连着**，命令根本没有发出去。
	//
	// 与「发出去了但失败了」分开是这条通道存在的理由之一：运维要知道的
	// 是「这台没执行」还是「这台执行了但出错」——两者的下一步完全不同。
	Unreachable bool
	// TimedOut 表示发出去了但没在期限内回话。
	TimedOut bool
	Message  string
	Duration time.Duration
}

// taskWaiter 是一条已发出、正在等结果的命令。
type taskWaiter struct {
	done chan TaskOutcome
	node string
}

// taskHub 管理正在等结果的命令。
type taskHub struct {
	mu      sync.Mutex
	seq     uint64
	waiting map[string]*taskWaiter // task id → 等待者
	// streams 是每个节点当前挂着的命令流。
	streams map[string]chan *agentpb.Task
}

func newTaskHub() *taskHub {
	return &taskHub{
		waiting: map[string]*taskWaiter{},
		streams: map[string]chan *agentpb.Task{},
	}
}

// Tasks 实现命令流：agent 拨出来挂住，mechd 按需推。
func (s *Server) Tasks(req *agentpb.TasksRequest, stream agentpb.Agent_TasksServer) error {
	node, err := s.nodeOf(stream.Context(), req.GetNodeName())
	if err != nil {
		return err
	}
	sess := s.session(node)
	if sess == nil || (req.GetSession() != "" && sess.id != req.GetSession()) {
		return status.Error(codes.FailedPrecondition, "会话不存在或已失效，请重新 Register")
	}

	ch := make(chan *agentpb.Task, 8)
	s.tasks.mu.Lock()
	// 一个节点只保留最新的那条流：重连之后旧的那条已经废了。
	s.tasks.streams[node] = ch
	s.tasks.mu.Unlock()

	defer func() {
		s.tasks.mu.Lock()
		if s.tasks.streams[node] == ch {
			delete(s.tasks.streams, node)
		}
		s.tasks.mu.Unlock()
	}()

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.draining:
			// mechd 要关了：主动收摊。对节点来说与一次网络抖动无异，
			// 它会重连——而**没有命令会因此被补做**，那正是我们要的。
			return status.Error(codes.Unavailable, "mechd 正在关闭，请重连")
		case t := <-ch:
			if err := stream.Send(t); err != nil {
				return err
			}
		}
	}
}

// ReportTaskResult 收一条命令的执行结果。
func (s *Server) ReportTaskResult(
	ctx context.Context, req *agentpb.TaskResultRequest,
) (*agentpb.TaskResultResponse, error) {
	node, err := s.nodeOf(ctx, req.GetNodeName())
	if err != nil {
		return nil, err
	}

	s.tasks.mu.Lock()
	w := s.tasks.waiting[req.GetTaskId()]
	if w != nil && w.node == node {
		delete(s.tasks.waiting, req.GetTaskId())
	} else {
		w = nil
	}
	s.tasks.mu.Unlock()

	if w == nil {
		// 结果来晚了（中心已经超时并回给用户了），或者不是这个节点的。
		// **不算错误**：节点没做错任何事，而重试只会再来一次。
		s.log.Debug("received a task result that is no longer awaited",
			"node", node, "task", req.GetTaskId())
		return &agentpb.TaskResultResponse{}, nil
	}

	// 非阻塞写：等待方可能已经超时走了
	select {
	case w.done <- TaskOutcome{
		Node: node, OK: req.GetOk(), Message: req.GetMessage(),
		Duration: time.Duration(req.GetDurationMs()) * time.Millisecond,
	}:
	default:
	}
	return &agentpb.TaskResultResponse{}, nil
}

// RunTasks 把一批命令发给各自的节点并等结果。
//
// **离线节点立刻判定，不排队**：mechd 本来就知道谁连着（命令流的注册表
// 就是那个事实），因此「这台不可达、未执行」是一句当场就能说出的确定
// 的话，而不是一个悬着的承诺。
//
// 并发发出、统一等待：一条命令打三台机器时，串行会让总耗时变成三倍，
// 而它们彼此无关。
func (s *Server) RunTasks(
	ctx context.Context, reqs []TaskRequest, timeout time.Duration,
) []TaskOutcome {
	if timeout <= 0 {
		timeout = DefaultTaskTimeout
	}
	deadline := time.Now().Add(timeout)

	// **结果按入参下标对齐，不按节点名。**
	//
	// 同一台机器上可以有同一个组件的两个角色（一台机器既是 primary 又是
	// journalnode），按节点名对齐会让后一条覆盖前一条——而那时输出里会
	// 少一行，且少的那一行看起来只是「没返回」。
	out := make([]TaskOutcome, len(reqs))
	type pending struct {
		idx int
		id  string
		w   *taskWaiter
	}
	var wait []pending

	for i, r := range reqs {
		s.tasks.mu.Lock()
		ch, online := s.tasks.streams[r.Node]
		if !online {
			s.tasks.mu.Unlock()
			out[i] = TaskOutcome{Node: r.Node, Unreachable: true}
			continue
		}
		s.tasks.seq++
		id := fmt.Sprintf("t%d-%d", time.Now().UnixNano(), s.tasks.seq)
		w := &taskWaiter{done: make(chan TaskOutcome, 1), node: r.Node}
		s.tasks.waiting[id] = w
		s.tasks.mu.Unlock()

		t := &agentpb.Task{
			Id: id, Kind: r.Kind, Component: r.Component, Role: r.Role,
			Deadline: deadline.UTC().Format(time.RFC3339),
		}
		select {
		case ch <- t:
			wait = append(wait, pending{idx: i, id: id, w: w})
		default:
			// 那条流的缓冲满了 = 节点在挤压命令，多半已经不健康。
			// **当成不可达**，而不是无限等下去。
			s.forgetTask(id)
			out[i] = TaskOutcome{
				Node: r.Node, Unreachable: true,
				Message: "命令通道拥塞",
			}
		}
	}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	for _, p := range wait {
		node := reqs[p.idx].Node
		select {
		case res := <-p.w.done:
			out[p.idx] = res
		case <-timer.C:
			s.forgetTask(p.id)
			out[p.idx] = TaskOutcome{Node: node, TimedOut: true}
		case <-ctx.Done():
			s.forgetTask(p.id)
			out[p.idx] = TaskOutcome{
				Node: node, TimedOut: true, Message: "请求已取消",
			}
		}
	}
	return out
}

func (s *Server) forgetTask(id string) {
	s.tasks.mu.Lock()
	delete(s.tasks.waiting, id)
	s.tasks.mu.Unlock()
}
