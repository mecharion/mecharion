// Package health 实现健康探针。
//
// 探针的**编排**跑在 Runtime 接口之上（docs/design/05-runtime.md §3 规则①）：
// 重试、阈值、超时、结果归一化在任何 Runtime 下完全一致，写一次即可。
// Runtime 原生的健康信息（docker HEALTHCHECK、systemd watchdog）经 Observe
// 汇入，不在这里重复建立机制。
//
// **只有一处例外**：`exec` 探针要问「在哪跑」。裸机上 pg_isready 在宿主机上，
// 容器化之后它只存在于镜像里。那一步由 Runtime 通过 ExecContext 提供
// （ADR-0032）；http 与 tcp 打的是已发布端口，宿主机上照常可达，不受影响。
//
// v1 只有这三种探针。「健康」与「就绪」**不做区分**——单机与小集群场景下
// 二者几乎总是同一件事，分开只会让 Pack 作者面对一个他答不上来的问题。
// 真有需要时再加，那时会有具体案例告诉我们该怎么分。
package health

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mecharion/mecharion/internal/command"
	"github.com/mecharion/mecharion/internal/spec"
)

// 默认值（spec §15.4）。
const (
	DefaultStartupGrace     = 60 * time.Second
	DefaultInterval         = 15 * time.Second
	DefaultTimeout          = 5 * time.Second
	DefaultFailureThreshold = 3
	DefaultSuccessThreshold = 1
)

// Prober 执行一次探测。返回 nil 表示这一次通过。
type Prober interface {
	// Describe 返回可读的探针描述，出现在状态输出里。
	Describe() string
	Probe(ctx context.Context) error
}

// Checker 按 Pack 声明的参数反复探测。
type Checker struct {
	Prober           Prober
	StartupGrace     time.Duration
	Interval         time.Duration
	Timeout          time.Duration
	FailureThreshold int
	SuccessThreshold int
}

// New 由已解析的 health 声明构造 Checker。
//
// h 为 nil 时返回 (nil, nil)——**没声明健康检查不是错误**。很多组件
// 没有可探测的端点，强制它们编一个只会得到一个永远通过的假探针。
//
// exec 只被 exec 探针用到，为 nil 时退回「在本机执行」。
func New(h *spec.Health, exec ExecContext) (*Checker, error) {
	if h == nil {
		return nil, nil
	}

	var p Prober
	n := 0
	if h.HTTP != nil {
		n++
		hp, err := newHTTP(h.HTTP)
		if err != nil {
			return nil, err
		}
		p = hp
	}
	if h.TCP != nil {
		n++
		if h.TCP.Port <= 0 || h.TCP.Port > 65535 {
			return nil, fmt.Errorf("health.tcp.port 非法: %d", h.TCP.Port)
		}
		p = &tcpProber{port: h.TCP.Port}
	}
	if h.Exec != nil {
		n++
		if len(h.Exec.Command) == 0 {
			return nil, fmt.Errorf("health.exec.command 为空")
		}
		p = &execProber{cmd: h.Exec.Command, exec: exec}
	}
	if n != 1 {
		return nil, fmt.Errorf("health 必须且只能声明一种探针，实际 %d 种", n)
	}

	c := &Checker{
		Prober:           p,
		StartupGrace:     parseDuration(h.StartupGrace, DefaultStartupGrace),
		Interval:         parseDuration(h.Interval, DefaultInterval),
		Timeout:          parseDuration(h.Timeout, DefaultTimeout),
		FailureThreshold: h.FailureThreshold,
		SuccessThreshold: h.SuccessThreshold,
	}
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = DefaultFailureThreshold
	}
	if c.SuccessThreshold <= 0 {
		c.SuccessThreshold = DefaultSuccessThreshold
	}
	return c, nil
}

// Once 执行一次探测，带超时。
func (c *Checker) Once(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	return c.Prober.Probe(ctx)
}

