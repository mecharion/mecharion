// Package docker 实现 docker Runtime。
//
// 设计见 docs/design/19-container-runtime.md，决策见 ADR-0011 与 ADR-0031。
//
// 三条贯穿全包的性质：
//
//   - **走 CLI 而非 SDK**。compose 根本没有 API，走 SDK 等于维护两套错误
//     处理；且这样与 systemd runtime 共用同一个 command.Runner 与替身。
//     输出一律用 `--format '{{json .}}'` 取——那是稳定契约，人读的表格不是。
//   - **容器不可变**。配置在创建时固化，因此「配置变了」意味着删掉重建。
//     靠 spec-digest 标签判定要不要重建。
//   - **只操作带标签的容器**。默认情况下这台 dockerd 是用户自己的，上面
//     跑着别人的东西。漏一处就可能删掉与 Mecharion 无关的生产容器。
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mecharion/mecharion/internal/command"
	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/spec"
)

// Name 是本 Runtime 的名字。
const Name = "docker"

// 标签。**全部以反向域名为前缀**，避免与用户或其它工具的标签撞名。
const (
	// LabelManagedBy 是**唯一的**过滤依据：只有带它的容器才会被碰。
	LabelManagedBy  = "dev.mecharion.managed-by"
	LabelSite       = "dev.mecharion.site"
	LabelComponent  = "dev.mecharion.component"
	LabelRole       = "dev.mecharion.role"
	LabelGeneration = "dev.mecharion.generation"
	// LabelSpecDigest 判定「现有容器是不是我要的那个」。
	//
	// 容器不可变，因此这是唯一可靠的判据——比对 env / mount / port 逐项
	// 既繁琐又必然漏（docker 会规范化一部分值，比对不出原样）。
	LabelSpecDigest = "dev.mecharion.spec-digest"
)

// ManagedByValue 是 LabelManagedBy 的取值。
const ManagedByValue = "mecharion"

// DefaultStopTimeout 是 docker stop 的默认宽限。
const DefaultStopTimeout = 30 * time.Second

// Runtime 是 docker 实现。
type Runtime struct {
	// Runner 执行 docker 命令。
	Runner command.Runner
	// Host 覆盖 DOCKER_HOST（socket 位置）。空则用 docker 自己的默认探测。
	Host string
}

// New 构造一个使用真实命令的 docker Runtime。
func New() *Runtime { return &Runtime{Runner: command.Exec{}} }

// Name 返回 "docker"。
func (r *Runtime) Name() string { return Name }

func (r *Runtime) runner() command.Runner {
	if r.Runner == nil {
		return command.Exec{}
	}
	return r.Runner
}

// docker 执行一条 docker 子命令。
//
// **退出码不是 error**：`docker inspect` 对不存在的容器返回 1，那是正常
// 结果而非故障（与 command 包的约定一致）。
func (r *Runtime) docker(ctx context.Context, args ...string) (command.Result, error) {
	if r.Host != "" {
		args = append([]string{"--host", r.Host}, args...)
	}
	return r.runner().Run(ctx, "docker", args...)
}

// ContainerName 返回一个角色实例的容器名。
//
// 与 systemd 的 unit 名同构（mecharion-<component>-<role>.service），
// 排障时三个 Runtime 的现场标识形状一致。
func ContainerName(component, role string) string {
	return "mecharion-" + component + "-" + role
}

// ── Probe ───────────────────────────────────────────────────────────────

