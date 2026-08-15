package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/protocol"
	"github.com/mecharion/mecharion/internal/reconcile"
	"github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/spec"
)

// taskRuntime 是一个只实现 restart 会用到的那几个方法的 Runtime。
//
// 内嵌的 runtime.Runtime 是 nil：**调到别的方法就会 panic**，那正是
// 我们要的——restart 不该去碰 Materialize 之类的东西（拆一次进程不需要
// 重新物化），而 panic 会把「顺手多做了一件事」当场暴露出来。
type taskRuntime struct {
	runtime.Runtime
	acts     []string
	stopErr  error
	startErr error
}

func (r *taskRuntime) Name() string { return "systemd" }

func (r *taskRuntime) RefFor(w runtime.WorkloadSpec) (runtime.Ref, error) {
	r.acts = append(r.acts, "refFor")
	return runtime.Ref{
		Runtime: "systemd", Component: w.Component, Role: w.Role,
		Native: "mecharion-" + w.Component + "-" + w.Role + ".service",
	}, nil
}

func (r *taskRuntime) Stop(context.Context, runtime.Ref, runtime.StopOpts) error {
	r.acts = append(r.acts, "stop")
	return r.stopErr
}

func (r *taskRuntime) Start(context.Context, runtime.Ref) error {
	r.acts = append(r.acts, "start")
	return r.startErr
}

