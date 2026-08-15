package mechd

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mecharion/mecharion/internal/packindex"
	"github.com/mecharion/mecharion/internal/protocol"
	"github.com/mecharion/mecharion/internal/store"
	"github.com/mecharion/mecharion/internal/vault"
)

// ── 夹具 ────────────────────────────────────────────────────────────────

type fixture struct {
	t       *testing.T
	svc     *Service
	site    store.Site
	notify  *fakeNotifier
	packDir string
	// clock 是可推的时钟。
	//
	// 滚动升级的健康门禁有一个 30 秒的稳定窗口（22-multi-node §2.5），
	// 拿真实时间测它意味着每条测试至少跑半分钟——那种测试没人会在改一行
	// 代码之后跑。推时钟还让「窗口差 1 秒」这类边界变得可写。
	clock *fakeClock
}

// fakeClock 是一个只会被显式推动的时钟。
type fakeClock struct{ at time.Time }

func (c *fakeClock) Now() time.Time { return c.at }

// advance 把时钟往前推。
func (c *fakeClock) advance(d time.Duration) { c.at = c.at.Add(d) }

type fakeNotifier struct{ woken []string }

func (f *fakeNotifier) Notify(node string) { f.woken = append(f.woken, node) }

func (f *fakeNotifier) wokeUp(node string) bool {
	for _, n := range f.woken {
		if n == node {
			return true
		}
	}
	return false
}

func ctx() context.Context { return context.Background() }

