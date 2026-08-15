package health

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mecharion/mecharion/internal/command"
	"github.com/mecharion/mecharion/internal/spec"
)

func ctx() context.Context { return context.Background() }

// portOf 从 httptest 服务器地址里取端口。
func portOf(t *testing.T, s *httptest.Server) int {
	t.Helper()
	_, p, err := net.SplitHostPort(strings.TrimPrefix(s.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// listenLocal 起一个 httptest 服务器，绑在 127.0.0.1 上——探针只探本机。
func listenLocal(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

// TestNilHealthIsNotAnError 钉住「没声明健康检查不是错误」。
//
// 很多组件没有可探测的端点，强制它们编一个只会得到一个永远通过的假探针。
func TestNilHealthIsNotAnError(t *testing.T) {
	c, err := New(nil, nil)
	if err != nil {
		t.Fatalf("nil 不该报错: %v", err)
	}
	if c != nil {
		t.Error("nil 应当返回 nil Checker")
	}
}

func TestHTTPProbe(t *testing.T) {
	srv := listenLocal(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))

	c, err := New(&spec.Health{
		HTTP: &spec.HTTPProbe{Path: "/healthz", Port: portOf(t, srv)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Once(ctx()); err != nil {
		t.Errorf("应当通过: %v", err)
	}

	// 路径不对 → 404 不在期望之内
	bad, _ := New(&spec.Health{
		HTTP: &spec.HTTPProbe{Path: "/nope", Port: portOf(t, srv)},
	}, nil)
	err = bad.Once(ctx())
	if err == nil {
		t.Fatal("404 应当算失败")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("错误信息应带上实际状态码: %v", err)
	}
}

func TestHTTPProbeExpectStatus(t *testing.T) {
	srv := listenLocal(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	// 默认只认 200
	def, _ := New(&spec.Health{HTTP: &spec.HTTPProbe{Port: portOf(t, srv)}}, nil)
	if err := def.Once(ctx()); err == nil {
		t.Error("默认只认 200，204 应当失败")
	}

	// 声明了就按声明来
	c, _ := New(&spec.Health{
		HTTP: &spec.HTTPProbe{Port: portOf(t, srv), ExpectStatus: []int{204, 200}},
	}, nil)
	if err := c.Once(ctx()); err != nil {
		t.Errorf("204 在期望之内，应当通过: %v", err)
	}
}

// TestHTTPProbeDoesNotFollowRedirect 钉住探针不跟随跳转。
//
// 302 到别处然后 200，说不上**本服务**是健康的。
func TestHTTPProbeDoesNotFollowRedirect(t *testing.T) {
	target := listenLocal(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv := listenLocal(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))

	c, _ := New(&spec.Health{HTTP: &spec.HTTPProbe{Port: portOf(t, srv)}}, nil)
	if err := c.Once(ctx()); err == nil {
		t.Error("302 不该被当成健康——跳转到别处的 200 说明不了本服务的状态")
	}
}

func TestHTTPProbePathNormalization(t *testing.T) {
	var got atomic.Value
	got.Store("")
	srv := listenLocal(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{"healthz", "/healthz"} {
		c, _ := New(&spec.Health{
			HTTP: &spec.HTTPProbe{Path: path, Port: portOf(t, srv)},
		}, nil)
		if err := c.Once(ctx()); err != nil {
			t.Fatalf("path=%q: %v", path, err)
		}
		if got.Load() != "/healthz" {
			t.Errorf("path=%q 请求到了 %q，期望 /healthz", path, got.Load())
		}
	}

	// 未声明 path 时探根路径
	c, _ := New(&spec.Health{HTTP: &spec.HTTPProbe{Port: portOf(t, srv)}}, nil)
	if err := c.Once(ctx()); err != nil {
		t.Fatal(err)
	}
	if got.Load() != "/" {
		t.Errorf("未声明 path 时应当探 /，实际 %q", got.Load())
	}
}

func TestTCPProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	_, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)

	c, err := New(&spec.Health{TCP: &spec.TCPProbe{Port: port}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Once(ctx()); err != nil {
		t.Errorf("应当连得上: %v", err)
	}

	ln.Close()
	// 端口关了之后应当探测失败（给内核一点时间释放）
	time.Sleep(50 * time.Millisecond)
	if err := c.Once(ctx()); err == nil {
		t.Error("端口关了应当探测失败")
	}
}

func TestExecProbe(t *testing.T) {
	fake := command.NewFake()
	fake.Set("pg_isready -p 5432", command.Result{})

	c, err := New(&spec.Health{
		Exec: &spec.ExecProbe{Command: []string{"pg_isready", "-p", "5432"}},
	}, RunnerExec(fake))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Once(ctx()); err != nil {
		t.Errorf("退出码 0 应当通过: %v", err)
	}

	fake.Set("pg_isready -p 5432", command.Result{ExitCode: 2, Stderr: "拒绝连接"})
	err = c.Once(ctx())
	if err == nil {
		t.Fatal("非零退出码应当失败")
	}
	if !strings.Contains(err.Error(), "拒绝连接") {
		t.Errorf("错误信息应带上命令输出: %v", err)
	}
}

func TestRejectsInvalidProbes(t *testing.T) {
	cases := []struct {
		name string
		h    *spec.Health
		want string
	}{
		{"没有探针", &spec.Health{}, "只能声明一种"},
		{"两种探针", &spec.Health{
			HTTP: &spec.HTTPProbe{Port: 80}, TCP: &spec.TCPProbe{Port: 80},
		}, "只能声明一种"},
		{"端口非法", &spec.Health{HTTP: &spec.HTTPProbe{Port: 0}}, "port"},
		{"端口超范围", &spec.Health{TCP: &spec.TCPProbe{Port: 70000}}, "port"},
		{"scheme 非法", &spec.Health{
			HTTP: &spec.HTTPProbe{Port: 80, Scheme: "ftp"},
		}, "scheme"},
		{"exec 命令为空", &spec.Health{Exec: &spec.ExecProbe{}}, "command"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.h, nil)
			if err == nil {
				t.Fatal("应当报错")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息应包含 %q，实际: %v", tc.want, err)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	c, err := New(&spec.Health{HTTP: &spec.HTTPProbe{Port: 80}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.StartupGrace != DefaultStartupGrace || c.Interval != DefaultInterval ||
		c.Timeout != DefaultTimeout {
		t.Errorf("默认值不对: %+v", c)
	}
	if c.FailureThreshold != DefaultFailureThreshold || c.SuccessThreshold != DefaultSuccessThreshold {
		t.Errorf("阈值默认值不对: %+v", c)
	}

	// 写坏的时长退回默认值而不是报错——一个拼错的 "15sec" 不该让
	// 整个组件装不上，而是按默认值跑并在别处被 lint 拦住
	c2, err := New(&spec.Health{
		HTTP: &spec.HTTPProbe{Port: 80}, Interval: "15sec", Timeout: "-3s",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Interval != DefaultInterval || c2.Timeout != DefaultTimeout {
		t.Errorf("无法解析的时长应退回默认值: %+v", c2)
	}
}

// TestWaitReadyToleratesSlowStart 钉住「启动宽限期内的失败不计」。
//
// 一个 JVM 组件冷启动要几十秒，期间探测必然失败——那不是故障。
func TestWaitReadyToleratesSlowStart(t *testing.T) {
	var ready atomic.Bool
	srv := listenLocal(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	c, err := New(&spec.Health{
		HTTP:         &spec.HTTPProbe{Port: portOf(t, srv)},
		StartupGrace: "5s",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(600 * time.Millisecond)
		ready.Store(true)
	}()

	start := time.Now()
	if err := c.WaitReady(ctx()); err != nil {
		t.Fatalf("宽限期内变健康应当通过: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("等了 %s —— 宽限期内应当探得比稳态更密，好早点发现就绪", elapsed)
	}
}

// TestWaitReadyFailsAfterGrace 钉住宽限期耗尽后报失败，且带上原因。
func TestWaitReadyFailsAfterGrace(t *testing.T) {
	srv := listenLocal(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	c, err := New(&spec.Health{
		HTTP:         &spec.HTTPProbe{Port: portOf(t, srv)},
		StartupGrace: "600ms",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	err = c.WaitReady(ctx())
	if err == nil {
		t.Fatal("一直不健康应当失败")
	}
	// 错误信息要能直接告诉人「探的是什么、失败在哪」
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("应当带上最后一次失败的原因: %v", err)
	}
	if !strings.Contains(err.Error(), "600ms") {
		t.Errorf("应当说明宽限期是多久: %v", err)
	}
}

// TestWaitReadyHonorsSuccessThreshold 钉住连续成功次数。
func TestWaitReadyHonorsSuccessThreshold(t *testing.T) {
	var hits atomic.Int32
	srv := listenLocal(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 一次成功一次失败地交替，永远凑不满连续 2 次
		if hits.Add(1)%2 == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	c, err := New(&spec.Health{
		HTTP:             &spec.HTTPProbe{Port: portOf(t, srv)},
		StartupGrace:     "600ms",
		SuccessThreshold: 2,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WaitReady(ctx()); err == nil {
		t.Error("凑不满连续 2 次成功就不该算就绪")
	}
}

func TestWaitReadyRespectsContext(t *testing.T) {
	srv := listenLocal(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	c, _ := New(&spec.Health{
		HTTP: &spec.HTTPProbe{Port: portOf(t, srv)}, StartupGrace: "60s",
	}, nil)

	cctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := c.WaitReady(cctx); err == nil {
		t.Fatal("ctx 取消后应当返回")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("必须尊重 ctx，而不是死等宽限期")
	}
}

func TestProbeDescriptions(t *testing.T) {
	// 描述会出现在报告与错误信息里，必须一眼看出探的是什么
	cases := []struct {
		h    *spec.Health
		want string
	}{
		{&spec.Health{HTTP: &spec.HTTPProbe{Path: "/healthz", Port: 8080}},
			"http://127.0.0.1:8080/healthz"},
		{&spec.Health{TCP: &spec.TCPProbe{Port: 5432}}, "127.0.0.1:5432"},
		{&spec.Health{Exec: &spec.ExecProbe{Command: []string{"pg_isready"}}}, "pg_isready"},
	}
	for _, tc := range cases {
		c, err := New(tc.h, RunnerExec(command.NewFake()))
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Prober.Describe(); !strings.Contains(got, tc.want) {
			t.Errorf("Describe = %q，应包含 %q", got, tc.want)
		}
	}
}

// TestExecProbeDistinguishesCannotProbe 钉住 ADR-0032 的核心区分。
//
// **命令跑了但失败** 与 **压根没跑起来** 是两回事：前者是探针失败，
// 后者是探不了。混为一谈会让一个刚被停掉的容器看起来像「健康检查
// 连续失败」，而真正的原因是它根本没在跑。
func TestExecProbeDistinguishesCannotProbe(t *testing.T) {
	// ① 命令跑了、退出码非零 → 探针失败（**不是** ErrCannotProbe）
	failing := command.NewFake()
	failing.Default = command.Result{ExitCode: 2, Stderr: "connection refused"}
	c, err := New(&spec.Health{
		Exec: &spec.ExecProbe{Command: []string{"pg_isready"}},
	}, RunnerExec(failing))
	if err != nil {
		t.Fatal(err)
	}
	err = c.Once(ctx())
	if err == nil {
		t.Fatal("非零退出码应当算探针失败")
	}
	if errors.Is(err, ErrCannotProbe) {
		t.Errorf("非零退出码是**探针失败**，不该被归为「探不了」: %v", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("错误信息应带上命令的输出，实际: %v", err)
	}

	// ② 命令压根没跑起来 → ErrCannotProbe
	broken := func(context.Context, []string) (command.Result, error) {
		return command.Result{}, errors.New("容器 xyz 没在运行")
	}
	c, err = New(&spec.Health{
		Exec: &spec.ExecProbe{Command: []string{"pg_isready"}},
	}, broken)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Once(ctx())
	if err == nil {
		t.Fatal("执行不了应当报错")
	}
	if !errors.Is(err, ErrCannotProbe) {
		t.Errorf("执行不了应当是 ErrCannotProbe，实际: %v", err)
	}
	// 原因要透出来，否则现场只知道「探不了」不知道为什么
	if !strings.Contains(err.Error(), "没在运行") {
		t.Errorf("错误信息应带上原因，实际: %v", err)
	}
}

// TestExecProbeUsesProvidedContext 钉住命令走的是 Runtime 给的执行上下文。
//
// 这是 ADR-0032 的全部意义：同一份 Pack 在 systemd 上跑宿主机命令、
// 在 docker 上跑 `docker exec`，探针本身一个字不改。
func TestExecProbeUsesProvidedContext(t *testing.T) {
	var got []string
	spy := func(_ context.Context, cmd []string) (command.Result, error) {
		got = cmd
		return command.Result{}, nil
	}
	c, err := New(&spec.Health{
		Exec: &spec.ExecProbe{Command: []string{"pg_isready", "-p", "5432"}},
	}, spy)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Once(ctx()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "pg_isready -p 5432" {
		t.Errorf("应把整条命令交给执行上下文，实际 %v", got)
	}
}

// TestExecProbeFallsBackToHost 钉住没给上下文时的退回行为。
//
// `mechlet apply -f` 这条调试路径没有 Runtime 在手，此时应当就在本机跑
// ——那正是 systemd 的语义。
func TestExecProbeFallsBackToHost(t *testing.T) {
	c, err := New(&spec.Health{
		Exec: &spec.ExecProbe{Command: []string{"nonexistent-command-xyz"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 本机上没有这个命令 → 执行不了，而不是「探针失败」
	err = c.Once(ctx())
	if err == nil {
		t.Fatal("不存在的命令应当报错")
	}
	if !errors.Is(err, ErrCannotProbe) {
		t.Errorf("命令不存在属于「探不了」，实际: %v", err)
	}
}
