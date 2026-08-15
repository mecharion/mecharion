// Package protocol 实现 mechd ↔ mechlet 的 gRPC 协议两侧。
//
// 设计见 docs/design/17-protocol.md 与 ADR-0029。三条性质贯穿全包：
//
//   - **mechlet 主动拨出**：节点上零入站端口
//   - **下发里没有动词**：Assignment 是「这台机器上应该有什么」，
//     不是「去做什么」。这正是 mechlet 断连时仍能工作的原因
//   - **全量重推，不做增量**：下发幂等（digest 相同即无操作），
//     一个内容寻址的期望状态让整类同步协议问题消失
package protocol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/mecharion/mecharion/internal/protocol/agentpb"
	"github.com/mecharion/mecharion/internal/spec"
)

// SpecSchemaMin 是 mechd 仍愿意服务的最低 ResolvedSpec 版本。
//
// 低于它的 mechlet 被明确拒绝并告知要升到哪个版本——静默降级服务会让
// 一个装了老 agent 的节点表现得像「配置没生效」，那是最难查的一类故障。
const SpecSchemaMin = 1

// BlobChunkSize 是 FetchBlob 的分块大小。
const BlobChunkSize = 1 << 20 // 1MB

// Backend 是 mechd 侧需要提供的能力。
//
// 定义成接口而不是直接依赖 mechd 的实现，是为了让协议层可以脱离数据库
// 单独测——协议的正确性（版本协商、全量语义、密钥不落盘）与数据从哪来无关。
type Backend interface {
	// Register 记录一次节点上线，返回该节点已知的名字。
	//
	// 返回 error 表示拒绝该节点（未知节点、名字冲突等）。
	Register(ctx context.Context, node NodeRegistration) error

	// Assignment 返回某节点当前**应有的全部实例**。
	//
	// 永远是全量：调用方不需要知道上次给过什么。
	Assignment(ctx context.Context, nodeName string) ([]InstanceSpec, error)

	// Report 接收一次状态上报。
	Report(ctx context.Context, r Report) error

	// Events 接收补报的事件，返回已接收数量。
	Events(ctx context.Context, nodeName string, events []Event) (int, error)

	// OpenBlob 按 sha256 打开一个载荷。
	OpenBlob(ctx context.Context, sha256 string) (io.ReadSeekCloser, int64, error)
}

// NodeRegistration 是一次注册携带的信息。
type NodeRegistration struct {
	Name         string
	AgentVersion string
	Address      string
	Labels       map[string]string
	Capabilities []Capability
}

// Capability 是节点上某项运行时的能力。
type Capability struct {
	Name      string
	Version   string
	Available bool
	Detail    map[string]string
}

// InstanceSpec 是一份待下发的规格及其密钥明文。
type InstanceSpec struct {
	Spec *spec.ResolvedSpec
	// Secrets 是 id → 明文。它不进规格、不进日志。
	//
	// **落盘的只有 mechlet 侧的信封加密副本**（ADR-0033）——周期调和要在
	// mechd 不在时也能渲染出配置，而规格里只有哨兵。
	Secrets map[string]string
}

// Report 是一次状态上报。
type Report struct {
	Node      string
	Instances []InstanceStatus
	Facts     []byte
	FactsAt   string
	// Orphans 是**机器上还在、但下发里没有**的实例键。
	//
	// 只报不删：卸载不可逆，而「mechd 少发了一条」与「用户真的删了这个
	// 组件」在节点侧分辨不了（20-continuous-reconcile §2.4）。
	Orphans []string
	// OrphanRecords 是同一批孤儿，外加它们在盘上留下了什么。
	//
	// 与 Orphans 并存而非取代：两个列表由同一段代码同时产出，不会分叉。
	OrphanRecords []OrphanRecord
}

// OrphanRecord 是一个孤儿实例，以及它在盘上留下的东西。
type OrphanRecord struct {
	Key string
	// RetainedPaths 是卸载时故意留下的目录。
	//
	// 空表示这个孤儿不是 remove 留下的，而是「下发里没有它了」——
	// 那时机器上留着的是一整个还装着的实例，不只是几个目录。
	RetainedPaths []string
	// Pack / Version 说明这堆东西是谁、哪一版留下的，决定「能不能删」
	// 时唯一要紧的信息。
	Pack      string
	Version   string
	RemovedAt string
}

