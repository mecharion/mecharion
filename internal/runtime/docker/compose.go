// compose runtime 与 docker runtime 同包，是刻意的。
//
// 两者共享的东西比不共享的多：标签常量、`docker inspect` 的解码、状态
// 归一化、`docker exec` 的退出码语义。分成两个包的话这些要么被复制一份
// （于是「两个 Runtime 对同一个容器状态给出不同结论」这类 bug 变得可能），
// 要么为了唯一的一个消费者导出一大片内部 API。
//
// 落地时才浮现的四个决策见 docs/design/19-container-runtime.md §6.6。

package docker

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/mecharion/mecharion/internal/command"
	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/spec"
	"gopkg.in/yaml.v3"
)

// ComposeName 是 compose Runtime 的名字。
const ComposeName = "compose"

// LabelExec 标出「exec 探针该进哪个容器」。
//
// 决策在 Materialize 时做（那里才拿得到 execService），记在标签上，
// 于是 ExecIn 只靠一个 Ref 就能找到目标，不依赖 compose 文件是否还在
// （§6.6.4）。
const LabelExec = "dev.mecharion.exec"

// LabelComposeProject 是 compose 自己打的 project 标签。
//
// 我们**读**它来枚举 project 的容器，但**不拿它判归属**——判归属仍然只看
// dev.mecharion.managed-by。
const LabelComposeProject = "com.docker.compose.project"

// labelsFileName 是那份只含标签的 override 文件。
//
// 加前缀点号不是为了藏，是为了在 generation 目录里一眼看出它不是 Pack
// 作者写的东西。
const labelsFileName = ".mecharion-labels.yaml"

// ComposeRuntime 是 compose 实现。
type ComposeRuntime struct {
	Runner command.Runner
	Host   string
}

// NewCompose 构造一个使用真实命令的 compose Runtime。
func NewCompose() *ComposeRuntime { return &ComposeRuntime{Runner: command.Exec{}} }

// Name 返回 "compose"。
func (r *ComposeRuntime) Name() string { return ComposeName }

// docker 借用 docker Runtime 的执行路径，连 --host 的处理一起。
func (r *ComposeRuntime) inner() *Runtime {
	return &Runtime{Runner: r.Runner, Host: r.Host}
}

// RemoveImage 与 docker runtime 是同一件事（runtime.ImageReclaimer）：
// compose 的镜像也在同一个本地镜像库里，没有第二套。
func (r *ComposeRuntime) RemoveImage(ctx context.Context, image string) error {
	return r.inner().RemoveImage(ctx, image)
}

// compose 执行一条 `docker compose` 子命令。
func (r *ComposeRuntime) compose(
	ctx context.Context, project string, args ...string,
) (command.Result, error) {
	full := append([]string{"compose", "--project-name", project}, args...)
	return r.inner().docker(ctx, full...)
}

// ProjectName 返回一个组件的 compose project 名。
func ProjectName(component string) string { return "mecharion-" + component }

// ── Probe ───────────────────────────────────────────────────────────────

// Probe 检查本机能否用 compose。
//
// 先问 docker：compose 插件在没有 daemon 时也能报版本，只看它会让放置
// 通过，然后在部署时才发现连不上。
func (r *ComposeRuntime) Probe(ctx context.Context) (runtime.Capability, error) {
	cap, err := r.inner().Probe(ctx)
	if err != nil || !cap.Available {
		return cap, err
	}

	res, err := r.inner().docker(ctx, "compose", "version", "--short")
	if err != nil {
		if command.IsNotFound(err) {
			return runtime.Capability{
				Available: false,
				Reason:    "本机有 docker 但没有 compose 插件",
			}, nil
		}
		return runtime.Capability{}, faults.Wrap(faults.Transient, "探测 compose", err)
	}
	if res.ExitCode != 0 {
		return runtime.Capability{
			Available: false,
			Reason:    "docker compose 不可用: " + firstLine(res.Message()),
		}, nil
	}

	v := strings.TrimSpace(res.Stdout)
	return runtime.Capability{
		Available: true,
		Version:   v,
		Detail: map[string]string{
			"docker": cap.Version,
			"os":     cap.Detail["os"],
			"arch":   cap.Detail["arch"],
		},
	}, nil
}

