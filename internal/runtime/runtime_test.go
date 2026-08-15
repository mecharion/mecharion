package runtime

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/command"
	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/spec"
)

// stub 是一个只用于测试注册表的最小 Runtime。
type stub struct {
	name     string
	cap      Capability
	probeErr error
}

func (s *stub) Name() string { return s.name }
func (s *stub) Probe(context.Context) (Capability, error) {
	return s.cap, s.probeErr
}
func (s *stub) Materialize(context.Context, WorkloadSpec) (Ref, error) { return Ref{}, nil }
func (s *stub) RefFor(WorkloadSpec) (Ref, error)                       { return Ref{}, nil }
func (s *stub) Start(context.Context, Ref) error                       { return nil }
func (s *stub) Stop(context.Context, Ref, StopOpts) error              { return nil }
func (s *stub) Reload(context.Context, Ref) error                      { return ErrReloadUnsupported }
func (s *stub) Observe(context.Context, Ref) (Status, error)           { return Status{}, nil }
func (s *stub) Remove(context.Context, Ref) error                      { return nil }
func (s *stub) ExecIn(context.Context, Ref, []string) (command.Result, error) {
	return command.Result{}, nil
}
func (s *stub) Logs(context.Context, Ref, LogOpts) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func TestRegistryLookup(t *testing.T) {
	reg := NewRegistry(
		&stub{name: "systemd"},
		&stub{name: "docker"},
	)

	if got := reg.Names(); len(got) != 2 || got[0] != "docker" || got[1] != "systemd" {
		t.Errorf("Names 应按字典序（错误信息要稳定）: %v", got)
	}

	rt, err := reg.Get("systemd")
	if err != nil || rt.Name() != "systemd" {
		t.Errorf("Get(systemd) = %v, %v", rt, err)
	}

	_, err = reg.Get("podman")
	if err == nil {
		t.Fatal("未注册的 runtime 应当报错")
	}
	// 错误信息要列出可选项——名字打错时这是最有用的一行
	if !strings.Contains(err.Error(), "docker") || !strings.Contains(err.Error(), "systemd") {
		t.Errorf("应当列出已注册的 runtime，实际: %v", err)
	}
	if faults.ClassOf(err) != faults.Permanent {
		t.Errorf("选错 runtime 是配置问题，应归为 permanent，实际 %s", faults.ClassOf(err))
	}
}

func TestRegistryForNilWorkload(t *testing.T) {
	if _, err := NewRegistry().For(nil); err == nil {
		t.Error("工作负载为空应当报错，而不是返回一个零值 Runtime")
	}
}

// TestRegistryProbeSurvivesFailure 钉住「一个 Runtime 探测失败不影响其它的」。
//
// Probe 的结果整份作为 Node capability 上报。若一次异常就让整份结果丢失，
// mechd 会以为这台机器什么都不支持。
func TestRegistryProbeSurvivesFailure(t *testing.T) {
	reg := NewRegistry(
		&stub{name: "systemd", cap: Capability{Available: true, Version: "252"}},
		&stub{name: "docker", probeErr: errors.New("socket 连不上")},
	)

	caps := reg.Probe(context.Background())
	if len(caps) != 2 {
		t.Fatalf("两个都该有结果，实际 %v", caps)
	}
	if !caps["systemd"].Available {
		t.Error("systemd 应当可用")
	}
	if caps["docker"].Available {
		t.Error("探测失败的应判为不可用")
	}
	if !strings.Contains(caps["docker"].Reason, "socket 连不上") {
		t.Errorf("失败原因要带出来: %q", caps["docker"].Reason)
	}
}

func TestRegisterOverwrites(t *testing.T) {
	reg := NewRegistry(&stub{name: "systemd", cap: Capability{Version: "旧"}})
	reg.Register(&stub{name: "systemd", cap: Capability{Version: "新"}})

	if n := len(reg.Names()); n != 1 {
		t.Errorf("同名应当覆盖而非并存，实际 %d 个", n)
	}
	rt, _ := reg.Get("systemd")
	c, _ := rt.Probe(context.Background())
	if c.Version != "新" {
		t.Errorf("应当是后注册的那个，实际 %q", c.Version)
	}
}

func TestWorkloadSpecValidate(t *testing.T) {
	ok := WorkloadSpec{
		Component: "webapp", Role: "default",
		Workload: &spec.Workload{Runtime: "systemd"},
	}
	if err := ok.Validate(); err != nil {
		t.Errorf("完整的规格应当通过: %v", err)
	}
	if got := ok.Key(); got != "webapp-default" {
		t.Errorf("Key = %q", got)
	}

	for _, tc := range []struct {
		name string
		mut  func(*WorkloadSpec)
		want string
	}{
		{"缺 component", func(w *WorkloadSpec) { w.Component = "" }, "component"},
		{"缺 role", func(w *WorkloadSpec) { w.Role = "" }, "role"},
		{"缺 workload", func(w *WorkloadSpec) { w.Workload = nil }, "workload"},
		{"缺 runtime", func(w *WorkloadSpec) {
			w.Workload = &spec.Workload{}
		}, "runtime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := ok
			tc.mut(&w)
			err := w.Validate()
			if err == nil {
				t.Fatal("应当校验失败")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息应包含 %q，实际: %v", tc.want, err)
			}
		})
	}
}

// TestRefStringFallsBackToIdentity 钉住「Ref 总能打印出点有用的东西」。
func TestRefStringFallsBackToIdentity(t *testing.T) {
	full := Ref{Component: "webapp", Role: "default", Native: "mecharion-webapp-default.service"}
	if got := full.String(); got != "mecharion-webapp-default.service" {
		t.Errorf("有 Native 时应当用它（排障要靠它）: %q", got)
	}
	bare := Ref{Component: "webapp", Role: "default"}
	if got := bare.String(); got != "webapp/default" {
		t.Errorf("没有 Native 时应退回身份，而不是空串: %q", got)
	}
}

func TestStateStrings(t *testing.T) {
	// 状态名会出现在 CLI 与 UI 上，必须稳定
	want := map[State]string{
		StateAbsent: "absent", StateStopped: "stopped", StateStarting: "starting",
		StateRunning: "running", StateFailed: "failed", StateDegraded: "degraded",
	}
	for s, w := range want {
		if got := s.String(); got != w {
			t.Errorf("State(%d) = %q，期望 %q", int(s), got, w)
		}
	}
	if !(Status{State: StateRunning}).Running() {
		t.Error("Running() 应当只在 running 时为真")
	}
	for _, s := range []State{StateStarting, StateDegraded, StateStopped} {
		if (Status{State: s}).Running() {
			t.Errorf("%s 不算 running —— 它还没在正常服务", s)
		}
	}
}

func TestHealthStateStrings(t *testing.T) {
	if HealthNone.String() != "none" || HealthPassing.String() != "passing" ||
		HealthFailing.String() != "failing" {
		t.Error("健康状态名必须稳定")
	}
}