// InstanceStatus 是一个实例的观测状态。
type InstanceStatus struct {
	Component string
	Role      string
	// WorkloadAction 是**最近一次**对工作负载的纠正：restored | stopped，
	// WorkloadActionAt 是它发生的时刻。
	//
	// 带时间才让它成为状态。不带的话它只是「上一轮做了什么」，而上报是
	// 周期快照——调和比上报密时那一轮会被覆盖，事件就丢了。
	WorkloadAction   string
	WorkloadActionAt string
	// Removed 表示这个实例已经按 runState: removed 拆干净了。
	//
	// **digest 在这里帮不上忙**：RunState 不参与 digest，一个拆完的实例
	// 与一个装着的实例上报的 digest 一模一样。这是 mechd 把 Component 从
	// removing 推进到「记录删除」的唯一依据。
	Removed bool
	// RetainedPaths 是卸载时故意留下的目录（数据默认保留）。
	RetainedPaths []string

	// RolledBackFrom 是被节点自动回滚掉的那个 digest。
	//
	// mechd 看到的只是「上报的 digest 不是期望的那个」，而那与
	// 「还没来得及升级」长得一样——这一条才把两者区分开。
	RolledBackFrom string
	// Digest 是收敛判据：Rollout 靠「上报的 digest == 期望的 digest 且健康」
	// 判定一批完成，而不是靠 mechlet 说「我成功了」。
	Digest     string
	Generation int
	Result     string
	Message    string
	Workload   *WorkloadStatus
	Health     *HealthStatus
	Resources  []ResourceStatus
	Suppressed []SuppressedDrift
}

// WorkloadStatus 是工作负载的观测状态。
type WorkloadStatus struct {
	Runtime  string
	State    string
	PID      int64
	Since    string
	Restarts int
}

// HealthStatus 是健康检查的观测状态。
type HealthStatus struct {
	State               string
	ConsecutiveFailures int
	LastError           string
	CheckedAt           string
}

// ResourceStatus 是单条资源的观测状态。
type ResourceStatus struct {
	ID     string
	Type   string
	State  string
	Detail string
}

// SuppressedDrift 是一条被 ack-drift 抑制的漂移。
type SuppressedDrift struct {
	ResourceID string
	Reason     string
	Until      string
}

// Event 是一条补报的事件。
type Event struct {
	At        string
	Level     string
	Component string
	Role      string
	Kind      string
	Message   string
}

// ServerOptions 配置服务端。
type ServerOptions struct {
	Backend Backend
	Log     *slog.Logger
	Intervals
}

// Intervals 是下发给 mechlet 的三个周期。**彼此独立可配**：
// 调和 60s、健康 15s、上报 15s（11-resource-engine §3.2）。
type Intervals struct {
	ReconcileSeconds uint32
	HealthSeconds    uint32
	ReportSeconds    uint32
}

func (i Intervals) withDefaults() Intervals {
	if i.ReconcileSeconds == 0 {
		i.ReconcileSeconds = 60
	}
	if i.HealthSeconds == 0 {
		i.HealthSeconds = 15
	}
	if i.ReportSeconds == 0 {
		i.ReportSeconds = 15
	}
	return i
}

// Server 实现 agentpb.AgentServer。
type Server struct {
	agentpb.UnimplementedAgentServer

	backend   Backend
	log       *slog.Logger
	intervals Intervals

	mu       sync.Mutex
	sessions map[string]*session // node name → 当前会话
	revision uint64
	// rejected 记录已经喊过一声的被吊销节点，避免审计与日志被重试淹掉。
	rejected map[string]bool

	// tasks 是 ad-hoc 命令通道（ADR-0038），与期望状态那条彻底分开。
	tasks *taskHub

	// draining 一旦关闭，所有订阅流自己收摊。
	//
	// **它存在的理由是关闭**：Subscribe 是长连的服务端流，正常情况下
	// 永远不返回。而 grpc 的 GracefulStop 会等所有活跃 RPC 结束——两者
	// 撞在一起，mechd 收到 SIGTERM 之后会一直挂到 systemd 的
	// TimeoutStopSec 再吃一发 SIGKILL：每次重启白等半分钟，而且 SQLite
	// 的 WAL 没有干净关闭。
	draining  chan struct{}
	drainOnce sync.Once
}

