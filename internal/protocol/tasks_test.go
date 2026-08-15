package protocol

import (
	"context"
	"testing"
	"time"

	"github.com/mecharion/mecharion/internal/protocol/agentpb"
)

// 这一组测的是 ad-hoc 通道的**语义**（ADR-0038），不是 gRPC 的接线：
//
//	离线   立刻判定，不排队、不补做
//	超时   有明确上限，不会挂住敲命令的人
//	对齐   同一台机器上的两个角色不会互相覆盖
//
// 它们直接驱动 taskHub，不起真的服务器——那三条与「怎么传输」无关，
// 而起服务器会让这几条最要紧的判据淹在一堆接线里。

func newTaskServer(t *testing.T) *Server {
	t.Helper()
	return &Server{tasks: newTaskHub(), draining: make(chan struct{})}
}

// attach 假装某个节点挂上了命令流，返回它收到的命令。
func attach(s *Server, node string, buf int) chan *agentpb.Task {
	ch := make(chan *agentpb.Task, buf)
	s.tasks.mu.Lock()
	s.tasks.streams[node] = ch
	s.tasks.mu.Unlock()
	return ch
}

// answer 模拟节点回报结果。
func answer(t *testing.T, s *Server, node string, task *agentpb.Task, ok bool, msg string) {
	t.Helper()
	s.tasks.mu.Lock()
	w := s.tasks.waiting[task.GetId()]
	s.tasks.mu.Unlock()
	if w == nil {
		t.Fatalf("没有人在等 %s 的结果", task.GetId())
	}
	select {
	case w.done <- TaskOutcome{Node: node, OK: ok, Message: msg}:
	default:
		t.Fatal("结果写不进去")
	}
}

// TestOfflineNodeIsUnreachableNotQueued 是这条通道存在的理由。
//
// **离线就是离线，如实报告。** 一个「等它上线再执行」的队列会让人以为
// 命令一定会生效，而那是期望状态的语义——真需要那种语义的东西，本来
// 就该做成期望状态（比如 orphans purge 就是）。
func TestOfflineNodeIsUnreachableNotQueued(t *testing.T) {
	s := newTaskServer(t)
	// n1 没挂命令流 = 离线

	start := time.Now()
	out := s.RunTasks(context.Background(),
		[]TaskRequest{{Node: "n1", Kind: TaskRestart}}, 5*time.Second)

	if len(out) != 1 || !out[0].Unreachable {
		t.Fatalf("离线节点应当判为 unreachable，得到 %+v", out)
	}
	// **立刻判定**，不等超时：这是「当场就能说出的确定的话」那半
	if d := time.Since(start); d > time.Second {
		t.Errorf("离线判定应当是立刻的，花了 %v", d)
	}
	// 也不该在库里留下任何等待中的任务
	s.tasks.mu.Lock()
	n := len(s.tasks.waiting)
	s.tasks.mu.Unlock()
	if n != 0 {
		t.Errorf("离线节点不该留下等待中的任务，还剩 %d 条", n)
	}
}

// TestUnreachableIsNotFailed 守两种失败的区分。
//
// 「那台机器没连着、命令根本没发出去」与「发出去了但没成功」——
// 运维的下一步完全不同，一个去查网络，一个去看日志。
func TestUnreachableIsNotFailed(t *testing.T) {
	s := newTaskServer(t)
	ch := attach(s, "n1", 4)

	done := make(chan []TaskOutcome, 1)
	go func() {
		done <- s.RunTasks(context.Background(), []TaskRequest{
			{Node: "n1", Kind: TaskRestart},
			{Node: "n2", Kind: TaskRestart}, // 没挂流
		}, 5*time.Second)
	}()

	task := <-ch
	answer(t, s, "n1", task, false, "启动失败")

	out := <-done
	if len(out) != 2 {
		t.Fatalf("两条请求应当有两条结果，得到 %+v", out)
	}
	if out[0].Unreachable || out[0].OK {
		t.Errorf("n1 应当是「发出去了但失败」，得到 %+v", out[0])
	}
	if !out[1].Unreachable {
		t.Errorf("n2 应当是 unreachable，得到 %+v", out[1])
	}
}