// RefFor 从工作负载推出 project 名，不碰机器。见 runtime.Runtime.RefFor。
//
// **必须解一次 compose 段**：project 名可以被 `compose.projectName` 覆盖，
// 猜一个 mecharion-<component> 会去删一个不存在的 project，而真正的那个
// 留在机器上——一次「报告成功的、什么也没删掉的卸载」。
func (r *ComposeRuntime) RefFor(w runtime.WorkloadSpec) (runtime.Ref, error) {
	if err := w.Validate(); err != nil {
		return runtime.Ref{}, faults.Wrap(faults.Permanent, "构造工作负载引用", err)
	}
	c, err := decodeCompose(w.Workload)
	if err != nil {
		return runtime.Ref{}, err
	}
	project := c.ProjectName
	if project == "" {
		project = ProjectName(w.Component)
	}
	return runtime.Ref{
		Runtime: ComposeName, Component: w.Component, Role: w.Role,
		Generation: w.Generation, Native: project,
	}, nil
}

// ── Materialize ─────────────────────────────────────────────────────────

// Materialize 把镜像装好、project 创建好，**但不启动**。
//
//	① 逐个 docker load imageBlobs
//	② compose config --services 取 service 列表（顺带校验了文件）
//	③ 写一份只含标签的 override
//	④ compose up --no-start
//
// 幂等由 spec-digest 判定，与 docker 同：project 里的容器都带着想要的
// 摘要就直接返回，不去 up。
func (r *ComposeRuntime) Materialize(
	ctx context.Context, w runtime.WorkloadSpec,
) (runtime.Ref, error) {
	if err := w.Validate(); err != nil {
		return runtime.Ref{}, faults.Wrap(faults.Permanent, "校验工作负载", err)
	}
	c, err := decodeCompose(w.Workload)
	if err != nil {
		return runtime.Ref{}, err
	}

	project := c.ProjectName
	if project == "" {
		project = ProjectName(w.Component)
	}
	ref := runtime.Ref{
		Runtime: ComposeName, Component: w.Component, Role: w.Role,
		Generation: w.Generation, Native: project,
	}

	if c.File == "" {
		return ref, faults.Permanentf("物化 project",
			"workload.compose.file 为空——渲染流水线应当把它改写成绝对路径")
	}
	if _, err := os.Stat(c.File); err != nil {
		return ref, faults.Permanentf("物化 project",
			"compose 文件 %s 不可读: %v\n"+
				"  它由渲染流水线作为 template 资源产出，应当先于工作负载落盘",
			c.File, err)
	}

	// **归属检查要在动任何东西之前**：project 已存在但不是我们的，
	// 后面的 up 会去改它的容器
	cur, err := r.projectContainers(ctx, project)
	if err != nil {
		return ref, err
	}
	if len(cur) > 0 && !allManaged(cur) {
		return ref, faults.Permanentf("物化 project",
			"compose project %s 已存在，但它的容器不带 %s=%s 标签——"+
				"Mecharion 不会碰不是自己创建的 project。\n"+
				"  请确认它的来源；确实该由 Mecharion 接管的话，请先手工 docker compose -p %s down",
			project, LabelManagedBy, ManagedByValue, project)
	}

	for _, name := range c.ImageBlobs {
		image, err := r.loadBlob(ctx, w, name)
		if err != nil {
			return ref, err
		}
		ref.Images = append(ref.Images, image)
	}

	services, err := r.services(ctx, project, c)
	if err != nil {
		return ref, err
	}
	execSvc, err := pickExecService(services, c.ExecService)
	if err != nil {
		return ref, err
	}

	if w.SpecDigest != "" && len(cur) > 0 && matchesDigest(cur, services, w.SpecDigest) {
		// 已经是想要的那个，什么都不用做
		return ref, nil
	}

	overridePath, err := writeLabelOverride(w, services, execSvc)
	if err != nil {
		return ref, err
	}

	args := []string{"--file", c.File, "--file", overridePath}
	if c.EnvFile != "" {
		args = append(args, "--env-file", c.EnvFile)
	}
	// --no-start：Materialize 只物化不启动，与 docker create 对齐。
	// --remove-orphans：service 被删掉时它留下的容器也要走
	args = append(args, "up", "--no-start", "--remove-orphans")

	res, err := r.compose(ctx, project, args...)
	if err != nil {
		return ref, faults.Wrap(faults.Transient, "创建 project", err)
	}
	if res.ExitCode != 0 {
		return ref, faults.Permanentf("创建 project",
			"docker compose -p %s up 失败: %s", project, firstLine(res.Message()))
	}
	return ref, nil
}