// Probe 检查本机能否用 docker。
//
// 判据是 **server 版本能取到**，而不是「docker 这个命令存在」——装了
// 客户端却连不上 daemon 是最常见的情形，那时任何操作都会以
// 「Cannot connect to the Docker daemon」失败。
func (r *Runtime) Probe(ctx context.Context) (runtime.Capability, error) {
	res, err := r.docker(ctx, "version", "--format", "{{json .}}")
	if err != nil {
		if command.IsNotFound(err) {
			return runtime.Capability{
				Available: false,
				Reason: "本机没有 docker 命令 —— " +
					"部署官方 Pack \"docker\" 以离线安装，" +
					"或在 mechlet.yaml 里指定已有的 docker socket",
			}, nil
		}
		return runtime.Capability{}, faults.Wrap(faults.Transient, "探测 docker", err)
	}
	if res.ExitCode != 0 {
		// 客户端在、daemon 不在。把 docker 自己的话原样带出去——
		// 它通常已经说清了是权限问题还是没起来
		return runtime.Capability{
			Available: false,
			Reason:    "docker daemon 不可达: " + firstLine(res.Message()),
		}, nil
	}

	var v struct {
		Client struct{ Version string }
		Server *struct {
			Version string
			Os      string
			Arch    string
		}
	}
	if err := json.Unmarshal([]byte(res.Stdout), &v); err != nil {
		return runtime.Capability{}, faults.Wrap(faults.Permanent,
			"解析 docker version", err)
	}
	if v.Server == nil || v.Server.Version == "" {
		return runtime.Capability{
			Available: false,
			Reason:    "只有 docker 客户端，连不上 daemon",
		}, nil
	}

	cap := runtime.Capability{
		Available: true,
		Version:   v.Server.Version,
		Detail: map[string]string{
			"client": v.Client.Version,
			"os":     v.Server.Os,
			"arch":   v.Server.Arch,
		},
	}
	// compose 是独立的插件，可能没装。它不影响 docker runtime 可用性，
	// 但要报上去——compose runtime 的放置校验要用它
	if cres, err := r.docker(ctx, "compose", "version", "--short"); err == nil &&
		cres.ExitCode == 0 {
		cap.Detail["compose"] = strings.TrimSpace(cres.Stdout)
	}
	return cap, nil
}

// ── Materialize ─────────────────────────────────────────────────────────

// Materialize 把镜像装好、容器创建好，**但不启动**。
//
//	① docker load 镜像（幂等：同一份 tar 反复 load 是无操作）
//	② 看现有容器
//	     不存在             → create
//	     存在且 digest 相同 → 无操作
//
// RefFor 从工作负载推出容器名，不碰机器。见 runtime.Runtime.RefFor。
//
// **不返回 Images**：镜像引用是 `docker load` 的输出，事后无从推算。
// 卸载路径因此拿不到本次的镜像——那由回收按台账处理，不该在这里猜。
func (r *Runtime) RefFor(w runtime.WorkloadSpec) (runtime.Ref, error) {
	if err := w.Validate(); err != nil {
		return runtime.Ref{}, faults.Wrap(faults.Permanent, "构造工作负载引用", err)
	}
	return runtime.Ref{
		Runtime: Name, Component: w.Component, Role: w.Role,
		Generation: w.Generation, Native: ContainerName(w.Component, w.Role),
	}, nil
}

// 存在但 digest 不同 → stop + rm + create   ★ 容器不可变
func (r *Runtime) Materialize(ctx context.Context, w runtime.WorkloadSpec) (runtime.Ref, error) {
	if err := w.Validate(); err != nil {
		return runtime.Ref{}, faults.Wrap(faults.Permanent, "校验工作负载", err)
	}
	d, err := decodeDocker(w.Workload)
	if err != nil {
		return runtime.Ref{}, err
	}

	name := ContainerName(w.Component, w.Role)
	ref := runtime.Ref{
		Runtime: Name, Component: w.Component, Role: w.Role,
		Generation: w.Generation, Native: name,
	}

	image, err := r.ensureImage(ctx, w, d)
	if err != nil {
		return ref, err
	}
	ref.Images = []string{image}

	cur, err := r.inspect(ctx, name)
	if err != nil {
		return ref, err
	}
	want := w.SpecDigest

	switch {
	case cur == nil:
		// 不存在，建一个

	case !isManaged(cur.Config.Labels):
		// **同名但不是我们的**。这是标签纪律最重要的一条：
		// 用户机器上可能已经有一个叫这个名字的容器，删掉它是灾难。
		return ref, faults.Permanentf("物化容器",
			"容器 %s 已存在，但它不带 %s=%s 标签——"+
				"Mecharion 不会碰不是自己创建的容器。\n"+
				"  请确认它的来源；确实该由 Mecharion 接管的话，请先手工删除",
			name, LabelManagedBy, ManagedByValue)

	case want != "" && cur.Config.Labels[LabelSpecDigest] == want:
		// 已经是想要的那个，什么都不用做
		return ref, nil

	default:
		// **容器不可变**：env / mount / port / command 只能靠重建生效
		if err := r.removeContainer(ctx, name); err != nil {
			return ref, err
		}
	}

	if err := r.create(ctx, w, d, image, name); err != nil {
		return ref, err
	}
	return ref, nil
}