// newFixture 起一套完整的控制面：库、保管库、Pack 集合、一个站点、三台节点。
func newFixture(t *testing.T, nodes ...string) *fixture {
	t.Helper()
	dir := t.TempDir()

	s, err := store.Open(ctx(), store.Options{Path: filepath.Join(dir, "mechd.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	v, err := vault.Open(ctx(), s, vault.Options{KeyPath: filepath.Join(dir, "secret.key")})
	if err != nil {
		t.Fatal(err)
	}

	idx := packindex.New()
	packDir := packsRoot(t)
	if err := idx.AddDir(packDir); err != nil {
		t.Fatalf("加载 Pack 集合: %v", err)
	}

	notify := &fakeNotifier{}
	clock := &fakeClock{at: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	svc := &Service{
		Store: s, Repos: s.Repos(), Vault: v, Packs: idx,
		BlobDir: filepath.Join(dir, "blobs"), Notify: notify,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: clock.Now,
	}

	site, err := svc.Repos.Sites().Create(ctx(), store.Site{
		Name: DefaultSite, Kind: "cluster",
	})
	if err != nil {
		t.Fatal(err)
	}

	f := &fixture{t: t, svc: svc, site: site, notify: notify,
		packDir: packDir, clock: clock}
	for i, name := range nodes {
		f.addNode(name, "10.0.0."+itoa(i+1))
	}
	return f
}

func (f *fixture) addNode(name, addr string) store.Node {
	f.t.Helper()
	n, err := f.svc.Repos.Nodes().Upsert(ctx(), store.Node{
		SiteID: f.site.ID, Name: name, Address: addr,
		Labels: map[string]string{"rack": "r1"}, Status: store.NodePending,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	// 事实：defaultFrom 要用
	if err := f.svc.Repos.Status().PutFacts(ctx(), store.NodeFacts{
		NodeID: n.ID, CollectedAt: time.Now(),
		Facts: map[string]any{
			"memory": map[string]any{"total": "32Gi"},
			"cpu":    map[string]any{"cores": 8},
		},
	}); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func packsRoot(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", "examples", "packs")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("没有源码树，跳过（容器内运行）")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// ── deploy ──────────────────────────────────────────────────────────────

// TestDeployEndToEnd 是第 8 步的验收：一条 deploy 走完整条链路。
func TestDeployEndToEnd(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	res := deployZK(t, f, "n1", "n2", "n3")

	if len(res.Specs) != 3 {
		t.Fatalf("应产出 3 份规格，实际 %d 份", len(res.Specs))
	}
	// ordinal 已固化：三份规格各不相同（myid 不同）
	seen := map[string]bool{}
	for _, sp := range res.Specs {
		seen[sp.Digest] = true
	}
	if len(seen) != 3 {
		t.Errorf("三个实例的 digest 应各不相同，实际只有 %d 个", len(seen))
	}

	// 落库了
	comp, err := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "zk-main")
	if err != nil {
		t.Fatalf("组件应已落库: %v", err)
	}
	insts, err := f.svc.Repos.Instances().List(ctx(), comp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 3 {
		t.Errorf("应有 3 个实例落库，实际 %d 个", len(insts))
	}

	// 三个节点都被唤醒
	for _, n := range []string{"n1", "n2", "n3"} {
		if !f.notify.wokeUp(n) {
			t.Errorf("节点 %s 应当被唤醒", n)
		}
	}
}

// TestDeployRefusesOverwrite 钉住「重复 deploy 默认拒绝」。
//
// 一次手滑的重复 deploy 不该悄悄改掉线上参数。
func TestDeployRefusesOverwrite(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployJDK(t, f, "n1", "n2", "n3")
	req := DeployRequest{
		Pack: "zookeeper", Component: "zk-main", Profile: "ensemble",
		Roles: map[string][]string{"server": {"n1", "n2", "n3"}},
	}
	if _, err := f.svc.Deploy(ctx(), req); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.Deploy(ctx(), req)
	if err == nil {
		t.Fatal("重复 deploy 应当被拒绝")
	}
	if !strings.Contains(err.Error(), "--update") {
		t.Errorf("错误信息应告诉用户怎么办，实际: %v", err)
	}

	req.Update = true
	if _, err := f.svc.Deploy(ctx(), req); err != nil {
		t.Errorf("加了 --update 应当放行: %v", err)
	}
}

// TestScaleOutKeepsOrdinals 钉住 ADR-0028 在真实链路上成立。
//
// 扩容改掉已有 ordinal 会让 ZooKeeper 的 myid 全体变化，集群当场损坏。
func TestScaleOutKeepsOrdinals(t *testing.T) {
	f := newFixture(t, "n2", "n3", "n4")
	// 先用 n2/n3/n4 部署
	deployZK(t, f, "n2", "n3", "n4")
	comp, _ := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "zk-main")
	before := ordinalsByNode(t, f, comp.ID)

	// 加入 n1 —— 它的名字排在最前
	f.addNode("n1", "10.0.0.9")
	if _, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "jdk11", Component: "jdk11", Update: true,
		Roles: map[string][]string{"runtime": {"n1", "n2", "n3", "n4"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "zookeeper", Component: "zk-main", Profile: "ensemble", Update: true,
		Roles: map[string][]string{"server": {"n1", "n2", "n3", "n4"}},
	}); err != nil {
		t.Fatal(err)
	}
	after := ordinalsByNode(t, f, comp.ID)

	for node, ord := range before {
		if after[node] != ord {
			t.Errorf("扩容改掉了 %s 的 ordinal：%d → %d —— "+
				"这会让集群里每个成员的身份都变，当场损坏",
				node, ord, after[node])
		}
	}
	if after["n1"] != 3 {
		t.Errorf("新节点应拿到 max+1=3，实际 %d", after["n1"])
	}
}

func ordinalsByNode(t *testing.T, f *fixture, componentID int64) map[string]int {
	t.Helper()
	insts, err := f.svc.Repos.Instances().List(ctx(), componentID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, ri := range insts {
		n, err := f.svc.Repos.Nodes().Get(ctx(), ri.NodeID)
		if err != nil {
			t.Fatal(err)
		}
		out[n.Name] = ri.Ordinal
	}
	return out
}

// TestScaleInRefusedByDefault 钉住「缩小规模必须是显式意图」。
func TestScaleInRefusedByDefault(t *testing.T) {
	// 用 go-webapp 而非 zookeeper：后者的 ensemble 形态要求 3-N，
	// 缩到 2 台会先被 cardinality 拦下，测不到缩容这条路径
	f := newFixture(t, "n1", "n2", "n3")
	deployWebapp(t, f, "n1", "n2", "n3")

	_, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "go-webapp", Component: "web", Update: true,
		Roles: map[string][]string{"default": {"n1", "n2"}},
	})
	if err == nil {
		t.Fatal("少写一个节点不该悄悄卸载一个实例")
	}
	// 错误里要列清被移除的是谁
	for _, want := range []string{"n3", "--allow-remove"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应含 %q，实际:\n%v", want, err)
		}
	}
}

// TestDryRunLeavesNoTrace 钉住干跑没有副作用。
func TestDryRunLeavesNoTrace(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployJDK(t, f, "n1", "n2", "n3")
	f.notify.woken = nil // 前置部署的唤醒不算数

	res, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "zookeeper", Component: "zk-main", Profile: "ensemble", DryRun: true,
		Roles: map[string][]string{"server": {"n1", "n2", "n3"}},
	})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if len(res.Specs) != 3 {
		t.Errorf("干跑也应产出完整规格，实际 %d 份", len(res.Specs))
	}

	if _, err := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "zk-main"); err == nil {
		t.Error("干跑不该把组件写进库")
	}
	if len(f.notify.woken) != 0 {
		t.Errorf("干跑不该唤醒任何节点，实际唤醒了 %v", f.notify.woken)
	}
	// 也不该往 Vault 里写密钥
	secrets, err := f.svc.Vault.List(ctx(), 1)
	if err == nil && len(secrets) > 0 {
		t.Error("干跑不该往保管库里写东西")
	}
}

// TestUnknownParamRejectedEarly 钉住参数名拼错在部署时就被拦住。
func TestUnknownParamRejectedEarly(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	_, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "zookeeper", Component: "zk-main", Profile: "ensemble",
		Roles: map[string][]string{"server": {"n1", "n2", "n3"}},
		Set:   map[string]any{"tickTime": 3000}, // 真名是 tick_time
	})
	if err == nil {
		t.Fatal("拼错的参数名应当被拒绝")
	}
	if !strings.Contains(err.Error(), "tick_time") {
		t.Errorf("错误信息应列出正确的参数名，实际:\n%v", err)
	}
}