// loadBlob 把一个镜像 tar 装进本地镜像库，返回它的引用。
//
// 返回引用而不是只返回 error：镜像回收的候选只能在这一刻拿到
// （22-upgrade §2.5 ①），compose 文件里的 `image:` 是 compose 自己解析的，
// 我们这边看不到。
func (r *ComposeRuntime) loadBlob(
	ctx context.Context, w runtime.WorkloadSpec, name string,
) (string, error) {
	path, ok := w.BlobPath(name)
	if !ok {
		return "", faults.Permanentf("加载镜像",
			"找不到载荷 %q 对应的文件——它应当已由 mechlet 取到本地", name)
	}
	res, err := r.inner().docker(ctx, "load", "--input", path)
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

// services 取出 project 里的 service 名。
//
// 顺带把 compose 文件校验了一遍——`config` 是 compose 自己的解析器，
// 比我们再写一个 YAML 检查靠谱。
func (r *ComposeRuntime) services(
	ctx context.Context, project string, c *composeArgs,
) ([]string, error) {
	args := []string{"--file", c.File}
	if c.EnvFile != "" {
		args = append(args, "--env-file", c.EnvFile)
	}
	args = append(args, "config", "--services")

	res, err := r.compose(ctx, project, args...)
	if err != nil {
		return nil, faults.Wrap(faults.Transient, "解析 compose 文件", err)
	}
	if res.ExitCode != 0 {
		return nil, faults.Permanentf("解析 compose 文件",
			"docker compose config %s 失败: %s", c.File, firstLine(res.Message()))
	}

	var out []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, faults.Permanentf("解析 compose 文件",
			"%s 里没有 service", c.File)
	}
	sort.Strings(out)
	return out, nil
}

// pickExecService 决定 exec 探针进哪个 service。
//
// **多个 service 而没声明时报错，不猜**：猜错会在一个无关的容器里跑诊断
// 命令，而它多半会「成功」——一个假的健康信号比一个明确的错误坏得多
// （ADR-0032）。
func pickExecService(services []string, declared string) (string, error) {
	if declared != "" {
		for _, s := range services {
			if s == declared {
				return s, nil
			}
		}
		return "", faults.Permanentf("确定 exec service",
			"workload.compose.execService=%q 不在 project 的 service 里（有 %s）",
			declared, strings.Join(services, ", "))
	}
	if len(services) == 1 {
		return services[0], nil
	}
	return "", faults.Permanentf("确定 exec service",
		"project 有 %d 个 service（%s），必须用 workload.compose.execService 指定"+
			"exec 探针进哪一个——猜错会在无关的容器里跑诊断命令",
		len(services), strings.Join(services, ", "))
}

// writeLabelOverride 生成那份只含标签的 override 文件。
//
// compose 没有 `--label`，标签只能写进文件。**不改主文件而是叠一层**，
// 是为了让 `compose config` 的输出仍然是 Pack 作者写的那份——用户 cat
// 一眼能看懂的文件在排障时值这点复杂度（§6.6.2）。
func writeLabelOverride(
	w runtime.WorkloadSpec, services []string, execSvc string,
) (string, error) {
	if w.GenerationDir == "" {
		return "", faults.Permanentf("写 compose 覆盖文件",
			"缺少 generation 目录——标签里含 spec-digest，它该随 generation 生灭")
	}

	base := map[string]string{}
	for _, kv := range labelArgs(w) {
		k, v, _ := strings.Cut(kv, "=")
		base[k] = v
	}

	svcs := map[string]any{}
	for _, s := range services {
		labels := make(map[string]string, len(base)+1)
		for k, v := range base {
			labels[k] = v
		}
		if s == execSvc {
			labels[LabelExec] = "true"
		}
		svcs[s] = map[string]any{"labels": labels}
	}

	body, err := yaml.Marshal(map[string]any{"services": svcs})
	if err != nil {
		return "", faults.Wrap(faults.Permanent, "写 compose 覆盖文件", err)
	}
	head := "# 由 Mecharion 生成：只含标签，与 Pack 的 compose 文件叠加使用。\n" +
		"# 手工修改无效——每次物化都会重写。\n"

	path := filepath.Join(w.GenerationDir, labelsFileName)
	if err := os.WriteFile(path, append([]byte(head), body...), 0o644); err != nil {
		return "", faults.Wrap(faults.Permanent, "写 compose 覆盖文件", err)
	}
	return path, nil
}