// session 是一个已连上的 mechlet。
type session struct {
	id   string
	node string
	// wake 是「有新东西要推」的信号。容量 1 且非阻塞写：
	// 连续多次变更合并成一次推送即可，因为推的是**全量**。
	wake chan struct{}
}

// NewServer 构造服务端。
func NewServer(opts ServerOptions) *Server {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		backend:   opts.Backend,
		log:       log,
		intervals: opts.Intervals.withDefaults(),
		sessions:  map[string]*session{},
		rejected:  map[string]bool{},
		draining:  make(chan struct{}),
		tasks:     newTaskHub(),
	}
}

// Register 实现版本协商与节点上线。
func (s *Server) Register(
	ctx context.Context, req *agentpb.RegisterRequest,
) (*agentpb.RegisterResponse, error) {
	node, err := s.nodeOf(ctx, req.GetNodeName())
	if err != nil {
		return nil, err
	}

	schema, err := negotiate(req.GetMaxSpecSchema())
	if err != nil {
		return nil, err
	}

	caps := make([]Capability, 0, len(req.GetCaps()))
	for _, c := range req.GetCaps() {
		caps = append(caps, Capability{
			Name: c.GetName(), Version: c.GetVersion(),
			Available: c.GetAvailable(), Detail: c.GetDetail(),
		})
	}
	reg := NodeRegistration{
		Name:         node,
		AgentVersion: req.GetAgentVersion(),
		Address:      req.GetAddress(),
		Labels:       req.GetLabels(),
		Capabilities: caps,
	}
	if err := s.backend.Register(ctx, reg); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "注册 %s: %v", reg.Name, err)
	}

	sess := s.newSession(node)
	s.log.Info("node registered",
		"node", reg.Name, "agent", reg.AgentVersion, "spec_schema", schema)

	return &agentpb.RegisterResponse{
		SpecSchema:         schema,
		ReconcileIntervalS: s.intervals.ReconcileSeconds,
		ReportIntervalS:    s.intervals.ReportSeconds,
		HealthIntervalS:    s.intervals.HealthSeconds,
		Session:            sess.id,
	}, nil
}

// negotiate 实现「控制面向后兼容 agent」。
//
//	下发版本 = min(mechd 支持的最高版本, mechlet 上报的 max_spec_schema)
//
// 方向是刻意的：多节点升级时先升控制面还是先升 agent，必须是运维自己能
// 决定的事；要求两者同版本会让升级变成一次全停。
func negotiate(agentMax uint32) (uint32, error) {
	if agentMax == 0 {
		return 0, status.Error(codes.InvalidArgument, "缺少 max_spec_schema")
	}
	if agentMax < SpecSchemaMin {
		return 0, status.Errorf(codes.FailedPrecondition,
			"mechlet 最高只支持 ResolvedSpec v%d，本 mechd 的下限是 v%d —— 请先升级 mechlet",
			agentMax, SpecSchemaMin)
	}
	if agentMax > spec.SchemaVersion {
		// mechlet 比 mechd 新：允许，按旧结构工作
		return spec.SchemaVersion, nil
	}
	return agentMax, nil
}

func (s *Server) newSession(node string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.revision++
	sess := &session{
		id:   fmt.Sprintf("%s-%d", node, s.revision),
		node: node,
		wake: make(chan struct{}, 1),
	}
	// 同一节点重连时，旧会话的流会在下一次 Send 或 ctx 取消时收摊
	s.sessions[node] = sess
	return sess
}

