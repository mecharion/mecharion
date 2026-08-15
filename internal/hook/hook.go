// Package hook 执行 Pack 的生命周期脚本。
//
// 设计见 docs/design/18-hooks.md，规范见 spec §16。职责切分是理解本包的
// 前提：
//
//	mechd    决定「这个 hook 这次要不要下发」   ← scope / when 在此求值
//	mechlet  收到就执行                        ← 完全不理解 once 语义
//
// 因此本包**没有 scope / when 的概念**。规格里出现的 hook 就是要跑的。
// 把 once 的仲裁放在唯一有全局视角的地方，mechlet 才永远不需要相互查询。
package hook

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mecharion/mecharion/internal/command"
	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/spec"
)

// 生命周期点（spec §16.2）。
const (
	PreInstall  = "preInstall"
	PostInstall = "postInstall"
	PreUpgrade  = "preUpgrade"
	PostUpgrade = "postUpgrade"
	PreRemove   = "preRemove"
	PostRemove  = "postRemove"
	PreStart    = "preStart"
	PostStart   = "postStart"
	PreStop     = "preStop"
	PostStop    = "postStop"
)

// DefaultTimeout 是未声明 timeout 时的取值（spec §16.3）。
const DefaultTimeout = 300 * time.Second

// DefaultRunDir 是敏感参数临时文件的落点。
//
// 在 /run 之下：那是 tmpfs，重启即失，且不会被任何备份工具扫到。
const DefaultRunDir = "/run/mecharion/hooks"

// Redacted 是脱敏后的替代文本。
const Redacted = "***"

// IDLookup 把用户名换成 uid/gid。
//
// 定义成接口是为了让 hook 不依赖资源引擎：`*resource.Env` 恰好满足它，
// 而查询要走 getent（CGO_ENABLED=0 下 os/user 只读 /etc/passwd，
// 会漏掉 LDAP / SSSD 的用户）。
type IDLookup interface {
	LookupUID(ctx context.Context, name string) (int, error)
	LookupGID(ctx context.Context, name string) (int, error)
}

// Executor 执行 hook。
type Executor struct {
	Runner command.Runner
	Lookup IDLookup
	// RunDir 是敏感参数临时文件的父目录，为空时用 DefaultRunDir。
	RunDir string
	// Now 供测试注入，为空时用 time.Now。
	Now func() time.Time
	// PackRoot 是 Pack 解开后的目录，hooks/ 在它下面。
	PackRoot string
	// GenerationDir 是 hook 的工作目录。
	GenerationDir string
}

// Result 是一次 hook 执行的结果。
type Result struct {
	Point    string
	Script   string
	ExitCode int
	Duration time.Duration
	// Output 是 stdout 与 stderr 的合并，**已按敏感值脱敏**。
	Output string
}

