package command

import (
	"context"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mecharion/mecharion/internal/faults"
)

func ctx() context.Context { return context.Background() }

// shell 返回本平台上「执行一句命令」的调用方式。
func shell(script string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", script}
	}
	return "/bin/sh", []string{"-c", script}
}

// TestExitCodeIsNotError 钉住「退出码不是 error」。
//
// getent 的退出码 2 是「查无此项」，systemctl is-active 的非零是
// 「没在跑」——都是正常结果。把它们当成 error，调用方就没法区分
// 「命令说了不」和「命令根本没跑起来」。
func TestExitCodeIsNotError(t *testing.T) {
	name, args := shell("exit 3")
	res, err := Exec{}.Run(ctx(), name, args...)
	if err != nil {
		t.Fatalf("非零退出码不该返回 error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d，期望 3", res.ExitCode)
	}
}

func TestRunCapturesOutput(t *testing.T) {
	name, args := shell("echo 标准输出 && echo 标准错误 1>&2")
	res, err := Exec{}.Run(ctx(), name, args...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "标准输出") {
		t.Errorf("Stdout = %q", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "标准错误") {
		t.Errorf("Stderr = %q", res.Stderr)
	}
}

// TestMissingCommandIsError 钉住「命令不存在」才是 error。
func TestMissingCommandIsError(t *testing.T) {
	_, err := Exec{}.Run(ctx(), "m7n-这个命令不存在-zzz")
	if err == nil {
		t.Fatal("命令不存在应当返回 error")
	}
	if !IsNotFound(err) {
		t.Errorf("应当能被 IsNotFound 识别，实际 %v", err)
	}
}

func TestMessagePrefersStderr(t *testing.T) {
	if got := (Result{Stdout: "out", Stderr: " err \n"}).Message(); got != "err" {
		t.Errorf("Message = %q，期望 err", got)
	}
	if got := (Result{Stdout: " out \n"}).Message(); got != "out" {
		t.Errorf("stderr 为空时应退回 stdout，实际 %q", got)
	}
}

func TestMustRunClassifies(t *testing.T) {
	name, args := shell("echo 说明原因 1>&2 && exit 1")
	err := MustRun(ctx(), Exec{}, "试一下", name, args...)
	if err == nil {
		t.Fatal("非零退出码应当被 MustRun 判为失败")
	}
	if faults.ClassOf(err) != faults.Permanent {
		t.Errorf("退出码非零是命令自己说不行，应归为 permanent，实际 %s", faults.ClassOf(err))
	}
	// 失败原因必须带出来，否则错误信息只剩一个退出码
	if !strings.Contains(err.Error(), "说明原因") {
		t.Errorf("应当带上命令的 stderr，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "试一下") {
		t.Errorf("应当带上操作名，实际: %v", err)
	}

	if err := MustRun(ctx(), Exec{}, "试一下", "m7n-不存在-zzz"); err == nil {
		t.Fatal("命令不存在应当报错")
	} else if !strings.Contains(err.Error(), "本机没有") {
		t.Errorf("应当说清是本机没有这个命令，实际: %v", err)
	}
}

// TestStreamDeliversOutput 验证流式取输出。
func TestStreamDeliversOutput(t *testing.T) {
	name, args := shell("echo 第一行 && echo 第二行")
	rc, err := Exec{}.Stream(ctx(), name, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"第一行", "第二行"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("输出应含 %q，实际 %q", want, body)
		}
	}
}

// TestStreamCloseReapsProcess 钉住「关闭流会回收进程」。
//
// mechlet 是长驻进程。`journalctl -f` 这种不会自己结束的命令，如果
// Close 不 Kill + Wait，每一次 UI 看日志都会留下一个僵尸进程。
func TestStreamCloseReapsProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有僵尸进程模型")
	}
	rc, err := Exec{}.Stream(ctx(), "/bin/sh", "-c", "echo 开始; sleep 300")
	if err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 16)
	if _, err := rc.Read(buf); err != nil {
		t.Fatalf("应当能读到第一段输出: %v", err)
	}

	pr, ok := rc.(*procReader)
	if !ok {
		t.Fatalf("Stream 应返回 *procReader，实际 %T", rc)
	}
	pid := pr.cmd.Process.Pid

	done := make(chan error, 1)
	go func() { done <- rc.Close() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close 卡住了——它必须 Kill 而不是等命令自己结束")
	}

	// Wait 已经回收过，进程状态应当已经落定
	if pr.cmd.ProcessState == nil {
		t.Errorf("Close 必须 Wait，否则 pid %d 会变成僵尸进程", pid)
	}

	// 重复 Close 无害
	if err := rc.Close(); err != nil {
		t.Errorf("重复 Close 应当无害: %v", err)
	}
}