// ensureImage 把镜像装进本地镜像库，返回它的引用。
func (r *Runtime) ensureImage(
	ctx context.Context, w runtime.WorkloadSpec, d *dockerArgs,
) (string, error) {
	if d.ImageBlob == "" {
		return "", faults.Permanentf("物化容器",
			"workload.docker.imageBlob 为空——"+
				"镜像必须来自 Pack 的 blob，不从 registry 拉（ADR-0015）")
	}
	path, ok := w.BlobPath(d.ImageBlob)
	if !ok {
		return "", faults.Permanentf("物化容器",
			"找不到载荷 %q 对应的文件——它应当已由 mechlet 取到本地",
			d.ImageBlob)
	}

	res, err := r.docker(ctx, "load", "--input", path)
	if err != nil {
		return "", faults.Wrap(faults.Transient, "加载镜像", err)
	}
	if res.ExitCode != 0 {
		return "", faults.Permanentf("加载镜像",
			"docker load %s 失败: %s", path, firstLine(res.Message()))
	}

	image, err := parseLoadedImage(res.Stdout)
	if err != nil {
		return "", faults.Wrap(faults.Permanent, "加载镜像", err)
	}
	return image, nil
}

// parseLoadedImage 从 docker load 的输出里取出镜像引用。
//
// 两种形态都要认：
//
//	Loaded image: repo:tag        有标签的镜像
//	Loaded image ID: sha256:…     无标签的镜像
//
// 只认第一种会让「用 docker save 一个无标签镜像」的 Pack 静默失败。
func parseLoadedImage(out string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "Loaded image ID: "); ok {
			return strings.TrimSpace(v), nil
		}
		if v, ok := strings.CutPrefix(line, "Loaded image: "); ok {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf(
		"docker load 的输出里没有镜像引用，实际输出:\n%s", strings.TrimSpace(out))
}

// create 建容器（不启动）。
func (r *Runtime) create(
	ctx context.Context, w runtime.WorkloadSpec, d *dockerArgs,
	image, name string,
) error {
	args := []string{"create", "--name", name}

	for _, kv := range labelArgs(w) {
		args = append(args, "--label", kv)
	}
	if d.User != "" {
		args = append(args, "--user", d.User)
	}
	if d.Network != "" {
		args = append(args, "--network", d.Network)
	}
	if d.Restart != "" {
		args = append(args, "--restart", d.Restart)
	}
	for _, k := range sortedKeys(d.Env) {
		args = append(args, "--env", k+"="+d.Env[k])
	}
	for _, m := range d.Mounts {
		args = append(args, "--volume", bindSpec(m))
	}
	for _, p := range d.Ports {
		args = append(args, "--publish", portSpec(p))
	}
	for _, c := range d.CapAdd {
		args = append(args, "--cap-add", c)
	}
	for _, s := range d.SecurityOpt {
		args = append(args, "--security-opt", s)
	}
	for _, k := range sortedKeys(d.Ulimits) {
		args = append(args, "--ulimit",
			fmt.Sprintf("%s=%d:%d", k, d.Ulimits[k], d.Ulimits[k]))
	}

	// entrypoint 与 command 的区分：Pack 的 `command` 覆盖镜像的
	// ENTRYPOINT，`args` 追加在其后。两者都为空时用镜像自带的。
	if len(d.Command) > 0 {
		args = append(args, "--entrypoint", d.Command[0])
	}
	args = append(args, image)
	if len(d.Command) > 1 {
		args = append(args, d.Command[1:]...)
	}
	args = append(args, d.Args...)

	res, err := r.docker(ctx, args...)
	if err != nil {
		return faults.Wrap(faults.Transient, "创建容器", err)
	}
	if res.ExitCode != 0 {
		return faults.Permanentf("创建容器",
			"docker create %s 失败: %s", name, firstLine(res.Message()))
	}
	return nil
}

