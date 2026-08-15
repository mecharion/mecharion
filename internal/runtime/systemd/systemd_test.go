package systemd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mecharion/mecharion/internal/command"
	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/spec"
)

// newRT 构造一个把 unit 写进临时目录、systemctl 走替身的 Runtime。
func newRT(t *testing.T) (*Runtime, *command.Fake) {
	t.Helper()
	fake := command.NewFake()
	// 默认：unit 已启用、系统正常，让测试只需覆写它关心的那条
	fake.SetPrefix("systemctl is-enabled", command.Result{Stdout: "enabled\n"})
	return &Runtime{UnitDir: t.TempDir(), Runner: fake}, fake
}

func ctx() context.Context { return context.Background() }

// ── Probe ───────────────────────────────────────────────────────────────

func TestProbeAvailable(t *testing.T) {
	rt, fake := newRT(t)
	fake.Set("systemctl --version", command.Result{
		Stdout: "systemd 252 (252.22-1~deb12u1)\n+PAM +AUDIT\n",
	})
	fake.Set("systemctl is-system-running", command.Result{Stdout: "running\n"})

	cap, err := rt.Probe(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if !cap.Available {
		t.Fatalf("应当可用: %+v", cap)
	}
	if cap.Version != "252" {
		t.Errorf("版本 = %q，期望 252", cap.Version)
	}
}

// TestProbeDegradedIsStillAvailable 钉住「有 unit 失败 ≠ systemd 不可用」。
//
// is-system-running 在 degraded 时退出码非 0。把它当成不可用，会让一台
// 只是某个无关服务挂了的机器整个被判定为不能部署。
func TestProbeDegradedIsStillAvailable(t *testing.T) {
	rt, fake := newRT(t)
	fake.Set("systemctl --version", command.Result{Stdout: "systemd 252\n"})
	fake.Set("systemctl is-system-running",
		command.Result{Stdout: "degraded\n", ExitCode: 1})

	cap, err := rt.Probe(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if !cap.Available {
		t.Errorf("degraded 时 systemd 本身可用，不该判为不可用: %+v", cap)
	}
	if cap.Detail["systemState"] != "degraded" {
		t.Errorf("应当把系统状态带进 Detail: %+v", cap.Detail)
	}
}

// TestProbeWithoutInit 钉住「装了 systemd 但没跑」的判定。
//
// 容器里最常见：systemctl 命令在，PID 1 不是 systemd，任何操作都会
// 以「System has not been booted with systemd」失败。
func TestProbeWithoutInit(t *testing.T) {
	rt, fake := newRT(t)
	fake.Set("systemctl --version", command.Result{Stdout: "systemd 252\n"})
	fake.Set("systemctl is-system-running", command.Result{
		ExitCode: 1,
		Stderr:   "System has not been booted with systemd as init system (PID 1).",
	})

	cap, err := rt.Probe(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if cap.Available {
		t.Fatal("没有 systemd 作为 init 时必须判为不可用")
	}
	// Reason 会被 mechd 原样展示给用户，必须说清楚
	if !strings.Contains(cap.Reason, "has not been booted") {
		t.Errorf("Reason 应带上 systemd 自己的说明，实际 %q", cap.Reason)
	}
}

// TestProbeRejectsTooOldSystemd 钉住版本下限的拦截。
//
// 明确拒绝并说清版本，好过让用户在某个用不了的属性上撞一鼻子灰。
func TestProbeRejectsTooOldSystemd(t *testing.T) {
	rt, fake := newRT(t)
	fake.Set("systemctl --version", command.Result{Stdout: "systemd 219\n"})
	fake.Set("systemctl is-system-running", command.Result{Stdout: "running\n"})

	cap, err := rt.Probe(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if cap.Available {
		t.Fatal("低于下限的 systemd 应当判为不可用")
	}
	for _, want := range []string{"219", "239"} {
		if !strings.Contains(cap.Reason, want) {
			t.Errorf("Reason 应当同时给出实际版本与下限，缺 %q: %q", want, cap.Reason)
		}
	}
}

// TestProbeAcceptsVendorVersionSuffix 钉住发行版的版本后缀不影响判定。
//
// 各家都会加后缀："252.22-1~deb12u1"、"239-78.el8"、"250~rc1"。
func TestProbeAcceptsVendorVersionSuffix(t *testing.T) {
	for _, v := range []string{"252.22-1~deb12u1", "239-78.el8", "247", "250~rc1"} {
		rt, fake := newRT(t)
		fake.Set("systemctl --version", command.Result{Stdout: "systemd " + v + " (…)\n"})
		fake.Set("systemctl is-system-running", command.Result{Stdout: "running\n"})

		cap, err := rt.Probe(ctx())
		if err != nil {
			t.Fatalf("version=%q: %v", v, err)
		}
		if !cap.Available {
			t.Errorf("version=%q 应当可用: %s", v, cap.Reason)
		}
	}
}

// TestProbeDoesNotBlockOnUnparsableVersion 钉住认不出版本时放行。
//
// 一个认不出的版本号更可能是我们的解析没跟上，而不是系统太旧——
// 拦掉的代价（装不上）比放行的代价（某个属性读不到）大得多。
func TestProbeDoesNotBlockOnUnparsableVersion(t *testing.T) {
	rt, fake := newRT(t)
	fake.Set("systemctl --version", command.Result{Stdout: "systemd v-future-9\n"})
	fake.Set("systemctl is-system-running", command.Result{Stdout: "running\n"})

	cap, err := rt.Probe(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if !cap.Available {
		t.Errorf("认不出版本时不该拦，实际: %s", cap.Reason)
	}
}

// TestReloadAcceptsBothShowFormats 钉住 `--value` 的两种输出形态都认。
//
// `--value` 是 systemd v230 才有的 flag；更老的版本原样输出 `Key=Value`。
// 不认后者就等于为一个 flag 抬高了版本下限。
func TestReloadAcceptsBothShowFormats(t *testing.T) {
	for _, out := range []string{"no\n", "CanReload=no\n"} {
		rt, fake := newRT(t)
		fake.SetPrefix("systemctl show --property=CanReload", command.Result{Stdout: out})

		if err := rt.Reload(ctx(), RefFor("webapp", "default", 7)); err != runtime.ErrReloadUnsupported {
			t.Errorf("输出 %q 时应当识别为不支持，实际 %v", out, err)
		}
	}
}

func TestProbeWithoutSystemctl(t *testing.T) {
	rt, fake := newRT(t)
	fake.NotFound = true

	cap, err := rt.Probe(ctx())
	if err != nil {
		t.Fatalf("命令不存在是一个正常的探测结果，不该返回 error: %v", err)
	}
	if cap.Available || !strings.Contains(cap.Reason, "systemctl") {
		t.Errorf("应当报告没有 systemctl: %+v", cap)
	}
}

// ── Materialize ─────────────────────────────────────────────────────────

func TestMaterializeWritesUnitAndEnables(t *testing.T) {
	rt, fake := newRT(t)
	fake.SetPrefix("systemctl is-enabled", command.Result{
		Stdout: "disabled\n", ExitCode: 1,
	})

	ref, err := rt.Materialize(ctx(), webapp())
	if err != nil {
		t.Fatal(err)
	}
	if ref.Native != "mecharion-webapp-default.service" {
		t.Errorf("Native = %q", ref.Native)
	}
	if ref.Runtime != Name {
		t.Errorf("Runtime = %q", ref.Runtime)
	}

	body, err := os.ReadFile(filepath.Join(rt.UnitDir, ref.Native))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "ExecStart=") {
		t.Errorf("unit 内容不对:\n%s", body)
	}
	if !fake.Ran("systemctl daemon-reload") {
		t.Error("写了新 unit 必须 daemon-reload，否则 systemd 看不到它")
	}
	if !fake.Ran("systemctl enable mecharion-webapp-default.service") {
		t.Errorf("应当 enable 让组件在重启后自己回来，实际: %v", fake.Calls())
	}
	// Materialize 明确「不启动」
	if fake.Ran("systemctl start") {
		t.Error("Materialize 不该启动服务")
	}
}

// TestMaterializeIsIdempotent 钉住「内容没变就不 daemon-reload」。
//
// daemon-reload 会让 systemd 重新解析全系统的 unit，在装了几百个 unit
// 的机器上并不便宜——而调和每 60 秒就会走一次这里。
func TestMaterializeIsIdempotent(t *testing.T) {
	rt, fake := newRT(t)
	w := webapp()

	if _, err := rt.Materialize(ctx(), w); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rt.UnitDir, UnitName(w.Component, w.Role))
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	fake.Reset()

	if _, err := rt.Materialize(ctx(), w); err != nil {
		t.Fatal(err)
	}
	if fake.Ran("systemctl daemon-reload") {
		t.Error("内容没变不该 daemon-reload")
	}
	if fake.Ran("systemctl enable") {
		t.Error("已经 enable 了不该再 enable 一次")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("内容没变不该重写 unit 文件")
	}
}

// TestMaterializeReloadsWhenContentChanges 钉住内容变了就要 reload。
func TestMaterializeReloadsWhenContentChanges(t *testing.T) {
	rt, fake := newRT(t)
	if _, err := rt.Materialize(ctx(), webapp()); err != nil {
		t.Fatal(err)
	}
	fake.Reset()

	changed := webapp(func(s *spec.SystemdWorkload) { s.LimitNofile = 65536 })
	if _, err := rt.Materialize(ctx(), changed); err != nil {
		t.Fatal(err)
	}
	if !fake.Ran("systemctl daemon-reload") {
		t.Error("unit 内容变了必须 daemon-reload")
	}
	body, _ := os.ReadFile(filepath.Join(rt.UnitDir, UnitName("webapp", "default")))
	if !strings.Contains(string(body), "LimitNOFILE=65536") {
		t.Errorf("新内容没写进去:\n%s", body)
	}
}

func TestMaterializeRejectsIncompleteSpec(t *testing.T) {
	rt, _ := newRT(t)
	for _, tc := range []struct {
		name string
		mut  func(*runtime.WorkloadSpec)
	}{
		{"缺 component", func(w *runtime.WorkloadSpec) { w.Component = "" }},
		{"缺 role", func(w *runtime.WorkloadSpec) { w.Role = "" }},
		{"缺 workload", func(w *runtime.WorkloadSpec) { w.Workload = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := webapp()
			tc.mut(&w)
			if _, err := rt.Materialize(ctx(), w); err == nil {
				t.Fatal("应当被拒绝")
			}
		})
	}
}

// ── 生命周期 ────────────────────────────────────────────────────────────

func TestStartStop(t *testing.T) {
	rt, fake := newRT(t)
	ref, err := rt.Materialize(ctx(), webapp())
	if err != nil {
		t.Fatal(err)
	}

	if err := rt.Start(ctx(), ref); err != nil {
		t.Fatal(err)
	}
	if !fake.Ran("systemctl start mecharion-webapp-default.service") {
		t.Errorf("实际执行了: %v", fake.Calls())
	}

	if err := rt.Stop(ctx(), ref, runtime.StopOpts{}); err != nil {
		t.Fatal(err)
	}
	if !fake.Ran("systemctl stop mecharion-webapp-default.service") {
		t.Errorf("实际执行了: %v", fake.Calls())
	}
}

// TestStartFailureIncludesLogs 钉住启动失败时把日志附上。
//
// `systemctl start` 失败只说「Job failed. See 'journalctl -xe'」，
// 真正的原因（端口占用、配置语法错误）在日志里。让人再敲一条命令才能
// 看到根因，是最常见的排障摩擦。
func TestStartFailureIncludesLogs(t *testing.T) {
	rt, fake := newRT(t)
	fake.SetPrefix("systemctl start", command.Result{
		ExitCode: 1,
		Stderr:   "Job for mecharion-webapp-default.service failed.",
	})
	fake.SetPrefix("journalctl", command.Result{
		Stdout: "webapp[1234]: listen tcp :8080: bind: address already in use\n",
	})

	ref := RefFor("webapp", "default", 7)
	err := rt.Start(ctx(), ref)
	if err == nil {
		t.Fatal("应当失败")
	}
	if !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("错误信息应带上最近日志，实际:\n%v", err)
	}
}

// TestStartFailureSurvivesMissingLogs 钉住取不到日志时不掩盖原始错误。
func TestStartFailureSurvivesMissingLogs(t *testing.T) {
	rt, fake := newRT(t)
	fake.SetPrefix("systemctl start", command.Result{
		ExitCode: 1, Stderr: "Job for … failed.",
	})
	fake.SetPrefix("journalctl", command.Result{ExitCode: 1, Stderr: "no journal"})

	err := rt.Start(ctx(), RefFor("webapp", "default", 7))
	if err == nil {
		t.Fatal("应当失败")
	}
	if !strings.Contains(err.Error(), "Job for") {
		t.Errorf("取不到日志时应保留原始错误，实际: %v", err)
	}
}

// TestStopForceKills 钉住 Force 在正常停止失败后强杀。
func TestStopForceKills(t *testing.T) {
	rt, fake := newRT(t)
	fake.SetPrefix("systemctl stop", command.Result{
		ExitCode: 1, Stderr: "Job timed out",
	})

	ref := RefFor("webapp", "default", 7)
	if err := rt.Stop(ctx(), ref, runtime.StopOpts{Force: true}); err != nil {
		t.Fatalf("Force 应当在强杀成功后返回 nil: %v", err)
	}
	if !fake.Ran("systemctl kill --signal=SIGKILL mecharion-webapp-default.service") {
		t.Errorf("实际执行了: %v", fake.Calls())
	}
}

// TestStopWithoutForceDoesNotKill 钉住不加 Force 就不强杀。
func TestStopWithoutForceDoesNotKill(t *testing.T) {
	rt, fake := newRT(t)
	fake.SetPrefix("systemctl stop", command.Result{ExitCode: 1, Stderr: "Job timed out"})

	if err := rt.Stop(ctx(), RefFor("webapp", "default", 7), runtime.StopOpts{}); err == nil {
		t.Fatal("停不下来就该报错")
	}
	if fake.Ran("systemctl kill") {
		t.Error("没要求 Force 时不该强杀——SIGKILL 会让有状态服务丢数据")
	}
}

// TestReloadUnsupportedIsDistinguishable 钉住「不支持热加载」可判定。
//
// 调用方要据此降级为 restart。若它只是一条普通错误，就只能靠匹配
// systemd 的英文错误串来识别——那会在换个 locale 或换个版本时失效。
func TestReloadUnsupportedIsDistinguishable(t *testing.T) {
	rt, fake := newRT(t)
	fake.SetPrefix("systemctl show --property=CanReload",
		command.Result{Stdout: "no\n"})

	err := rt.Reload(ctx(), RefFor("webapp", "default", 7))
	if err != runtime.ErrReloadUnsupported {
		t.Fatalf("应当返回 ErrReloadUnsupported，实际 %v", err)
	}
	if fake.Ran("systemctl reload") {
		t.Error("明知不支持就不该去试")
	}
}

func TestReloadSupported(t *testing.T) {
	rt, fake := newRT(t)
	fake.SetPrefix("systemctl show --property=CanReload",
		command.Result{Stdout: "yes\n"})

	if err := rt.Reload(ctx(), RefFor("webapp", "default", 7)); err != nil {
		t.Fatal(err)
	}
	if !fake.Ran("systemctl reload mecharion-webapp-default.service") {
		t.Errorf("实际执行了: %v", fake.Calls())
	}
}

func TestRemove(t *testing.T) {
	rt, fake := newRT(t)
	ref, err := rt.Materialize(ctx(), webapp())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rt.UnitDir, ref.Native)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}

	if err := rt.Remove(ctx(), ref); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("unit 文件应当被删除")
	}
	if !fake.Ran("systemctl disable --now") {
		t.Error("应当先停止并禁用")
	}
	if !fake.Ran("systemctl reset-failed") {
		t.Error("应当清掉 failed 状态，否则 `systemctl --failed` 会一直" +
			"挂着一个已经不存在的 unit")
	}
}