// Subscribe 是服务端流：拨上来就先推一次全量，之后按需推送。
func (s *Server) Subscribe(
	req *agentpb.SubscribeRequest, stream agentpb.Agent_SubscribeServer,
) error {
	node, err := s.nodeOf(stream.Context(), req.GetNodeName())
	if err != nil {
		return err
	}
	sess := s.session(node)
	if sess == nil || (req.GetSession() != "" && sess.id != req.GetSession()) {
		return status.Error(codes.FailedPrecondition, "会话不存在或已失效，请重新 Register")
	}

	ctx := stream.Context()

	// **连上就先推全量**：重连、mechd 重启、期望状态变化——都走同一条路径。
	// 不区分「首次」与「后续」，因为区分了就会有一条只在重连时才走的代码，
	// 而那条路径几乎不可能被测到。
	if err := s.push(ctx, stream, node); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			s.dropSession(node, sess)
			return ctx.Err()
		case <-s.draining:
			// mechd 要关了：**主动收摊**，别让 GracefulStop 等一个
			// 永远不会结束的流。
			//
			// 对节点来说这与一次网络抖动没有区别——它会重连，而重连
			// 就是重推全量（ADR-0029）。因此这里不需要节点侧配合。
			s.dropSession(node, sess)
			return status.Error(codes.Unavailable, "mechd 正在关闭，请重连")
		case <-sess.wake:
			if s.session(node) != sess {
				return status.Error(codes.Aborted, "该节点已建立新会话")
			}
			if err := s.push(ctx, stream, node); err != nil {
				return err
			}
		}
	}
}

func (s *Server) push(
	ctx context.Context, stream agentpb.Agent_SubscribeServer, node string,
) error {
	specs, err := s.backend.Assignment(ctx, node)
	if err != nil {
		return status.Errorf(codes.Internal, "取 %s 的期望状态: %v", node, err)
	}

	msg := &agentpb.Assignment{
		FullSync: true, Revision: s.currentRevision(),
		Cordoned:     s.cordoned(ctx, node),
		PurgeOrphans: s.purgeOrphans(ctx, node),
	}
	for _, in := range specs {
		env, err := encodeSpec(in)
		if err != nil {
			return status.Errorf(codes.Internal, "编码 %s 的规格: %v", node, err)
		}
		msg.Specs = append(msg.Specs, env)
	}
	return stream.Send(msg)
}

// encodeSpec 把一份规格连同密钥打包。
func encodeSpec(in InstanceSpec) (*agentpb.ResolvedSpecEnvelope, error) {
	if in.Spec == nil {
		return nil, errors.New("规格为空")
	}
	blob, err := spec.Marshal(in.Spec)
	if err != nil {
		return nil, err
	}
	env := &agentpb.ResolvedSpecEnvelope{SpecJson: blob}

	// 只下发这份规格真正引用到的密钥。多给不会更方便，只会扩大暴露面。
	if len(in.Secrets) > 0 && len(in.Spec.SecretRefs) > 0 {
		env.Secrets = map[string][]byte{}
		for _, ref := range in.Spec.SecretRefs {
			v, ok := in.Secrets[ref.ID]
			if !ok {
				return nil, fmt.Errorf("规格引用了密钥 %s，但没有它的值", ref.ID)
			}
			env.Secrets[ref.ID] = []byte(v)
		}
	}
	return env, nil
}

// Notify 唤醒某个节点的下发流。变更发生后由 mechd 调用。
//
// 非阻塞：wake 容量为 1，连续多次变更合并成一次推送。这是安全的，
// 因为推的是全量而不是增量——合并掉的中间态本就不需要单独送达。
func (s *Server) Notify(node string) {
	s.mu.Lock()
	sess := s.sessions[node]
	s.revision++
	s.mu.Unlock()

	if sess == nil {
		return // 该节点没连上，等它 Subscribe 时自然拿到最新的
	}
	select {
	case sess.wake <- struct{}{}:
	default:
	}
}

// NotifyAll 唤醒全部已连接节点。
func (s *Server) NotifyAll() {
	s.mu.Lock()
	nodes := make([]string, 0, len(s.sessions))
	for n := range s.sessions {
		nodes = append(nodes, n)
	}
	s.mu.Unlock()

	for _, n := range nodes {
		s.Notify(n)
	}
}

// Drain 让所有订阅流收摊，供优雅关闭用。
//
// **不调它的话，优雅关闭会挂住。** grpc 的 GracefulStop 等所有活跃 RPC
// 结束，而 Subscribe 是长连的服务端流，正常情况下永远不返回——mechd 因此
// 会一直挂到 systemd 的 TimeoutStopSec 再被 SIGKILL。
//
// 可以重复调用：关闭路径上被调两次不该 panic，而那正是它最容易发生的
// 地方（信号处理与 defer 撞在一起）。
func (s *Server) Drain() {
	s.drainOnce.Do(func() { close(s.draining) })
}