// labelArgs 构造标签。顺序固定，便于比对与阅读。
func labelArgs(w runtime.WorkloadSpec) []string {
	return []string{
		LabelManagedBy + "=" + ManagedByValue,
		LabelSite + "=" + w.Site,
		LabelComponent + "=" + w.Component,
		LabelRole + "=" + w.Role,
		LabelGeneration + "=" + strconv.Itoa(w.Generation),
		LabelSpecDigest + "=" + w.SpecDigest,
	}
}

func bindSpec(m mountArg) string {
	s := m.From + ":" + m.To
	if m.ReadOnly {
		s += ":ro"
	}
	return s
}

func portSpec(p portArg) string {
	s := p.Host + ":" + strconv.Itoa(p.Container)
	if p.Protocol != "" {
		s += "/" + p.Protocol
	}
	return s
}

// ── Start / Stop / Reload ───────────────────────────────────────────────

// Start 启动容器。
func (r *Runtime) Start(ctx context.Context, ref runtime.Ref) error {
	if err := r.requireManaged(ctx, ref.Native); err != nil {
		return err
	}
	res, err := r.docker(ctx, "start", ref.Native)
	if err != nil {
		return faults.Wrap(faults.Transient, "启动容器", err)
	}
	if res.ExitCode != 0 {
		return faults.Permanentf("启动容器",
			"docker start %s 失败: %s", ref.Native, firstLine(res.Message()))
	}
	return nil
}

// Stop 停止容器。
func (r *Runtime) Stop(ctx context.Context, ref runtime.Ref, opts runtime.StopOpts) error {
	if err := r.requireManaged(ctx, ref.Native); err != nil {
		return err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultStopTimeout
	}
	res, err := r.docker(ctx, "stop",
		"--timeout", strconv.Itoa(int(timeout.Seconds())), ref.Native)
	if err != nil {
		return faults.Wrap(faults.Transient, "停止容器", err)
	}
	if res.ExitCode != 0 {
		return faults.Permanentf("停止容器",
			"docker stop %s 失败: %s", ref.Native, firstLine(res.Message()))
	}
	return nil
}

// Reload 不被支持。
//
// docker 没有原生热加载，而 `docker kill -s HUP` 要求 Pack 声明信号——
// `workload.docker` 里没有这个字段，pack/v1 现在是 draft-stable，不为
// 这类目前用不上的边缘情况新增字段。
//
// 返回 ErrReloadUnsupported 让上层降级为重启：那条路径已经实现并测过
// （systemd 上「声明了 reload 但 unit 不支持」走的是同一条）。
func (r *Runtime) Reload(context.Context, runtime.Ref) error {
	return runtime.ErrReloadUnsupported
}

// ── Observe ─────────────────────────────────────────────────────────────

