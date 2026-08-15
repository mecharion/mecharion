// Package systemd 实现 systemd Runtime——v1 的主路径。
//
// generation 是一个目录，unit 文件写入 /etc/systemd/system/
// mecharion-<component>-<role>.service。
//
// **unit 名不含 generation 序号**：unit 引用的是稳定的 `current` 软链
// （spec §15.1 里 Pack 写的是 {{ .Paths.Current }}），因此换 generation
// 时切软链 + 重启即可，不需要换一个 unit。若 unit 名随 generation 变，
// 每次升级都会在系统里留下一串废弃 unit。
package systemd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mecharion/mecharion/internal/command"
	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/runtime"
)

// Name 是本 Runtime 的名字。
const Name = "systemd"

// UnitDir 是 unit 文件的安装位置。
//
// 用 /etc/systemd/system 而非 /usr/lib/systemd/system：前者是本地管理员
// 的地盘，优先级更高，也不会在系统包升级时被覆盖。
const UnitDir = "/etc/systemd/system"

// MinVersion 是承诺支持的 systemd 最低主版本。
//
// 239 对应 RHEL 8 / Debian 10 / Ubuntu 20.04——当下仍在受支持的发行版
// 里最低的那一档（docs/design/25-roadmap.md「支持的 Linux」）。
//
// 更低的版本**多半也能用**（本包只依赖 v220 就有的子命令），但没有 CI
// 覆盖，因此不承诺。Probe 会明确拒绝并把版本号告诉用户，而不是让他在
// 某个用不了的属性上撞一鼻子灰。
const MinVersion = 239

// Runtime 是 systemd 实现。
type Runtime struct {
	// UnitDir 可覆盖，供测试使用。
	UnitDir string
	// Runner 执行 systemctl / journalctl。
	Runner command.Runner
}

// New 构造一个使用真实命令的 systemd Runtime。
func New() *Runtime {
	return &Runtime{UnitDir: UnitDir, Runner: command.Exec{}}
}

// Name 返回 "systemd"。
func (r *Runtime) Name() string { return Name }

func (r *Runtime) runner() command.Runner {
	if r.Runner == nil {
		return command.Exec{}
	}
	return r.Runner
}

func (r *Runtime) unitDir() string {
	if r.UnitDir == "" {
		return UnitDir
	}
	return r.UnitDir
}

func (r *Runtime) unitPath(unit string) string {
	return filepath.Join(r.unitDir(), unit)
}

// ── Probe ───────────────────────────────────────────────────────────────

// Probe 检查本机是否跑着 systemd。
//
// 判据是 `systemctl is-system-running` 能作答，而不是「systemctl 这个
// 命令存在」——容器里常装着 systemd 的包却没有 PID 1，那时任何
// systemctl 操作都会以「System has not been booted with systemd」失败。
func (r *Runtime) Probe(ctx context.Context) (runtime.Capability, error) {
	ver, err := r.runner().Run(ctx, "systemctl", "--version")
	if err != nil {
		if command.IsNotFound(err) {
			return runtime.Capability{
				Available: false,
				Reason:    "本机没有 systemctl 命令",
			}, nil
		}
		return runtime.Capability{}, faults.Wrap(faults.Transient, "探测 systemd", err)
	}

	version := parseVersion(ver.Stdout)
	if n, ok := majorVersion(version); ok && n < MinVersion {
		return runtime.Capability{
			Available: false,
			Version:   version,
			Reason: fmt.Sprintf("systemd %s 低于支持下限 %d —— "+
				"请升级发行版，或改用其它 runtime", version, MinVersion),
			Detail: map[string]string{"minVersion": strconv.Itoa(MinVersion)},
		}, nil
	}

	state, err := r.runner().Run(ctx, "systemctl", "is-system-running")
	if err != nil {
		return runtime.Capability{}, faults.Wrap(faults.Transient, "探测 systemd", err)
	}
	// is-system-running 在 degraded（有 unit 失败）时退出码非 0，
	// 但 systemd 本身是可用的——只看输出，不看退出码。
	sys := strings.TrimSpace(state.Stdout)
	switch sys {
	case "running", "degraded", "starting", "maintenance", "stopping":
		return runtime.Capability{
			Available: true,
			Version:   version,
			Detail:    map[string]string{"systemState": sys},
		}, nil
	default:
		reason := "systemd 未作为 init 运行"
		if m := state.Message(); m != "" {
			reason += "：" + m
		} else if sys != "" {
			reason += "（is-system-running = " + sys + "）"
		}
		return runtime.Capability{
			Available: false,
			Version:   version,
			Reason:    reason,
			Detail:    map[string]string{"systemState": sys},
		}, nil
	}
}