// TestRemoveIsIdempotent 钉住重复 Remove 无害。
func TestRemoveIsIdempotent(t *testing.T) {
	rt, _ := newRT(t)
	ref := RefFor("webapp", "default", 7)
	for i := 0; i < 3; i++ {
		if err := rt.Remove(ctx(), ref); err != nil {
			t.Fatalf("第 %d 次 Remove: %v", i+1, err)
		}
	}
}

// ── Observe ─────────────────────────────────────────────────────────────

func TestObserveStateMapping(t *testing.T) {
	cases := []struct {
		name  string
		props map[string]string
		want  runtime.State
	}{
		{"未安装", map[string]string{"LoadState": "not-found"}, runtime.StateAbsent},
		{"正常运行", map[string]string{
			"LoadState": "loaded", "ActiveState": "active", "SubState": "running",
		}, runtime.StateRunning},
		{"启动中", map[string]string{
			"LoadState": "loaded", "ActiveState": "activating", "SubState": "start",
		}, runtime.StateStarting},
		{"崩溃后自动重启中", map[string]string{
			"LoadState": "loaded", "ActiveState": "activating", "SubState": "auto-restart",
		}, runtime.StateStarting},
		{"已停止", map[string]string{
			"LoadState": "loaded", "ActiveState": "inactive", "SubState": "dead",
			"Result": "success",
		}, runtime.StateStopped},
		{"正在停止", map[string]string{
			"LoadState": "loaded", "ActiveState": "deactivating", "SubState": "stop-sigterm",
		}, runtime.StateStopped},
		{"失败", map[string]string{
			"LoadState": "loaded", "ActiveState": "failed", "SubState": "failed",
			"ExecMainStatus": "1",
		}, runtime.StateFailed},
		{"重载中仍算运行", map[string]string{
			"LoadState": "loaded", "ActiveState": "reloading", "SubState": "reload",
		}, runtime.StateRunning},
		{"active 但子状态异常", map[string]string{
			"LoadState": "loaded", "ActiveState": "active", "SubState": "exited",
		}, runtime.StateDegraded},
		{"inactive 但结果非 success", map[string]string{
			"LoadState": "loaded", "ActiveState": "inactive", "SubState": "dead",
			"Result": "exit-code", "ExecMainStatus": "2",
		}, runtime.StateFailed},
	}

	ref := RefFor("webapp", "default", 7)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := statusFrom(ref, tc.props)
			if got.State != tc.want {
				t.Errorf("State = %s，期望 %s", got.State, tc.want)
			}
			if got.Native != ref.Native {
				t.Error("Native 不可省略——排障时要靠它知道去 journalctl -u 哪个 unit")
			}
		})
	}
}