// WaitReady 等待服务变健康。
//
// **启动宽限期内的失败不计**（spec §15.4）：一个 JVM 组件冷启动要几十秒，
// 期间探测必然失败，那不是故障。宽限期内只要连续通过 SuccessThreshold 次
// 就算就绪；宽限期耗尽仍未通过，才报失败并带上最后一次的原因。
func (c *Checker) WaitReady(ctx context.Context) error {
	deadline := time.Now().Add(c.StartupGrace)
	var lastErr error
	consecutive := 0

	for {
		err := c.Once(ctx)
		if err == nil {
			consecutive++
			if consecutive >= c.SuccessThreshold {
				return nil
			}
		} else {
			consecutive = 0
			lastErr = err
		}

		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("等待 %s 就绪被中断: %w", c.Prober.Describe(), ctxErr)
		}
		if !time.Now().Before(deadline) {
			if lastErr == nil {
				lastErr = fmt.Errorf("连续通过次数不足 %d", c.SuccessThreshold)
			}
			return fmt.Errorf("%s 在启动宽限期 %s 内未就绪: %w",
				c.Prober.Describe(), c.StartupGrace, lastErr)
		}

		// 宽限期内探测得比稳态更密——冷启动时早几秒发现就绪，
		// 就少几秒的服务中断窗口。
		wait := min(c.Interval, time.Second)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

func parseDuration(v string, fallback time.Duration) time.Duration {
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// ── http ────────────────────────────────────────────────────────────────

type httpProber struct {
	url    string
	expect map[int]bool
	client *http.Client
}

func newHTTP(h *spec.HTTPProbe) (*httpProber, error) {
	if h.Port <= 0 || h.Port > 65535 {
		return nil, fmt.Errorf("health.http.port 非法: %d", h.Port)
	}
	scheme := h.Scheme
	if scheme == "" {
		scheme = "http"
	}
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("health.http.scheme 只能是 http 或 https，实际 %q", scheme)
	}
	path := h.Path
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	expect := map[int]bool{}
	for _, s := range h.ExpectStatus {
		expect[s] = true
	}
	if len(expect) == 0 {
		expect[http.StatusOK] = true
	}

	return &httpProber{
		// 一律探本机：探针跑在服务所在节点的 mechlet 里，走 127.0.0.1
		// 既绕开了防火墙，也避免把「网络通不通」混进「服务活没活」。
		url:    scheme + "://127.0.0.1:" + strconv.Itoa(h.Port) + path,
		expect: expect,
		client: &http.Client{
			// 探测不该跟随跳转：302 到别处然后 200，说不上本服务是健康的
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (p *httpProber) Describe() string { return "HTTP 探针 " + p.url }

func (p *httpProber) Probe(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if !p.expect[resp.StatusCode] {
		return fmt.Errorf("状态码 %d 不在期望之内（%s）", resp.StatusCode, p.expectList())
	}
	return nil
}

func (p *httpProber) expectList() string {
	out := make([]int, 0, len(p.expect))
	for s := range p.expect {
		out = append(out, s)
	}
	// 排序只为让错误信息稳定——map 遍历顺序随机会让同一个故障
	// 每次报出不同的文字
	slices.Sort(out)

	parts := make([]string, len(out))
	for i, s := range out {
		parts[i] = strconv.Itoa(s)
	}
	return strings.Join(parts, ", ")
}

// ── tcp ─────────────────────────────────────────────────────────────────

type tcpProber struct{ port int }

func (p *tcpProber) Describe() string {
	return "TCP 探针 127.0.0.1:" + strconv.Itoa(p.port)
}

func (p *tcpProber) Probe(ctx context.Context) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:"+strconv.Itoa(p.port))
	if err != nil {
		return err
	}
	return conn.Close()
}

// ── exec ────────────────────────────────────────────────────────────────

// ExecContext 是「在工作负载的上下文里执行一条命令」。
//
// 它由 Runtime 提供（`Runtime.ExecIn` 的一个闭包）：systemd 下就在宿主机
// 执行，docker 下 `docker exec` 进容器。**三种探针里只有 exec 需要它**——
// http 与 tcp 打的是已发布端口，宿主机上照常可达。
//
// 定义在这里而不是直接依赖 internal/runtime，是为了不让 health 反向依赖
// Runtime 包（那会形成环：runtime 的实现要用 health 的类型吗？不要，
// 但一个函数类型足以表达这条边界）。
type ExecContext func(ctx context.Context, cmd []string) (command.Result, error)

// RunnerExec 把一个 command.Runner 变成执行上下文：就在本机跑。
//
// 这正是 systemd 的语义——**它的工作负载上下文就是这台机器**。
// 也用于测试注入替身。
func RunnerExec(r command.Runner) ExecContext {
	return func(ctx context.Context, cmd []string) (command.Result, error) {
		return r.Run(ctx, cmd[0], cmd[1:]...)
	}
}

// hostExec 是默认的执行上下文：就在本机跑。
//
// 用于 `mechlet apply -f` 这类没有 Runtime 在手的调试路径，行为与
// systemd 实现一致。
func hostExec(ctx context.Context, cmd []string) (command.Result, error) {
	return command.Exec{}.Run(ctx, cmd[0], cmd[1:]...)
}

// ErrCannotProbe 表示**探不了**，而不是探针失败。
//
// 容器没在跑、`docker exec` 本身报错都属于这一类。两者必须分开：
// 一个刚被停掉的容器如果被记成「健康检查连续失败」，看起来就像服务
// 出了问题，而真正的原因是它根本没在跑（ADR-0032）。
var ErrCannotProbe = errors.New("无法执行探测")

type execProber struct {
	cmd  []string
	exec ExecContext
}

func (p *execProber) Describe() string { return "exec 探针 " + strings.Join(p.cmd, " ") }

func (p *execProber) Probe(ctx context.Context) error {
	run := p.exec
	if run == nil {
		run = hostExec
	}
	res, err := run(ctx, p.cmd)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCannotProbe, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("退出码 %d: %s", res.ExitCode, res.Message())
	}
	return nil
}
