package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/mecharion/mecharion/internal/command"
	rt "github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/state"
)

// ── 假 Runtime ──────────────────────────────────────────────────────────

// fakeRuntime 记录被调用的动作，并允许测试摆布观测状态。
type fakeRuntime struct {
	mu sync.Mutex

	// state 是 Observe 要返回的状态；Start/Stop 会改动它。
	state rt.State
	// startErr 让 Start 失败。
	startErr error
	// reloadErr 让 Reload 失败；置为 ErrReloadUnsupported 可测降级。
	reloadErr error

	actions []string
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{state: rt.StateAbsent}
}

func (f *fakeRuntime) Name() string { return "systemd" }

func (f *fakeRuntime) Probe(context.Context) (rt.Capability, error) {
	return rt.Capability{Available: true, Version: "252"}, nil
}

func (f *fakeRuntime) record(a string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, a)
}

func (f *fakeRuntime) Actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.actions...)
}

func (f *fakeRuntime) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = nil
}

func (f *fakeRuntime) Materialize(_ context.Context, w rt.WorkloadSpec) (rt.Ref, error) {
	f.record("materialize")
	f.mu.Lock()
	if f.state == rt.StateAbsent {
		f.state = rt.StateStopped
	}
	f.mu.Unlock()
	return rt.Ref{
		Runtime: "systemd", Component: w.Component, Role: w.Role,
		Generation: w.Generation,
		Native:     "mecharion-" + w.Component + "-" + w.Role + ".service",
		// 每一代一个不同的镜像，回收测试才能分辨「删对了哪一个」。
		// 真的 systemd runtime 当然不产镜像——这里要测的是台账与
		// 回收清单这条路，Runtime 是哪一个不重要。
		Images: []string{fmt.Sprintf("fake-image:%d", w.Generation)},
	}, nil
}

// RefFor 只推名字，**不改状态、不产镜像**——这正是卸载路径要的。
// 它记一条 refFor，测试才能断言「卸载没有顺手物化一遍」。
func (f *fakeRuntime) RefFor(w rt.WorkloadSpec) (rt.Ref, error) {
	f.record("refFor")
	return rt.Ref{
		Runtime: "systemd", Component: w.Component, Role: w.Role,
		Generation: w.Generation,
		Native:     "mecharion-" + w.Component + "-" + w.Role + ".service",
	}, nil
}

func (f *fakeRuntime) Start(context.Context, rt.Ref) error {
	f.record("start")
	if f.startErr != nil {
		return f.startErr
	}
	f.mu.Lock()
	f.state = rt.StateRunning
	f.mu.Unlock()
	return nil
}

func (f *fakeRuntime) Stop(context.Context, rt.Ref, rt.StopOpts) error {
	f.record("stop")
	f.mu.Lock()
	f.state = rt.StateStopped
	f.mu.Unlock()
	return nil
}

func (f *fakeRuntime) Reload(context.Context, rt.Ref) error {
	f.record("reload")
	return f.reloadErr
}

func (f *fakeRuntime) Observe(_ context.Context, ref rt.Ref) (rt.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return rt.Status{State: f.state, Native: ref.Native, Raw: map[string]string{}}, nil
}

func (f *fakeRuntime) Remove(context.Context, rt.Ref) error {
	f.record("remove")
	f.mu.Lock()
	f.state = rt.StateAbsent
	f.mu.Unlock()
	return nil
}

func (f *fakeRuntime) Logs(context.Context, rt.Ref, rt.LogOpts) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

// ── 夹具 ────────────────────────────────────────────────────────────────

// fixture 是一次调和测试的全套环境。
type fixture struct {
	t       *testing.T
	Root    string // 假装的文件系统根
	DataDir string
	Store   *state.Store
	RT      *fakeRuntime
	Runner  *command.Fake
	R       *Reconciler
}