func TestObserveParsesDetails(t *testing.T) {
	rt, fake := newRT(t)
	fake.SetPrefix("systemctl show", command.Result{Stdout: strings.Join([]string{
		"LoadState=loaded",
		"ActiveState=failed",
		"SubState=failed",
		"NRestarts=4",
		"ExecMainStatus=137",
		"StateChangeTimestamp=Sun 2026-08-03 10:05:00 UTC",
		"Result=signal",
		"MainPID=0",
		"", // 尾部空行不该让解析出错
	}, "\n")})

	st, err := rt.Observe(ctx(), RefFor("webapp", "default", 7))
	if err != nil {
		t.Fatal(err)
	}
	if st.State != runtime.StateFailed {
		t.Errorf("State = %s", st.State)
	}
	if st.Restarts != 4 {
		t.Errorf("Restarts = %d，期望 4", st.Restarts)
	}
	if st.ExitCode == nil || *st.ExitCode != 137 {
		t.Errorf("ExitCode = %v，期望 137", st.ExitCode)
	}
	if st.Since.IsZero() {
		t.Error("应当解析出状态变更时刻")
	}
	if st.Raw["Result"] != "signal" {
		t.Errorf("Raw 应保留原始属性供 UI 展开: %v", st.Raw)
	}
	if st.Health != runtime.HealthNone {
		t.Error("systemd 对普通 unit 没有原生健康概念，应为 HealthNone")
	}
}