// matchesDigest 判断现有 project 是不是我们要的那个。
//
// 三件事都要对：容器数量与 service 数量一致（少一个说明上次 up 没做完）、
// 每个容器都带着想要的摘要、且 service 覆盖齐全。
func matchesDigest(cur []containerJSON, services []string, want string) bool {
	if len(cur) != len(services) {
		return false
	}
	for _, c := range cur {
		if c.Config.Labels[LabelSpecDigest] != want {
			return false
		}
	}
	return true
}

// ── Start / Stop / Reload ───────────────────────────────────────────────

// Start 启动 project。
func (r *ComposeRuntime) Start(ctx context.Context, ref runtime.Ref) error {
	if err := r.requireManagedProject(ctx, ref.Native); err != nil {
		return err
	}
	res, err := r.compose(ctx, ref.Native, "start")
	if err != nil {
		return faults.Wrap(faults.Transient, "启动 project", err)
	}
	if res.ExitCode != 0 {
		return faults.Permanentf("启动 project",
			"docker compose -p %s start 失败: %s", ref.Native, firstLine(res.Message()))
	}
	return nil
}

// Stop 停止 project。
func (r *ComposeRuntime) Stop(
	ctx context.Context, ref runtime.Ref, opts runtime.StopOpts,
) error {
	if err := r.requireManagedProject(ctx, ref.Native); err != nil {
		return err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultStopTimeout
	}
	res, err := r.compose(ctx, ref.Native,
		"stop", "--timeout", strconv.Itoa(int(timeout.Seconds())))
	if err != nil {
		return faults.Wrap(faults.Transient, "停止 project", err)
	}
	if res.ExitCode != 0 {
		return faults.Permanentf("停止 project",
			"docker compose -p %s stop 失败: %s", ref.Native, firstLine(res.Message()))
	}
	return nil
}

// Reload 不被支持，理由与 docker 同。
func (r *ComposeRuntime) Reload(context.Context, runtime.Ref) error {
	return runtime.ErrReloadUnsupported
}

// ── Observe ─────────────────────────────────────────────────────────────

// Observe 聚合整个 project 的状态。
//
// 聚合规则**取最坏**：只要有一个 service 挂了，整个 project 就是 Failed。
// 粒度粗一档是「不把 service 映射成 Role」的已知代价（ADR-0011），
// 因此逐 service 的明细一定要进 Raw——否则用户只看到「坏了」而不知是谁坏了。
func (r *ComposeRuntime) Observe(
	ctx context.Context, ref runtime.Ref,
) (runtime.Status, error) {
	cs, err := r.projectContainers(ctx, ref.Native)
	if err != nil {
		return runtime.Status{}, err
	}
	if len(cs) == 0 {
		return runtime.Status{State: runtime.StateAbsent, Native: ref.Native}, nil
	}
	if !allManaged(cs) {
		// 同名但不是我们的：**当作不存在**，与 docker 一致
		return runtime.Status{
			State: runtime.StateAbsent, Native: ref.Native,
			Raw: map[string]string{"note": "同名 project 的容器不带 Mecharion 标签，已忽略"},
		}, nil
	}

	st := runtime.Status{Native: ref.Native, Raw: map[string]string{}}
	worst := -1
	for _, c := range cs {
		one := statusOf(&c, strings.TrimPrefix(c.Name, "/"))
		svc := c.Config.Labels["com.docker.compose.service"]
		if svc == "" {
			svc = strings.TrimPrefix(c.Name, "/")
		}
		st.Raw[svc] = one.State.String()
		st.Restarts += one.Restarts

		if rank := severity(one.State); rank > worst {
			worst = rank
			st.State = one.State
			st.ExitCode = one.ExitCode
		}
		// Since 取最早的一个：project 是一个整体，它「起来多久了」应当
		// 以最先起来的那个为准，否则一次单 service 重启会让整个 project
		// 看起来刚起来
		if !one.Since.IsZero() && (st.Since.IsZero() || one.Since.Before(st.Since)) {
			st.Since = one.Since
		}
		if one.Health == runtime.HealthFailing {
			st.Health = runtime.HealthFailing
		} else if one.Health == runtime.HealthPassing && st.Health == runtime.HealthNone {
			st.Health = runtime.HealthPassing
		}
	}
	st.Raw["services"] = strconv.Itoa(len(cs))
	if d := cs[0].Config.Labels[LabelSpecDigest]; d != "" {
		st.Raw["spec-digest"] = d
	}
	return st, nil
}

