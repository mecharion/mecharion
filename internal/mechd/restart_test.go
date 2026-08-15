package mechd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mecharion/mecharion/internal/protocol"
	"github.com/mecharion/mecharion/internal/store"
)

// fakeTasker 记下发出去的命令，并按预设作答。
//
// 服务层要验的是**编排**：谁被选中、结果怎么对齐、0 个匹配时说什么。
// 真正的送达语义（离线立刻判定、超时、拥塞）在 internal/protocol 那边
// 验过了，这里再验一遍只是把同一件事测两次。
type fakeTasker struct {
	got  []protocol.TaskRequest
	outs []protocol.TaskOutcome
}

func (f *fakeTasker) RunTasks(
	_ context.Context, reqs []protocol.TaskRequest, _ time.Duration,
) []protocol.TaskOutcome {
	f.got = append(f.got, reqs...)
	if f.outs != nil {
		return f.outs
	}
	out := make([]protocol.TaskOutcome, len(reqs))
	for i, r := range reqs {
		out[i] = protocol.TaskOutcome{Node: r.Node, OK: true, Duration: time.Second}
	}
	return out
}

func restartFixture(t *testing.T) (*fixture, *fakeTasker) {
	t.Helper()
	f := formFixture(t)
	deployKit(t, f, nil) // paramkit 的 main 角色在 n1、n2
	tk := &fakeTasker{}
	f.svc.Tasks = tk
	return f, tk
}

// TestRestartTargetsEveryInstance：不给范围就是整个组件。
func TestRestartTargetsEveryInstance(t *testing.T) {
	f, tk := restartFixture(t)

	res, err := f.svc.Restart(ctx(), RestartRequest{Component: "paramkit", Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tk.got) != 2 {
		t.Fatalf("应当发出 2 条命令，实际 %d: %+v", len(tk.got), tk.got)
	}
	for _, r := range tk.got {
		if r.Kind != protocol.TaskRestart {
			t.Errorf("命令类型不对: %q", r.Kind)
		}
		if r.Component != "paramkit" {
			t.Errorf("组件名不对: %q", r.Component)
		}
	}
	if len(res.Instances) != 2 || res.Failed() {
		t.Errorf("两个实例都该成功: %+v", res.Instances)
	}
}

// TestRestartHonoursNodeFilter：--node 只动那一台。
func TestRestartHonoursNodeFilter(t *testing.T) {
	f, tk := restartFixture(t)

	if _, err := f.svc.Restart(ctx(), RestartRequest{
		Component: "paramkit", Node: "n2", Actor: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if len(tk.got) != 1 || tk.got[0].Node != "n2" {
		t.Fatalf("--node n2 应当只发给 n2，实际 %+v", tk.got)
	}
}

// TestRestartWithNoMatchesFails 守一句会骗人的成功。
//
// **0 个匹配要报错而不是静默成功**：一句「已重启」而实际什么都没重启，
// 会让人以为问题已经处理过了——然后去查别处。
func TestRestartWithNoMatchesFails(t *testing.T) {
	f, tk := restartFixture(t)

	_, err := f.svc.Restart(ctx(), RestartRequest{
		Component: "paramkit", Node: "nope", Actor: "test",
	})
	if err == nil {
		t.Fatal("没有匹配的实例时必须报错")
	}
	if len(tk.got) != 0 {
		t.Errorf("不该发出任何命令，实际 %+v", tk.got)
	}
}

// TestRestartWithoutATaskChannelSaysSo：nil 通道要明确报错，不静默。
//
// 一个「装配漏了 Tasks」的 mechd 若静默返回空结果，restart 会表现成
// 「跑了但什么也没发生」——而那与「机器上出了问题」长得一模一样。
func TestRestartWithoutATaskChannelSaysSo(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)
	f.svc.Tasks = nil

	_, err := f.svc.Restart(ctx(), RestartRequest{Component: "paramkit", Actor: "test"})
	if err == nil {
		t.Fatal("没有命令通道时必须报错")
	}
	if !strings.Contains(err.Error(), "command channel") {
		t.Errorf("要说清是通道没接上，得到: %v", err)
	}
}

// TestRestartReportsPerInstanceStates 是这条命令的产出本身。
//
// **unreachable 与 failed 必须分开**：一个去查网络，一个去看日志。
func TestRestartReportsPerInstanceStates(t *testing.T) {
	f, tk := restartFixture(t)
	tk.outs = []protocol.TaskOutcome{
		{Node: "n1", OK: true, Duration: 1200 * time.Millisecond},
		{Node: "n2", Unreachable: true},
	}

	res, err := f.svc.Restart(ctx(), RestartRequest{Component: "paramkit", Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Failed() {
		t.Error("有实例不可达时整体应当算失败")
	}
	var states = map[string]string{}
	for _, o := range res.Instances {
		states[o.Node] = o.State
	}
	if states["n1"] != "ok" {
		t.Errorf("n1 应当是 ok，得到 %q", states["n1"])
	}
	if states["n2"] != "unreachable" {
		t.Errorf("n2 应当是 unreachable（不是 failed），得到 %q", states["n2"])
	}
}

// TestOutcomeStatesAreAllDistinct 把四个状态一次钉死。
//
// **审查补的**：`outcomeState` 收尾时只走过 ok 与 unreachable 两支，
// `timeout` 从没被执行过。四个状态对运维是四件不同的事：
//
//	unreachable  那台没连着，命令根本没发出去 → 查网络
//	timeout      发出去了，但没在时限内回话   → **机器上可能真的在重启**
//	failed       发出去了，节点明确说没成     → 看日志
//	ok           成了
//
// timeout 与 failed 混为一谈最危险：它会让人以为「没重启」，
// 于是再敲一次。
func TestOutcomeStatesAreAllDistinct(t *testing.T) {
	cases := []struct {
		name string
		out  protocol.TaskOutcome
		want string
	}{
		{"不可达", protocol.TaskOutcome{Unreachable: true}, "unreachable"},
		{"超时", protocol.TaskOutcome{TimedOut: true}, "timeout"},
		{"成功", protocol.TaskOutcome{OK: true}, "ok"},
		{"失败", protocol.TaskOutcome{Message: "端口被占用"}, "failed"},
		// 不可达优先于超时：连都没连上，「等超时」这件事没发生过
		{"不可达且超时", protocol.TaskOutcome{Unreachable: true, TimedOut: true}, "unreachable"},
	}
	for _, c := range cases {
		if got := outcomeState(c.out); got != c.want {
			t.Errorf("%s: 期望 %q，得到 %q", c.name, c.want, got)
		}
	}
}

// TestRestartRefusedWhileRemoving：正在被删的组件不该还能重启。
//
// 它走的是与其它写动词同一道闸门——restart 虽然不改期望状态，但对一个
// 正在拆的东西做任何动作都只会制造困惑。
func TestRestartRefusedWhileRemoving(t *testing.T) {
	f, tk := restartFixture(t)
	startRemoving(t, f, store.ComponentRemoval{})

	_, err := f.svc.Restart(ctx(), RestartRequest{Component: "paramkit", Actor: "test"})
	if err == nil {
		t.Fatal("正在移除的组件不该能重启")
	}
	if len(tk.got) != 0 {
		t.Errorf("不该发出任何命令，实际 %+v", tk.got)
	}
}
