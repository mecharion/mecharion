package protocol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/mecharion/mecharion/internal/protocol/agentpb"
	"github.com/mecharion/mecharion/internal/spec"
)

// ── 夹具 ────────────────────────────────────────────────────────────────

// fakeBackend 是一份可控的 mechd 侧实现。
type fakeBackend struct {
	mu sync.Mutex

	registered []NodeRegistration
	rejectNode string

	assignments map[string][]InstanceSpec
	reports     []Report
	events      []Event
	blobs       map[string][]byte
}

func newBackend() *fakeBackend {
	return &fakeBackend{
		assignments: map[string][]InstanceSpec{},
		blobs:       map[string][]byte{},
	}
}

func (b *fakeBackend) Register(_ context.Context, n NodeRegistration) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n.Name == b.rejectNode {
		return errors.New("该节点不在册")
	}
	b.registered = append(b.registered, n)
	return nil
}

func (b *fakeBackend) Assignment(_ context.Context, node string) ([]InstanceSpec, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.assignments[node], nil
}

func (b *fakeBackend) Report(_ context.Context, r Report) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reports = append(b.reports, r)
	return nil
}

func (b *fakeBackend) Events(_ context.Context, _ string, e []Event) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, e...)
	return len(e), nil
}

func (b *fakeBackend) OpenBlob(_ context.Context, sum string) (io.ReadSeekCloser, int64, error) {
	b.mu.Lock()
	data, ok := b.blobs[sum]
	b.mu.Unlock()
	if !ok {
		return nil, 0, errors.New("不存在")
	}
	return nopCloser{bytes.NewReader(data)}, int64(len(data)), nil
}

func (b *fakeBackend) setAssignment(node string, specs []InstanceSpec) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.assignments[node] = specs
}

func (b *fakeBackend) addBlob(data []byte) string {
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])
	b.mu.Lock()
	defer b.mu.Unlock()
	b.blobs[hexSum] = data
	return hexSum
}

func (b *fakeBackend) reportCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.reports)
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }

// harness 起一个真实的 gRPC 服务端与客户端，走内存管道。
type harness struct {
	srv    *Server
	client *Client
	back   *fakeBackend
	// gs 是承载 srv 的 grpc 服务端，关闭路径的测试要用到它。
	gs *grpc.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	back := newBackend()
	srv := NewServer(ServerOptions{
		Backend: back,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	agentpb.RegisterAgentServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	cli, err := Dial(ClientOptions{
		Target: "passthrough:///bufnet",
		Node:   "n1", AgentVersion: "0.0.0-test",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialOptions: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	return &harness{srv: srv, client: cli, back: back, gs: gs}
}

// makeSpec 造一份最小可用的规格。
func makeSpec(t *testing.T, component, role, node string) *spec.ResolvedSpec {
	t.Helper()
	s := &spec.ResolvedSpec{
		SchemaVersion: spec.SchemaVersion,
		Component:     component, Role: role,
		ConfigGroup: "default",
		Node:        spec.NodeRef{Name: node},
		Paths: map[string]spec.PathValue{
			"home": {Name: "home", Values: []string{"/opt/x"}, Kind: "single", Mode: "0755"},
		},
		Resources: []spec.Resource{},
		Topology:  spec.Topology{Roles: map[string][]spec.Instance{}},
	}
	if err := spec.Seal(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// collector 收下发。
type collector struct {
	mu       sync.Mutex
	calls    [][]InstanceSpec
	full     []bool
	cordoned []bool
	got      chan struct{}
}

func newCollector() *collector {
	return &collector{got: make(chan struct{}, 16)}
}

func (c *collector) Apply(_ context.Context, a Assignment) error {
	c.mu.Lock()
	c.calls = append(c.calls, a.Specs)
	c.full = append(c.full, a.FullSync)
	c.cordoned = append(c.cordoned, a.Cordoned)
	c.mu.Unlock()
	select {
	case c.got <- struct{}{}:
	default:
	}
	return nil
}

func (c *collector) wait(t *testing.T) []InstanceSpec {
	t.Helper()
	select {
	case <-c.got:
	case <-time.After(5 * time.Second):
		t.Fatal("等待下发超时")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[len(c.calls)-1]
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// ── 版本协商 ────────────────────────────────────────────────────────────

// TestNegotiationIsBackwardCompatible 钉住「控制面向后兼容 agent」。
//
// 方向是刻意的：多节点升级时先升控制面还是先升 agent，必须是运维自己能
// 决定的事；要求两者同版本会让升级变成一次全停。
func TestNegotiationIsBackwardCompatible(t *testing.T) {
	for _, tc := range []struct {
		name     string
		agentMax uint32
		want     uint32
		wantErr  bool
	}{
		{"同版本", spec.SchemaVersion, spec.SchemaVersion, false},
		{"agent 更新则按旧结构工作", spec.SchemaVersion + 5, spec.SchemaVersion, false},
		{"agent 低于下限则拒绝", SpecSchemaMin - 1, 0, true},
		{"没报版本", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := negotiate(tc.agentMax)
			if tc.wantErr {
				if err == nil {
					t.Fatal("应当被拒绝")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("下发版本应为 %d，实际 %d", tc.want, got)
			}
		})
	}
}

func TestRegisterCarriesIntervals(t *testing.T) {
	h := newHarness(t)
	if err := h.client.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	iv := h.client.Intervals()
	// 三个周期彼此独立，默认值来自 11-resource-engine §3.2
	if iv.ReconcileSeconds != 60 || iv.HealthSeconds != 15 || iv.ReportSeconds != 15 {
		t.Errorf("周期默认值不对: %+v", iv)
	}
	if h.client.Session() == "" {
		t.Error("注册后应当有会话 id")
	}
}

func TestRejectedNodeGetsClearError(t *testing.T) {
	h := newHarness(t)
	h.back.rejectNode = "n1"
	err := h.client.Register(context.Background())
	if err == nil {
		t.Fatal("后端拒绝时注册应当失败")
	}
	if !strings.Contains(err.Error(), "不在册") {
		t.Errorf("错误信息应带上后端给的理由，实际: %v", err)
	}
}

// ── 下发 ────────────────────────────────────────────────────────────────

// TestSubscribeDeliversFullSyncImmediately 钉住「连上就先推全量」。
func TestSubscribeDeliversFullSyncImmediately(t *testing.T) {
	h := newHarness(t)
	h.back.setAssignment("n1", []InstanceSpec{
		{Spec: makeSpec(t, "demo", "server", "n1")},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.client.Register(ctx); err != nil {
		t.Fatal(err)
	}
	c := newCollector()
	go func() { _ = h.client.Subscribe(ctx, c) }()

	specs := c.wait(t)
	if len(specs) != 1 || specs[0].Spec.Component != "demo" {
		t.Fatalf("首次下发应含 1 份 demo 的规格，实际 %d 份", len(specs))
	}
	c.mu.Lock()
	full := c.full[0]
	c.mu.Unlock()
	if !full {
		t.Error("下发必须置 full_sync —— 未列出的实例应被移除")
	}
}

// TestNotifyPushesFullStateNotDelta 钉住「全量重推，不做增量」。
//
// 不做增量同步、不做确认序号、不做差异计算：下发本身幂等，
// 一个内容寻址的期望状态让整类同步协议问题消失。
func TestNotifyPushesFullStateNotDelta(t *testing.T) {
	h := newHarness(t)
	h.back.setAssignment("n1", []InstanceSpec{
		{Spec: makeSpec(t, "a", "server", "n1")},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.client.Register(ctx); err != nil {
		t.Fatal(err)
	}
	c := newCollector()
	go func() { _ = h.client.Subscribe(ctx, c) }()
	c.wait(t)

	// 加一个实例后通知
	h.back.setAssignment("n1", []InstanceSpec{
		{Spec: makeSpec(t, "a", "server", "n1")},
		{Spec: makeSpec(t, "b", "server", "n1")},
	})
	h.srv.Notify("n1")

	specs := c.wait(t)
	if len(specs) != 2 {
		t.Fatalf("第二次下发应当是**全量**的 2 份，而不是增量的 1 份，实际 %d 份", len(specs))
	}

	// 删掉一个：全量语义下 mechlet 靠「没列出来」知道要移除
	h.back.setAssignment("n1", []InstanceSpec{
		{Spec: makeSpec(t, "b", "server", "n1")},
	})
	h.srv.Notify("n1")
	specs = c.wait(t)
	if len(specs) != 1 || specs[0].Spec.Component != "b" {
		t.Fatalf("移除后应下发剩下的 1 份 b，实际 %v", componentsOf(specs))
	}
}

func componentsOf(specs []InstanceSpec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Spec.Component)
	}
	return out
}

// TestNotifyForDisconnectedNodeIsNoop 钉住通知一个没连上的节点不会出事。
//
// 它下次 Subscribe 时自然拿到最新的——这正是全量语义省掉的那类状态。
func TestNotifyForDisconnectedNodeIsNoop(t *testing.T) {
	h := newHarness(t)
	h.srv.Notify("nobody") // 不该 panic 或阻塞
	if h.srv.Connected("nobody") {
		t.Error("没连上的节点不该被算作已连接")
	}
}

// ── 密钥 ────────────────────────────────────────────────────────────────

// TestSecretsTravelBesideSpecNotInside 钉住 17-protocol §3 的纪律。
//
// secrets 是唯一承载明文的地方：不进规格、不落盘、不参与 digest。
func TestSecretsTravelBesideSpecNotInside(t *testing.T) {
	h := newHarness(t)

	s := makeSpec(t, "demo", "server", "n1")
	s.SecretRefs = []spec.SecretRef{{ID: "sec1", Version: 3, Param: "pw"}}
	s.Resources = []spec.Resource{{
		ID: "conf", Type: "file",
		Args: json.RawMessage(`{"path":"/etc/x","content":"pw=` + spec.SecretToken("sec1") + `"}`),
	}}
	if err := spec.Seal(s); err != nil {
		t.Fatal(err)
	}
	h.back.setAssignment("n1", []InstanceSpec{
		{Spec: s, Secrets: map[string]string{"sec1": "the-real-password"}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.client.Register(ctx); err != nil {
		t.Fatal(err)
	}
	c := newCollector()
	go func() { _ = h.client.Subscribe(ctx, c) }()
	got := c.wait(t)

	// 规格本体里只有引用
	blob, _ := json.Marshal(got[0].Spec)
	if bytes.Contains(blob, []byte("the-real-password")) {
		t.Fatal("明文进了规格——它会随归档、审计、diff 一起流出去")
	}
	if !bytes.Contains(blob, []byte(spec.SecretToken("sec1"))) {
		t.Error("规格里应当是哨兵串")
	}
	// 明文走单独的字段
	if got[0].Secrets["sec1"] != "the-real-password" {
		t.Errorf("密钥明文应随消息单独下发，实际 %v", got[0].Secrets)
	}

	// 消费点还原
	final, err := spec.ResolveSecrets(got[0].Spec, got[0].Secrets)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(final.Resources[0].Args, []byte("pw=the-real-password")) {
		t.Errorf("还原后应是明文，实际 %s", final.Resources[0].Args)
	}
}

// TestMissingSecretRefusedAtDecode 钉住「缺密钥要在收到时就发现」。
//
// 等到写盘时才报，现场已经是「配置看起来正常、应用却认证失败」。
func TestMissingSecretRefusedAtDecode(t *testing.T) {
	s := makeSpec(t, "demo", "server", "n1")
	s.SecretRefs = []spec.SecretRef{{ID: "sec1", Version: 1, Param: "pw"}}
	if err := spec.Seal(s); err != nil {
		t.Fatal(err)
	}
	blob, err := spec.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}

	_, err = decodeAssignment(&agentpb.Assignment{
		Specs: []*agentpb.ResolvedSpecEnvelope{{SpecJson: blob}}, // 故意不带 secrets
	})
	if err == nil {
		t.Fatal("引用了密钥却没给值时必须报错")
	}
	if !strings.Contains(err.Error(), "sec1") {
		t.Errorf("错误信息应指名到密钥，实际: %v", err)
	}
}

// TestEncodeRefusesUnknownSecret 是发送侧的对称检查。
func TestEncodeRefusesUnknownSecret(t *testing.T) {
	s := makeSpec(t, "demo", "server", "n1")
	s.SecretRefs = []spec.SecretRef{{ID: "sec1", Version: 1}}
	if err := spec.Seal(s); err != nil {
		t.Fatal(err)
	}
	_, err := encodeSpec(InstanceSpec{Spec: s, Secrets: map[string]string{"other": "x"}})
	if err == nil {
		t.Fatal("下发一份缺值的规格必须在发送端就被拦住")
	}
}

// TestOnlyReferencedSecretsAreSent 钉住不多给。
//
// 多给不会更方便，只会扩大暴露面。
func TestOnlyReferencedSecretsAreSent(t *testing.T) {
	s := makeSpec(t, "demo", "server", "n1")
	s.SecretRefs = []spec.SecretRef{{ID: "used", Version: 1}}
	if err := spec.Seal(s); err != nil {
		t.Fatal(err)
	}
	env, err := encodeSpec(InstanceSpec{
		Spec:    s,
		Secrets: map[string]string{"used": "a", "unused": "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := env.Secrets["unused"]; leaked {
		t.Error("没被这份规格引用的密钥不该下发")
	}
	if len(env.Secrets) != 1 {
		t.Errorf("只该下发 1 条密钥，实际 %d 条", len(env.Secrets))
	}
}

// TestTamperedSpecRefusedBeforeSend 钉住 digest 在发送端就被核对。
func TestTamperedSpecRefusedBeforeSend(t *testing.T) {
	s := makeSpec(t, "demo", "server", "n1")
	s.Component = "tampered" // 改了内容却不重算 digest
	_, err := encodeSpec(InstanceSpec{Spec: s})
	if err == nil {
		t.Fatal("digest 对不上的规格不该被发出去")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Errorf("错误信息应说明是 digest 问题，实际: %v", err)
	}
}

// ── 上报 ────────────────────────────────────────────────────────────────

func TestReportRoundTrip(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.client.Register(ctx); err != nil {
		t.Fatal(err)
	}

	resync, err := h.client.Report(ctx, Report{
		Node: "n1",
		Instances: []InstanceStatus{{
			Component: "demo", Role: "server",
			Digest: "abc123", Generation: 7, Result: "ok",
			Workload:  &WorkloadStatus{Runtime: "systemd", State: "running", PID: 42},
			Health:    &HealthStatus{State: "healthy"},
			Resources: []ResourceStatus{{ID: "conf", Type: "file", State: "ok"}},
		}},
		Facts: []byte(`{"memory":{"total":"32Gi"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resync {
		t.Error("会话有效时不该要求重新握手")
	}

	h.back.mu.Lock()
	defer h.back.mu.Unlock()
	if len(h.back.reports) != 1 {
		t.Fatalf("后端应收到 1 次上报，实际 %d 次", len(h.back.reports))
	}
	got := h.back.reports[0].Instances[0]
	// digest 是收敛判据，必须原样送达
	if got.Digest != "abc123" || got.Generation != 7 {
		t.Errorf("digest / generation 传丢了: %+v", got)
	}
	if got.Workload == nil || got.Workload.PID != 42 {
		t.Errorf("workload 传丢了: %+v", got.Workload)
	}
}

// TestStaleSessionTriggersResync 钉住过期会话不被默默接受。
//
// 否则一个用着过期会话的 agent 会一直上报，看起来一切正常。
func TestStaleSessionTriggersResync(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.client.Register(ctx); err != nil {
		t.Fatal(err)
	}
	// 服务端换了会话（例如 mechd 重启后节点重新注册过）
	h.srv.newSession("n1")

	resync, err := h.client.Report(ctx, Report{Node: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if !resync {
		t.Error("会话对不上时应当要求 mechlet 重新握手")
	}
}

func TestPushEvents(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.client.Register(ctx); err != nil {
		t.Fatal(err)
	}
	n, err := h.client.PushEvents(ctx, []Event{
		{At: "2026-01-01T00:00:00Z", Level: "warn", Kind: "drift", Message: "配置被改"},
		{At: "2026-01-01T00:01:00Z", Level: "error", Kind: "apply", Message: "失败"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("应接收 2 条事件，实际 %d 条", n)
	}
	h.back.mu.Lock()
	defer h.back.mu.Unlock()
	if len(h.back.events) != 2 || h.back.events[0].Kind != "drift" {
		t.Errorf("事件内容传丢了: %+v", h.back.events)
	}
}

// ── blob ────────────────────────────────────────────────────────────────

// TestFetchBlobVerifiesAndIsIdempotent 钉住内容寻址带来的两条性质。
func TestFetchBlobVerifiesAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	// 跨过分块边界，确保流式路径真的被走到
	data := bytes.Repeat([]byte("mecharion"), BlobChunkSize/3)
	sum := h.back.addBlob(data)

	ctx := context.Background()
	dir := t.TempDir()

	path, err := h.client.FetchBlob(ctx, sum, dir)
	if err != nil {
		t.Fatalf("取载荷: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("内容不一致：期望 %d 字节，实际 %d 字节", len(data), len(got))
	}

	// 幂等：已有且校验通过就直接返回，不该再拉一次
	again, err := h.client.FetchBlob(ctx, sum, dir)
	if err != nil {
		t.Fatal(err)
	}
	if again != path {
		t.Errorf("重复取应返回同一路径，实际 %q 与 %q", again, path)
	}

	// 目录里不该留下任何半成品
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".partial-") {
			t.Errorf("留下了未清理的临时文件 %s", e.Name())
		}
	}
}

// TestCorruptBlobIsDiscarded 钉住「校验通过后才 rename」。
//
// 否则一次中断的传输会留下一个名字正确、内容残缺的文件，
// 而下一次调和会认为它已经在了。
func TestCorruptBlobIsDiscarded(t *testing.T) {
	h := newHarness(t)
	data := []byte("real content")
	realSum := h.back.addBlob(data)

	// 让服务端在这个 sha256 下返回别的内容
	wrong := sha256.Sum256([]byte("what the client asked for"))
	wrongSum := hex.EncodeToString(wrong[:])
	h.back.mu.Lock()
	h.back.blobs[wrongSum] = data
	h.back.mu.Unlock()

	dir := t.TempDir()
	_, err := h.client.FetchBlob(context.Background(), wrongSum, dir)
	if err == nil {
		t.Fatal("内容与 sha256 对不上时必须失败")
	}
	if !strings.Contains(err.Error(), "校验失败") {
		t.Errorf("错误信息应说明是校验问题，实际: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, wrongSum)); err == nil {
		t.Fatal("校验失败的内容不该出现在最终位置")
	}
	_ = realSum
}

func TestFetchBlobRejectsBadDigest(t *testing.T) {
	h := newHarness(t)
	for _, bad := range []string{"", "abc", strings.Repeat("z", 64)} {
		if _, err := h.client.FetchBlob(context.Background(), bad, t.TempDir()); err == nil {
			t.Errorf("非法 sha256 %q 应被拒绝", bad)
		}
	}
}

func TestFetchBlobNotFound(t *testing.T) {
	h := newHarness(t)
	missing := hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32))
	_, err := h.client.FetchBlob(context.Background(), missing, t.TempDir())
	if err == nil {
		t.Fatal("不存在的载荷应当报错")
	}
}

// TestConcurrentFetchCoalesces 钉住同一 blob 的并发请求被合并。
//
// N 个实例引用同一个 200MB 的 JDK 时，不该各拉一份。
func TestConcurrentFetchCoalesces(t *testing.T) {
	h := newHarness(t)
	data := bytes.Repeat([]byte("x"), 4096)
	sum := h.back.addBlob(data)
	dir := t.TempDir()

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = h.client.FetchBlob(context.Background(), sum, dir)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("第 %d 个并发请求失败: %v", i, err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("并发取同一载荷只该留下 1 个文件，实际 %d 个", len(entries))
	}
}

// ── 断连与重连 ──────────────────────────────────────────────────────────

// TestBackoffIsBoundedAndJittered 钉住退避的两条性质。
func TestBackoffIsBoundedAndJittered(t *testing.T) {
	// 上限：一个断了一小时的节点应当在 mechd 回来后一分钟内重连上
	d := BackoffMin
	for i := 0; i < 20; i++ {
		d = min(d*2, BackoffMax)
	}
	if d != BackoffMax {
		t.Errorf("退避应收敛到上限 %v，实际 %v", BackoffMax, d)
	}

	// 抖动：没有它，一次 mechd 重启会让全部节点在同一毫秒重连
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[jitter(BackoffMax)] = true
	}
	if len(seen) < 10 {
		t.Errorf("退避应带抖动，50 次只得到 %d 个不同值", len(seen))
	}
	for d := range seen {
		if d < BackoffMax*3/4 || d > BackoffMax*5/4 {
			t.Errorf("抖动应在 ±25%% 内，实际 %v", d)
		}
	}
}

// TestReconnectGetsFullResync 钉住「重连即全量重推」。
//
// 因此 mechlet 侧不需要任何「补差」逻辑，mechd 侧也不需要记住上次给过什么。
func TestReconnectGetsFullResync(t *testing.T) {
	h := newHarness(t)
	h.back.setAssignment("n1", []InstanceSpec{
		{Spec: makeSpec(t, "a", "server", "n1")},
	})

	// 第一次连上
	ctx1, cancel1 := context.WithCancel(context.Background())
	if err := h.client.Register(ctx1); err != nil {
		t.Fatal(err)
	}
	c1 := newCollector()
	done := make(chan struct{})
	go func() { _ = h.client.Subscribe(ctx1, c1); close(done) }()
	c1.wait(t)

	// 断开
	cancel1()
	<-done

	// 断连期间期望状态变了
	h.back.setAssignment("n1", []InstanceSpec{
		{Spec: makeSpec(t, "a", "server", "n1")},
		{Spec: makeSpec(t, "c", "server", "n1")},
	})

	// 重连：不做任何补差，直接拿到全量
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	if err := h.client.Register(ctx2); err != nil {
		t.Fatal(err)
	}
	c2 := newCollector()
	go func() { _ = h.client.Subscribe(ctx2, c2) }()

	specs := c2.wait(t)
	if len(specs) != 2 {
		t.Fatalf("重连应当收到全量的 2 份，实际 %d 份 %v", len(specs), componentsOf(specs))
	}
}

// TestSubscribeWithStaleSessionRefused 钉住会话失效要重新握手。
func TestSubscribeWithStaleSessionRefused(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.client.Register(ctx); err != nil {
		t.Fatal(err)
	}
	h.srv.newSession("n1") // 服务端换了会话

	err := h.client.Subscribe(ctx, newCollector())
	if err == nil {
		t.Fatal("拿着过期会话订阅应当被拒绝")
	}
	if !strings.Contains(err.Error(), "重新 Register") {
		t.Errorf("错误信息应告诉 agent 该怎么办，实际: %v", err)
	}
}

// TestRunReconnectsAfterServerRestart 是最接近真实的一条：
// mechd 重启，mechlet 自己爬起来重连并拿到全量。
func TestRunReconnectsAfterServerRestart(t *testing.T) {
	back := newBackend()
	back.setAssignment("n1", []InstanceSpec{{Spec: makeSpec(t, "a", "server", "n1")}})
	srv := NewServer(ServerOptions{
		Backend: back, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	// 走回环 TCP 而非 unix socket：这条测的是**重连**，而 unix socket 的
	// 路径处理在各平台上不一致，会把平台差异混进一条协议行为的测试里。
	// unix socket 本身另有 TestUnixSocketTransport 覆盖。
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()

	serve := func(l net.Listener) *grpc.Server {
		gs := grpc.NewServer()
		agentpb.RegisterAgentServer(gs, srv)
		go func() { _ = gs.Serve(l) }()
		return gs
	}
	gs := serve(lis)

	cli, err := Dial(ClientOptions{
		Target: addr, Node: "n1",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := newCollector()
	go func() { _ = cli.Run(ctx, c) }()

	c.wait(t) // 首次下发
	before := c.count()

	// mechd 重启：连接断开，mechlet 应当自己退避重连。
	// **不用 systemd Requires= 把 mechlet 绑到 mechd 上**——那会让 mechd
	// 的一次重启把所有 mechlet 拖下水（01-architecture §4.2）。
	gs.Stop()
	back.setAssignment("n1", []InstanceSpec{
		{Spec: makeSpec(t, "a", "server", "n1")},
		{Spec: makeSpec(t, "b", "server", "n1")},
	})

	lis2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("重新监听 %s: %v", addr, err)
	}
	gs2 := serve(lis2)
	defer gs2.Stop()

	deadline := time.After(50 * time.Second)
	for c.count() <= before {
		select {
		case <-deadline:
			t.Fatal("mechd 重启后 mechlet 没有重连并拿到全量")
		case <-time.After(100 * time.Millisecond):
		}
	}

	c.mu.Lock()
	last := c.calls[len(c.calls)-1]
	c.mu.Unlock()
	if len(last) != 2 {
		t.Errorf("重连后应拿到全量的 2 份，实际 %d 份", len(last))
	}
}

// TestUnixSocketTransport 钉住单机形态的传输方式。
//
// 单机走 unix socket 且不用 mTLS：文件系统权限已经表达了「谁能连」，
// 再加一层证书是纯粹的运维负担（01-architecture §2.3）。
func TestUnixSocketTransport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上 gRPC 的 unix:// 目标不接受盘符路径；生产形态是 Linux")
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "m.sock")

	back := newBackend()
	back.setAssignment("n1", []InstanceSpec{{Spec: makeSpec(t, "a", "server", "n1")}})
	srv := NewServer(ServerOptions{
		Backend: back, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("本平台不支持 unix socket: %v", err)
	}
	gs := grpc.NewServer()
	agentpb.RegisterAgentServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	cli, err := Dial(ClientOptions{
		Target: "unix://" + sock, Node: "n1",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := cli.Register(ctx); err != nil {
		t.Fatalf("经 unix socket 注册: %v", err)
	}
	c := newCollector()
	go func() { _ = cli.Subscribe(ctx, c) }()

	if specs := c.wait(t); len(specs) != 1 {
		t.Errorf("应经 unix socket 收到 1 份规格，实际 %d 份", len(specs))
	}
}

// TestReportSurvivesWithoutSubscribe 钉住「不用双向流」的收益。
//
// 上报与下发分开之后，一条通道出问题不影响另一条。
func TestReportSurvivesWithoutSubscribe(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.client.Register(ctx); err != nil {
		t.Fatal(err)
	}
	// 从不 Subscribe，照样能上报
	for i := 0; i < 3; i++ {
		if _, err := h.client.Report(ctx, Report{Node: "n1"}); err != nil {
			t.Fatalf("第 %d 次上报失败: %v", i, err)
		}
	}
	if h.back.reportCount() != 3 {
		t.Errorf("应收到 3 次上报，实际 %d 次", h.back.reportCount())
	}
}

// ── 端到端 ──────────────────────────────────────────────────────────────

// TestFullLoop 是第 6 步的验收：**连上、拿到规格、上报 digest**。
//
// 走的是真实的 gRPC 服务端与客户端，不是打桩。它同时演示了收敛判据的
// 闭环：mechd 下发一份 digest 为 X 的规格，mechlet 收下、还原密钥、
// 把 X 报回来——Rollout 据此判定这一批完成。
func TestFullLoop(t *testing.T) {
	h := newHarness(t)

	want := makeSpec(t, "demo", "server", "n1")
	want.SecretRefs = []spec.SecretRef{{ID: "s1", Version: 1, Param: "pw"}}
	want.Resources = []spec.Resource{{
		ID: "conf", Type: "file",
		Args: json.RawMessage(`{"path":"/etc/demo.conf","content":"pw=` +
			spec.SecretToken("s1") + `"}`),
	}}
	if err := spec.Seal(want); err != nil {
		t.Fatal(err)
	}
	h.back.setAssignment("n1", []InstanceSpec{
		{Spec: want, Secrets: map[string]string{"s1": "p4ssw0rd-from-vault"}},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ① 连上
	if err := h.client.Register(ctx); err != nil {
		t.Fatalf("注册: %v", err)
	}

	// ② 拿到规格
	c := newCollector()
	go func() { _ = h.client.Subscribe(ctx, c) }()
	got := c.wait(t)
	if len(got) != 1 {
		t.Fatalf("应收到 1 份规格，实际 %d 份", len(got))
	}
	if got[0].Spec.Digest != want.Digest {
		t.Fatalf("规格在传输中变了: 期望 digest %s，实际 %s",
			want.Digest, got[0].Spec.Digest)
	}

	// mechlet 在写盘的最后一刻还原密钥
	final, err := spec.ResolveSecrets(got[0].Spec, got[0].Secrets)
	if err != nil {
		t.Fatalf("还原密钥: %v", err)
	}
	if !bytes.Contains(final.Resources[0].Args, []byte("pw=p4ssw0rd-from-vault")) {
		t.Errorf("消费点应是明文，实际 %s", final.Resources[0].Args)
	}
	// 还原**不改变身份**：digest 覆盖的是 secretRefs 的 id 与 version，
	// 不是值。否则每次还原都会得到一个新 generation。
	if final.Digest != want.Digest {
		t.Errorf("还原密钥不该改变 digest：%s → %s", want.Digest, final.Digest)
	}

	// ③ 上报 digest
	resync, err := h.client.Report(ctx, Report{
		Node: "n1",
		Instances: []InstanceStatus{{
			Component: "demo", Role: "server",
			Digest: got[0].Spec.Digest, Generation: 1, Result: "ok",
			Health: &HealthStatus{State: "healthy"},
		}},
	})
	if err != nil {
		t.Fatalf("上报: %v", err)
	}
	if resync {
		t.Error("会话有效时不该要求重新握手")
	}

	h.back.mu.Lock()
	defer h.back.mu.Unlock()
	if len(h.back.reports) != 1 {
		t.Fatalf("mechd 应收到 1 次上报，实际 %d 次", len(h.back.reports))
	}
	// **收敛判据**：上报的 digest == 期望的 digest 且健康
	st := h.back.reports[0].Instances[0]
	if st.Digest != want.Digest {
		t.Errorf("上报的 digest 应等于下发的，期望 %s，实际 %s", want.Digest, st.Digest)
	}
	if st.Health == nil || st.Health.State != "healthy" {
		t.Errorf("健康状态传丢了: %+v", st.Health)
	}
}

// ── 协议形状 ────────────────────────────────────────────────────────────

// TestAssignmentHasNoVerbs 钉住 17-protocol §7 那条根本性的约束。
//
// mechlet 收到的是「这台机器上应该有什么」，不是「去做什么」。指令式接口
// 会让断连自治不可能——这条得靠人守，因此把它写成测试。
func TestAssignmentHasNoVerbs(t *testing.T) {
	fields := (&agentpb.Assignment{}).ProtoReflect().Descriptor().Fields()
	var names []string
	for i := 0; i < fields.Len(); i++ {
		names = append(names, string(fields.Get(i).Name()))
	}
	got := strings.Join(names, ",")

	// 每一个字段都必须是**这台机器现在是什么情况**，不是**去做什么**：
	//
	//	specs          应该有哪些实例
	//	full_sync      这是不是一份完整快照
	//	revision       仅供排障
	//	cordoned       这台机器当前被暂停调和（M7 第 6 步）
	//	purge_orphans  哪些孤儿**不该继续留在这台机器上**（M9 第 4 步）
	//
	// `cordoned` 通过这条判据：它是状态，随全量一起重推，节点不需要记住
	// 任何一次性指令——重连之后自然还是对的。若写成 `pause_reconcile`
	// 那种动词，断连期间的语义就得由节点自己猜。
	//
	// `purge_orphans` 同样通过，而且是刻意按这条判据设计的：
	//
	//   · 它说的是「这些孤儿不该存在」，不是「去执行一次删除」
	//   · 每轮下发都重复带上，节点每次照做一遍；删一个已经不在的目录
	//     是 no-op，断连三天回来仍然会收到它
	//   · **自限**：节点清完之后本地收据就没了，孤儿不再出现在上报里，
	//     mechd 跟着丢掉这一项——不需要任何确认序号
	//
	// 带的是**实例键而不是绝对路径**，这一点也是判据的一部分：路径是
	// 「去删这个」，实例键是「这个东西不该在」。节点删的是自己收据里
	// 记的目录，因此一个同名重新部署过的组件不会被误删。
	want := "specs,full_sync,revision,cordoned,purge_orphans"
	if got != want {
		t.Errorf("Assignment 的字段变成了 %q（原为 %q）。\n"+
			"  若新增的是动词（execute / restart / run_hook 之类），请重新考虑：\n"+
			"  下发的必须是**期望状态**，指令式接口会让断连自治不可能", got, want)
	}
}

// TestSpecJSONIsOpaqueToProto 钉住规格用 JSON 而非展开成 proto 字段。
//
// ResolvedSpec 的结构由 pack/v1 决定，会持续演进；把它展开成 proto 会让
// 每次加字段都变成一次协议变更与两端同时升级。
func TestSpecJSONIsOpaqueToProto(t *testing.T) {
	f := (&agentpb.ResolvedSpecEnvelope{}).ProtoReflect().Descriptor().
		Fields().ByName("spec_json")
	if f == nil {
		t.Fatal("信封里应当有 spec_json")
	}
	if f.Kind().String() != "bytes" {
		t.Errorf("spec_json 应当是 bytes（不透明），实际 %s", f.Kind())
	}
}

func TestPackageIsVersioned(t *testing.T) {
	name := string((&agentpb.Assignment{}).ProtoReflect().Descriptor().FullName())
	if !strings.HasPrefix(name, "mecharion.agent.v1.") {
		t.Errorf("包名必须带大版本（不兼容变更换包名，两版并存），实际 %s", name)
	}
}