// severity 给状态排一个「有多坏」的序，用于聚合取最坏。
//
// Absent 排在最坏，是因为 project 里少一个容器比它跑挂了更需要人看：
// 挂了会重启，少了不会自己回来。
func severity(s runtime.State) int {
	switch s {
	case runtime.StateRunning:
		return 0
	case runtime.StateStarting:
		return 1
	case runtime.StateStopped:
		return 2
	case runtime.StateDegraded:
		return 3
	case runtime.StateFailed:
		return 4
	case runtime.StateAbsent:
		return 5
	}
	return 3
}

// ── Logs / Remove / ExecIn ──────────────────────────────────────────────

// Logs 取整个 project 的日志（compose 会带上 service 前缀）。
func (r *ComposeRuntime) Logs(
	ctx context.Context, ref runtime.Ref, opts runtime.LogOpts,
) (io.ReadCloser, error) {
	args := []string{"compose", "--project-name", ref.Native, "logs", "--no-color"}
	if opts.Follow {
		args = append(args, "--follow")
	}
	if opts.Tail > 0 {
		args = append(args, "--tail", strconv.Itoa(opts.Tail))
	}
	if !opts.Since.IsZero() {
		args = append(args, "--since", opts.Since.UTC().Format("2006-01-02T15:04:05Z"))
	}
	if r.Host != "" {
		args = append([]string{"--host", r.Host}, args...)
	}
	if r.Runner == nil {
		return command.Exec{}.Stream(ctx, "docker", args...)
	}
	return r.Runner.Stream(ctx, "docker", args...)
}

// Remove 拆掉整个 project。
//
// 与 docker 一致**不删镜像**：它可能被别的组件共用，误删要靠重新分发
// 几百 MB 来补。
func (r *ComposeRuntime) Remove(ctx context.Context, ref runtime.Ref) error {
	cs, err := r.projectContainers(ctx, ref.Native)
	if err != nil {
		return err
	}
	if len(cs) == 0 {
		return nil // 已经没了
	}
	if !allManaged(cs) {
		return faults.Permanentf("移除 project",
			"compose project %s 的容器不带 %s=%s 标签，拒绝删除",
			ref.Native, LabelManagedBy, ManagedByValue)
	}
	res, err := r.compose(ctx, ref.Native, "down", "--remove-orphans")
	if err != nil {
		return faults.Wrap(faults.Transient, "移除 project", err)
	}
	if res.ExitCode != 0 {
		return faults.Permanentf("移除 project",
			"docker compose -p %s down 失败: %s", ref.Native, firstLine(res.Message()))
	}
	return nil
}

// ExecIn 在 project 的 exec service 里执行一条命令。
//
// 走 `docker exec` 而不是 `docker compose exec`：后者需要 project 模型，
// 没有 -f 时 compose 会从容器标签里读回配置文件路径再解析一遍，而那个
// 文件在 generation 被回收之后就没了。进哪个容器的决策在 Materialize 时
// 就做完并记在标签上（§6.6.4）。
func (r *ComposeRuntime) ExecIn(
	ctx context.Context, ref runtime.Ref, cmd []string,
) (command.Result, error) {
	if len(cmd) == 0 {
		return command.Result{}, faults.Permanentf("执行命令", "命令为空")
	}
	if ref.Native == "" {
		return command.Result{}, faults.Permanentf("执行命令",
			"%s/%s 的 Ref 没有 project 名——调用方应当把 Observe 得到的 Native 传下来",
			ref.Component, ref.Role)
	}

	names, err := r.listContainers(ctx, ref.Native, LabelExec+"="+"true")
	if err != nil {
		return command.Result{}, err
	}
	if len(names) == 0 {
		// project 没在跑，或者标签没打上。这是「探不了」而非「探针失败」
		return command.Result{}, faults.Permanentf("执行命令",
			"compose project %s 里找不到带 %s 标签的容器——project 可能没在跑",
			ref.Native, LabelExec)
	}
	// scale > 1 时取容器名排序的第一个。确定性比「哪个副本」更重要——
	// 探针要的是一个能代表 service 的容器，不是特定那一个
	sort.Strings(names)

	return r.inner().ExecIn(ctx, runtime.Ref{
		Component: ref.Component, Role: ref.Role, Native: names[0],
	}, cmd)
}