func TestStreamMissingCommand(t *testing.T) {
	_, err := Exec{}.Stream(ctx(), "m7n-不存在-zzz")
	if err == nil {
		t.Fatal("命令不存在应当报错")
	}
	if !IsNotFound(err) {
		t.Errorf("应当能被 IsNotFound 识别，实际 %v", err)
	}
}

// ── 替身 ────────────────────────────────────────────────────────────────

func TestFakeMatching(t *testing.T) {
	f := NewFake()
	f.Set("systemctl show -p X unit", Result{Stdout: "精确\n"})
	f.SetPrefix("systemctl show", Result{Stdout: "短前缀\n"})
	f.SetPrefix("systemctl show -p", Result{Stdout: "长前缀\n"})
	f.Default = Result{Stdout: "兜底\n"}

	cases := []struct{ args, want string }{
		{"systemctl show -p X unit", "精确\n"},
		{"systemctl show -p Y unit", "长前缀\n"}, // 前缀取最长的一条
		{"systemctl show unit", "短前缀\n"},
		{"systemctl status unit", "兜底\n"},
	}
	for _, tc := range cases {
		parts := strings.Fields(tc.args)
		res, err := f.Run(ctx(), parts[0], parts[1:]...)
		if err != nil {
			t.Fatal(err)
		}
		if res.Stdout != tc.want {
			t.Errorf("%q → %q，期望 %q", tc.args, res.Stdout, tc.want)
		}
	}
}

func TestFakeRecordsCalls(t *testing.T) {
	f := NewFake()
	_, _ = f.Run(ctx(), "systemctl", "start", "a.service")
	_, _ = f.Run(ctx(), "systemctl", "start", "b.service")
	_, _ = f.Run(ctx(), "systemctl", "stop", "a.service")

	if !f.Ran("systemctl start a.service") {
		t.Error("应当记录到执行过的命令")
	}
	if n := f.CountRan("systemctl start"); n != 2 {
		t.Errorf("CountRan = %d，期望 2", n)
	}
	if len(f.Calls()) != 3 {
		t.Errorf("Calls = %v", f.Calls())
	}

	f.Reset()
	if len(f.Calls()) != 0 {
		t.Error("Reset 应当清空调用记录")
	}
	if f.Ran("systemctl") {
		t.Error("Reset 之后不该还认为执行过命令")
	}
}

func TestFakeNotFound(t *testing.T) {
	f := NewFake()
	f.NotFound = true

	if _, err := f.Run(ctx(), "systemctl"); !IsNotFound(err) {
		t.Errorf("应当模拟出 exec.ErrNotFound，实际 %v", err)
	}
	if _, err := f.Stream(ctx(), "journalctl"); !IsNotFound(err) {
		t.Errorf("Stream 也应当，实际 %v", err)
	}
}

func TestFakeStream(t *testing.T) {
	f := NewFake()
	f.SetStream("journalctl", "一行日志\n")

	rc, err := f.Stream(ctx(), "journalctl", "-u", "x.service")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if string(body) != "一行日志\n" {
		t.Errorf("Stream 输出 = %q", body)
	}
}

// 编译期断言：两个实现都满足接口。
var (
	_ Runner = Exec{}
	_ Runner = (*Fake)(nil)
)