// taskAgent 起一个只带期望状态与 Runtime 的 agent——restart 不需要别的。
func taskAgent(t *testing.T, withWorkload bool) (*Agent, *taskRuntime) {
	t.Helper()
	ds, _ := newDesired(t)

	in := instSpec("web", "default")
	if withWorkload {
		in.Spec.Workload = &spec.Workload{
			Runtime: "systemd",
			Systemd: &spec.SystemdWorkload{Exec: "/bin/true"},
		}
	}
	if err := ds.Save(in); err != nil {
		t.Fatal(err)
	}

	rt := &taskRuntime{}
	a := &Agent{
		opts: Options{
			Desired:    ds,
			Reconciler: &reconcile.Reconciler{Runtimes: runtime.NewRegistry(rt)},
			Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
		busy: map[string]bool{},
	}
	return a, rt
}

func restartCmd() protocol.TaskCommand {
	return protocol.TaskCommand{
		ID: "t1", Kind: protocol.TaskRestart, Component: "web", Role: "default",
	}
}

// TestRestartStopsThenStarts 是这条命令的主判据。
func TestRestartStopsThenStarts(t *testing.T) {
	a, rt := taskAgent(t, true)

	res := a.RunTask(context.Background(), restartCmd())
	if !res.OK {
		t.Fatalf("应当成功，得到: %s", res.Message)
	}
	// **也没有物化**：拆一次进程不需要重新装一遍。Materialize 在
	// taskRuntime 上是 nil，真调到它这条测试会 panic 而不是失败——更明显。
	if got := strings.Join(rt.acts, ","); got != "refFor,stop,start" {
		t.Errorf("顺序应当是 refFor→stop→start，实际 %s", got)
	}
	// 不断言 Duration > 0：那条断言在 Windows 上会随机变红（整包跑的时候
	// 抓到过一次），而它守住的东西——「耗时被填上了」——不值得一条会
	// 时不时骗人的测试。一条偶尔变红的测试，最后换来的是所有人都不看红色。
}

// TestRestartReportsStoppedButNotStarted 是最要紧的一条。
//
// **停掉了但没起来**：这种结果必须原样说出来——运维需要知道现在那台
// 机器上的服务是**停着**的。一句笼统的「重启失败」会让人以为它还在跑。
func TestRestartReportsStoppedButNotStarted(t *testing.T) {
	a, rt := taskAgent(t, true)
	rt.startErr = errors.New("端口被占用")

	res := a.RunTask(context.Background(), restartCmd())
	if res.OK {
		t.Fatal("启动失败时不该报成功")
	}
	if !strings.Contains(res.Message, "停止状态") {
		t.Errorf("必须说清服务现在是停着的，得到: %s", res.Message)
	}
	if !strings.Contains(res.Message, "端口被占用") {
		t.Errorf("要带上底层原因，得到: %s", res.Message)
	}
	if got := strings.Join(rt.acts, ","); got != "refFor,stop,start" {
		t.Errorf("即使启动失败也该走完 stop→start，实际 %s", got)
	}
}

// TestRestartStopFailureDoesNotStart：停不掉就别起。
//
// 停失败之后照常 start，会得到一个「以为重启了、其实是旧进程」的状态。
func TestRestartStopFailureDoesNotStart(t *testing.T) {
	a, rt := taskAgent(t, true)
	rt.stopErr = errors.New("unit 不存在")

	res := a.RunTask(context.Background(), restartCmd())
	if res.OK {
		t.Fatal("停止失败时不该报成功")
	}
	for _, act := range rt.acts {
		if act == "start" {
			t.Errorf("停不掉就不该再 start，实际 %v", rt.acts)
		}
	}
}

// TestRestartOnAnInstanceWithoutWorkload：纯配置分发的角色没有进程。
//
// **说清楚而不是假装成功**：一句「已重启」会让人以为那个角色有进程在跑。
func TestRestartOnAnInstanceWithoutWorkload(t *testing.T) {
	a, rt := taskAgent(t, false)

	res := a.RunTask(context.Background(), restartCmd())
	if res.OK {
		t.Fatal("没有工作负载时不该报成功")
	}
	if !strings.Contains(res.Message, "没有工作负载") {
		t.Errorf("要说清原因，得到: %s", res.Message)
	}
	if len(rt.acts) != 0 {
		t.Errorf("不该碰 Runtime，实际 %v", rt.acts)
	}
}

// TestRestartOnAnUnknownInstance：本机没有那个实例。
func TestRestartOnAnUnknownInstance(t *testing.T) {
	a, _ := taskAgent(t, true)

	res := a.RunTask(context.Background(), protocol.TaskCommand{
		ID: "t2", Kind: protocol.TaskRestart, Component: "nope", Role: "default",
	})
	if res.OK {
		t.Fatal("本机没有的实例不该报成功")
	}
	if !strings.Contains(res.Message, "没有实例") {
		t.Errorf("要说清原因，得到: %s", res.Message)
	}
}

// TestRestartSkipsABusyInstance 守一条会与调和抢同一个 unit 的路。
func TestRestartSkipsABusyInstance(t *testing.T) {
	a, rt := taskAgent(t, true)
	if !a.acquire("web__default") {
		t.Fatal("造不出忙状态")
	}

	res := a.RunTask(context.Background(), restartCmd())
	if res.OK {
		t.Fatal("实例正忙时不该动手")
	}
	if !strings.Contains(res.Message, "正在调和") {
		t.Errorf("要说清原因，得到: %s", res.Message)
	}
	if len(rt.acts) != 0 {
		t.Errorf("不该碰 Runtime，实际 %v", rt.acts)
	}
}

// TestUnknownTaskKindIsRefused 守向前兼容的方向。
//
// 一个新 mechd 发来的新命令，旧 mechlet 做不了。**静默成功会让中心
// 以为它生效了**——而那正是「命令必须带回结果」想避免的东西。
func TestUnknownTaskKindIsRefused(t *testing.T) {
	a, rt := taskAgent(t, true)

	res := a.RunTask(context.Background(), protocol.TaskCommand{
		ID: "t3", Kind: "teleport", Component: "web", Role: "default",
	})
	if res.OK {
		t.Fatal("不认识的命令类型必须如实拒绝")
	}
	if !strings.Contains(res.Message, "升级 mechlet") {
		t.Errorf("要给出可行动的下一步，得到: %s", res.Message)
	}
	if len(rt.acts) != 0 {
		t.Errorf("不该碰 Runtime，实际 %v", rt.acts)
	}
}