// TestUnknownNodeRejected 钉住 mechd 不凭空创建节点。
func TestUnknownNodeRejected(t *testing.T) {
	f := newFixture(t, "n1")
	_, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "zookeeper", Component: "zk-main", Profile: "ensemble",
		Roles: map[string][]string{"server": {"n1", "ghost"}},
	})
	if err == nil {
		t.Fatal("落在未注册节点上应当被拒绝")
	}
	for _, want := range []string{"ghost", "is not registered", "n1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应含 %q，实际:\n%v", want, err)
		}
	}
}

// ── 事实快照 ────────────────────────────────────────────────────────────

// TestFactsAreFrozenAtPlacement 钉住 spec §9.4.1 最关键的那条。
//
// 配置取值**不跟随实时事实**：节点加了内存不该让 heap 变化、digest 变化、
// 服务在业务时间被重启。更糟的是某次采集报了 0 字节 → heap=0 → 起不来。
//
// 这条在「解析管线按需重算」的实现下尤其容易破——它每次都会去读一遍事实。
func TestFactsAreFrozenAtPlacement(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployZK(t, f, "n1", "n2", "n3")
	before, err := f.svc.Status(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}

	// 节点加了内存，心跳把新事实报上来
	nodes, _ := f.svc.Repos.Nodes().List(ctx(), f.site.ID)
	for _, n := range nodes {
		if err := f.svc.Repos.Status().PutFacts(ctx(), store.NodeFacts{
			NodeID: n.ID, CollectedAt: time.Now(),
			Facts: map[string]any{
				"memory": map[string]any{"total": "128Gi"},
				"cpu":    map[string]any{"cores": 64},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	after, err := f.svc.Status(ctx(), "", "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	for i := range before.Instances {
		if before.Instances[i].Want != after.Instances[i].Want {
			t.Fatalf("事实刷新改变了 %s 的期望 digest：%s → %s\n"+
				"  配置取值必须用放置时的快照，否则一次加内存就会在"+
				"业务时间重启服务（spec §9.4.1）",
				before.Instances[i].Node,
				before.Instances[i].Want[:12], after.Instances[i].Want[:12])
		}
	}
}

// ── 依赖绑定 ────────────────────────────────────────────────────────────

// TestBindingResolvesAndFreezes 钉住依赖绑定的解析与固化。
func TestBindingResolvesAndFreezes(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")

	// 先部署提供方
	if _, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "jdk11", Component: "jdk11",
		Roles: map[string][]string{"runtime": {"n1", "n2", "n3"}},
	}); err != nil {
		t.Fatalf("部署 jdk11: %v", err)
	}
	// 再部署消费方
	if _, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "zookeeper", Component: "zk-main", Profile: "ensemble",
		Roles: map[string][]string{"server": {"n1", "n2", "n3"}},
	}); err != nil {
		t.Fatalf("部署 zookeeper: %v", err)
	}

	comp, _ := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "zk-main")
	b, err := f.svc.Repos.Bindings().Get(ctx(), comp.ID, "jdk11")
	if err != nil {
		t.Fatalf("绑定应当被固化: %v", err)
	}
	provider, _ := f.svc.Repos.Components().Get(ctx(), b.BoundComponentID)
	if provider.Name != "jdk11" {
		t.Errorf("应绑到 jdk11，实际 %s", provider.Name)
	}
}