// Connected 报告某节点当前是否有活跃会话。
func (s *Server) Connected(node string) bool { return s.session(node) != nil }

func (s *Server) session(node string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[node]
}

func (s *Server) dropSession(node string, sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[node] == sess {
		delete(s.sessions, node)
	}
}

func (s *Server) currentRevision() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

// Report 接收状态上报。
func (s *Server) Report(
	ctx context.Context, req *agentpb.ReportRequest,
) (*agentpb.ReportResponse, error) {
	node, err := s.nodeOf(ctx, req.GetNodeName())
	if err != nil {
		return nil, err
	}

	r := Report{Node: node, Facts: req.GetFacts().GetFactsJson(),
		Orphans: req.GetOrphans(), OrphanRecords: decodeOrphans(req.GetOrphanRecords()),
		FactsAt: req.GetFacts().GetCollectedAt()}
	for _, in := range req.GetInstances() {
		r.Instances = append(r.Instances, decodeStatus(in))
	}
	if err := s.backend.Report(ctx, r); err != nil {
		return nil, status.Errorf(codes.Internal, "接收 %s 的上报: %v", node, err)
	}

	// 会话对不上时让 mechlet 重新握手，而不是默默接受——否则一个用着
	// 过期会话的 agent 会一直上报，看起来一切正常
	resync := req.GetSession() != "" && !s.sessionMatches(node, req.GetSession())
	return &agentpb.ReportResponse{Resync: resync}, nil
}

func (s *Server) sessionMatches(node, id string) bool {
	sess := s.session(node)
	return sess != nil && sess.id == id
}

func decodeStatus(in *agentpb.InstanceStatus) InstanceStatus {
	out := InstanceStatus{
		Component: in.GetComponent(), Role: in.GetRole(),
		Digest: in.GetDigest(), Generation: int(in.GetGeneration()),
		Result: in.GetResult(), Message: in.GetMessage(),
		WorkloadAction: in.GetWorkloadAction(), WorkloadActionAt: in.GetWorkloadActionAt(),
		RolledBackFrom: in.GetRolledBackFrom(),
		Removed:        in.GetRemoved(), RetainedPaths: in.GetRetainedPaths(),
	}
	if w := in.GetWorkload(); w != nil {
		out.Workload = &WorkloadStatus{
			Runtime: w.GetRuntime(), State: w.GetState(), PID: w.GetPid(),
			Since: w.GetSince(), Restarts: int(w.GetRestarts()),
		}
	}
	if h := in.GetHealth(); h != nil {
		out.Health = &HealthStatus{
			State: h.GetState(), ConsecutiveFailures: int(h.GetConsecutiveFailures()),
			LastError: h.GetLastError(), CheckedAt: h.GetCheckedAt(),
		}
	}
	for _, r := range in.GetResources() {
		out.Resources = append(out.Resources, ResourceStatus{
			ID: r.GetId(), Type: r.GetType(), State: r.GetState(), Detail: r.GetDetail(),
		})
	}
	for _, d := range in.GetSuppressed() {
		out.Suppressed = append(out.Suppressed, SuppressedDrift{
			ResourceID: d.GetResourceId(), Reason: d.GetReason(), Until: d.GetUntil(),
		})
	}
	return out
}

// PushEvents 接收断连期间缓冲的事件。
func (s *Server) PushEvents(
	ctx context.Context, req *agentpb.PushEventsRequest,
) (*agentpb.PushEventsResponse, error) {
	events := make([]Event, 0, len(req.GetEvents()))
	for _, e := range req.GetEvents() {
		events = append(events, Event{
			At: e.GetAt(), Level: e.GetLevel(), Component: e.GetComponent(),
			Role: e.GetRole(), Kind: e.GetKind(), Message: e.GetMessage(),
		})
	}
	node, err := s.nodeOf(ctx, req.GetNodeName())
	if err != nil {
		return nil, err
	}
	n, err := s.backend.Events(ctx, node, events)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "接收事件: %v", err)
	}
	return &agentpb.PushEventsResponse{Accepted: uint32(n)}, nil
}

