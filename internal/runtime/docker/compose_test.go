package docker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/command"
	"github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/spec"
	"gopkg.in/yaml.v3"
)

// newCompose 造一个用替身的 compose Runtime，外加一个可写的 generation 目录。
func newCompose(t *testing.T) (*ComposeRuntime, *command.Fake, string) {
	t.Helper()
	fake := command.NewFake()
	genDir := t.TempDir()
	return &ComposeRuntime{Runner: fake}, fake, genDir
}

// composeWorkload 造一份 compose 工作负载，附带一个真的 compose 文件。
//
// 文件必须真的存在：Materialize 会 stat 它——「渲染流水线还没把文件放上去
// 就去 up」是一种要报错的情形，不能被替身掩盖掉。
func composeWorkload(
	t *testing.T, genDir, digest string, mut ...func(*composeArgs),
) runtime.WorkloadSpec {
	t.Helper()
	file := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(file, []byte("services:\n  web: {image: x}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := composeArgs{File: file, ImageBlobs: []string{"web"}}
	for _, m := range mut {
		m(&c)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return runtime.WorkloadSpec{
		Site: "site1", Component: "shop", Role: "default",
		Generation: 1, SpecDigest: digest, GenerationDir: genDir,
		Blobs: map[string]string{
			"web":    "/var/lib/mecharion/blobs/sha256/ab/web",
			"worker": "/var/lib/mecharion/blobs/sha256/cd/worker",
		},
		Workload: &spec.Workload{Runtime: "compose", Compose: raw},
	}
}

// projectJSON 造一份 project 里若干容器的 inspect 输出。
func projectJSON(svcLabels map[string]map[string]string) string {
	var list []containerJSON
	for _, svc := range sortedKeys(svcLabels) {
		c := containerJSON{}
		c.Name = "/mecharion-shop-" + svc + "-1"
		c.State.Status = "running"
		c.State.StartedAt = "2026-08-05T10:00:00.000000000Z"
		c.Config.Image = svc + ":1.0"
		c.Config.Labels = svcLabels[svc]
		list = append(list, c)
	}
	b, _ := json.Marshal(list)
	return string(b)
}

// composeOurs 是我们自己打的一组标签。
func composeOurs(svc, digest string) map[string]string {
	m := map[string]string{
		LabelManagedBy:               ManagedByValue,
		LabelComponent:               "shop",
		LabelRole:                    "default",
		LabelSpecDigest:              digest,
		LabelComposeProject:          "mecharion-shop",
		"com.docker.compose.service": svc,
	}
	return m
}

// stubProject 让替身把 project 枚举与 inspect 都答上。
func stubProject(f *command.Fake, names []string, inspect string) {
	f.SetPrefix("docker ps --all --no-trunc --format {{.Names}} --filter label="+
		LabelComposeProject+"=mecharion-shop",
		command.Result{Stdout: strings.Join(names, "\n")})
	f.SetPrefix("docker inspect --type container", command.Result{Stdout: inspect})
}

// ── Probe ───────────────────────────────────────────────────────────────

func TestComposeProbeAvailable(t *testing.T) {
	rt, fake, _ := newCompose(t)
	fake.Set("docker version --format {{json .}}", command.Result{
		Stdout: `{"Client":{"Version":"29.7.1"},"Server":{"Version":"29.7.1","Os":"linux","Arch":"amd64"}}`,
	})
	fake.Set("docker compose version --short", command.Result{Stdout: "5.4.0\n"})

	cap, err := rt.Probe(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if !cap.Available || cap.Version != "5.4.0" {
		t.Errorf("应报可用且版本为 compose 的版本，实际 %+v", cap)
	}
	if cap.Detail["docker"] != "29.7.1" {
		t.Errorf("docker 版本也该报上去，实际 %v", cap.Detail)
	}
}

// TestComposeProbeNeedsDaemon 钉住「有 compose 插件但连不上 daemon」也算不可用。
//
// compose 插件在没有 daemon 时照样能报版本。只看它会让放置通过，
// 然后在真正部署时才失败——而那时用户已经在等了。
func TestComposeProbeNeedsDaemon(t *testing.T) {
	rt, fake, _ := newCompose(t)
	fake.Set("docker version --format {{json .}}", command.Result{
		ExitCode: 1, Stderr: "Cannot connect to the Docker daemon",
	})
	fake.Set("docker compose version --short", command.Result{Stdout: "5.4.0\n"})

	cap, err := rt.Probe(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if cap.Available {
		t.Fatal("连不上 daemon 时不该因为 compose 插件在就报可用")
	}
}

func TestComposeProbeNoPlugin(t *testing.T) {
	rt, fake, _ := newCompose(t)
	fake.Set("docker version --format {{json .}}", command.Result{
		Stdout: `{"Client":{"Version":"29.7.1"},"Server":{"Version":"29.7.1"}}`,
	})
	fake.Set("docker compose version --short", command.Result{
		ExitCode: 125, Stderr: "docker: 'compose' is not a docker command.",
	})

	cap, err := rt.Probe(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if cap.Available {
		t.Fatal("没有 compose 插件时不该报可用")
	}
	if !strings.Contains(cap.Reason, "compose") {
		t.Errorf("Reason 应指明缺的是 compose，实际 %q", cap.Reason)
	}
}

// ── 标签纪律 ────────────────────────────────────────────────────────────

// TestComposeRefusesForeignProject 是本文件**最重要**的一条测试。
//
// `docker compose -p X down` 会拆掉整个 project，不挑着删。一个同名但属于
// 用户自己的 project 被拆掉，是这里能造成的最大破坏。
func TestComposeRefusesForeignProject(t *testing.T) {
	foreign := projectJSON(map[string]map[string]string{
		"web": {"com.example.owner": "someone-else", LabelComposeProject: "mecharion-shop"},
	})

	cases := []struct {
		name string
		call func(*ComposeRuntime, string) error
	}{
		{"Materialize", func(r *ComposeRuntime, gen string) error {
			_, err := r.Materialize(ctx(), composeWorkload(t, gen, "d1"))
			return err
		}},
		{"Start", func(r *ComposeRuntime, _ string) error {
			return r.Start(ctx(), runtime.Ref{Native: "mecharion-shop"})
		}},
		{"Stop", func(r *ComposeRuntime, _ string) error {
			return r.Stop(ctx(), runtime.Ref{Native: "mecharion-shop"}, runtime.StopOpts{})
		}},
		{"Remove", func(r *ComposeRuntime, _ string) error {
			return r.Remove(ctx(), runtime.Ref{Native: "mecharion-shop"})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, fake, genDir := newCompose(t)
			stubProject(fake, []string{"mecharion-shop-web-1"}, foreign)
			fake.SetPrefix("docker load", command.Result{Stdout: "Loaded image: web:1.0\n"})

			err := tc.call(rt, genDir)
			if err == nil {
				t.Fatalf("%s 不该碰一个没有 Mecharion 标签的同名 project", tc.name)
			}
			if !strings.Contains(err.Error(), "标签") {
				t.Errorf("错误信息应说清是标签问题，实际: %v", err)
			}
			// **一次都不能动它**
			for _, c := range fake.Calls() {
				for _, verb := range []string{
					"docker compose --project-name mecharion-shop down",
					"docker compose --project-name mecharion-shop stop",
					"docker compose --project-name mecharion-shop start",
				} {
					if strings.HasPrefix(c, verb) {
						t.Errorf("%s 执行了 %q —— 那会拆掉用户自己的 project", tc.name, c)
					}
				}
			}
			// override 文件也不该被写出来：它落在 generation 目录里，
			// 一个被拒绝的物化不该留下痕迹
			if _, err := os.Stat(filepath.Join(genDir, labelsFileName)); err == nil {
				t.Error("被拒绝的 Materialize 不该写出 override 文件")
			}
		})
	}
}

// TestComposeRefusesPartiallyForeignProject 钉住「全部」而非「任意」。
//
// 一个混着别人容器的 project，动它同样会伤到那些容器——`compose down`
// 不挑着删。用「有一个是我们的就算我们的」会让这种情形通过。
func TestComposeRefusesPartiallyForeignProject(t *testing.T) {
	rt, fake, _ := newCompose(t)
	stubProject(fake,
		[]string{"mecharion-shop-web-1", "mecharion-shop-db-1"},
		projectJSON(map[string]map[string]string{
			"web": composeOurs("web", "d1"),
			"db":  {"com.example.owner": "someone-else"},
		}))

	if err := rt.Remove(ctx(), runtime.Ref{Native: "mecharion-shop"}); err == nil {
		t.Fatal("project 里混着别人的容器时应当拒绝")
	}
	if fake.Ran("docker compose --project-name mecharion-shop down") {
		t.Error("执行了 down —— 那会连别人的容器一起拆掉")
	}
}

// TestComposeObserveIgnoresForeignProject 钉住观测侧同样按标签过滤。
func TestComposeObserveIgnoresForeignProject(t *testing.T) {
	rt, fake, _ := newCompose(t)
	stubProject(fake, []string{"mecharion-shop-web-1"},
		projectJSON(map[string]map[string]string{"web": {"other": "x"}}))

	st, err := rt.Observe(ctx(), runtime.Ref{Native: "mecharion-shop"})
	if err != nil {
		t.Fatal(err)
	}
	if st.State != runtime.StateAbsent {
		t.Errorf("无标签的同名 project 应当被当作不存在，实际 %s", st.State)
	}
}

// ── Materialize ─────────────────────────────────────────────────────────

// TestComposeMaterializeWritesLabelOverride 钉住标签真的进了 compose。
//
// compose 没有 `--label`，标签只能写进文件。这条测试同时验证三件事：
// override 被写出来了、每个 service 都有标签、它被传给了 compose。
func TestComposeMaterializeCreatesWithLabels(t *testing.T) {
	rt, fake, genDir := newCompose(t)
	fake.SetPrefix("docker load", command.Result{Stdout: "Loaded image: web:1.0\n"})
	stubProject(fake, nil, "")
	fake.SetPrefix("docker compose --project-name mecharion-shop --file",
		command.Result{Stdout: "web\nworker\n"})

	w := composeWorkload(t, genDir, "digest-aaa", func(c *composeArgs) {
		c.ImageBlobs = []string{"web", "worker"}
		c.ExecService = "web"
	})
	ref, err := rt.Materialize(ctx(), w)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Native != "mecharion-shop" {
		t.Errorf("Ref.Native 对 compose 应是 project 名，实际 %q", ref.Native)
	}

	// ① 两个镜像都 load 了
	if n := fake.CountRan("docker load"); n != 2 {
		t.Errorf("imageBlobs 有两个就该 load 两次，实际 %d", n)
	}

	// ② override 文件的内容
	body, err := os.ReadFile(filepath.Join(genDir, labelsFileName))
	if err != nil {
		t.Fatalf("override 文件没写出来: %v", err)
	}
	var doc struct {
		Services map[string]struct {
			Labels map[string]string `yaml:"labels"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("override 不是合法 YAML: %v\n%s", err, body)
	}
	if len(doc.Services) != 2 {
		t.Fatalf("两个 service 都要打标签，实际 %d 个", len(doc.Services))
	}
	for svc, s := range doc.Services {
		if s.Labels[LabelManagedBy] != ManagedByValue {
			t.Errorf("service %s 缺少 %s 标签", svc, LabelManagedBy)
		}
		if s.Labels[LabelSpecDigest] != "digest-aaa" {
			t.Errorf("service %s 的 spec-digest 应为 digest-aaa，实际 %q",
				svc, s.Labels[LabelSpecDigest])
		}
	}
	// **只有 execService 那个带 exec 标签**：多打一个，ExecIn 就可能进错容器
	if doc.Services["web"].Labels[LabelExec] != "true" {
		t.Error("execService=web 的容器应带 exec 标签")
	}
	if _, ok := doc.Services["worker"].Labels[LabelExec]; ok {
		t.Error("非 execService 的 service 不该带 exec 标签")
	}

	// ③ override 真的传给了 compose up
	up := callWith(fake, "up --no-start")
	if up == "" {
		t.Fatalf("Materialize 应当 up --no-start（只物化不启动），实际执行了:\n%s",
			strings.Join(fake.Calls(), "\n"))
	}
	if !strings.Contains(up, labelsFileName) {
		t.Errorf("override 文件应当作为第二个 --file 传进去，实际:\n%s", up)
	}
}

// TestComposeMaterializeIsNoopWhenDigestMatches 钉住幂等。
func TestComposeMaterializeIsNoopWhenDigestMatches(t *testing.T) {
	rt, fake, genDir := newCompose(t)
	fake.SetPrefix("docker load", command.Result{Stdout: "Loaded image: web:1.0\n"})
	stubProject(fake, []string{"mecharion-shop-web-1"},
		projectJSON(map[string]map[string]string{"web": composeOurs("web", "digest-aaa")}))
	fake.SetPrefix("docker compose --project-name mecharion-shop --file",
		command.Result{Stdout: "web\n"})

	if _, err := rt.Materialize(ctx(), composeWorkload(t, genDir, "digest-aaa")); err != nil {
		t.Fatal(err)
	}
	for _, c := range fake.Calls() {
		if strings.Contains(c, " up ") {
			t.Errorf("digest 未变时不该 up，却执行了 %q", c)
		}
	}
}

// TestComposeMaterializeRecreatesWhenServiceMissing 钉住「容器数对不上也要重建」。
//
// 只比 digest 的话，一个上次 up 到一半失败、只起来一个 service 的 project
// 会被判成「已经是想要的那个」——于是缺的那个永远补不回来。
func TestComposeMaterializeRecreatesWhenServiceMissing(t *testing.T) {
	rt, fake, genDir := newCompose(t)
	fake.SetPrefix("docker load", command.Result{Stdout: "Loaded image: web:1.0\n"})
	stubProject(fake, []string{"mecharion-shop-web-1"},
		projectJSON(map[string]map[string]string{"web": composeOurs("web", "digest-aaa")}))
	// compose 文件里其实有两个 service
	fake.SetPrefix("docker compose --project-name mecharion-shop --file",
		command.Result{Stdout: "web\nworker\n"})

	w := composeWorkload(t, genDir, "digest-aaa", func(c *composeArgs) {
		c.ExecService = "web"
	})
	if _, err := rt.Materialize(ctx(), w); err != nil {
		t.Fatal(err)
	}
	if callWith(fake, "up --no-start") == "" {
		t.Error("project 里少一个 service 时应当重新 up")
	}
}

// TestComposeMaterializeRefusesMissingFile 钉住「compose 文件还没落盘」要报清楚。
//
// 该文件由渲染流水线作为 template 资源产出。它不在，说明资源阶段没跑或
// 跑失败了——直接 up 的话 compose 会报一句关于文件路径的话，而真正的
// 原因在别处。
func TestComposeMaterializeRefusesMissingFile(t *testing.T) {
	rt, _, genDir := newCompose(t)
	w := composeWorkload(t, genDir, "d1", func(c *composeArgs) {
		c.File = filepath.Join(t.TempDir(), "nope.yaml")
	})
	_, err := rt.Materialize(ctx(), w)
	if err == nil {
		t.Fatal("compose 文件不存在时应当报错")
	}
	if !strings.Contains(err.Error(), "渲染流水线") {
		t.Errorf("错误应指向真正的原因（资源阶段），实际: %v", err)
	}
}

// ── execService ─────────────────────────────────────────────────────────

// TestPickExecService 钉住「多个 service 而没声明时报错，不猜」。
//
// 猜错会在一个无关的容器里跑诊断命令，而它多半会「成功」——一个假的
// 健康信号比一个明确的错误坏得多（ADR-0032）。
func TestPickExecService(t *testing.T) {
	cases := []struct {
		name     string
		services []string
		declared string
		want     string
		wantErr  string
	}{
		{"单 service 可省略", []string{"web"}, "", "web", ""},
		{"声明了就用声明的", []string{"web", "worker"}, "worker", "worker", ""},
		{"多 service 未声明报错", []string{"web", "worker"}, "", "", "execService"},
		{"声明了不存在的 service", []string{"web"}, "cache", "", "不在 project"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickExecService(tc.services, tc.declared)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("应当报错，实际取了 %q", got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("错误应含 %q，实际: %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("应取 %q，实际 %q", tc.want, got)
			}
		})
	}
}

// TestComposeExecInFindsLabeledContainer 钉住 ExecIn 按 exec 标签定位容器。
func TestComposeExecInFindsLabeledContainer(t *testing.T) {
	rt, fake, _ := newCompose(t)
	fake.SetPrefix("docker ps --all --no-trunc --format {{.Names}} --filter label="+
		LabelComposeProject+"=mecharion-shop --filter label="+LabelExec+"=true",
		command.Result{Stdout: "mecharion-shop-web-1\n"})
	fake.SetPrefix("docker exec mecharion-shop-web-1",
		command.Result{Stdout: "accepting connections\n"})

	res, err := rt.ExecIn(ctx(),
		runtime.Ref{Component: "shop", Role: "default", Native: "mecharion-shop"},
		[]string{"pg_isready"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Errorf("命令应当成功，实际 %+v", res)
	}
	if !fake.Ran("docker exec mecharion-shop-web-1 pg_isready") {
		t.Errorf("应当进带 exec 标签的那个容器，实际执行了 %v", fake.Calls())
	}
}

// TestComposeExecInReportsCannotProbe 钉住「探不了」与「探针失败」分开。
//
// project 没在跑时找不到容器。把它当成一次探针失败，会让一个刚被停掉的
// project 看起来像「健康检查连续失败」。
func TestComposeExecInReportsCannotProbe(t *testing.T) {
	rt, fake, _ := newCompose(t)
	fake.SetPrefix("docker ps", command.Result{Stdout: ""})

	_, err := rt.ExecIn(ctx(),
		runtime.Ref{Component: "shop", Role: "default", Native: "mecharion-shop"},
		[]string{"pg_isready"})
	if err == nil {
		t.Fatal("找不到容器时应当报 error（探不了），而不是返回一个失败的退出码")
	}
	if fake.Ran("docker exec") {
		t.Error("没找到容器就不该去 exec")
	}
}

// TestComposeExecInRejectsEmptyNative 与 docker 侧同源：漏传 Native 是调用方的 bug。
func TestComposeExecInRejectsEmptyNative(t *testing.T) {
	rt, fake, _ := newCompose(t)
	_, err := rt.ExecIn(ctx(), runtime.Ref{Component: "shop", Role: "default"},
		[]string{"pg_isready"})
	if err == nil {
		t.Fatal("Ref 没有 project 名时应当报错")
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("不该真的去执行 docker，实际执行了 %v", fake.Calls())
	}
}

// ── Observe ─────────────────────────────────────────────────────────────

// TestComposeObserveAggregates 钉住聚合规则**取最坏**，且明细进 Raw。
//
// 粒度粗一档是「不把 service 映射成 Role」的已知代价（ADR-0011），
// 因此明细一定要在——否则用户只看到「坏了」而不知是谁坏了。
func TestComposeObserveAggregates(t *testing.T) {
	list := []containerJSON{}
	add := func(svc, status string, exit int) {
		c := containerJSON{}
		c.Name = "/mecharion-shop-" + svc + "-1"
		c.State.Status = status
		c.State.ExitCode = exit
		c.State.StartedAt = "2026-08-05T10:00:00.000000000Z"
		c.Config.Labels = composeOurs(svc, "d1")
		list = append(list, c)
	}
	add("web", "running", 0)
	add("worker", "exited", 137)
	b, _ := json.Marshal(list)

	rt, fake, _ := newCompose(t)
	stubProject(fake, []string{"mecharion-shop-web-1", "mecharion-shop-worker-1"}, string(b))

	st, err := rt.Observe(ctx(), runtime.Ref{Native: "mecharion-shop"})
	if err != nil {
		t.Fatal(err)
	}
	if st.State != runtime.StateFailed {
		t.Errorf("有一个 service 挂了整个 project 就该是 failed，实际 %s", st.State)
	}
	if st.ExitCode == nil || *st.ExitCode != 137 {
		t.Errorf("应当带上挂掉那个的退出码，实际 %v", st.ExitCode)
	}
	if st.Raw["web"] != "running" || st.Raw["worker"] != "failed" {
		t.Errorf("逐 service 明细应进 Raw，实际 %v", st.Raw)
	}
}

// TestComposeObserveAbsent 钉住 project 不存在。
func TestComposeObserveAbsent(t *testing.T) {
	rt, fake, _ := newCompose(t)
	fake.SetPrefix("docker ps", command.Result{Stdout: "\n"})

	st, err := rt.Observe(ctx(), runtime.Ref{Native: "mecharion-shop"})
	if err != nil {
		t.Fatal(err)
	}
	if st.State != runtime.StateAbsent {
		t.Errorf("没有容器时应报 absent，实际 %s", st.State)
	}
	if fake.Ran("docker inspect") {
		t.Error("一个容器都没有就不该去 inspect")
	}
}

// ── 接口一致性 ──────────────────────────────────────────────────────────

func TestComposeImplementsRuntime(t *testing.T) {
	var _ runtime.Runtime = (*ComposeRuntime)(nil)
	var _ runtime.Runtime = (*Runtime)(nil)
}

// TestComposeReloadUnsupported 钉住降级为重启的那条路。
func TestComposeReloadUnsupported(t *testing.T) {
	rt, _, _ := newCompose(t)
	if err := rt.Reload(ctx(), runtime.Ref{Native: "mecharion-shop"}); err != runtime.ErrReloadUnsupported {
		t.Errorf("compose 不支持 reload，应返回 ErrReloadUnsupported，实际 %v", err)
	}
}

// callWith 返回第一条含指定片段的调用。
//
// compose 的命令行是 `docker compose -p P --file F [--file O] <子命令>`——
// 子命令在**末尾**，按前缀找会把 `config --services` 和 `up` 混在一起。
func callWith(f *command.Fake, frag string) string {
	for _, c := range f.Calls() {
		if strings.Contains(c, frag) {
			return c
		}
	}
	return ""
}
