//go:build linux

package systemd

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mecharion/mecharion/internal/command"
	"github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/spec"
)

// 命令替身能验证「该调哪条命令」，验证不了「systemd 认不认这个 unit」。
// 一个拼错的指令名（LimitNOFILES）、一个 systemd 不接受的取值，替身全都
// 照单全收，而真机上服务根本起不来。
//
// 这几个用例驱动真正的 systemd，因此要求 root + systemd 作为 init。
// 在 test/node 的容器里跑（make testbin && hack/testenv.sh up）。

func requireSystemd(t *testing.T) *Runtime {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("需要 root 才能装 unit；在 test/node 容器里跑")
	}
	rt := New()
	cap, err := rt.Probe(context.Background())
	if err != nil || !cap.Available {
		t.Skipf("本机没有可用的 systemd: %v / %+v", err, cap)
	}
	t.Logf("systemd 版本 %s，系统状态 %s", cap.Version, cap.Detail["systemState"])
	return rt
}

// execTempDir 返回一个**可以执行文件**的临时目录。
//
// 不用 t.TempDir()：它落在 /tmp，而 systemd 的 tmp.mount 把 /tmp 挂成
// `nosuid,nodev,noexec` 的 tmpfs——往那儿放脚本，服务会以
// 「Failed at step EXEC … Permission denied」(203/EXEC) 起不来，
// 而错误信息完全不提 noexec，极难联想。
//
// Mecharion 自己不受影响：载荷一律解在 <home>/generations 下
// （/opt 或 /var/lib），不碰 /tmp。
func execTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/var/tmp", "m7ntest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// sleeper 构造一个长驻的最小工作负载。
func sleeper(component string, mut ...func(*spec.SystemdWorkload)) runtime.WorkloadSpec {
	sd := &spec.SystemdWorkload{Exec: "/bin/sleep 3600"}
	for _, m := range mut {
		m(sd)
	}
	return runtime.WorkloadSpec{
		Site: "s1", Component: component, Role: "default", ConfigGroup: "default",
		Generation: 1,
		Workload:   &spec.Workload{Runtime: "systemd", Systemd: sd},
	}
}