// TestResultsAlignByIndexNotByNode 守一个会让输出少一行的错误。
//
// 同一台机器上可以有同一个组件的两个角色（既是 primary 又是
// journalnode）。按节点名对齐会让后一条覆盖前一条，而少掉的那一行
// 看起来只是「没返回」。
func TestResultsAlignByIndexNotByNode(t *testing.T) {
	s := newTaskServer(t)
	ch := attach(s, "n1", 4)

	done := make(chan []TaskOutcome, 1)
	go func() {
		done <- s.RunTasks(context.Background(), []TaskRequest{
			{Node: "n1", Kind: TaskRestart, Role: "primary"},
			{Node: "n1", Kind: TaskRestart, Role: "journalnode"},
		}, 5*time.Second)
	}()

	t1, t2 := <-ch, <-ch
	if t1.GetRole() == t2.GetRole() {
		t.Fatalf("两条命令的角色应当不同: %s / %s", t1.GetRole(), t2.GetRole())
	}
	answer(t, s, "n1", t1, true, "")
	answer(t, s, "n1", t2, false, "起不来")

	out := <-done
	if len(out) != 2 {
		t.Fatalf("同一台机器上的两条命令要有两条结果，得到 %+v", out)
	}
	if !out[0].OK || out[1].OK {
		t.Errorf("结果对错位了: %+v", out)
	}
}

// TestTimeoutDoesNotHangTheCaller：敲命令的人正在终端前等着。
func TestTimeoutDoesNotHangTheCaller(t *testing.T) {
	s := newTaskServer(t)
	attach(s, "n1", 4) // 挂上流但**永远不回话**

	start := time.Now()
	out := s.RunTasks(context.Background(),
		[]TaskRequest{{Node: "n1", Kind: TaskRestart}}, 200*time.Millisecond)

	if len(out) != 1 || !out[0].TimedOut {
		t.Fatalf("应当超时，得到 %+v", out)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("超时应当在期限附近发生，花了 %v", d)
	}
	// 超时之后不该继续占着等待表
	s.tasks.mu.Lock()
	n := len(s.tasks.waiting)
	s.tasks.mu.Unlock()
	if n != 0 {
		t.Errorf("超时之后应当清掉等待记录，还剩 %d 条", n)
	}
}

// TestLateResultIsHarmless：结果来晚了不该崩，也不该被算进任何人的答案。
func TestLateResultIsHarmless(t *testing.T) {
	s := newTaskServer(t)
	ch := attach(s, "n1", 4)

	done := make(chan []TaskOutcome, 1)
	go func() {
		done <- s.RunTasks(context.Background(),
			[]TaskRequest{{Node: "n1", Kind: TaskRestart}}, 150*time.Millisecond)
	}()
	task := <-ch
	<-done // 等它超时走人

	// 现在节点才回话——中心早就把答案给用户了
	s.tasks.mu.Lock()
	_, still := s.tasks.waiting[task.GetId()]
	s.tasks.mu.Unlock()
	if still {
		t.Fatal("超时之后等待记录还在")
	}
}

// TestCongestedChannelIsUnreachable：缓冲满 = 节点在挤压命令。
//
// **当成不可达，而不是无限等下去**：一个挤压着命令的节点多半已经不健康，
// 而挂住敲命令的人不会让它变好。
func TestCongestedChannelIsUnreachable(t *testing.T) {
	s := newTaskServer(t)
	ch := attach(s, "n1", 1)
	ch <- &agentpb.Task{Id: "占位"} // 把缓冲塞满

	// **超时给得很长**：判据是「立刻放弃」，而不是「最终会返回」。
	// 一个阻塞着等缓冲腾出来的实现同样会返回，只是把敲命令的人挂在
	// 那里——而那正是这条分支要避免的。
	start := time.Now()
	out := s.RunTasks(context.Background(),
		[]TaskRequest{{Node: "n1", Kind: TaskRestart}}, 10*time.Second)
	elapsed := time.Since(start)

	if len(out) != 1 || !out[0].Unreachable {
		t.Fatalf("拥塞应当判为 unreachable，得到 %+v", out)
	}
	if out[0].Message == "" {
		t.Error("要说清是拥塞而不是没连上")
	}
	if elapsed > time.Second {
		t.Errorf("拥塞应当立刻放弃，却等了 %v——敲命令的人被挂住了", elapsed)
	}
}

// TestExpiredCommandIsNotExecuted 守节点侧那一半。
//
// 一条已经超时的命令，中心早就把它报成失败并返回给用户了。此时再去动
// 机器，是一次**没人预期的重启**——而那正是运维最怕的那种。
func TestExpiredCommandIsNotExecuted(t *testing.T) {
	now := time.Now()
	past := TaskCommand{Deadline: now.Add(-time.Second)}
	future := TaskCommand{Deadline: now.Add(time.Minute)}
	none := TaskCommand{}

	if !past.Expired(now) {
		t.Error("过期的命令要判为过期")
	}
	if future.Expired(now) {
		t.Error("没到期的不该判为过期")
	}
	if none.Expired(now) {
		t.Error("没给期限的不该判为过期")
	}
}
