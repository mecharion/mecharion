package command

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// Fake 是测试用的命令替身。
//
// 它放在非 _test.go 文件里，因为资源引擎与 Runtime 两个包的测试都要用它，
// 而 Go 不允许跨包引用测试文件里的类型。
type Fake struct {
	mu sync.Mutex

	// responses 按 "命令 参数..." 全串精确匹配。
	responses map[string]Result
	// prefixes 按前缀匹配，在 responses 未命中时生效。
	prefixes map[string]Result
	// Default 是都没命中时的作答。零值即退出码 0、无输出。
	Default Result
	// NotFound 为 true 时模拟「本机没有这个命令」。
	NotFound bool
	// Streams 是 Stream 的预设输出，按前缀匹配。
	Streams map[string]string

	calls   []string
	optsLog []Opts
}

// NewFake 构造一个替身。
func NewFake() *Fake { return &Fake{} }

// Set 预设一条精确匹配的应答。
func (f *Fake) Set(cmdline string, r Result) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.responses == nil {
		f.responses = map[string]Result{}
	}
	f.responses[cmdline] = r
	return f
}

// SetPrefix 预设一条前缀匹配的应答。
func (f *Fake) SetPrefix(prefix string, r Result) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prefixes == nil {
		f.prefixes = map[string]Result{}
	}
	f.prefixes[prefix] = r
	return f
}

// SetStream 预设 Stream 的输出。
func (f *Fake) SetStream(prefix, out string) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Streams == nil {
		f.Streams = map[string]string{}
	}
	f.Streams[prefix] = out
	return f
}

// Run 按预设作答并记录调用。
func (f *Fake) Run(_ context.Context, name string, args ...string) (Result, error) {
	key := strings.Join(append([]string{name}, args...), " ")

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, key)

	if f.NotFound {
		return Result{}, exec.ErrNotFound
	}
	if r, ok := f.responses[key]; ok {
		return r, nil
	}
	// 前缀匹配取最长的一条，避免 "systemctl show" 抢走 "systemctl show -p X"
	best, found := "", Result{}
	for p, r := range f.prefixes {
		if strings.HasPrefix(key, p) && len(p) > len(best) {
			best, found = p, r
		}
	}
	if best != "" {
		return found, nil
	}
	return f.Default, nil
}

// RunWith 按预设作答，并记下这次执行用的设置。
//
// 记下 Opts 是刻意的：hook 的正确性有一半在**环境**上（敏感值有没有
// 走文件、身份对不对、cwd 是不是 generation 目录），而这些从命令行上
// 完全看不出来。不留档的话测试只能验证「跑了」，验证不了「怎么跑的」。
func (f *Fake) RunWith(
	ctx context.Context, o Opts, name string, args ...string,
) (Result, error) {
	f.mu.Lock()
	f.optsLog = append(f.optsLog, o)
	f.mu.Unlock()
	return f.Run(ctx, name, args...)
}

// LastOpts 返回最近一次 RunWith 用的设置；没有则返回零值与 false。
func (f *Fake) LastOpts() (Opts, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.optsLog) == 0 {
		return Opts{}, false
	}
	return f.optsLog[len(f.optsLog)-1], true
}

// AllOpts 返回每一次 RunWith 用的设置。
func (f *Fake) AllOpts() []Opts {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Opts(nil), f.optsLog...)
}

// Stream 按预设返回一段输出。
func (f *Fake) Stream(_ context.Context, name string, args ...string) (io.ReadCloser, error) {
	key := strings.Join(append([]string{name}, args...), " ")

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, key)

	if f.NotFound {
		return nil, exec.ErrNotFound
	}
	for p, out := range f.Streams {
		if strings.HasPrefix(key, p) {
			return io.NopCloser(strings.NewReader(out)), nil
		}
	}
	return io.NopCloser(strings.NewReader("")), nil
}

// Calls 返回至今执行过的全部命令行。
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Reset 清空调用记录，保留预设。
func (f *Fake) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// Ran 报告是否执行过以 prefix 开头的命令。
func (f *Fake) Ran(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// CountRan 统计执行过多少次以 prefix 开头的命令。
func (f *Fake) CountRan(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}