// Observe 返回归一化的运行状态。
func (r *Runtime) Observe(ctx context.Context, ref runtime.Ref) (runtime.Status, error) {
	c, err := r.inspect(ctx, ref.Native)
	if err != nil {
		return runtime.Status{}, err
	}
	if c == nil {
		return runtime.Status{State: runtime.StateAbsent, Native: ref.Native}, nil
	}
	if !isManaged(c.Config.Labels) {
		// 同名但不是我们的：**当作不存在**，而不是报告它的状态。
		// 报上去会让 UI 显示一个我们既不管理也无权动的容器。
		return runtime.Status{
			State: runtime.StateAbsent, Native: ref.Native,
			Raw: map[string]string{"note": "同名容器不带 Mecharion 标签，已忽略"},
		}, nil
	}
	return statusOf(c, ref.Native), nil
}

// statusOf 把 docker 的状态字段映射到归一化枚举。
func statusOf(c *containerJSON, name string) runtime.Status {
	st := runtime.Status{
		Native:   name,
		Restarts: c.RestartCount,
		Raw: map[string]string{
			"status": c.State.Status,
			"image":  c.Config.Image,
		},
	}
	if t, err := time.Parse(time.RFC3339Nano, c.State.StartedAt); err == nil {
		st.Since = t
	}

	switch c.State.Status {
	case "created":
		st.State = runtime.StateStopped
	case "running":
		st.State = runtime.StateRunning
	case "restarting":
		st.State = runtime.StateStarting
	case "paused":
		// 在跑但处在预期之外的子状态——不好断言是好是坏，交给人看
		st.State = runtime.StateDegraded
	case "exited", "dead":
		st.State = runtime.StateFailed
		code := c.State.ExitCode
		st.ExitCode = &code
		// **退出码 0 且不是重启策略下的退出**仍算 Stopped：
		// 一个被正常停掉的容器不该显示成 failed
		if code == 0 {
			st.State = runtime.StateStopped
		}
	default:
		st.State = runtime.StateDegraded
	}

	// 容器原生 HEALTHCHECK 的结果——**不是** Pack 声明的探针，
	// 那些在 Runtime 之上跑（05-runtime §3 规则①）
	if h := c.State.Health; h != nil {
		st.Raw["health"] = h.Status
		switch h.Status {
		case "healthy":
			st.Health = runtime.HealthPassing
		case "unhealthy":
			st.Health = runtime.HealthFailing
		case "starting":
			// 原生健康检查还在起步期：这时报 Running 会让上层以为已经就绪
			if st.State == runtime.StateRunning {
				st.State = runtime.StateStarting
			}
		}
	}
	if d := c.Config.Labels[LabelSpecDigest]; d != "" {
		st.Raw["spec-digest"] = d
	}
	return st
}

// ── Logs / Remove / ExecIn ──────────────────────────────────────────────

// Logs 取容器日志。
func (r *Runtime) Logs(
	ctx context.Context, ref runtime.Ref, opts runtime.LogOpts,
) (io.ReadCloser, error) {
	args := []string{"logs"}
	if opts.Follow {
		args = append(args, "--follow")
	}
	if opts.Tail > 0 {
		args = append(args, "--tail", strconv.Itoa(opts.Tail))
	}
	if !opts.Since.IsZero() {
		args = append(args, "--since", opts.Since.UTC().Format(time.RFC3339))
	}
	args = append(args, ref.Native)
	if r.Host != "" {
		args = append([]string{"--host", r.Host}, args...)
	}
	return r.runner().Stream(ctx, "docker", args...)
}

// Remove 停掉并删除容器。
//
// **不删镜像**：它是内容寻址的、可能被别的组件共用，而一次误删要靠
// 重新分发几百 MB 来补。镜像回收随 M6 的 generation 回收一起做——
// 那时才知道哪些 generation 已经不可回滚。
func (r *Runtime) Remove(ctx context.Context, ref runtime.Ref) error {
	c, err := r.inspect(ctx, ref.Native)
	if err != nil {
		return err
	}
	if c == nil {
		return nil // 已经没了
	}
	if !isManaged(c.Config.Labels) {
		return faults.Permanentf("移除容器",
			"容器 %s 不带 %s=%s 标签，拒绝删除",
			ref.Native, LabelManagedBy, ManagedByValue)
	}
	return r.removeContainer(ctx, ref.Native)
}