// TestMissingDependencyIsExplained 钉住缺依赖时的错误可操作。
func TestMissingDependencyIsExplained(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	_, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "zookeeper", Component: "zk-main", Profile: "ensemble",
		Roles: map[string][]string{"server": {"n1", "n2", "n3"}},
	})
	if err == nil {
		t.Fatal("缺少 jdk11 依赖时应当失败")
	}
	if !strings.Contains(err.Error(), "jdk11") ||
		!strings.Contains(err.Error(), "deploy") {
		t.Errorf("错误信息应告诉用户先部署什么，实际:\n%v", err)
	}
}

// ── status / diff ───────────────────────────────────────────────────────

// TestStatusReflectsConvergence 钉住收敛判据。
//
// **靠 digest 一致且健康判定**，而不是靠 mechlet 说「我成功了」——
// 前者是状态，可以重复确认；后者是事件，丢一次就永远丢了。
func TestStatusReflectsConvergence(t *testing.T) {
	f := newFixture(t, "n1")
	deployWebapp(t, f, "n1")

	st, err := f.svc.Status(ctx(), "", "web")
	if err != nil {
		t.Fatal(err)
	}
	if st.Converged {
		t.Fatal("还没有任何上报时不该算收敛")
	}
	want := st.Instances[0].Want

	// mechlet 报上来一个对得上的 digest 且健康
	reportDigest(t, f, "web", "default", "n1", want, "healthy")

	st, err = f.svc.Status(ctx(), "", "web")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Converged {
		t.Errorf("digest 一致且健康时应当算收敛: %+v", st.Instances[0])
	}

	// 报一个对不上的 digest：不收敛
	reportDigest(t, f, "web", "default", "n1", "0000", "healthy")
	st, _ = f.svc.Status(ctx(), "", "web")
	if st.Converged {
		t.Error("digest 对不上时不该算收敛")
	}

	// digest 对但不健康：同样不收敛
	reportDigest(t, f, "web", "default", "n1", want, "unhealthy")
	st, _ = f.svc.Status(ctx(), "", "web")
	if st.Converged {
		t.Error("不健康时不该算收敛——收敛是「digest 一致**且**健康」")
	}
}