// requireSymlink 跳过不允许建软链的环境。
//
// `current` 软链的原子切换是整个 generation 模型的地基（ADR-0008），
// 没有它调和器无从测起。Windows 默认要求管理员权限或开发者模式——
// 而 mechlet 只跑在 Linux 上，真机验证在 test/node 容器里做。
func requireSymlink(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		return
	}
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "t"), filepath.Join(dir, "l")); err != nil {
		t.Skip("本机不允许创建软链（Windows 需开发者模式）；调和器的验证在容器里")
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireSymlink(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "var", "lib", "mecharion")

	store, err := state.New(filepath.Join(dataDir, "mechlet"))
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakeRuntime()
	runner := command.NewFake()
	runner.SetPrefix("getent ", command.Result{ExitCode: 2})

	f := &fixture{
		t: t, Root: root, DataDir: dataDir, Store: store, RT: fake, Runner: runner,
	}
	f.R = &Reconciler{
		Store:    store,
		Runtimes: rt.NewRegistry(fake),
		BlobDir:  filepath.Join(dataDir, "blobs"),
		PackDir:  filepath.Join(dataDir, "packs"),
		Runner:   runner,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return f
}

func (f *fixture) path(parts ...string) string {
	return filepath.Join(append([]string{f.Root}, parts...)...)
}

// Reconcile 封一层，顺手 Seal 规格。
func (f *fixture) Reconcile(s *spec.ResolvedSpec) (*Report, error) {
	f.t.Helper()
	if err := spec.Seal(s); err != nil {
		f.t.Fatal(err)
	}
	return f.R.Reconcile(context.Background(), s)
}

// MustReconcile 要求调和成功。
func (f *fixture) MustReconcile(s *spec.ResolvedSpec) *Report {
	f.t.Helper()
	rep, err := f.Reconcile(s)
	if err != nil {
		f.t.Fatalf("调和失败: %v\n报告: %s", err, rep.Summary())
	}
	return rep
}

// Instance 读回本地状态。
func (f *fixture) Instance(component, role string) *state.Instance {
	f.t.Helper()
	in, err := f.Store.LoadInstance(state.InstanceKey(component, role))
	if err != nil {
		f.t.Fatal(err)
	}
	return in
}

// webappSpec 构造一份最小的、可调和的 go-webapp 规格。
func (f *fixture) webappSpec(mut ...func(*spec.ResolvedSpec)) *spec.ResolvedSpec {
	f.t.Helper()
	home := f.path("opt", "mecharion", "apps", "webapp")
	config := f.path("etc", "mecharion", "apps", "webapp")
	data := f.path("var", "lib", "mecharion", "apps", "webapp")

	s := &spec.ResolvedSpec{
		SchemaVersion: spec.SchemaVersion,
		Site:          spec.SiteRef{Name: "s1", Kind: "standalone"},
		Component:     "webapp", Role: "default", ConfigGroup: "default",
		Node: spec.NodeRef{Name: "node-1", Address: "10.0.0.1"},
		Pack: spec.PackRef{Name: "go-webapp", Version: "1.2.0", Revision: 1},
		Paths: map[string]spec.PathValue{
			"home":   {Name: "home", Values: []string{home}, Kind: "single", Mode: "0755"},
			"config": {Name: "config", Values: []string{config}, Kind: "single", Mode: "0750"},
			"data":   {Name: "data", Values: []string{data}, Kind: "single", Mode: "0750"},
		},
		Resources: []spec.Resource{
			{
				ID: "template:app.yaml", Type: "template",
				Args:        mustJSON(map[string]any{"dest": config + "/app.yaml", "content": "port: 8080\n"}),
				DriftPolicy: "report", Notify: "reload",
			},
		},
		Workload: &spec.Workload{
			Runtime: "systemd",
			Systemd: &spec.SystemdWorkload{
				Exec: home + "/current/bin/webapp --config " + config + "/app.yaml",
			},
		},
		Topology: spec.Topology{Roles: map[string][]spec.Instance{
			"default": {{Node: "node-1", Address: "10.0.0.1", Ordinal: 0}},
		}},
	}
	for _, m := range mut {
		m(s)
	}
	return s
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// setContent 改动模板资源的内容，用于制造「配置变了」。
func setContent(s *spec.ResolvedSpec, content string) {
	for i, r := range s.Resources {
		if r.ID == "template:app.yaml" {
			var args map[string]any
			_ = json.Unmarshal(r.Args, &args)
			args["content"] = content
			s.Resources[i].Args = mustJSON(args)
			return
		}
	}
}

// readLink 读一条软链，失败即终止。
func readLink(t *testing.T, p string) string {
	t.Helper()
	got, err := os.Readlink(p)
	if err != nil {
		t.Fatalf("读取软链 %s: %v", p, err)
	}
	return got
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// ExecIn 让替身满足 Runtime 接口。
//
// 记一笔调用即可：这个包测的是调和编排，「命令在哪执行」是 Runtime
// 自己的事，systemd 与 docker 的实现各有自己的测试。
func (f *fakeRuntime) ExecIn(
	_ context.Context, _ rt.Ref, cmd []string,
) (command.Result, error) {
	f.record("exec:" + strings.Join(cmd, " "))
	return command.Result{}, nil
}