// RemoveImage 从本地镜像库删除一个镜像（runtime.ImageReclaimer）。
//
// **不用 --force**：带 force 会把还有容器在用的镜像也删掉，而「还有容器在
// 用」恰恰说明调用方的引用判据算错了。让它以「image is being used」失败，
// 那条错误信息本身就是诊断。
//
// 两种「已经没了」都当成功：镜像不存在，或者它只是丢了一个 tag
// （同一个镜像 ID 有多个 tag 时 `docker rmi` 只摘 tag）。回收要可以重复
// 执行，终态一致就行。
func (r *Runtime) RemoveImage(ctx context.Context, image string) error {
	if image == "" {
		return nil
	}
	res, err := r.docker(ctx, "image", "rm", image)
	if err != nil {
		return faults.Wrap(faults.Transient, "删除镜像", err)
	}
	if res.ExitCode == 0 {
		return nil
	}
	msg := res.Message()
	if strings.Contains(msg, "No such image") || strings.Contains(msg, "not found") {
		return nil
	}
	return faults.Permanentf("删除镜像",
		"docker image rm %s 失败: %s", image, firstLine(msg))
}

// removeContainer 强制删除一个**已确认是我们的**容器。
//
// 调用方必须先做过标签检查。这个函数本身不检查，是因为它也被
// Materialize 的「重建」路径用——那里刚查过，再查一次是多余的 fork。
func (r *Runtime) removeContainer(ctx context.Context, name string) error {
	res, err := r.docker(ctx, "rm", "--force", "--volumes", name)
	if err != nil {
		return faults.Wrap(faults.Transient, "删除容器", err)
	}
	if res.ExitCode != 0 && !strings.Contains(res.Message(), "No such container") {
		return faults.Permanentf("删除容器",
			"docker rm %s 失败: %s", name, firstLine(res.Message()))
	}
	return nil
}

// ExecIn 在容器里执行一条命令（ADR-0032）。
func (r *Runtime) ExecIn(
	ctx context.Context, ref runtime.Ref, cmd []string,
) (command.Result, error) {
	if len(cmd) == 0 {
		return command.Result{}, faults.Permanentf("执行命令", "命令为空")
	}
	// Ref.Native 为空是**调用方的 bug**（没把 Observe 拿到的原生标识传下来）。
	// 不拦的话 docker 会报一句 "invalid container name or ID: value is empty"，
	// 而它看起来像是探针失败——排查会从容器和探针命令查起，方向全错。
	if ref.Native == "" {
		return command.Result{}, faults.Permanentf("执行命令",
			"%s/%s 的 Ref 没有容器名——调用方应当把 Observe 得到的 Native 传下来",
			ref.Component, ref.Role)
	}
	args := append([]string{"exec", ref.Native}, cmd...)
	res, err := r.docker(ctx, args...)
	if err != nil {
		return command.Result{}, err
	}
	// **容器没在跑时 docker exec 自己失败**，那是「探不了」而不是
	// 「命令失败」。把它变成 error，让调用方能分开处理（ADR-0032）。
	if res.ExitCode != 0 && looksLikeExecUnavailable(res.Message()) {
		return command.Result{}, faults.Permanentf("执行命令",
			"无法进入容器 %s: %s", ref.Native, firstLine(res.Message()))
	}
	return res, nil
}