// FetchBlob 按 sha256 分块流式返回载荷。
func (s *Server) FetchBlob(
	req *agentpb.FetchBlobRequest, stream agentpb.Agent_FetchBlobServer,
) error {
	sum := req.GetSha256()
	if err := checkSHA256(sum); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}

	rc, size, err := s.backend.OpenBlob(stream.Context(), sum)
	if err != nil {
		return status.Errorf(codes.NotFound, "载荷 %s: %v", short(sum), err)
	}
	defer rc.Close()

	if off := req.GetOffset(); off > 0 {
		if off > size {
			return status.Errorf(codes.InvalidArgument,
				"断点 %d 超过载荷长度 %d", off, size)
		}
		if _, err := rc.Seek(off, io.SeekStart); err != nil {
			return status.Errorf(codes.Internal, "定位到 %d: %v", off, err)
		}
	}

	buf := make([]byte, BlobChunkSize)
	first := true
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			chunk := &agentpb.BlobChunk{Data: buf[:n]}
			if first {
				chunk.TotalSize = size
				first = false
			}
			if err := stream.Send(chunk); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return status.Errorf(codes.Internal, "读取载荷 %s: %v", short(sum), err)
		}
	}
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}

// RenewCert 用当前证书换一张新的。
//
// **身份由 mTLS 确定**：nodeOf 已经把「你是谁」和「你还能不能连」都问过了，
// 因此这里不需要任何额外凭据。一台连不出有效证书的机器根本走不到这儿——
// 它该走重新加入。
func (s *Server) RenewCert(
	ctx context.Context, req *agentpb.RenewCertRequest,
) (*agentpb.RenewCertResponse, error) {
	node, err := s.nodeOf(ctx, req.GetNodeName())
	if err != nil {
		return nil, err
	}
	if _, mtls := peerCN(ctx); !mtls {
		// 单机走 unix socket，本来就没有证书可续。明确拒绝比签一张
		// 没人用的证书好——后者会让人以为轮换在单机上也生效了。
		return nil, status.Error(codes.FailedPrecondition,
			"这条连接没有证书（unix socket），无从续期")
	}
	renewer, ok := s.backend.(CertRenewer)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "本 mechd 不支持证书续期")
	}
	cert, ca, err := renewer.RenewCert(ctx, node, []byte(req.GetCsrPem()))
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "续期证书: %v", err)
	}
	s.log.Info("certificate renewed for node", "node", node)
	return &agentpb.RenewCertResponse{
		CertPem: string(cert), CaPem: string(ca),
	}, nil
}

// cordoned 查这台机器现在是不是被暂停调和了。
//
// **查不到就当没被 cordon**：一次查询失败不该让整台机器停下来——那与
// cordon 的语义相反（它是「别动」，不是「什么都别做」），而且下发本来
// 就是幂等的，下一轮会带上正确的值。
// purgeOrphans 取该节点上待清理的孤儿键。
//
// 查不到时返回空而不是报错：purge 是一个补救动作，让它的一次查询失败
// 拖垮整条下发，代价远大于「这一轮先不清」。下一轮会再问一次。
func (s *Server) purgeOrphans(ctx context.Context, node string) []string {
	p, ok := s.backend.(Purger)
	if !ok {
		return nil
	}
	keys, err := p.PurgeOrphans(ctx, node)
	if err != nil {
		s.log.Warn("failed to query orphans pending cleanup, skipping this round", "node", node, "err", err)
		return nil
	}
	return keys
}

func (s *Server) cordoned(ctx context.Context, node string) bool {
	c, ok := s.backend.(Cordoner)
	if !ok {
		return false
	}
	yes, err := c.Cordoned(ctx, node)
	if err != nil {
		s.log.Warn("failed to query cordon status, dispatching as not-paused this round", "node", node, "err", err)
		return false
	}
	return yes
}

// decodeOrphans 把线上的孤儿记录转成本包的类型。
func decodeOrphans(in []*agentpb.OrphanRecord) []OrphanRecord {
	if len(in) == 0 {
		return nil
	}
	out := make([]OrphanRecord, 0, len(in))
	for _, r := range in {
		out = append(out, OrphanRecord{
			Key: r.GetKey(), RetainedPaths: r.GetRetainedPaths(),
			Pack: r.GetPack(), Version: r.GetVersion(), RemovedAt: r.GetRemovedAt(),
		})
	}
	return out
}
