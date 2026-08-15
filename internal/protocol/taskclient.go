package protocol

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/mecharion/mecharion/internal/protocol/agentpb"
)

// TaskCommand 是节点收到的一条 ad-hoc 命令。
type TaskCommand struct {
	ID        string
	Kind      string
	Component string
	Role      string
	// Deadline 是这条命令的最晚完成时刻；零值表示没给。
	Deadline time.Time
}

// Expired 报告这条命令是不是已经过期了。
//
// **节点侧也要判**：一条已经超时的命令，中心早就把它报成失败并返回给
// 用户了。此时再去动机器，是一次没人预期的重启——而那正是运维最怕的
// 那种「我明明没让它重启」。
func (c TaskCommand) Expired(now time.Time) bool {
	return !c.Deadline.IsZero() && now.After(c.Deadline)
}

// TaskResult 是一条命令的执行结果。
type TaskResult struct {
	OK       bool
	Message  string
	Duration time.Duration
}

// TaskHandler 执行一条 ad-hoc 命令。
type TaskHandler interface {
	RunTask(ctx context.Context, cmd TaskCommand) TaskResult
}

// RunTasks 挂住命令流，收到什么执行什么。
//
// 与 Run（期望状态那条）**分开跑**：命令流断了不影响下发，反之亦然。
// 这与「不用双向流」是同一条理由，只是这次用在了两条流之间。
func (c *Client) RunTasks(ctx context.Context, h TaskHandler) error {
	backoff := BackoffMin
	for {
		if err := c.tasksOnce(ctx, h); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// **断了就重连，不喊错。** 命令流空闲时断开是常态（网络抖动、
			// mechd 重启），而它断开期间**不会有命令被积压**——中心那边
			// 会把那些节点判成不可达并如实告诉用户。
			c.log.Debug("task stream disconnected, reconnecting later", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(jitter(backoff)):
		}
		backoff = min(backoff*2, BackoffMax)
	}
}

func (c *Client) tasksOnce(ctx context.Context, h TaskHandler) error {
	if c.Session() == "" {
		return errors.New("还没握手")
	}
	stream, err := c.agent.Tasks(ctx, &agentpb.TasksRequest{
		NodeName: c.opts.Node, Session: c.Session(),
	})
	if err != nil {
		return err
	}
	c.log.Debug("task channel connected")

	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		cmd := TaskCommand{
			ID: msg.GetId(), Kind: msg.GetKind(),
			Component: msg.GetComponent(), Role: msg.GetRole(),
		}
		if d := msg.GetDeadline(); d != "" {
			if t, perr := time.Parse(time.RFC3339, d); perr == nil {
				cmd.Deadline = t
			}
		}
		// **每条命令各跑各的**：一条慢命令不该挡住后面的。
		go c.runOneTask(ctx, h, cmd)
	}
}

func (c *Client) runOneTask(ctx context.Context, h TaskHandler, cmd TaskCommand) {
	if cmd.Expired(time.Now()) {
		// 中心已经放弃等它了。**不执行**——那会是一次没人预期的动作。
		c.log.Warn("received an expired command, not executing it",
			"task", cmd.ID, "kind", cmd.Kind, "deadline", cmd.Deadline)
		c.reportTask(ctx, cmd.ID, TaskResult{Message: "命令已过期，未执行"})
		return
	}

	runCtx := ctx
	if !cmd.Deadline.IsZero() {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithDeadline(ctx, cmd.Deadline)
		defer cancel()
	}
	started := time.Now()
	res := h.RunTask(runCtx, cmd)
	if res.Duration == 0 {
		res.Duration = time.Since(started)
	}
	c.reportTask(ctx, cmd.ID, res)
}

func (c *Client) reportTask(ctx context.Context, id string, res TaskResult) {
	_, err := c.agent.ReportTaskResult(ctx, &agentpb.TaskResultRequest{
		NodeName: c.opts.Node, Session: c.Session(), TaskId: id,
		Ok: res.OK, Message: res.Message,
		DurationMs: res.Duration.Milliseconds(),
	})
	if err != nil {
		// 回不去就算了：中心会超时并如实报告。**不重试**——重试一条
		// 结果没有意义，中心那边早就把答案给用户了。
		c.log.Warn("failed to report task result", "task", id, "err", err)
	}
}