// TestObserveTimestampFallback 钉住解析不了时间戳不影响观测。
func TestObserveTimestampFallback(t *testing.T) {
	got := statusFrom(RefFor("x", "y", 1), map[string]string{
		"LoadState": "loaded", "ActiveState": "active", "SubState": "running",
		"StateChangeTimestamp": "n/a",
		"ActiveEnterTimestamp": "看不懂的本地化格式",
	})
	if got.State != runtime.StateRunning {
		t.Error("时间戳解析失败不该影响状态判定")
	}
	if !got.Since.IsZero() {
		t.Error("解析不了就该是零值，不该瞎猜一个时间")
	}
}

// TestObserveWithoutSystemdErrors 钉住「systemd 没跑」是 error 而非某个状态。
//
// 这是「本环境不适用」，应当中止调和——糊弄成 Absent 会让引擎以为
// 只要 Materialize 一下就好，然后在每一轮调和里重复失败。
func TestObserveWithoutSystemdErrors(t *testing.T) {
	rt, fake := newRT(t)
	fake.SetPrefix("systemctl show", command.Result{
		ExitCode: 1,
		Stderr:   "System has not been booted with systemd as init system (PID 1).",
	})

	_, err := rt.Observe(ctx(), RefFor("webapp", "default", 7))
	if err == nil {
		t.Fatal("systemd 不可用时必须报错")
	}
	if faults.ClassOf(err) != faults.Permanent {
		t.Errorf("应归为 permanent，实际 %s", faults.ClassOf(err))
	}
}