// looksLikeExecUnavailable 判断失败是不是「进不去」而非「命令失败」。
//
// docker 对这两种情况用的是同一个退出码族，只能靠信息判断。判错的代价
// 不对称：把「命令失败」误判成「进不去」只是少记一次探针失败，
// 反过来则会让一个停掉的容器看起来像服务异常。
func looksLikeExecUnavailable(msg string) bool {
	for _, s := range []string{
		"is not running",
		"No such container",
		"is restarting",
		"Cannot connect to the Docker daemon",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// ── inspect ─────────────────────────────────────────────────────────────

// containerJSON 是 docker inspect 输出里我们用到的部分。
type containerJSON struct {
	Name  string
	State struct {
		Status     string
		ExitCode   int
		StartedAt  string
		FinishedAt string
		Health     *struct{ Status string }
	}
	RestartCount int
	Config       struct {
		Image  string
		Labels map[string]string
	}
}

// inspect 取一个容器的详情；不存在时返回 (nil, nil)。
func (r *Runtime) inspect(ctx context.Context, name string) (*containerJSON, error) {
	res, err := r.docker(ctx, "inspect", "--type", "container", name)
	if err != nil {
		return nil, faults.Wrap(faults.Transient, "查看容器", err)
	}
	if res.ExitCode != 0 {
		// 不存在是正常结果，不是故障
		if strings.Contains(res.Message(), "No such") {
			return nil, nil
		}
		return nil, faults.Permanentf("查看容器",
			"docker inspect %s 失败: %s", name, firstLine(res.Message()))
	}

	var list []containerJSON
	if err := json.Unmarshal([]byte(res.Stdout), &list); err != nil {
		return nil, faults.Wrap(faults.Permanent, "解析 docker inspect", err)
	}
	if len(list) == 0 {
		return nil, nil
	}
	return &list[0], nil
}

// requireManaged 在动一个容器之前确认它是我们的。
//
// **凡是按名字直接操作的调用都要先过这一关**（19-container-runtime §4.3）。
// 容器不存在时放行——让后续的 docker 命令去报「没有这个容器」，
// 那句话比我们编的更准确。
func (r *Runtime) requireManaged(ctx context.Context, name string) error {
	c, err := r.inspect(ctx, name)
	if err != nil || c == nil {
		return err
	}
	if !isManaged(c.Config.Labels) {
		return faults.Permanentf("操作容器",
			"容器 %s 不带 %s=%s 标签——Mecharion 不会碰不是自己创建的容器",
			name, LabelManagedBy, ManagedByValue)
	}
	return nil
}

func isManaged(labels map[string]string) bool {
	return labels[LabelManagedBy] == ManagedByValue
}

// ── 参数解码 ────────────────────────────────────────────────────────────

// dockerArgs 是 workload.docker 解码后的形态。
type dockerArgs struct {
	ImageBlob   string            `json:"imageBlob"`
	Command     []string          `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	User        string            `json:"user"`
	Mounts      []mountArg        `json:"mounts"`
	Ports       []portArg         `json:"ports"`
	Network     string            `json:"network"`
	Restart     string            `json:"restart"`
	CapAdd      []string          `json:"capAdd"`
	SecurityOpt []string          `json:"securityOpt"`
	Ulimits     map[string]int    `json:"ulimits"`
}

type mountArg struct {
	From     string `json:"from"`
	To       string `json:"to"`
	ReadOnly bool   `json:"readOnly"`
}

type portArg struct {
	Host      string `json:"host"`
	Container int    `json:"container"`
	Protocol  string `json:"protocol"`
}

func decodeDocker(w *spec.Workload) (*dockerArgs, error) {
	if len(w.Docker) == 0 {
		return nil, faults.Permanentf("解析工作负载",
			"workload.runtime=docker 但缺少 docker 段")
	}
	var d dockerArgs
	if err := json.Unmarshal(w.Docker, &d); err != nil {
		return nil, faults.Wrap(faults.Permanent, "解析 workload.docker", err)
	}
	return &d, nil
}

// ── 辅助 ────────────────────────────────────────────────────────────────

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// 排序让生成的命令行稳定——否则同一份规格每次产出不同的参数顺序，
	// 日志与测试都会跟着抖
	sort.Strings(out)
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
