package docker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/command"
	"github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/spec"
)

func ctx() context.Context { return context.Background() }

// newRT 造一个用替身的 docker Runtime。
func newRT(t *testing.T) (*Runtime, *command.Fake) {
	t.Helper()
	fake := command.NewFake()
	return &Runtime{Runner: fake}, fake
}

// inspectJSON 造一份 docker inspect 的输出。
func inspectJSON(status string, labels map[string]string, mut ...func(*containerJSON)) string {
	c := containerJSON{}
	c.Name = "/mecharion-web-default"
	c.State.Status = status
	c.State.StartedAt = "2026-08-04T10:00:00.000000000Z"
	c.Config.Image = "webapp:1.0"
	c.Config.Labels = labels
	for _, m := range mut {
		m(&c)
	}
	b, _ := json.Marshal([]containerJSON{c})
	return string(b)
}

// ours 是一份 Mecharion 自己的标签。
func ours(digest string) map[string]string {
	return map[string]string{
		LabelManagedBy:  ManagedByValue,
		LabelComponent:  "web",
		LabelRole:       "default",
		LabelSpecDigest: digest,
	}
}

func workload(t *testing.T, digest string, mut ...func(*dockerArgs)) runtime.WorkloadSpec {
	t.Helper()
	d := dockerArgs{ImageBlob: "main"}
	for _, m := range mut {
		m(&d)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return runtime.WorkloadSpec{
		Site: "site1", Component: "web", Role: "default",
		Generation: 1, SpecDigest: digest,
		Blobs:    map[string]string{"main": "/var/lib/mecharion/blobs/sha256/ab/abcd"},
		Workload: &spec.Workload{Runtime: "docker", Docker: raw},
	}
}

// ── Probe ───────────────────────────────────────────────────────────────

func TestProbeAvailable(t *testing.T) {
	rt, fake := newRT(t)
	fake.Set("docker version --format {{json .}}", command.Result{
		Stdout: `{"Client":{"Version":"29.7.1"},"Server":{"Version":"29.7.1","Os":"linux","Arch":"amd64"}}`,
	})
	fake.Set("docker compose version --short", command.Result{Stdout: "5.4.0\n"})

	cap, err := rt.Probe(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if !cap.Available || cap.Version != "29.7.1" {
		t.Errorf("应报可用且版本 29.7.1，实际 %+v", cap)
	}
	if cap.Detail["compose"] != "5.4.0" {
		t.Errorf("compose 版本应一并上报，实际 %v", cap.Detail)
	}
}

// TestProbeClientOnly 钉住「装了客户端但连不上 daemon」。
//
// 这是最常见的情形。判据是 **server 版本能取到**，而不是「docker 命令存在」——
// 后者会让放置通过，然后在真正部署时才失败。
func TestProbeClientOnly(t *testing.T) {
	rt, fake := newRT(t)
	fake.Set("docker version --format {{json .}}", command.Result{
		ExitCode: 1,
		Stderr:   "Cannot connect to the Docker daemon at unix:///var/run/docker.sock.",
	})

	cap, err := rt.Probe(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if cap.Available {
		t.Fatal("连不上 daemon 时不该报可用")
	}
	// docker 自己的话原样带出去——它通常已经说清了是权限问题还是没起来
	if !strings.Contains(cap.Reason, "Cannot connect") {
		t.Errorf("Reason 应带上 docker 自己的错误，实际 %q", cap.Reason)
	}
}

func TestProbeNoDocker(t *testing.T) {
	rt, fake := newRT(t)
	fake.NotFound = true

	cap, err := rt.Probe(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if cap.Available {
		t.Fatal("没有 docker 命令时不该报可用")
	}
	// 错误要可操作：告诉用户怎么装
	if !strings.Contains(cap.Reason, "docker") {
		t.Errorf("Reason 应指导用户怎么办，实际 %q", cap.Reason)
	}
}

// ── 标签纪律 ────────────────────────────────────────────────────────────

// TestRefusesToTouchUnlabeledContainer 是本包**最重要**的一条测试。
//
// 默认情况下这台 dockerd 是用户自己的，上面跑着别人的东西。
// 漏一处就可能删掉与 Mecharion 无关的生产容器（ADR-0011 把这条列为
// 高风险点）。因此：**每一个会改变或删除容器的操作都要拒绝无标签容器。**
func TestRefusesToTouchUnlabeledContainer(t *testing.T) {
	// 一个同名、但是用户自己建的容器
	foreign := inspectJSON("running", map[string]string{
		"com.example.owner": "someone-else",
	})

	cases := []struct {
		name string
		call func(*Runtime) error
	}{
		{"Materialize", func(r *Runtime) error {
			_, err := r.Materialize(ctx(), workload(t, "d1"))
			return err
		}},
		{"Start", func(r *Runtime) error {
			return r.Start(ctx(), runtime.Ref{Native: "mecharion-web-default"})
		}},
		{"Stop", func(r *Runtime) error {
			return r.Stop(ctx(), runtime.Ref{Native: "mecharion-web-default"}, runtime.StopOpts{})
		}},
		{"Remove", func(r *Runtime) error {
			return r.Remove(ctx(), runtime.Ref{Native: "mecharion-web-default"})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, fake := newRT(t)
			fake.SetPrefix("docker inspect", command.Result{Stdout: foreign})
			fake.SetPrefix("docker load", command.Result{
				Stdout: "Loaded image: webapp:1.0\n",
			})

			err := tc.call(rt)
			if err == nil {
				t.Fatalf("%s 不该碰一个没有 Mecharion 标签的同名容器", tc.name)
			}
			if !strings.Contains(err.Error(), "标签") {
				t.Errorf("错误信息应说清是标签问题，实际: %v", err)
			}
			// **一次都不能动它**
			for _, c := range fake.Calls() {
				for _, verb := range []string{"docker rm", "docker stop", "docker start"} {
					if strings.HasPrefix(c, verb) {
						t.Errorf("%s 执行了 %q —— 那会动到用户自己的容器", tc.name, c)
					}
				}
			}
		})
	}
}

// TestObserveIgnoresUnlabeledContainer 钉住观测侧同样按标签过滤。
//
// 报上去会让 UI 显示一个我们既不管理也无权动的容器。
func TestObserveIgnoresUnlabeledContainer(t *testing.T) {
	rt, fake := newRT(t)
	fake.SetPrefix("docker inspect", command.Result{
		Stdout: inspectJSON("running", map[string]string{"other": "x"}),
	})

	st, err := rt.Observe(ctx(), runtime.Ref{Native: "mecharion-web-default"})
	if err != nil {
		t.Fatal(err)
	}
	if st.State != runtime.StateAbsent {
		t.Errorf("无标签的同名容器应当被当作不存在，实际 %s", st.State)
	}
}

// ── Materialize ─────────────────────────────────────────────────────────

// TestMaterializeCreatesWithLabels 钉住新建容器带全套标签。
func TestMaterializeCreatesWithLabels(t *testing.T) {
	rt, fake := newRT(t)
	fake.SetPrefix("docker load", command.Result{Stdout: "Loaded image: webapp:1.0\n"})
	fake.SetPrefix("docker inspect", command.Result{
		ExitCode: 1, Stderr: "Error: No such object: mecharion-web-default",
	})

	ref, err := rt.Materialize(ctx(), workload(t, "digest-aaa"))
	if err != nil {
		t.Fatal(err)
	}
	if ref.Native != "mecharion-web-default" {
		t.Errorf("容器名应与 systemd unit 同构，实际 %q", ref.Native)
	}

	create := findCall(fake, "docker create")
	if create == "" {
		t.Fatal("应当执行 docker create")
	}
	for _, want := range []string{
		LabelManagedBy + "=" + ManagedByValue,
		LabelComponent + "=web",
		LabelRole + "=default",
		LabelSpecDigest + "=digest-aaa",
	} {
		if !strings.Contains(create, want) {
			t.Errorf("create 应带标签 %s，实际:\n%s", want, create)
		}
	}
}

// TestMaterializeIsNoopWhenDigestMatches 钉住幂等。
//
// digest 相同意味着期望状态没变，此时重建容器等于无谓地中断服务。
func TestMaterializeIsNoopWhenDigestMatches(t *testing.T) {
	rt, fake := newRT(t)
	fake.SetPrefix("docker load", command.Result{Stdout: "Loaded image: webapp:1.0\n"})
	fake.SetPrefix("docker inspect", command.Result{
		Stdout: inspectJSON("running", ours("digest-aaa")),
	})

	if _, err := rt.Materialize(ctx(), workload(t, "digest-aaa")); err != nil {
		t.Fatal(err)
	}
	for _, c := range fake.Calls() {
		if strings.HasPrefix(c, "docker create") || strings.HasPrefix(c, "docker rm") {
			t.Errorf("digest 未变时不该重建容器，却执行了 %q", c)
		}
	}
}

// TestMaterializeRecreatesWhenDigestDiffers 钉住**容器不可变**。
//
// env / mount / port / command 在创建时固化，`docker update` 改不了它们——
// 配置变了只能删掉重建。
func TestMaterializeRecreatesWhenDigestDiffers(t *testing.T) {
	rt, fake := newRT(t)
	fake.SetPrefix("docker load", command.Result{Stdout: "Loaded image: webapp:1.0\n"})
	fake.SetPrefix("docker inspect", command.Result{
		Stdout: inspectJSON("running", ours("digest-OLD")),
	})

	if _, err := rt.Materialize(ctx(), workload(t, "digest-NEW")); err != nil {
		t.Fatal(err)
	}
	if findCall(fake, "docker rm") == "" {
		t.Error("digest 变了应当先删掉旧容器")
	}
	create := findCall(fake, "docker create")
	if !strings.Contains(create, LabelSpecDigest+"=digest-NEW") {
		t.Errorf("新容器应带新的 digest 标签，实际:\n%s", create)
	}
}

// TestMaterializeBuildsExpectedArgs 钉住各字段翻译成正确的 docker 参数。
func TestMaterializeBuildsExpectedArgs(t *testing.T) {
	rt, fake := newRT(t)
	fake.SetPrefix("docker load", command.Result{Stdout: "Loaded image: webapp:1.0\n"})
	fake.SetPrefix("docker inspect", command.Result{ExitCode: 1, Stderr: "No such object"})

	w := workload(t, "d1", func(d *dockerArgs) {
		d.Env = map[string]string{"B_VAR": "2", "A_VAR": "1"}
		d.User = "999:999"
		d.Network = "bridge"
		d.Restart = "unless-stopped"
		d.Mounts = []mountArg{
			{From: "/var/lib/mecharion/apps/web", To: "/data"},
			{From: "/etc/mecharion/apps/web", To: "/etc/app", ReadOnly: true},
		}
		d.Ports = []portArg{{Host: "8080", Container: 80, Protocol: "tcp"}}
		d.CapAdd = []string{"NET_ADMIN"}
		d.Ulimits = map[string]int{"nofile": 65536}
		d.Command = []string{"/webapp"}
		d.Args = []string{"--config", "/etc/app/app.yaml"}
	})
	if _, err := rt.Materialize(ctx(), w); err != nil {
		t.Fatal(err)
	}

	create := findCall(fake, "docker create")
	for _, want := range []string{
		"--user 999:999",
		"--network bridge",
		"--restart unless-stopped",
		"--env A_VAR=1",
		"--env B_VAR=2",
		"--volume /var/lib/mecharion/apps/web:/data",
		"--volume /etc/mecharion/apps/web:/etc/app:ro",
		"--publish 8080:80/tcp",
		"--cap-add NET_ADMIN",
		"--ulimit nofile=65536:65536",
		"--entrypoint /webapp",
		"webapp:1.0 --config /etc/app/app.yaml",
	} {
		if !strings.Contains(create, want) {
			t.Errorf("create 里应含 %q，实际:\n%s", want, create)
		}
	}

	// **环境变量按名字排序**：否则同一份规格每次产出不同的参数顺序，
	// 日志与测试都会跟着抖
	if strings.Index(create, "A_VAR") > strings.Index(create, "B_VAR") {
		t.Error("env 应按名字排序，让生成的命令行稳定")
	}
}

// TestMaterializeRejectsMissingImageBlob 钉住镜像必须来自 Pack 的 blob。
//
// 不从 registry 拉是 hermetic 约束的一部分（ADR-0015）。
func TestMaterializeRejectsMissingImageBlob(t *testing.T) {
	rt, _ := newRT(t)
	w := workload(t, "d1", func(d *dockerArgs) { d.ImageBlob = "" })
	_, err := rt.Materialize(ctx(), w)
	if err == nil {
		t.Fatal("没有 imageBlob 应当报错")
	}
	if !strings.Contains(err.Error(), "imageBlob") {
		t.Errorf("错误应指名到 imageBlob，实际: %v", err)
	}
}

// ── docker load 输出解析 ────────────────────────────────────────────────

// TestParseLoadedImage 钉住两种输出形态都认。
//
// 只认带标签的那种，会让「用 docker save 一个无标签镜像」的 Pack 静默失败。
func TestParseLoadedImage(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Loaded image: webapp:1.0\n", "webapp:1.0"},
		{"Loaded image ID: sha256:abc123\n", "sha256:abc123"},
		{"a\nLoaded image: repo/app:v2\nb\n", "repo/app:v2"},
	}
	for _, tc := range cases {
		got, err := parseLoadedImage(tc.in)
		if err != nil {
			t.Errorf("解析 %q: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("解析 %q = %q，期望 %q", tc.in, got, tc.want)
		}
	}

	if _, err := parseLoadedImage("something went wrong\n"); err == nil {
		t.Error("取不到镜像引用时应当报错，而不是返回空串")
	}
}

// ── Observe ─────────────────────────────────────────────────────────────

func TestObserveStateMapping(t *testing.T) {
	cases := []struct {
		status   string
		exitCode int
		want     runtime.State
	}{
		{"created", 0, runtime.StateStopped},
		{"running", 0, runtime.StateRunning},
		{"restarting", 0, runtime.StateStarting},
		{"paused", 0, runtime.StateDegraded},
		{"exited", 1, runtime.StateFailed},
		{"dead", 137, runtime.StateFailed},
		// **正常退出的容器不该显示成 failed**——一个被停掉的服务
		// 与一个崩掉的服务，运维要做的事完全不同
		{"exited", 0, runtime.StateStopped},
	}
	for _, tc := range cases {
		rt, fake := newRT(t)
		fake.SetPrefix("docker inspect", command.Result{
			Stdout: inspectJSON(tc.status, ours("d1"), func(c *containerJSON) {
				c.State.ExitCode = tc.exitCode
			}),
		})
		st, err := rt.Observe(ctx(), runtime.Ref{Native: "mecharion-web-default"})
		if err != nil {
			t.Fatal(err)
		}
		if st.State != tc.want {
			t.Errorf("%s(exit=%d) 应映射为 %s，实际 %s",
				tc.status, tc.exitCode, tc.want, st.State)
		}
	}
}

// TestObserveNativeHealth 钉住容器原生 HEALTHCHECK 的处理。
func TestObserveNativeHealth(t *testing.T) {
	withHealth := func(s string) func(*containerJSON) {
		return func(c *containerJSON) {
			c.State.Health = &struct{ Status string }{Status: s}
		}
	}

	rt, fake := newRT(t)
	fake.SetPrefix("docker inspect", command.Result{
		Stdout: inspectJSON("running", ours("d1"), withHealth("healthy")),
	})
	st, _ := rt.Observe(ctx(), runtime.Ref{Native: "mecharion-web-default"})
	if st.Health != runtime.HealthPassing || st.State != runtime.StateRunning {
		t.Errorf("healthy 应是 Running + HealthPassing，实际 %s/%s", st.State, st.Health)
	}

	// **starting 期间不该报 Running**：上层会以为已经就绪
	rt, fake = newRT(t)
	fake.SetPrefix("docker inspect", command.Result{
		Stdout: inspectJSON("running", ours("d1"), withHealth("starting")),
	})
	st, _ = rt.Observe(ctx(), runtime.Ref{Native: "mecharion-web-default"})
	if st.State != runtime.StateStarting {
		t.Errorf("原生健康检查还在起步期时应报 Starting，实际 %s", st.State)
	}
}

func TestObserveAbsent(t *testing.T) {
	rt, fake := newRT(t)
	fake.SetPrefix("docker inspect", command.Result{
		ExitCode: 1, Stderr: "Error: No such object: mecharion-web-default",
	})
	st, err := rt.Observe(ctx(), runtime.Ref{Native: "mecharion-web-default"})
	if err != nil {
		t.Fatalf("容器不存在不是故障: %v", err)
	}
	if st.State != runtime.StateAbsent {
		t.Errorf("应报 Absent，实际 %s", st.State)
	}
}

// ── Reload ──────────────────────────────────────────────────────────────

// TestReloadUnsupported 钉住 docker 不实现热加载。
//
// 上层会降级为重启，那条路径已经实现并测过。
func TestReloadUnsupported(t *testing.T) {
	rt, _ := newRT(t)
	if err := rt.Reload(ctx(), runtime.Ref{Native: "x"}); err != runtime.ErrReloadUnsupported {
		t.Errorf("docker 应返回 ErrReloadUnsupported，实际 %v", err)
	}
}

// ── ExecIn ──────────────────────────────────────────────────────────────

// TestExecInDistinguishesCannotExec 钉住 ADR-0032 的区分在 docker 侧成立。
func TestExecInDistinguishesCannotExec(t *testing.T) {
	ref := runtime.Ref{Native: "mecharion-web-default"}

	// 命令跑了但失败 → 原样返回结果，**不是** error
	rt, fake := newRT(t)
	fake.SetPrefix("docker exec", command.Result{ExitCode: 1, Stderr: "no response"})
	res, err := rt.ExecIn(ctx(), ref, []string{"pg_isready"})
	if err != nil {
		t.Fatalf("命令失败不该变成 error（那会被当成「探不了」）: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("应原样返回退出码，实际 %d", res.ExitCode)
	}

	// 容器没在跑 → error（探不了）
	rt, fake = newRT(t)
	fake.SetPrefix("docker exec", command.Result{
		ExitCode: 1,
		Stderr:   "Error response from daemon: Container mecharion-web-default is not running",
	})
	if _, err := rt.ExecIn(ctx(), ref, []string{"pg_isready"}); err == nil {
		t.Error("容器没在跑属于「探不了」，应当返回 error")
	}
}

// ── 辅助 ────────────────────────────────────────────────────────────────

func findCall(f *command.Fake, prefix string) string {
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, prefix) {
			return c
		}
	}
	return ""
}

// TestExecInRejectsEmptyNative 钉住一个曾经真发生过的 bug。
//
// 调和器构造 Ref 时漏了 Native（systemd 的 ExecIn 用不到它，因此在只有
// 一个实现时这个洞不可见）。不拦的话 docker 报的是
// 「invalid container name or ID: value is empty」——**看起来像探针失败**，
// 排查会从容器和探针命令查起，方向全错。
func TestExecInRejectsEmptyNative(t *testing.T) {
	rt, fake := newRT(t)
	_, err := rt.ExecIn(ctx(),
		runtime.Ref{Component: "web", Role: "default"}, // 故意不给 Native
		[]string{"pg_isready"})
	if err == nil {
		t.Fatal("Ref 没有容器名时应当报错，而不是去 docker exec 一个空名字")
	}
	if !strings.Contains(err.Error(), "容器名") {
		t.Errorf("错误应说清缺的是什么，实际: %v", err)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("不该真的去执行 docker，实际执行了 %v", fake.Calls())
	}
}