// ── project 容器枚举 ────────────────────────────────────────────────────

// listContainers 按标签列出 project 的容器名。
func (r *ComposeRuntime) listContainers(
	ctx context.Context, project string, extra ...string,
) ([]string, error) {
	args := []string{"ps", "--all", "--no-trunc", "--format", "{{.Names}}",
		"--filter", "label=" + LabelComposeProject + "=" + project}
	for _, f := range extra {
		args = append(args, "--filter", "label="+f)
	}

	res, err := r.inner().docker(ctx, args...)
	if err != nil {
		return nil, faults.Wrap(faults.Transient, "枚举 project 容器", err)
	}
	if res.ExitCode != 0 {
		return nil, faults.Permanentf("枚举 project 容器",
			"docker ps 失败: %s", firstLine(res.Message()))
	}

	var out []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// projectContainers 取 project 全部容器的详情。
//
// 不用 `docker compose ps`：它的输出里**没有标签**，判不了归属，而归属
// 判定是这里风险最高的一条规则；它的 JSON 形状也在 v2 的小版本之间
// 改过（数组 ↔ JSON-lines）。`docker inspect` 是稳定契约（§6.6.3）。
func (r *ComposeRuntime) projectContainers(
	ctx context.Context, project string,
) ([]containerJSON, error) {
	names, err := r.listContainers(ctx, project)
	if err != nil || len(names) == 0 {
		return nil, err
	}
	sort.Strings(names)

	args := append([]string{"inspect", "--type", "container"}, names...)
	res, err := r.inner().docker(ctx, args...)
	if err != nil {
		return nil, faults.Wrap(faults.Transient, "查看 project 容器", err)
	}
	if res.ExitCode != 0 && strings.TrimSpace(res.Stdout) == "" {
		// 容器在两次调用之间被删掉是正常竞态，不是故障
		if strings.Contains(res.Message(), "No such") {
			return nil, nil
		}
		return nil, faults.Permanentf("查看 project 容器",
			"docker inspect 失败: %s", firstLine(res.Message()))
	}

	var list []containerJSON
	if err := json.Unmarshal([]byte(res.Stdout), &list); err != nil {
		return nil, faults.Wrap(faults.Permanent, "解析 docker inspect", err)
	}
	return list, nil
}

// requireManagedProject 在动一个 project 之前确认它是我们的。
//
// project 不存在时放行——让后续的 compose 命令去报，那句话比我们编的更准确。
func (r *ComposeRuntime) requireManagedProject(ctx context.Context, project string) error {
	cs, err := r.projectContainers(ctx, project)
	if err != nil || len(cs) == 0 {
		return err
	}
	if !allManaged(cs) {
		return faults.Permanentf("操作 project",
			"compose project %s 的容器不带 %s=%s 标签——"+
				"Mecharion 不会碰不是自己创建的 project",
			project, LabelManagedBy, ManagedByValue)
	}
	return nil
}

// allManaged 要求**每一个**容器都带标签。
//
// 用「全部」而非「任意」：一个混着别人容器的 project，动它同样会伤到
// 那些容器——`compose down` 不会挑着删。
func allManaged(cs []containerJSON) bool {
	for _, c := range cs {
		if !isManaged(c.Config.Labels) {
			return false
		}
	}
	return len(cs) > 0
}

// ── 参数解码 ────────────────────────────────────────────────────────────

// composeArgs 是 workload.compose 解码后的形态。
type composeArgs struct {
	// File 是**渲染产物的绝对路径**，不是 Pack 里写的模板名。
	File        string   `json:"file"`
	ImageBlobs  []string `json:"imageBlobs"`
	ProjectName string   `json:"projectName"`
	EnvFile     string   `json:"envFile"`
	ExecService string   `json:"execService"`
}

func decodeCompose(w *spec.Workload) (*composeArgs, error) {
	if len(w.Compose) == 0 {
		return nil, faults.Permanentf("解析工作负载",
			"workload.runtime=compose 但缺少 compose 段")
	}
	var c composeArgs
	if err := json.Unmarshal(w.Compose, &c); err != nil {
		return nil, faults.Wrap(faults.Permanent, "解析 workload.compose", err)
	}
	return &c, nil
}
