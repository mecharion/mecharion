// Package command 封装外部命令的执行。
//
// 资源引擎与 Runtime 都要 fork 外部命令（getent / useradd / systemctl /
// journalctl），且都需要在测试里替身化。抽成一处，让两边共用同一套
// 「退出码不是错误」的约定与同一个替身。
package command

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"

	"github.com/mecharion/mecharion/internal/faults"
)

// Result 是一次外部命令的结果。
//
// **退出码不是 error**：`getent` 的退出码 2 表示「查无此项」，
// `systemctl is-active` 的非零表示「没在跑」——都是正常结果。
// err 只在命令根本没跑起来（不存在、超时、被杀）时非 nil。
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Message 返回可读的失败原因：优先 stderr，为空时退回 stdout。
func (r Result) Message() string {
	if m := strings.TrimSpace(r.Stderr); m != "" {
		return m
	}
	return strings.TrimSpace(r.Stdout)
}

// Runner 执行外部命令。
type Runner interface {
	// Run 执行命令并等待它结束。
	Run(ctx context.Context, name string, args ...string) (Result, error)

	// RunWith 在给定的工作目录、环境与身份下执行命令。
	RunWith(ctx context.Context, o Opts, name string, args ...string) (Result, error)

	// Stream 启动命令并把它的标准输出接出来，用于 journalctl -f 这类
	// 长流。调用方关闭返回的 ReadCloser 即终止命令。
	Stream(ctx context.Context, name string, args ...string) (io.ReadCloser, error)
}

// Exec 是基于 os/exec 的真实实现。
type Exec struct{}

// Run 执行命令并捕获输出。
func (Exec) Run(ctx context.Context, name string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}

	var ee *exec.ExitError
	switch {
	case err == nil:
		return res, nil
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
		return res, nil
	default:
		return res, err
	}
}

// Stream 启动命令并返回它的标准输出。
func (Exec) Stream(ctx context.Context, name string, args ...string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// stderr 混进同一条流：journalctl 的告警（如「找不到该 unit 的日志」）
	// 对看日志的人同样有用，分开只会让它消失。
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &procReader{cmd: cmd, out: out}, nil
}

// procReader 把进程的生命周期绑到 ReadCloser 上。
type procReader struct {
	cmd  *exec.Cmd
	out  io.ReadCloser
	once sync.Once
}

func (p *procReader) Read(b []byte) (int, error) { return p.out.Read(b) }

// Close 关掉管道并回收进程。
//
// 必须 Wait，否则 `journalctl -f` 会变成僵尸进程一直挂着——mechlet 是
// 长驻进程，泄漏会累积。
func (p *procReader) Close() error {
	var err error
	p.once.Do(func() {
		err = p.out.Close()
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		_ = p.cmd.Wait()
	})
	return err
}

// ── 便利函数 ────────────────────────────────────────────────────────────

// MustRun 执行命令并要求退出码为 0，失败时返回已分类的错误。
func MustRun(ctx context.Context, r Runner, op, name string, args ...string) error {
	res, err := r.Run(ctx, name, args...)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return faults.Permanentf(op, "本机没有 %s 命令", name)
		}
		return faults.Wrap(faults.Transient, op, err)
	}
	if res.ExitCode != 0 {
		return faults.Permanentf(op, "%s %s 退出码 %d: %s",
			name, strings.Join(args, " "), res.ExitCode, res.Message())
	}
	return nil
}

// IsNotFound 报告错误是否为「命令不存在」。
func IsNotFound(err error) bool { return errors.Is(err, exec.ErrNotFound) }