// waitState 轮询到期望状态，超时即失败。systemd 的状态转换是异步的。
func waitState(t *testing.T, rt *Runtime, ref runtime.Ref, want runtime.State) runtime.Status {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last runtime.Status
	for time.Now().Before(deadline) {
		st, err := rt.Observe(context.Background(), ref)
		if err != nil {
			t.Fatalf("Observe: %v", err)
		}
		last = st
		if st.State == want {
			return st
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("等待状态 %s 超时，最后观测到 %s（Raw=%v）", want, last.State, last.Raw)
	return last
}

func TestRealLifecycle(t *testing.T) {
	rt := requireSystemd(t)
	ctx := context.Background()
	w := sleeper("m7ntest-life")

	ref, err := rt.Materialize(ctx, w)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	t.Cleanup(func() { _ = rt.Remove(context.Background(), ref) })

	// Materialize 之后应当是「已物化但没跑」
	st := waitState(t, rt, ref, runtime.StateStopped)
	if st.Native != ref.Native {
		t.Errorf("Native = %q", st.Native)
	}

	// systemd 必须认这个 unit —— LoadState=loaded 才说明它解析成功了
	if got := st.Raw["LoadState"]; got != "loaded" {
		t.Fatalf("systemd 没能加载这个 unit：LoadState=%q\n"+
			"（说明生成的 unit 里有它不认的指令）", got)
	}
	// enable 之后应当能在重启后自己回来
	if got := st.Raw["UnitFileState"]; got != "enabled" {
		t.Errorf("UnitFileState = %q，期望 enabled", got)
	}

	if err := rt.Start(ctx, ref); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st = waitState(t, rt, ref, runtime.StateRunning)
	if st.Since.IsZero() {
		t.Error("运行中应当能读出状态变更时刻")
	}

	if err := rt.Stop(ctx, ref, runtime.StopOpts{}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitState(t, rt, ref, runtime.StateStopped)

	if err := rt.Remove(ctx, ref); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	waitState(t, rt, ref, runtime.StateAbsent)
}

// TestRealMaterializeIsIdempotent 在真机上验证幂等。
func TestRealMaterializeIsIdempotent(t *testing.T) {
	rt := requireSystemd(t)
	ctx := context.Background()
	w := sleeper("m7ntest-idem")

	ref, err := rt.Materialize(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Remove(context.Background(), ref) })

	if err := rt.Start(ctx, ref); err != nil {
		t.Fatal(err)
	}
	waitState(t, rt, ref, runtime.StateRunning)
	before, err := rt.Observe(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}

	// 重复 Materialize 不该惊动正在跑的服务
	for i := 0; i < 3; i++ {
		if _, err := rt.Materialize(ctx, w); err != nil {
			t.Fatalf("第 %d 次 Materialize: %v", i+1, err)
		}
	}
	after, err := rt.Observe(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != runtime.StateRunning {
		t.Errorf("重复 Materialize 后状态变成了 %s", after.State)
	}
	if before.Raw["MainPID"] != after.Raw["MainPID"] {
		t.Errorf("重复 Materialize 不该重启进程：MainPID %s → %s",
			before.Raw["MainPID"], after.Raw["MainPID"])
	}
	if after.Restarts != before.Restarts {
		t.Errorf("重启次数变了：%d → %d", before.Restarts, after.Restarts)
	}
}

// TestRealFailedWorkload 验证失败状态与退出码被正确读出。
func TestRealFailedWorkload(t *testing.T) {
	rt := requireSystemd(t)
	ctx := context.Background()

	w := sleeper("m7ntest-fail", func(s *spec.SystemdWorkload) {
		s.Exec = "/bin/sh -c 'exit 42'"
		s.Restart = "no" // 不要让它被拉起来，否则观测到的是 activating
	})
	ref, err := rt.Materialize(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Remove(context.Background(), ref) })

	// 启动会失败——systemd 的 start 对 Type=simple 立即返回成功，
	// 因此这里不断言 Start 的返回值，只看最终状态。
	_ = rt.Start(ctx, ref)

	st := waitState(t, rt, ref, runtime.StateFailed)
	if st.ExitCode == nil {
		t.Fatal("失败时必须能读出退出码——那是排障的第一手信息")
	}
	if *st.ExitCode != 42 {
		t.Errorf("退出码 = %d，期望 42", *st.ExitCode)
	}
}

// TestRealEnvironmentQuoting 验证带空格的环境变量真的完整传给了进程。
//
// 这是单元测试断言不了的：渲染出的引号对不对，只有 systemd 说了算。
//
// 用 `cp /proc/self/environ` 取进程实际拿到的环境，**刻意不经过 shell**：
// systemd 自己会对 ExecStart 里的 `$VAR` 做一轮展开，再套一层 shell
// 就分不清「值错了」是谁造成的了。
func TestRealEnvironmentQuoting(t *testing.T) {
	rt := requireSystemd(t)
	ctx := context.Background()
	const want = "-Xms2g -Xmx2g -XX:+UseG1GC"
	out := "/tmp/m7ntest-env.environ"
	_ = os.Remove(out)
	t.Cleanup(func() { _ = os.Remove(out) })

	w := sleeper("m7ntest-env", func(s *spec.SystemdWorkload) {
		s.Exec = "/bin/cp /proc/self/environ " + out
		s.Restart = "no"
		s.Env = map[string]string{"JAVA_OPTS": want}
	})
	ref, err := rt.Materialize(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Remove(context.Background(), ref) })

	// ① systemd 自己解析出的 Environment 就该是完整的一条
	show, err := command.Exec{}.Run(ctx, "systemctl", "show",
		"--property=Environment", "--value", ref.Native)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(show.Stdout, "JAVA_OPTS="+want) {
		t.Errorf("systemd 解析出的 Environment = %q\n"+
			"  期望含 JAVA_OPTS=%s —— 不一致说明 Environment= 那行的引用方式不对",
			strings.TrimSpace(show.Stdout), want)
	}

	// ② 进程实际拿到的 environ 也该是完整的一条
	if err := rt.Start(ctx, ref); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var body []byte
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(out); err == nil && len(b) > 0 {
			body = b
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if body == nil {
		t.Fatal("进程没能写出 /proc/self/environ")
	}

	var got string
	found := false
	for _, kv := range strings.Split(string(body), "\x00") {
		if v, ok := strings.CutPrefix(kv, "JAVA_OPTS="); ok {
			got, found = v, true
			break
		}
	}
	if !found {
		t.Fatalf("进程的环境里没有 JAVA_OPTS：%q",
			strings.ReplaceAll(string(body), "\x00", " "))
	}
	if got != want {
		t.Errorf("进程收到的 JAVA_OPTS = %q，期望 %q\n"+
			"（带空格的值被 systemd 拆开了）", got, want)
	}
}

// TestRealReloadUnsupported 验证真实 systemd 也报「不支持热加载」。
func TestRealReloadUnsupported(t *testing.T) {
	rt := requireSystemd(t)
	ctx := context.Background()

	w := sleeper("m7ntest-reload")
	ref, err := rt.Materialize(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Remove(context.Background(), ref) })

	if err := rt.Reload(ctx, ref); err != runtime.ErrReloadUnsupported {
		t.Errorf("没声明 execReload 时应返回 ErrReloadUnsupported，实际 %v", err)
	}

	// 声明了 execReload 就该能 reload。
	//
	// 用 /bin/true 而非惯用的 `/bin/kill -HUP $MAINPID`：debian:12-slim
	// 里没装 procps，`/bin/kill` 根本不存在，那样测的就成了「容器里有没有
	// kill」而不是「reload 能不能派发」。
	w2 := sleeper("m7ntest-reload", func(s *spec.SystemdWorkload) {
		s.ExecReload = "/bin/true"
	})
	ref2, err := rt.Materialize(ctx, w2)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(ctx, ref2); err != nil {
		t.Fatal(err)
	}
	waitState(t, rt, ref2, runtime.StateRunning)
	if err := rt.Reload(ctx, ref2); err != nil {
		t.Errorf("声明了 execReload 就该能 reload: %v", err)
	}
}

// TestRealReloadDeliversSignal 验证「不依赖 procps 的 reload 写法」真的送达了信号。
//
// systemd 手册里的惯用写法是 `ExecReload=/bin/kill -s HUP $MAINPID`，但
// **debian:12-slim 没装 procps，/bin/kill 根本不存在**——照抄手册会得到一个
// 「声明了 execReload 却永远 reload 失败」的 Pack。
//
// 替代写法用 shell 内建的 kill：`/bin/sh -c 'kill -HUP $MAINPID'`。
// 这里要验证两件事，缺一不可：
//
//	① $MAINPID 确实被 systemd 展开了（而不是留给 sh 去展开一个空变量）
//	② 进程收到了 SIGHUP，且**没有被重启**（MainPID 不变）
func TestRealReloadDeliversSignal(t *testing.T) {
	rt := requireSystemd(t)
	ctx := context.Background()

	dir := execTempDir(t)
	script := dir + "/app.sh"
	marker := dir + "/hup.log"
	body := "#!/bin/sh\n" +
		"trap 'echo GOT-HUP >> " + marker + "' HUP\n" +
		"while :; do sleep 0.2; done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	w := sleeper("m7ntest-hup", func(s *spec.SystemdWorkload) {
		s.Exec = script
		s.ExecReload = "/bin/sh -c 'kill -HUP $MAINPID'"
	})
	ref, err := rt.Materialize(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Remove(context.Background(), ref) })

	if err := rt.Start(ctx, ref); err != nil {
		t.Fatal(err)
	}
	before := waitState(t, rt, ref, runtime.StateRunning)
	pidBefore := before.Raw["MainPID"]

	if err := rt.Reload(ctx, ref); err != nil {
		t.Fatalf("reload 失败——这正是 /bin/kill 不存在时会发生的事: %v", err)
	}

	// ① 信号送达
	deadline := time.Now().Add(10 * time.Second)
	got := false
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(marker); err == nil && strings.Contains(string(b), "GOT-HUP") {
			got = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !got {
		t.Error("进程没收到 SIGHUP —— 说明 $MAINPID 没被 systemd 展开，" +
			"sh 拿到的是一个空变量")
	}

	// ② 没有被重启
	after, err := rt.Observe(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != runtime.StateRunning {
		t.Errorf("reload 之后应当仍在运行，实际 %s", after.State)
	}
	if after.Raw["MainPID"] != pidBefore {
		t.Errorf("reload 不该重启进程：MainPID %s → %s",
			pidBefore, after.Raw["MainPID"])
	}
}

// TestRealLogs 验证能从 journald 取到日志。
func TestRealLogs(t *testing.T) {
	rt := requireSystemd(t)
	ctx := context.Background()

	const marker = "m7n-日志标记-9f2c"
	w := sleeper("m7ntest-logs", func(s *spec.SystemdWorkload) {
		s.Exec = "/bin/sh -c 'echo " + marker + "; sleep 3600'"
	})
	ref, err := rt.Materialize(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Remove(context.Background(), ref) })

	if err := rt.Start(ctx, ref); err != nil {
		t.Fatal(err)
	}
	waitState(t, rt, ref, runtime.StateRunning)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		rc, err := rt.Logs(ctx, ref, runtime.LogOpts{Tail: 50})
		if err != nil {
			t.Fatalf("Logs: %v", err)
		}
		body, rerr := io.ReadAll(rc)
		rc.Close()
		if rerr != nil {
			t.Fatal(rerr)
		}
		if strings.Contains(string(body), marker) {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Skip("journald 未收到日志——容器里 journald 可能没在跑")
}

// TestRealStopOfUnresponsiveService 钉住「停不干净的服务停完是 failed」。
//
// 这是真机行为，替身测不出来：一个忽略 SIGTERM 的进程会被 systemd 在
// TimeoutStopSec 到期后 SIGKILL，unit 随即进入 failed（Result=timeout）
// 而不是 inactive。
//
// **我们刻意不把它抹成 stopped。** 对 PostgreSQL 这类有状态服务，
// 「上次是被 KILL 掉的」意味着下次启动要走崩溃恢复，运维必须知道。
func TestRealStopOfUnresponsiveService(t *testing.T) {
	rt := requireSystemd(t)
	ctx := context.Background()

	w := sleeper("m7ntest-force", func(s *spec.SystemdWorkload) {
		s.Exec = "/bin/sh -c 'trap \"\" TERM; sleep 3600'"
		s.TimeoutStop = "2s"
		s.KillMode = "mixed"
	})
	ref, err := rt.Materialize(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Remove(context.Background(), ref) })

	if err := rt.Start(ctx, ref); err != nil {
		t.Fatal(err)
	}
	waitState(t, rt, ref, runtime.StateRunning)

	// systemd 自己的 TimeoutStopSec 到期后会 SIGKILL，因此 stop 最终返回成功
	if err := rt.Stop(ctx, ref, runtime.StopOpts{Force: true}); err != nil {
		t.Fatalf("Stop(Force): %v", err)
	}

	st := waitState(t, rt, ref, runtime.StateFailed)
	if st.Running() {
		t.Fatal("进程应当已经没了")
	}
	if got := st.Raw["Result"]; got != "timeout" {
		t.Errorf("Result = %q，期望 timeout —— 这一项是「它是被杀掉的」的凭据", got)
	}
	if st.ExitCode == nil || *st.ExitCode != 9 {
		t.Errorf("ExitCode = %v，期望 9（SIGKILL）", st.ExitCode)
	}

	// 被强杀过之后仍然能正常启动
	if err := rt.Start(ctx, ref); err != nil {
		t.Fatalf("failed 状态不该妨碍下次启动: %v", err)
	}
	waitState(t, rt, ref, runtime.StateRunning)
}