// ── Logs ────────────────────────────────────────────────────────────────

func TestLogsBuildsJournalctlArgs(t *testing.T) {
	rt, fake := newRT(t)
	fake.SetStream("journalctl", "第一行\n第二行\n")

	rc, err := rt.Logs(ctx(), RefFor("webapp", "default", 7), runtime.LogOpts{
		Tail: 100, Follow: true,
		Since: time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "第一行") {
		t.Errorf("日志内容 = %q", body)
	}

	call := fake.Calls()[len(fake.Calls())-1]
	for _, want := range []string{
		"-u mecharion-webapp-default.service", "--no-pager",
		"-n 100", "--since 2026-08-03 10:00:00", "-f",
	} {
		if !strings.Contains(call, want) {
			t.Errorf("journalctl 参数应含 %q，实际: %s", want, call)
		}
	}
}

func TestLogsWithoutOptions(t *testing.T) {
	rt, fake := newRT(t)
	rc, err := rt.Logs(ctx(), RefFor("webapp", "default", 7), runtime.LogOpts{})
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()

	call := fake.Calls()[len(fake.Calls())-1]
	if strings.Contains(call, "-f") || strings.Contains(call, "--since") {
		t.Errorf("没要求就不该加这些参数: %s", call)
	}
}

// ── 注册表 ──────────────────────────────────────────────────────────────

func TestRegistryResolvesByWorkload(t *testing.T) {
	rt, _ := newRT(t)
	reg := runtime.NewRegistry(rt)

	got, err := reg.For(&spec.Workload{Runtime: "systemd"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name() != Name {
		t.Errorf("取到了 %q", got.Name())
	}

	_, err = reg.For(&spec.Workload{Runtime: "docker"})
	if err == nil {
		t.Fatal("未注册的 runtime 应当报错")
	}
	if !strings.Contains(err.Error(), "systemd") {
		t.Errorf("错误信息应列出可用的 runtime，实际: %v", err)
	}
}

func TestRegistryProbeCollectsAll(t *testing.T) {
	rt, fake := newRT(t)
	fake.Set("systemctl --version", command.Result{Stdout: "systemd 252\n"})
	fake.Set("systemctl is-system-running", command.Result{Stdout: "running\n"})

	caps := runtime.NewRegistry(rt).Probe(ctx())
	if c, ok := caps["systemd"]; !ok || !c.Available {
		t.Errorf("应当探测到 systemd 可用: %+v", caps)
	}
}

// TestRefForIsPureFunction 钉住 Ref 可由组件与角色重建。
//
// mechlet 重启后不必重新 Materialize 就能 Observe / Stop——unit 名不
// 依赖任何本地状态。
func TestRefForIsPureFunction(t *testing.T) {
	rt, _ := newRT(t)
	materialized, err := rt.Materialize(ctx(), webapp())
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := RefFor("webapp", "default", 7)
	if materialized.Native != rebuilt.Native {
		t.Errorf("Materialize 产出 %q，重建得到 %q", materialized.Native, rebuilt.Native)
	}
}

// TestExecInRunsOnHost 钉住 systemd 的执行上下文就是这台机器。
//
// 这是 ADR-0032 的另一半：接缝加进来了，但 **systemd 侧的行为一个字
// 没变**——命令原样在宿主机上跑，不包 systemd-run、不进 cgroup。
// 那些会带来一堆与目的无关的复杂度，而探针要的只是「在这台机器上
// 跑一下 pg_isready」。
func TestExecInRunsOnHost(t *testing.T) {
	rt, fake := newRT(t)
	fake.Set("pg_isready -p 5432", command.Result{Stdout: "accepting connections"})

	res, err := rt.ExecIn(ctx(), runtime.Ref{Component: "pg", Role: "primary"},
		[]string{"pg_isready", "-p", "5432"})
	if err != nil {
		t.Fatalf("ExecIn: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "accepting") {
		t.Errorf("应原样返回命令结果，实际 %+v", res)
	}

	// 命令行就是运维自己能敲的那条，没有包装
	calls := fake.Calls()
	if len(calls) != 1 || calls[0] != "pg_isready -p 5432" {
		t.Errorf("systemd 应当原样执行命令，实际执行了 %v", calls)
	}
}

// TestExecInRejectsEmptyCommand 钉住空命令被明确拒绝。
func TestExecInRejectsEmptyCommand(t *testing.T) {
	rt, _ := newRT(t)
	if _, err := rt.ExecIn(ctx(), runtime.Ref{}, nil); err == nil {
		t.Error("空命令应当报错，而不是去执行一个空字符串")
	}
}