func deployWebapp(t *testing.T, f *fixture, nodes ...string) {
	t.Helper()
	if _, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "go-webapp", Component: "web",
		Roles: map[string][]string{"default": nodes},
	}); err != nil {
		t.Fatalf("部署 go-webapp: %v", err)
	}
}

func reportDigest(t *testing.T, f *fixture, comp, role, node, digest, health string) {
	t.Helper()
	b := &Backend{S: f.svc}
	if err := b.Report(ctx(), protocol.Report{
		Node: node,
		Instances: []protocol.InstanceStatus{{
			Component: comp, Role: role, Digest: digest, Generation: 1,
			Result: "ok",
			Health: &protocol.HealthStatus{State: health},
			Workload: &protocol.WorkloadStatus{
				Runtime: "systemd", State: "running",
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestDiffShowsPendingChange 钉住 diff 反映「还没下发下去的变化」。
func TestDiffShowsPendingChange(t *testing.T) {
	f := newFixture(t, "n1")
	deployWebapp(t, f, "n1")

	st, _ := f.svc.Status(ctx(), "", "web")
	reportDigest(t, f, "web", "default", "n1", st.Instances[0].Want, "healthy")

	d, err := f.svc.Diff(ctx(), "", "web")
	if err != nil {
		t.Fatal(err)
	}
	if d.Changed {
		t.Errorf("已收敛时不该有变化: %+v", d.Entries)
	}

	// 改一个参数
	if _, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "go-webapp", Component: "web", Update: true,
		Roles: map[string][]string{"default": {"n1"}},
		Set:   map[string]any{"log_level": "debug"},
	}); err != nil {
		t.Fatal(err)
	}

	d, err = f.svc.Diff(ctx(), "", "web")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Changed || d.Entries[0].Change != "update" {
		t.Errorf("改了参数后应当报 update，实际 %+v", d.Entries)
	}
}

// TestDiffLeavesNoTrace 钉住 diff 是只读的。
func TestDiffLeavesNoTrace(t *testing.T) {
	f := newFixture(t, "n1")
	deployWebapp(t, f, "n1")
	before := len(f.notify.woken)

	if _, err := f.svc.Diff(ctx(), "", "web"); err != nil {
		t.Fatal(err)
	}
	if len(f.notify.woken) != before {
		t.Error("diff 不该唤醒任何节点——它是只读的解析管线出口")
	}
}

// ── ack-drift ───────────────────────────────────────────────────────────

// TestAckDriftRequiresReasonAndDuration 钉住抑制不能是无名无期的。
//
// 没有理由的抑制半年后没人知道为什么，那时它和「忘了处理」无法区分——
// 而这正是抑制机制最容易退化成的样子。
func TestAckDriftRequiresReasonAndDuration(t *testing.T) {
	f := newFixture(t, "n1")
	deployWebapp(t, f, "n1")

	if _, err := f.svc.AckDrift(ctx(), AckDriftRequest{
		Component: "web", Duration: 4 * time.Hour,
	}); err == nil {
		t.Error("没有 reason 的抑制应当被拒绝")
	}
	if _, err := f.svc.AckDrift(ctx(), AckDriftRequest{
		Component: "web", Reason: "临时调参",
	}); err == nil {
		t.Error("没有 duration 的抑制应当被拒绝")
	}
}

// TestAckDriftExpires 钉住抑制**有期限**。
//
// 到点自动恢复告警，不会悄悄变永久。
func TestAckDriftExpires(t *testing.T) {
	f := newFixture(t, "n1")
	deployWebapp(t, f, "n1")

	now := time.Now().UTC()
	f.svc.Now = func() time.Time { return now }

	n, err := f.svc.AckDrift(ctx(), AckDriftRequest{
		Component: "web", Resource: "template:app.yaml",
		Duration: 4 * time.Hour, Reason: "凌晨救火临时调了日志级别",
		Actor: "ops",
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应抑制 1 个实例，实际 %d", n)
	}

	st, _ := f.svc.Status(ctx(), "", "web")
	if len(st.Instances[0].Suppressed) != 1 {
		t.Errorf("status 里应当显示「已抑制」，实际 %+v", st.Instances[0])
	}

	// 4 小时后：自动恢复
	f.svc.Now = func() time.Time { return now.Add(5 * time.Hour) }
	st, _ = f.svc.Status(ctx(), "", "web")
	if len(st.Instances[0].Suppressed) != 0 {
		t.Error("过期的抑制应当自动失效，不需要任何人记得去清理")
	}
}

// ── HTTP ────────────────────────────────────────────────────────────────

func newAPI(t *testing.T, f *fixture) (*httptest.Server, string) {
	t.Helper()
	token := TokenPrefix + "test-token-value"
	api := &API{S: f.svc, Auth: NewTokenAuth(token)}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, token
}

func do(t *testing.T, srv *httptest.Server, token, method, path, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// TestAPIRequiresAuth 钉住「认证不是可选的」。
//
// 默认绑 0.0.0.0 是为了让人拿笔记本连门店那台机看 UI，
// 而一旦对外监听，无认证的写接口等于把机器交出去。
func TestAPIRequiresAuth(t *testing.T) {
	f := newFixture(t, "n1")
	srv, token := newAPI(t, f)

	for _, path := range []string{
		APIPrefix + "/components", APIPrefix + "/nodes",
	} {
		if code, _ := do(t, srv, "", "GET", path, ""); code != http.StatusUnauthorized {
			t.Errorf("%s 无 token 应当 401，实际 %d", path, code)
		}
		if code, _ := do(t, srv, "wrong-token", "GET", path, ""); code != http.StatusUnauthorized {
			t.Errorf("%s 错误 token 应当 401，实际 %d", path, code)
		}
		if code, _ := do(t, srv, token, "GET", path, ""); code != http.StatusOK {
			t.Errorf("%s 正确 token 应当 200，实际 %d", path, code)
		}
	}
}

// TestAPIRefusesWithoutAuthenticator 钉住「忘了配认证」是拒绝服务而非敞开。
func TestAPIRefusesWithoutAuthenticator(t *testing.T) {
	f := newFixture(t, "n1")
	api := &API{S: f.svc} // 故意不给 Auth
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	if code, _ := do(t, srv, "any", "GET", APIPrefix+"/components", ""); code != http.StatusUnauthorized {
		t.Errorf("没配认证器时必须拒绝，实际 %d", code)
	}
}

// TestAPIDeployAndStatus 走一遍 HTTP 上的 deploy → status。
func TestAPIDeployAndStatus(t *testing.T) {
	f := newFixture(t, "n1")
	srv, token := newAPI(t, f)

	code, body := do(t, srv, token, "POST", APIPrefix+"/components",
		`{"pack":"go-webapp","component":"web","roles":{"default":["n1"]}}`)
	if code != http.StatusCreated {
		t.Fatalf("deploy 应当 201，实际 %d: %s", code, body)
	}

	code, body = do(t, srv, token, "GET", APIPrefix+"/components/web/status", "")
	if code != http.StatusOK {
		t.Fatalf("status 应当 200，实际 %d: %s", code, body)
	}
	var st StatusView
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Instances) != 1 || st.Instances[0].Node != "n1" {
		t.Errorf("status 内容不对: %s", body)
	}
}

// TestAPIRejectsUnknownFields 钉住拼错的字段不被静默忽略。
func TestAPIRejectsUnknownFields(t *testing.T) {
	f := newFixture(t, "n1")
	srv, token := newAPI(t, f)

	code, body := do(t, srv, token, "POST", APIPrefix+"/components",
		`{"pack":"go-webapp","nodez":["n1"]}`)
	if code != http.StatusBadRequest {
		t.Errorf("拼错的字段应当 400，实际 %d: %s", code, body)
	}
}

// TestAPIUserErrorIsNot500 钉住用户输入错误不报成服务端错误。
//
// 500 会让人去翻服务端日志，而问题在他自己的命令行上。
func TestAPIUserErrorIsNot500(t *testing.T) {
	f := newFixture(t, "n1")
	srv, token := newAPI(t, f)

	code, body := do(t, srv, token, "POST", APIPrefix+"/components",
		`{"pack":"go-webapp","roles":{"default":["ghost"]}}`)
	if code != http.StatusBadRequest {
		t.Errorf("落在未注册节点上应当 400，实际 %d: %s", code, body)
	}
}

func TestAPINotFound(t *testing.T) {
	f := newFixture(t, "n1")
	srv, token := newAPI(t, f)
	if code, _ := do(t, srv, token, "GET", APIPrefix+"/components/nope/status", ""); code != http.StatusNotFound {
		t.Errorf("不存在的组件应当 404，实际 %d", code)
	}
}

// TestTokenIsStoredAsHash 钉住 token 只存哈希。
func TestTokenIsStoredAsHash(t *testing.T) {
	tok := TokenPrefix + "secret-value"
	a := NewTokenAuth(tok)
	for _, h := range a.hashes {
		if strings.Contains(string(h[:]), "secret-value") {
			t.Fatal("认证器里不该留下明文 token")
		}
	}
	if HashToken(tok) == tok {
		t.Error("HashToken 应当是哈希而非原样返回")
	}
}

// deployJDK 部署 zookeeper 的 scope:node 依赖。
//
// 单独抽出来是因为**依赖必须先在**：zookeeper 的 requires 声明了 jdk11，
// 而绑定解析是 deploy 的一部分。这正是 13-mechd §2 里「② 早于 ③」的含义。
func deployJDK(t *testing.T, f *fixture, nodes ...string) {
	t.Helper()
	if _, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "jdk11", Component: "jdk11",
		Roles: map[string][]string{"runtime": nodes},
	}); err != nil {
		t.Fatalf("部署 jdk11: %v", err)
	}
}

// deployZK 部署一套 ZooKeeper 仲裁集群（含它的依赖）。
func deployZK(t *testing.T, f *fixture, nodes ...string) *DeployResult {
	t.Helper()
	deployJDK(t, f, nodes...)
	res, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "zookeeper", Component: "zk-main", Profile: "ensemble",
		Roles: map[string][]string{"server": nodes}, Actor: "tester",
	})
	if err != nil {
		t.Fatalf("部署 zookeeper: %v", err)
	}
	return res
}

// TestDiffStableForComponentsWithGeneratedSecrets 钉住一个曾经踩到的坑。
//
// diff 若用一次性密钥渲染，secretRefs 的 id 与版本每次都不同，于是
// **任何带 generate 参数的组件都会永远显示「有待下发的变更」**。
// 一个永远报有变更的 diff 比没有 diff 更糟——它会让人学会忽略它。
//
// 用真实 Vault 不引入副作用：密钥已经固化，读它什么都不写。
// 真正有副作用的是首次生成，而那只发生在 deploy 上。
func TestDiffStableForComponentsWithGeneratedSecrets(t *testing.T) {
	f := newFixture(t, "n1")

	if _, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "postgresql", Component: "pg-main",
		Roles: map[string][]string{"primary": {"n1"}},
		Set:   map[string]any{"admin_password": "pg-admin-password"},
	}); err != nil {
		t.Fatalf("部署 postgresql: %v", err)
	}

	st, err := f.svc.Status(ctx(), "", "pg-main")
	if err != nil {
		t.Fatal(err)
	}
	reportDigest(t, f, "pg-main", "primary", "n1", st.Instances[0].Want, "healthy")

	// 连着跑几次：每次都必须报「没有变化」
	for i := 1; i <= 3; i++ {
		d, err := f.svc.Diff(ctx(), "", "pg-main")
		if err != nil {
			t.Fatal(err)
		}
		if d.Changed {
			t.Fatalf("第 %d 次 diff 报了变化，而什么都没改过:\n  %+v\n"+
				"  带 generate 参数的组件不该每次 diff 都显示有变更", i, d.Entries)
		}
	}
}