// parseVersion 从 `systemctl --version` 的首行取版本号。
// 输出形如 "systemd 252 (252.22-1~deb12u1)"。
func parseVersion(out string) string {
	line, _, _ := strings.Cut(out, "\n")
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		return fields[1]
	}
	return strings.TrimSpace(line)
}

// majorVersion 取版本号的主版本。
//
// 发行版会加各种后缀（"252.22-1~deb12u1"、"239-78.el8"、"250~rc1"），
// 因此只取前导数字；取不出来时返回 ok=false，此时**不做拦截**——
// 因为一个认不出的版本号更可能是我们的解析没跟上，而不是系统太旧。
func majorVersion(v string) (int, bool) {
	i := 0
	for i < len(v) && v[i] >= '0' && v[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(v[:i])
	if err != nil {
		return 0, false
	}
	return n, true
}

// RefFor 从工作负载推出 unit 名，不碰机器。见 runtime.Runtime.RefFor。
func (r *Runtime) RefFor(w runtime.WorkloadSpec) (runtime.Ref, error) {
	if err := w.Validate(); err != nil {
		return runtime.Ref{}, faults.Wrap(faults.Permanent, "构造工作负载引用", err)
	}
	return runtime.Ref{
		Runtime: Name, Component: w.Component, Role: w.Role,
		Generation: w.Generation, Native: UnitName(w.Component, w.Role),
	}, nil
}

// ── Materialize ─────────────────────────────────────────────────────────

// Materialize 写 unit 文件、daemon-reload、enable，但**不启动**。
//
// 幂等：内容没变时既不重写也不 daemon-reload。daemon-reload 会让 systemd
// 重新解析全系统的 unit，在装了几百个 unit 的机器上并不便宜，而调和
// 每 60 秒就会走一次这里。
func (r *Runtime) Materialize(ctx context.Context, w runtime.WorkloadSpec) (runtime.Ref, error) {
	if err := w.Validate(); err != nil {
		return runtime.Ref{}, faults.Wrap(faults.Permanent, "物化工作负载", err)
	}
	unit := UnitName(w.Component, w.Role)
	ref := runtime.Ref{
		Runtime: Name, Component: w.Component, Role: w.Role,
		Generation: w.Generation, Native: unit,
	}

	content, err := renderUnit(w)
	if err != nil {
		return runtime.Ref{}, err
	}

	changed, err := r.writeUnit(unit, content)
	if err != nil {
		return runtime.Ref{}, err
	}
	if changed {
		if err := r.daemonReload(ctx); err != nil {
			return runtime.Ref{}, err
		}
	}

	// enable 让组件在重启后自己回来。它只是建一个软链，重复执行无害；
	// 但已经 enable 时跳过可以少 fork 一次。
	enabled, err := r.isEnabled(ctx, unit)
	if err != nil {
		return runtime.Ref{}, err
	}
	if !enabled {
		if err := command.MustRun(ctx, r.runner(), "启用 unit",
			"systemctl", "enable", unit); err != nil {
			return runtime.Ref{}, err
		}
	}
	return ref, nil
}

// writeUnit 原子写 unit 文件，返回内容是否发生了变化。
func (r *Runtime) writeUnit(unit, content string) (bool, error) {
	path := r.unitPath(unit)
	if old, err := os.ReadFile(path); err == nil && string(old) == content {
		return false, nil
	}

	if err := os.MkdirAll(r.unitDir(), 0o755); err != nil {
		return false, faults.Wrap(faults.Transient, "创建 unit 目录", err)
	}
	tmp, err := os.CreateTemp(r.unitDir(), "."+unit+".*")
	if err != nil {
		return false, faults.Wrap(faults.Transient, "创建临时 unit", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return false, faults.Wrap(faults.Transient, "写 unit", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, faults.Wrap(faults.Transient, "刷盘", err)
	}
	if err := tmp.Close(); err != nil {
		return false, faults.Wrap(faults.Transient, "写 unit", err)
	}
	// unit 文件对所有人可读是 systemd 的惯例，里面不该有秘密——
	// 密码走 EnvironmentFile，那个文件由 template 资源按 0640 落盘。
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return false, faults.Wrap(faults.Transient, "设置 unit 权限", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, faults.Wrap(faults.Transient, "替换 unit", err)
	}
	return true, nil
}

func (r *Runtime) daemonReload(ctx context.Context) error {
	return command.MustRun(ctx, r.runner(), "重载 systemd 配置",
		"systemctl", "daemon-reload")
}

// isEnabled 查 unit 是否已 enable。
func (r *Runtime) isEnabled(ctx context.Context, unit string) (bool, error) {
	res, err := r.runner().Run(ctx, "systemctl", "is-enabled", unit)
	if err != nil {
		return false, faults.Wrap(faults.Transient, "查询 unit 是否启用", err)
	}
	// 退出码非 0 表示 disabled / static / 不存在，都当作「需要 enable」
	return strings.TrimSpace(res.Stdout) == "enabled", nil
}

// ── 生命周期 ────────────────────────────────────────────────────────────

// Start 启动 unit。
func (r *Runtime) Start(ctx context.Context, ref runtime.Ref) error {
	if err := command.MustRun(ctx, r.runner(), "启动 "+ref.Native,
		"systemctl", "start", ref.Native); err != nil {
		return r.explainStartFailure(ctx, ref, err)
	}
	return nil
}

// explainStartFailure 在启动失败时把最近几行日志附进错误信息。
//
// `systemctl start` 失败只说「Job failed. See 'journalctl -xe'」，
// 而真正的原因（端口占用、配置语法错误）在日志里。让人再去敲一条命令
// 才能看到根因，是最常见的排障摩擦（原则七 现场可诊断）。
func (r *Runtime) explainStartFailure(ctx context.Context, ref runtime.Ref, cause error) error {
	tail := r.recentLogs(ctx, ref, 15)
	if tail == "" {
		return cause
	}
	return faults.Permanentf("启动 "+ref.Native, "%v\n  最近日志:\n%s", cause, indent(tail))
}

// recentLogs 尽力取最近几行日志；取不到就返回空串，不让它影响主错误。
func (r *Runtime) recentLogs(ctx context.Context, ref runtime.Ref, n int) string {
	res, err := r.runner().Run(ctx, "journalctl", "-u", ref.Native,
		"--no-pager", "-n", strconv.Itoa(n))
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return strings.TrimRight(res.Stdout, "\n")
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}

// Stop 停止 unit。
//
// 注意一个真机行为：**忽略 SIGTERM 的服务停完之后是 failed 而不是
// stopped**。systemd 在 TimeoutStopSec 到期后 SIGKILL，并把 unit 标成
// failed（Result=timeout）。
//
// 我们刻意不去 reset-failed 把它抹平——对 PostgreSQL 这类有状态服务，
// 「上次是被 KILL 掉的」意味着下次启动要走崩溃恢复，运维必须知道。
// 这不妨碍下一次 Start。
func (r *Runtime) Stop(ctx context.Context, ref runtime.Ref, opts runtime.StopOpts) error {
	args := []string{"stop"}
	if opts.Timeout > 0 {
		// --job-mode 不管超时，systemd 没有 per-call 的停止超时；
		// 用 --wait 让 systemctl 等作业真正结束，超时交给 ctx。
		args = append(args, "--wait")
	}
	args = append(args, ref.Native)

	stopCtx := ctx
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		stopCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	err := command.MustRun(stopCtx, r.runner(), "停止 "+ref.Native, "systemctl", args...)
	if err == nil {
		return nil
	}
	if !opts.Force {
		return err
	}

	// Force：停不下来就强杀。用一个独立的 ctx，因为上面的很可能已经超时了。
	killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if kerr := command.MustRun(killCtx, r.runner(), "强制停止 "+ref.Native,
		"systemctl", "kill", "--signal=SIGKILL", ref.Native); kerr != nil {
		return faults.Permanentf("强制停止 "+ref.Native,
			"正常停止失败（%v），强杀也失败（%v）", err, kerr)
	}
	return nil
}

// Reload 触发热加载。
func (r *Runtime) Reload(ctx context.Context, ref runtime.Ref) error {
	// 没声明 ExecReload 时 systemd 会以「Job type reload is not applicable」
	// 失败。先问一句，好让调用方拿到可判定的 ErrReloadUnsupported 而不是
	// 一条要靠字符串匹配才能识别的错误。
	res, err := r.runner().Run(ctx, "systemctl", "show",
		"--property=CanReload", "--value", ref.Native)
	if err != nil {
		return faults.Wrap(faults.Transient, "查询 unit 是否支持 reload", err)
	}
	// `--value` 是 systemd v230 才有的；更老的版本会原样输出
	// "CanReload=no"。两种形态都认，免得为一个 flag 抬高版本下限。
	if propValue(res.Stdout, "CanReload") == "no" {
		return runtime.ErrReloadUnsupported
	}
	return command.MustRun(ctx, r.runner(), "重载 "+ref.Native,
		"systemctl", "reload", ref.Native)
}

// Remove 停止、禁用并删除 unit。
//
// **不删除数据与配置**——那是资源引擎与 purge 的事。
func (r *Runtime) Remove(ctx context.Context, ref runtime.Ref) error {
	// disable --now 一步完成停止与禁用；unit 已经不存在时它会报错，
	// 而那正是我们想要的终态，因此忽略退出码。
	if _, err := r.runner().Run(ctx, "systemctl", "disable", "--now", ref.Native); err != nil {
		return faults.Wrap(faults.Transient, "禁用 "+ref.Native, err)
	}
	if err := os.Remove(r.unitPath(ref.Native)); err != nil && !os.IsNotExist(err) {
		return faults.Wrap(faults.Transient, "删除 unit 文件", err)
	}
	if err := r.daemonReload(ctx); err != nil {
		return err
	}
	// 清掉 failed 状态，否则 `systemctl --failed` 会一直挂着一个
	// 已经不存在的 unit，干扰运维判断整机健康。
	_, _ = r.runner().Run(ctx, "systemctl", "reset-failed", ref.Native)
	return nil
}

// ── Observe ─────────────────────────────────────────────────────────────

// observedProperties 是一次 `systemctl show` 要取的属性。
//
// 一次取全而不是问五遍：Observe 每 15 秒跑一次，每个角色实例一次，
// 一台装了十几个组件的机器上 fork 的次数很可观。
var observedProperties = []string{
	"LoadState", "ActiveState", "SubState", "UnitFileState",
	"NRestarts", "ExecMainStatus", "ExecMainCode",
	"ActiveEnterTimestamp", "StateChangeTimestamp",
	"Result", "MainPID", "FragmentPath",
}

// Observe 返回归一化的运行状态。
func (r *Runtime) Observe(ctx context.Context, ref runtime.Ref) (runtime.Status, error) {
	res, err := r.runner().Run(ctx, "systemctl", "show",
		"--property="+strings.Join(observedProperties, ","), ref.Native)
	if err != nil {
		if command.IsNotFound(err) {
			return runtime.Status{}, faults.Permanentf("观测 "+ref.Native,
				"本机没有 systemctl 命令")
		}
		return runtime.Status{}, faults.Wrap(faults.Transient, "观测 "+ref.Native, err)
	}
	if res.ExitCode != 0 {
		// systemd 没跑起来（容器无 init）时 show 会失败。这是「本环境
		// 不适用」，应当中止调和，不是一个可以糊弄过去的状态。
		return runtime.Status{}, faults.Permanentf("观测 "+ref.Native,
			"systemctl show 退出码 %d: %s", res.ExitCode, res.Message())
	}

	props := parseProperties(res.Stdout)
	return statusFrom(ref, props), nil
}

// propValue 从 `systemctl show` 的输出里取一个属性值，兼容加不加
// `--value` 两种形态。
func propValue(out, key string) string {
	v := strings.TrimSpace(out)
	if after, ok := strings.CutPrefix(v, key+"="); ok {
		return strings.TrimSpace(after)
	}
	return v
}

// parseProperties 解析 `systemctl show` 的 Key=Value 输出。
func parseProperties(out string) map[string]string {
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !ok {
			continue
		}
		props[k] = v
	}
	return props
}

// statusFrom 把 systemd 的属性映射成归一化状态。
func statusFrom(ref runtime.Ref, p map[string]string) runtime.Status {
	st := runtime.Status{
		Native: ref.Native,
		Health: runtime.HealthNone, // systemd 对普通 unit 没有原生健康概念
		Raw:    p,
	}

	if n, err := strconv.Atoi(p["NRestarts"]); err == nil {
		st.Restarts = n
	}
	st.Since = parseTimestamp(p["StateChangeTimestamp"])
	if st.Since.IsZero() {
		st.Since = parseTimestamp(p["ActiveEnterTimestamp"])
	}

	active, sub := p["ActiveState"], p["SubState"]

	// LoadState=not-found 表示 unit 文件不在——没物化过。
	if p["LoadState"] == "not-found" || p["LoadState"] == "" {
		st.State = runtime.StateAbsent
		return st
	}

	switch active {
	case "failed":
		st.State = runtime.StateFailed
		if code, err := strconv.Atoi(p["ExecMainStatus"]); err == nil {
			st.ExitCode = &code
		}
	case "activating":
		// auto-restart 也落在这里：崩了正在被 Restart= 拉起来。
		st.State = runtime.StateStarting
	case "deactivating":
		// 正在停。归到 Stopped 而非 Running——它已经不再对外服务了。
		st.State = runtime.StateStopped
	case "reloading":
		st.State = runtime.StateRunning
	case "active":
		switch sub {
		case "running":
			st.State = runtime.StateRunning
		default:
			// active 但子状态不是 running（exited / dead / …）。
			// 不好断言是好是坏——Type=oneshot 的 exited 是成功，
			// Type=simple 的 exited 多半是问题。交给人看。
			st.State = runtime.StateDegraded
		}
	case "inactive":
		st.State = runtime.StateStopped
		if p["Result"] != "" && p["Result"] != "success" {
			st.State = runtime.StateFailed
			if code, err := strconv.Atoi(p["ExecMainStatus"]); err == nil {
				st.ExitCode = &code
			}
		}
	default:
		st.State = runtime.StateDegraded
	}
	return st
}

// parseTimestamp 解析 systemd 的时间戳。
//
// 形如 "Sun 2026-08-03 10:05:00 UTC"。解析不出来就返回零值——时间戳
// 只用于展示，不该因为一个本地化格式让整次观测失败。
func parseTimestamp(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" || v == "n/a" {
		return time.Time{}
	}
	for _, layout := range []string{
		"Mon 2006-01-02 15:04:05 MST",
		"Mon 2006-01-02 15:04:05",
		"2006-01-02 15:04:05 MST",
	} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ── Logs ────────────────────────────────────────────────────────────────

// Logs 从 journald 取日志。
func (r *Runtime) Logs(ctx context.Context, ref runtime.Ref, opts runtime.LogOpts) (io.ReadCloser, error) {
	args := []string{"-u", ref.Native, "--no-pager"}
	if opts.Tail > 0 {
		args = append(args, "-n", strconv.Itoa(opts.Tail))
	}
	if !opts.Since.IsZero() {
		args = append(args, "--since", opts.Since.Format("2006-01-02 15:04:05"))
	}
	if opts.Follow {
		args = append(args, "-f")
	}

	rc, err := r.runner().Stream(ctx, "journalctl", args...)
	if err != nil {
		if command.IsNotFound(err) {
			return nil, faults.Permanentf("取日志", "本机没有 journalctl 命令")
		}
		return nil, faults.Wrap(faults.Transient, "取日志", err)
	}
	return rc, nil
}

// RefFor 由组件与角色名重建 Ref。
//
// 调和器在重启后不必重新 Materialize 就能 Observe / Stop——unit 名是
// 由组件与角色确定的纯函数，不依赖任何本地状态。
func RefFor(component, role string, generation int) runtime.Ref {
	return runtime.Ref{
		Runtime: Name, Component: component, Role: role,
		Generation: generation, Native: UnitName(component, role),
	}
}

// 编译期断言：*Runtime 实现了接口。
var _ runtime.Runtime = (*Runtime)(nil)

// ExecIn 在宿主机上执行一条命令。
//
// **systemd 工作负载的上下文就是这台机器**：它管的是宿主机上的一个进程，
// 命令与那个进程看到的是同一个文件系统、同一套已安装的二进制。
//
// 因此这里不用 `systemd-run` 之类把命令塞进 unit 的 cgroup——那会带来
// 一堆与目的无关的复杂度（transient unit 的生命周期、退出码回传），
// 而探针要的只是「在这台机器上跑一下 pg_isready」。
//
// 与 docker 实现的差异正是 ADR-0032 存在的理由：那边这条命令必须进容器。
func (r *Runtime) ExecIn(
	ctx context.Context, ref runtime.Ref, cmd []string,
) (command.Result, error) {
	if len(cmd) == 0 {
		return command.Result{}, faults.Permanentf("执行命令", "命令为空")
	}
	return r.runner().Run(ctx, cmd[0], cmd[1:]...)
}
