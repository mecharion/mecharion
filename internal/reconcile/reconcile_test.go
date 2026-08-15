package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	rt "github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/spec"
)

// TestFirstInstall 走通首次安装的完整链路。
func TestFirstInstall(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec()

	rep := f.MustReconcile(s)

	if rep.Result != ResultChanged {
		t.Errorf("首次安装应当报 changed，实际 %s", rep.Result)
	}
	if rep.Generation != 1 {
		t.Errorf("首个 generation 应当是 1，实际 %d", rep.Generation)
	}
	if !rep.Switched {
		t.Error("首次安装必须切一次 current 软链")
	}

	// ① paths 声明的目录都建出来了
	for _, name := range []string{"home", "config", "data"} {
		dir := s.Paths[name].First()
		fi, err := os.Stat(dir)
		if err != nil {
			t.Errorf("paths.%s 未创建: %v", name, err)
			continue
		}
		if !fi.IsDir() {
			t.Errorf("paths.%s 不是目录", name)
		}
	}

	// ② 资源落地
	body, err := os.ReadFile(filepath.Join(s.Paths["config"].First(), "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "port: 8080\n" {
		t.Errorf("渲染出的配置 = %q", body)
	}

	// ③ generation 目录 + current 软链
	home := s.Paths["home"].First()
	wantDir := filepath.Join(home, "generations", "0001-1.2.0-1")
	if rep.GenerationDir != wantDir {
		t.Errorf("generation 目录 = %q，期望 %q", rep.GenerationDir, wantDir)
	}
	if got := readLink(t, filepath.Join(home, CurrentLink)); got != wantDir {
		t.Errorf("current → %q，期望 %q", got, wantDir)
	}

	// ④ Runtime 被驱动了，且顺序是 物化 → 切软链 → 启动
	acts := f.RT.Actions()
	if len(acts) < 2 || acts[0] != "materialize" || acts[len(acts)-1] != "start" {
		t.Errorf("Runtime 动作顺序 = %v", acts)
	}
	if rep.Workload == nil || rep.Workload.State != rt.StateRunning {
		t.Errorf("工作负载应当在跑: %+v", rep.Workload)
	}

	// ⑤ 本地状态
	in := f.Instance("webapp", "default")
	if in == nil {
		t.Fatal("应当写下本地状态")
	}
	if in.CurrentGeneration != 1 || len(in.Generations) != 1 {
		t.Errorf("台账不对: %+v", in.Generations)
	}
	if in.Generations[0].Digest != s.Digest {
		t.Error("台账里的 digest 应当与规格一致")
	}
	if len(in.Paths) != 3 {
		t.Errorf("路径应当被固化: %v", in.Paths)
	}
}

// TestSameDigestIsNoop 钉住「同一份规格重复下发不产生 churn」。
//
// 调和每 60 秒跑一次。若每轮都切一次 generation，服务会被无休止地重启。
func TestSameDigestIsNoop(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec()

	first := f.MustReconcile(s)
	f.RT.Reset()

	second := f.MustReconcile(s)

	if second.Generation != first.Generation {
		t.Errorf("同一份规格不该产生新 generation：%d → %d",
			first.Generation, second.Generation)
	}
	if second.Switched {
		t.Error("同一份规格不该切软链")
	}
	if second.Result != ResultOK {
		t.Errorf("无差异时应报 ok，实际 %s（%v）", second.Result, second.Resources)
	}
	for _, a := range f.RT.Actions() {
		if a == "stop" || a == "start" || a == "reload" {
			t.Errorf("无差异时不该惊动进程，实际执行了 %v", f.RT.Actions())
			break
		}
	}
	if len(second.Notified) > 0 {
		t.Errorf("Diff 为空绝不能触发 notify，实际 %v", second.Notified)
	}
}

// TestConfigChangeProducesNewGeneration 钉住「配置变更也产生新 generation」。
//
// 于是回滚是统一动作：切回上一个 generation，不区分「回滚版本」还是
// 「回滚配置」（04-paths-and-storage §2）。
func TestConfigChangeProducesNewGeneration(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec()
	f.MustReconcile(s)

	s2 := f.webappSpec(func(x *spec.ResolvedSpec) {
		setContent(x, "port: 9090\n")
	})
	rep := f.MustReconcile(s2)

	if rep.Generation != 2 {
		t.Errorf("配置变了应当分配新 generation，实际 %d", rep.Generation)
	}
	if !rep.Switched {
		t.Error("新 generation 必须切软链")
	}

	home := s.Paths["home"].First()
	if got := readLink(t, filepath.Join(home, CurrentLink)); !strings.HasSuffix(got, "0002-1.2.0-1") {
		t.Errorf("current → %q", got)
	}
	// 旧 generation 目录必须完整保留——回滚靠的就是它还在
	if !exists(filepath.Join(home, "generations", "0001-1.2.0-1")) {
		t.Error("旧 generation 目录必须保留，否则回滚就得重新物化")
	}

	body, _ := os.ReadFile(filepath.Join(s.Paths["config"].First(), "app.yaml"))
	if string(body) != "port: 9090\n" {
		t.Errorf("新配置没落地: %q", body)
	}
}

// TestRollbackReusesRetainedGeneration 钉住「回滚是一次软链切换」。
func TestRollbackReusesRetainedGeneration(t *testing.T) {
	f := newFixture(t)
	v1 := f.webappSpec()
	f.MustReconcile(v1)

	v2 := f.webappSpec(func(x *spec.ResolvedSpec) { setContent(x, "port: 9090\n") })
	f.MustReconcile(v2)

	// 回滚 = 重新下发 v1 的规格
	back := f.webappSpec()
	rep := f.MustReconcile(back)

	if !rep.Rollback {
		t.Error("命中历史 digest 应当标记为回滚")
	}
	if rep.Generation != 1 {
		t.Errorf("应当切回 generation 1，实际 %d", rep.Generation)
	}
	if !rep.Switched {
		t.Error("回滚要切软链")
	}

	home := v1.Paths["home"].First()
	if got := readLink(t, filepath.Join(home, CurrentLink)); !strings.HasSuffix(got, "0001-1.2.0-1") {
		t.Errorf("current → %q，期望回到 0001", got)
	}

	in := f.Instance("webapp", "default")
	if in.CurrentGeneration != 1 {
		t.Errorf("台账里的 current = %d", in.CurrentGeneration)
	}
	// 被回滚掉的那个降级为 retained，仍然可以再切回去
	for _, g := range in.Generations {
		if g.Seq == 2 && g.State != "retained" {
			t.Errorf("原 active 应降级为 retained，实际 %s", g.State)
		}
	}
}

// TestPathsArePinned 钉住「路径固化后不可变」。
//
// 若不固化，用户改了 Node.Roots 或 Pack 改了默认路径，已装组件会静默
// 搬家，旧数据变成孤儿。
func TestPathsArePinned(t *testing.T) {
	f := newFixture(t)
	f.MustReconcile(f.webappSpec())

	moved := f.webappSpec(func(x *spec.ResolvedSpec) {
		p := x.Paths["data"]
		p.Values = []string{f.path("data1", "apps", "webapp")}
		x.Paths["data"] = p
	})
	_, err := f.Reconcile(moved)
	if err == nil {
		t.Fatal("路径变更必须被拒绝")
	}
	if !strings.Contains(err.Error(), "已固化") {
		t.Errorf("错误信息应说明是固化冲突: %v", err)
	}
}

// TestUnresolvedPlaceholderIsReplaced 钉住 generation 占位符的替换。
func TestUnresolvedPlaceholderIsReplaced(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec(func(x *spec.ResolvedSpec) {
		x.Workload.Systemd.Exec = spec.GenerationPlaceholder + "/bin/webapp"
		x.Resources = append(x.Resources, spec.Resource{
			ID: "file:marker", Type: "file",
			Args: mustJSON(map[string]any{
				"path":    spec.GenerationPlaceholder + "/marker.txt",
				"content": "x\n",
			}),
		})
	})

	rep := f.MustReconcile(s)

	// 占位符替换成了真实目录，文件落在 generation 里
	marker := filepath.Join(rep.GenerationDir, "marker.txt")
	if !exists(marker) {
		t.Errorf("占位符未被替换，%s 不存在", marker)
	}
}

// TestNoHomeRejectsPlaceholder 钉住「没有 home 就没有 generation 目录」。
func TestNoHomeRejectsPlaceholder(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec(func(x *spec.ResolvedSpec) {
		delete(x.Paths, "home")
		x.Workload = nil
		x.Resources = []spec.Resource{{
			ID: "file:x", Type: "file",
			Args: mustJSON(map[string]any{
				"path":    spec.GenerationPlaceholder + "/x",
				"content": "x",
			}),
		}}
	})

	_, err := f.Reconcile(s)
	if err == nil {
		t.Fatal("引用 generation 却没有 home，应当报错")
	}
	if !strings.Contains(err.Error(), "home") {
		t.Errorf("错误信息应指出缺少 home: %v", err)
	}
}

// TestHostConfigPackWithoutWorkload 钉住「无 workload 的 Pack 只落文件」。
//
// 主机配置与组件部署是同一个引擎（06-state-and-drift §3）。
func TestHostConfigPackWithoutWorkload(t *testing.T) {
	f := newFixture(t)
	confDir := f.path("etc", "sysctl.d")
	s := f.webappSpec(func(x *spec.ResolvedSpec) {
		x.Component = "host-tuning"
		x.Workload = nil
		x.Health = nil
		x.Paths = map[string]spec.PathValue{
			"config": {Name: "config", Values: []string{confDir}, Kind: "single", Mode: "0755"},
		}
		x.Resources = []spec.Resource{{
			ID: "file:99-tuning.conf", Type: "file",
			Args: mustJSON(map[string]any{
				"path": confDir + "/99-tuning.conf", "content": "vm.swappiness=1\n",
			}),
		}}
	})

	rep := f.MustReconcile(s)

	if rep.Workload != nil {
		t.Error("没有 workload 就不该有工作负载状态")
	}
	if !exists(filepath.Join(confDir, "99-tuning.conf")) {
		t.Error("文件应当落地")
	}
	for _, a := range f.RT.Actions() {
		t.Errorf("无 workload 时不该碰 Runtime，实际执行了 %s", a)
	}
}

// TestServiceStoppedByHandIsRestarted 钉住「手工停掉的服务会被拉起来」。
//
// 这正是常驻 Agent 相对 Ansible 的核心价值：只有周期性重读才能发现。
func TestServiceStoppedByHandIsRestarted(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec()
	f.MustReconcile(s)

	// 有人手工 systemctl stop 了
	f.RT.state = rt.StateStopped
	f.RT.Reset()

	rep := f.MustReconcile(s)

	if !f.RT.actionsContain("start") {
		t.Errorf("应当把服务拉回来，实际执行了 %v", f.RT.Actions())
	}
	if rep.Workload == nil || rep.Workload.State != rt.StateRunning {
		t.Errorf("拉起后应当在跑: %+v", rep.Workload)
	}
}

// TestFailedReconcileRecordsFailedGeneration 钉住失败的 generation 被记为 failed。
//
// 记下来而不是丢掉：目录留一个供诊断，而 state=failed 保证它不会被当成
// 回滚的落脚点——回滚到一个从没成功过的 generation 是灾难。
func TestFailedReconcileRecordsFailedGeneration(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec(func(x *spec.ResolvedSpec) {
		// 指向一个不存在的 blob，资源阶段必然失败
		x.Resources = append(x.Resources, spec.Resource{
			ID: "archive:main", Type: "archive",
			Args: mustJSON(map[string]any{
				"blob": "main", "dest": spec.GenerationPlaceholder,
			}),
		})
		x.Blobs = []spec.BlobRef{{
			Name: "main", SHA256: strings.Repeat("ab", 32), Size: 1,
		}}
	})

	rep, err := f.Reconcile(s)
	if err == nil {
		t.Fatal("载荷不在本地时应当失败")
	}
	if rep.Result != ResultFailed {
		t.Errorf("报告应当标记失败，实际 %s", rep.Result)
	}
	// 报告要说清是哪个资源出的事
	if !strings.Contains(rep.Error, "archive:main") {
		t.Errorf("失败原因应当指名资源: %s", rep.Error)
	}

	in := f.Instance("webapp", "default")
	if in == nil || len(in.Generations) != 1 {
		t.Fatalf("失败也应当记台账: %+v", in)
	}
	if in.Generations[0].State != "failed" {
		t.Errorf("应当记为 failed，实际 %s", in.Generations[0].State)
	}
	if in.CurrentGeneration != 0 {
		t.Error("失败的 generation 不该被设为 current")
	}
}

// TestPruneKeepsRollbackFootholds 钉住回收策略。
func TestPruneKeepsRollbackFootholds(t *testing.T) {
	f := newFixture(t)
	home := f.path("opt", "mecharion", "apps", "webapp")

	// 连续 5 次配置变更 → 5 个 generation，保留数默认 3
	for i := 0; i < 5; i++ {
		s := f.webappSpec(func(x *spec.ResolvedSpec) {
			setContent(x, "port: "+string(rune('1'+i))+"\n")
		})
		f.MustReconcile(s)
	}

	in := f.Instance("webapp", "default")
	if len(in.Generations) != 3 {
		t.Errorf("应当保留 3 个 generation，实际 %d: %+v", len(in.Generations), in.Generations)
	}
	if in.CurrentGeneration != 5 {
		t.Errorf("current = %d", in.CurrentGeneration)
	}

	entries, err := os.ReadDir(filepath.Join(home, "generations"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("磁盘上也应当只剩 3 个，实际 %v", names)
	}
	// 台账与磁盘必须一致——留一条指向已删目录的记录，会让回滚在
	// 「命中 digest」之后才发现目录不在，那时已经停了服务
	for _, g := range in.Generations {
		if !exists(g.Dir) {
			t.Errorf("台账里的 generation %04d 目录已不存在: %s", g.Seq, g.Dir)
		}
	}
}

// TestPrunedGenerationsHandOverTheirRefs 是 **M6 第 7 步的验收之一**：
// 被回收的那几代所引用的镜像与载荷，要在记录被删之前交进回收清单。
//
// 记录一删，这些引用就再也无从得知——磁盘上多出几百 MB 且没有任何线索
// 指向它们（22-upgrade §2.5 ②）。
func TestPrunedGenerationsHandOverTheirRefs(t *testing.T) {
	f := newFixture(t)

	for i := 0; i < 5; i++ {
		s := f.webappSpec(func(x *spec.ResolvedSpec) {
			setContent(x, "port: "+string(rune('1'+i))+"\n")
		})
		f.MustReconcile(s)
	}

	in := f.Instance("webapp", "default")
	// 保留下来的三代（3、4、5）各自记着自己的镜像
	for _, g := range in.Generations {
		want := fmt.Sprintf("fake-image:%d", g.Seq)
		if len(g.Images) != 1 || g.Images[0] != want {
			t.Errorf("generation %04d 的镜像应当是 %q，实际 %v", g.Seq, want, g.Images)
		}
	}

	// 被回收的 1、2 两代的镜像进了清单
	g, err := f.Store.LoadGarbage()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range g.Images {
		got = append(got, it.ID)
	}
	want := []string{"fake-image:1", "fake-image:2"}
	if !equalStrings(got, want) {
		t.Errorf("回收清单应当是 %v，实际 %v", want, got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestMissingGenerationDirTriggersRematerialize 钉住目录被删后重新物化。
func TestMissingGenerationDirTriggersRematerialize(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec()
	first := f.MustReconcile(s)

	// 有人把 generation 目录删了
	if err := os.RemoveAll(first.GenerationDir); err != nil {
		t.Fatal(err)
	}

	rep := f.MustReconcile(s)
	if rep.Generation == first.Generation {
		t.Error("目录不在了就该重新物化一个新的 generation，而不是当作已就绪")
	}
	if !exists(rep.GenerationDir) {
		t.Error("新 generation 目录应当被建出来")
	}
}

// TestDryRunUnaffectedByRuntimeErrors 确认 Report 在失败时仍带着进度。
func TestReportCarriesProgressOnFailure(t *testing.T) {
	f := newFixture(t)
	f.RT.startErr = errStart

	rep, err := f.Reconcile(f.webappSpec())
	if err == nil {
		t.Fatal("Start 失败应当让调和失败")
	}
	if rep == nil {
		t.Fatal("失败时也必须返回报告——那是排障最需要的东西")
	}
	if len(rep.Resources) == 0 {
		t.Error("报告应当带着已经走完的资源阶段")
	}
	if rep.Generation == 0 {
		t.Error("报告应当带着 generation 序号")
	}
}

var errStart = &startError{}

type startError struct{}

func (*startError) Error() string { return "模拟启动失败" }

// actionsContain 是 fakeRuntime 的小辅助。
func (f *fakeRuntime) actionsContain(a string) bool {
	for _, x := range f.Actions() {
		if x == a {
			return true
		}
	}
	return false
}

// TestOwnershipOnPaths 验证 paths 的 mode 被应用（Linux）。
func TestOwnershipOnPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不实现 Unix 权限位")
	}
	f := newFixture(t)
	s := f.webappSpec()
	f.MustReconcile(s)

	fi, err := os.Stat(s.Paths["config"].First())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o750 {
		t.Errorf("paths.config 权限 = %04o，期望 0750", fi.Mode().Perm())
	}
}