// Run 执行某个生命周期点上的全部 hook，按声明顺序。
//
// **不做重试。** 一个不幂等的 hook 重试一次就可能把事情做坏两遍，而引擎
// 无从判断它是否幂等。重试是运维的显式决定（重新 apply）。
//
// 任何一个失败即返回，后续的不再执行——它们多半依赖前一个的结果。
func (e *Executor) Run(
	ctx context.Context, s *spec.ResolvedSpec, point string,
) ([]Result, error) {
	hooks := hooksAt(s, point)
	if len(hooks) == 0 {
		return nil, nil
	}

	// 敏感值走文件，整个目录在本次调用结束时删掉——无论成败
	secretDir, files, err := e.writeSecretFiles(s)
	if err != nil {
		return nil, err
	}
	if secretDir != "" {
		defer os.RemoveAll(secretDir)
	}

	env := e.buildEnv(s, files)
	out := make([]Result, 0, len(hooks))
	for _, h := range hooks {
		r, err := e.runOne(ctx, s, h, env, secretDir)
		if r != nil {
			out = append(out, *r)
		}
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// hooksAt 挑出某个生命周期点上的 hook。
func hooksAt(s *spec.ResolvedSpec, point string) []spec.Hook {
	var out []spec.Hook
	for _, h := range s.Hooks {
		if h.Point == point {
			out = append(out, h)
		}
	}
	return out
}

// Has 报告规格里是否有某个点的 hook。
func Has(s *spec.ResolvedSpec, point string) bool { return len(hooksAt(s, point)) > 0 }

// runOne 执行一个 hook。
func (e *Executor) runOne(
	ctx context.Context, s *spec.ResolvedSpec, h spec.Hook, env []string, secretDir string,
) (*Result, error) {
	script, err := e.scriptPath(h.Script)
	if err != nil {
		return nil, err
	}

	timeout, err := parseTimeout(h.Timeout)
	if err != nil {
		return nil, faults.Wrap(faults.Permanent, "执行 hook "+h.Script, err)
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// cwd 不存在时 Go 的 fork/exec 报的是「<脚本路径>: no such file or
	// directory」——**它指向脚本，而脚本明明在那儿**。这句话把人引向
	// 完全错误的方向，因此这里自己先说清楚。
	if e.GenerationDir != "" {
		if _, err := os.Stat(e.GenerationDir); err != nil {
			return nil, faults.Permanentf("执行 hook",
				"%s: generation 目录 %s 不可用: %v\n"+
					"  hook 的工作目录就是它（18-hooks §3），"+
					"报错指向脚本是 fork/exec 的措辞，与脚本本身无关",
				h.Script, e.GenerationDir, err)
		}
	}

	opts := command.Opts{
		Dir: e.GenerationDir,
		Env: env,
	}
	if h.User != "" {
		if err := e.resolveUser(ctx, &opts, h.User); err != nil {
			return nil, err
		}
		// 以非 root 身份跑的 hook 读不了 root 拥有的 0600 文件。
		// 放宽权限位是错的解法——那等于让同机任何用户都读得到。
		if secretDir != "" {
			if err := ChownSecrets(secretDir, opts.UID, opts.GID); err != nil {
				return nil, faults.Wrap(faults.Transient,
					"把密钥文件交给 "+h.User, err)
			}
		}
	}

	started := e.now()
	res, err := e.Runner.RunWith(runCtx, opts, script, h.Args...)
	elapsed := e.now().Sub(started)

	// 输出在任何一条返回路径上都要脱敏：失败路径恰恰是最可能把口令
	// 打进日志与事件流的地方
	redact := redactor(s)
	out := &Result{
		Point: h.Point, Script: h.Script,
		ExitCode: res.ExitCode, Duration: elapsed,
		Output: redact(strings.TrimSpace(res.Stdout + res.Stderr)),
	}

	// 超时要**先于**退出码判断。被 SIGKILL 掉的进程报的是退出码 -1，
	// 走通用分支只会得到一句「退出码 -1」——那既没说清发生了什么，
	// 也没告诉用户该怎么办。
	if runCtx.Err() == context.DeadlineExceeded {
		return out, faults.Permanentf("执行 hook",
			"%s 超时（%s）——如果它本就需要更久，请在 Pack 里调大 timeout",
			h.Script, timeout)
	}
	if err != nil {
		return out, faults.Wrap(faults.Permanent, "执行 hook "+h.Script, redactErr(err, redact))
	}
	if res.ExitCode != 0 {
		return out, faults.Permanentf("执行 hook",
			"%s 退出码 %d\n%s", h.Script, res.ExitCode, indent(out.Output))
	}
	return out, nil
}

// scriptPath 定位脚本并确认它可执行。
func (e *Executor) scriptPath(script string) (string, error) {
	if e.PackRoot == "" {
		return "", faults.Permanentf("执行 hook",
			"%s: 没有 Pack 目录，无法定位脚本", script)
	}
	p := filepath.Join(e.PackRoot, filepath.FromSlash(script))
	st, err := os.Stat(p)
	if err != nil {
		return "", faults.Permanentf("执行 hook", "%s: %v", script, err)
	}
	if st.IsDir() {
		return "", faults.Permanentf("执行 hook", "%s 是一个目录", script)
	}
	return p, nil
}

// resolveUser 把用户名换成 uid/gid。
func (e *Executor) resolveUser(ctx context.Context, o *command.Opts, name string) error {
	if e.Lookup == nil {
		return faults.Permanentf("执行 hook",
			"hook 声明了 user: %s，但没有可用的身份查询", name)
	}
	uid, err := e.Lookup.LookupUID(ctx, name)
	if err != nil {
		return faults.Permanentf("执行 hook",
			"查找用户 %s: %v —— 该用户通常由同一个 Pack 的 user 资源创建，"+
				"请确认它排在 hook 之前", name, err)
	}
	gid, err := e.Lookup.LookupGID(ctx, name)
	if err != nil {
		// 同名组不存在时退回用户的主组语义：用 uid 当 gid 是 useradd
		// 的默认行为，比直接失败更贴近实际
		gid = uid
	}
	o.User, o.UID, o.GID = name, uid, gid
	return nil
}

func (e *Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func parseTimeout(s string) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return DefaultTimeout, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("timeout %q 不是合法 duration（如 300s / 10m）", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("timeout %q 必须为正", s)
	}
	return d, nil
}

func indent(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// ── 脱敏 ────────────────────────────────────────────────────────────────

// redactor 返回一个把全部敏感值换成 *** 的函数。
//
// hook 的 stdout/stderr 进事件流。一个 `set -x` 的脚本会把口令原样打出来，
// 而那正是最常见的泄漏途径——比配置文件本身常见得多。
func redactor(s *spec.ResolvedSpec) func(string) string {
	values := s.SecretParams()
	if len(values) == 0 {
		return func(x string) string { return x }
	}
	// 长的先替换：短值可能是长值的子串，先换短的会在长值里留下残片
	list := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			list = append(list, v)
		}
	}
	sort.Slice(list, func(i, j int) bool { return len(list[i]) > len(list[j]) })

	return func(x string) string {
		for _, v := range list {
			x = strings.ReplaceAll(x, v, Redacted)
		}
		return x
	}
}

func redactErr(err error, redact func(string) string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s", redact(err.Error()))
}
