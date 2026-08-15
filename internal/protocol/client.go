package protocol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/mecharion/mecharion/internal/protocol/agentpb"
	"github.com/mecharion/mecharion/internal/spec"
)

// 重连退避参数。
const (
	// BackoffMin 是首次重试的等待。
	BackoffMin = 1 * time.Second
	// BackoffMax 是退避上限。**不设成无穷**：一个断了一小时的节点应当在
	// mechd 回来后一分钟内重连上，而不是等半小时。
	BackoffMax = 60 * time.Second
)

// ClientOptions 配置 mechlet 侧的客户端。
type ClientOptions struct {
	// Target 是 mechd 的地址。单机形态是 unix socket，多机是 host:port。
	Target string
	Node   string
	// AgentVersion 仅用于展示与排障。
	AgentVersion string
	Address      string
	Labels       map[string]string
	Capabilities []Capability

	Log *slog.Logger
	// DialOptions 供调用方注入 mTLS 凭证。为空时用明文——
	// 那只在 unix socket 上成立（01-architecture §2.3）。
	DialOptions []grpc.DialOption
}

// Client 是 mechlet 侧的协议实现。
type Client struct {
	opts ClientOptions
	log  *slog.Logger

	conn  *grpc.ClientConn
	agent agentpb.AgentClient

	mu         sync.Mutex
	session    string
	specSchema uint32
	intervals  Intervals
}

// Dial 连上 mechd。**不做握手**——握手在 Run 里做，因为重连时要重做一遍。
func Dial(opts ClientOptions) (*Client, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	dialOpts := opts.DialOptions
	if len(dialOpts) == 0 {
		dialOpts = []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		}
	}
	// keepalive 追加在**调用方给的选项之后**，不做成默认值的一部分：
	// 它不该被一个只想换传输凭据的调用方顺手覆盖掉。
	dialOpts = append(dialOpts, ClientKeepalive())
	conn, err := grpc.NewClient(opts.Target, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("连接 mechd (%s): %w", opts.Target, err)
	}
	return &Client{
		opts: opts, log: log,
		conn: conn, agent: agentpb.NewAgentClient(conn),
	}, nil
}

// Close 关闭连接。
func (c *Client) Close() error { return c.conn.Close() }

// Session 返回当前会话 id，未注册时为空。
func (c *Client) Session() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

// Intervals 返回 mechd 下发的三个周期。
func (c *Client) Intervals() Intervals {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.intervals
}

// Handler 接收下发的期望状态。
type Handler interface {
	// Apply 收到一次全量下发。
	//
	// 参数就是「这台机器上应该有什么」——**没有动词**。怎么达成是
	// mechlet 自己的事，这正是它断连时仍能工作的原因。
	Apply(ctx context.Context, a Assignment) error
}

// Assignment 是一次下发的全部内容。
//
// 用结构体而不是继续往参数表上加：这里已经有「规格」「是不是全量」
// 「这台机器被 cordon 了没」三件事，而它们都是**同一份状态快照**的组成
// 部分。散成参数的话，加第四件时每个实现都要改签名。
type Assignment struct {
	Specs    []InstanceSpec
	FullSync bool
	// Cordoned 表示这台机器当前被暂停调和。
	//
	// **它是状态不是动词**：随全量一起重推，节点不需要记住任何一次性
	// 指令，重连之后自然还是对的。
	Cordoned bool
	// PurgeOrphans 是该被清掉的孤儿实例键。
	//
	// **状态不是动词**，与 Cordoned 同：节点每次收到都照做一遍，删一个
	// 已经不在的目录是 no-op。带的是实例键而不是绝对路径——节点删的是
	// 自己本地收据里记的目录，因此一个同名重新部署过的组件不会被误删。
	PurgeOrphans []string
}

// HandlerFunc 让普通函数满足 Handler。
type HandlerFunc func(ctx context.Context, a Assignment) error

// Apply 实现 Handler。
func (f HandlerFunc) Apply(ctx context.Context, a Assignment) error { return f(ctx, a) }

// Register 握手并记录会话。
func (c *Client) Register(ctx context.Context) error {
	caps := make([]*agentpb.Capability, 0, len(c.opts.Capabilities))
	for _, cp := range c.opts.Capabilities {
		caps = append(caps, &agentpb.Capability{
			Name: cp.Name, Version: cp.Version,
			Available: cp.Available, Detail: cp.Detail,
		})
	}

	resp, err := c.agent.Register(ctx, &agentpb.RegisterRequest{
		NodeName:      c.opts.Node,
		AgentVersion:  c.opts.AgentVersion,
		MaxSpecSchema: spec.SchemaVersion,
		Address:       c.opts.Address,
		Labels:        c.opts.Labels,
		Caps:          caps,
	})
	if err != nil {
		return err
	}
	if resp.GetSpecSchema() > spec.SchemaVersion {
		// mechd 不该这么做（协商就是为了避免它），但真发生时必须拒绝：
		// 按更高版本解析等于把未知字段当已知字段处理
		return fmt.Errorf(
			"mechd 要用 ResolvedSpec v%d 下发，而本 mechlet 只支持到 v%d",
			resp.GetSpecSchema(), spec.SchemaVersion)
	}

	c.mu.Lock()
	c.session = resp.GetSession()
	c.specSchema = resp.GetSpecSchema()
	c.intervals = Intervals{
		ReconcileSeconds: resp.GetReconcileIntervalS(),
		HealthSeconds:    resp.GetHealthIntervalS(),
		ReportSeconds:    resp.GetReportIntervalS(),
	}
	c.mu.Unlock()
	return nil
}

// Subscribe 挂住下发流，逐条交给 handler。
//
// 返回时表示流断了；调用方（Run）负责退避重连。
func (c *Client) Subscribe(ctx context.Context, h Handler) error {
	stream, err := c.agent.Subscribe(ctx, &agentpb.SubscribeRequest{
		NodeName: c.opts.Node, Session: c.Session(),
	})
	if err != nil {
		return err
	}

	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		specs, err := decodeAssignment(msg)
		if err != nil {
			// 一份解不开的规格不该拖垮整条流：记下来，等下一次全量。
			// 全量重推让「跳过这一次」成为安全的选择。
			c.log.Error("failed to parse dispatched spec, waiting for next full sync", "err", err)
			continue
		}
		if err := h.Apply(ctx, Assignment{
			Specs: specs, FullSync: msg.GetFullSync(),
			Cordoned: msg.GetCordoned(), PurgeOrphans: msg.GetPurgeOrphans(),
		}); err != nil {
			c.log.Error("failed to apply dispatch", "err", err)
		}
	}
}

// decodeAssignment 解开一次下发。
func decodeAssignment(msg *agentpb.Assignment) ([]InstanceSpec, error) {
	out := make([]InstanceSpec, 0, len(msg.GetSpecs()))
	for i, env := range msg.GetSpecs() {
		s, err := spec.Parse(env.GetSpecJson())
		if err != nil {
			return nil, fmt.Errorf("specs[%d]: %w", i, err)
		}
		in := InstanceSpec{Spec: s}
		if len(env.GetSecrets()) > 0 {
			in.Secrets = make(map[string]string, len(env.GetSecrets()))
			for id, v := range env.GetSecrets() {
				in.Secrets[id] = string(v)
			}
		}
		// 缺密钥要在这里就发现：等到写盘时才报，现场已经是
		// 「配置看起来正常、应用却认证失败」
		for _, ref := range s.SecretRefs {
			if _, ok := in.Secrets[ref.ID]; !ok {
				return nil, fmt.Errorf(
					"specs[%d] (%s/%s) 引用了密钥 %s，但下发里没有它的值",
					i, s.Component, s.Role, ref.ID)
			}
		}
		out = append(out, in)
	}
	return out, nil
}

// Run 是 mechlet 的主循环：注册 → 订阅 → 断了就退避重连。
//
// **不使用 systemd `Requires=` 把 mechlet 绑到 mechd 上**：那会让 mechd 的
// 一次重启把所有 mechlet 拖下水。启动依赖用重试解决
// （01-architecture §4.2）。
func (c *Client) Run(ctx context.Context, h Handler) error {
	backoff := BackoffMin
	for {
		err := c.once(ctx, h)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.log.Warn("connection to mechd dropped, retrying", "err", err, "after", backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(jitter(backoff)):
		}
		backoff = min(backoff*2, BackoffMax)
	}
}

func (c *Client) once(ctx context.Context, h Handler) error {
	if err := c.Register(ctx); err != nil {
		return err
	}
	c.log.Info("registered with mechd", "node", c.opts.Node, "session", c.Session())
	// 重连即全量重推：Subscribe 一挂上就会收到一次全量，
	// 因此这里不需要任何「补差」逻辑
	return c.Subscribe(ctx, h)
}

// jitter 给退避加上 ±25% 的抖动。
//
// 没有抖动时，一次 mechd 重启会让全部节点在同一毫秒重连——1000 个节点
// 同时握手足以把刚起来的 mechd 再打趴一次。
func jitter(d time.Duration) time.Duration {
	spread := float64(d) * 0.25
	return d + time.Duration((rand.Float64()*2-1)*spread)
}

// Report 上报一次观测状态。
func (c *Client) Report(ctx context.Context, r Report) (bool, error) {
	req := &agentpb.ReportRequest{
		NodeName: c.opts.Node, Session: c.Session(),
		Orphans: r.Orphans, OrphanRecords: encodeOrphans(r.OrphanRecords),
	}
	if len(r.Facts) > 0 {
		req.Facts = &agentpb.NodeFacts{FactsJson: r.Facts, CollectedAt: r.FactsAt}
	}
	for _, in := range r.Instances {
		req.Instances = append(req.Instances, encodeStatus(in))
	}
	resp, err := c.agent.Report(ctx, req)
	if err != nil {
		return false, err
	}
	return resp.GetResync(), nil
}

func encodeStatus(in InstanceStatus) *agentpb.InstanceStatus {
	out := &agentpb.InstanceStatus{
		Component: in.Component, Role: in.Role, Digest: in.Digest,
		Generation: int32(in.Generation), Result: in.Result, Message: in.Message,
		WorkloadAction: in.WorkloadAction, WorkloadActionAt: in.WorkloadActionAt,
		RolledBackFrom: in.RolledBackFrom,
		Removed:        in.Removed, RetainedPaths: in.RetainedPaths,
	}
	if w := in.Workload; w != nil {
		out.Workload = &agentpb.WorkloadStatus{
			Runtime: w.Runtime, State: w.State, Pid: w.PID,
			Since: w.Since, Restarts: int32(w.Restarts),
		}
	}
	if h := in.Health; h != nil {
		out.Health = &agentpb.HealthStatus{
			State: h.State, ConsecutiveFailures: int32(h.ConsecutiveFailures),
			LastError: h.LastError, CheckedAt: h.CheckedAt,
		}
	}
	for _, r := range in.Resources {
		out.Resources = append(out.Resources, &agentpb.ResourceStatus{
			Id: r.ID, Type: r.Type, State: r.State, Detail: r.Detail,
		})
	}
	for _, d := range in.Suppressed {
		out.Suppressed = append(out.Suppressed, &agentpb.SuppressedDrift{
			ResourceId: d.ResourceID, Reason: d.Reason, Until: d.Until,
		})
	}
	return out
}

// PushEvents 补报断连期间缓冲的事件。
func (c *Client) PushEvents(ctx context.Context, events []Event) (int, error) {
	req := &agentpb.PushEventsRequest{NodeName: c.opts.Node, Session: c.Session()}
	for _, e := range events {
		req.Events = append(req.Events, &agentpb.Event{
			At: e.At, Level: e.Level, Component: e.Component,
			Role: e.Role, Kind: e.Kind, Message: e.Message,
		})
	}
	resp, err := c.agent.PushEvents(ctx, req)
	if err != nil {
		return 0, err
	}
	return int(resp.GetAccepted()), nil
}

// ── blob ────────────────────────────────────────────────────────────────

// FetchBlob 把一个载荷取到 dir 下的内容寻址位置。
//
// 内容寻址让它**幂等**：已有且校验通过就直接返回。传坏了 sha256 对不上，
// 临时文件被删掉，下次重来——不需要任何额外的状态机。
func (c *Client) FetchBlob(ctx context.Context, sum, dir string) (string, error) {
	// 在本地就校验形状：sum 会被拼进文件路径，一个空串或畸形值要么
	// 让临时文件名越界，要么在 dir 之外落盘。服务端也查一遍，但那太晚了。
	if err := checkSHA256(sum); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, sum)
	if ok, _ := verifyFile(dst, sum); ok {
		return dst, nil
	}

	// 同一 blob 的并发请求在这里合并：N 个实例引用同一个 JDK 时，
	// 只拉一份
	unlock := c.lockBlob(sum)
	defer unlock()
	if ok, _ := verifyFile(dst, sum); ok {
		return dst, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".partial-"+sum[:8]+"-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // rename 成功后这次 Remove 找不到文件，无害
	}()

	stream, err := c.agent.FetchBlob(ctx, &agentpb.FetchBlobRequest{Sha256: sum})
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if _, err := tmp.Write(chunk.GetData()); err != nil {
			return "", err
		}
		h.Write(chunk.GetData())
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	got := hex.EncodeToString(h.Sum(nil))
	if got != sum {
		return "", fmt.Errorf(
			"载荷校验失败: 期望 %s，实际 %s —— 内容已丢弃，下次调和会重取",
			short(sum), short(got))
	}
	// **校验通过后才 rename 进最终位置**：否则一次中断的传输会留下一个
	// 名字正确、内容残缺的文件，而下一次调和会认为它已经在了
	if err := os.Rename(tmpName, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// checkSHA256 校验一个摘要字符串的形状。
func checkSHA256(sum string) error {
	if len(sum) != 2*sha256.Size {
		return fmt.Errorf("sha256 应为 %d 个十六进制字符，实际 %d 个", 2*sha256.Size, len(sum))
	}
	if _, err := hex.DecodeString(sum); err != nil {
		return fmt.Errorf("sha256 %q 不是合法的十六进制", sum)
	}
	return nil
}

func verifyFile(path, want string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
}

// blobLocks 让同一 blob 的并发拉取合并成一次。
var blobLocks sync.Map // sha256 → *sync.Mutex

func (c *Client) lockBlob(sum string) func() {
	v, _ := blobLocks.LoadOrStore(sum, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// RenewCert 用当前证书换一张新的。
//
// **私钥不过网**：CSR 由调用方在本机生成，这里只把它送出去。
func (c *Client) RenewCert(ctx context.Context, csrPEM []byte) (cert, ca []byte, err error) {
	resp, err := c.agent.RenewCert(ctx, &agentpb.RenewCertRequest{
		NodeName: c.opts.Node, CsrPem: string(csrPEM),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("续期证书: %w", err)
	}
	if resp.GetCertPem() == "" || resp.GetCaPem() == "" {
		return nil, nil, fmt.Errorf("续期应答里没有证书")
	}
	return []byte(resp.GetCertPem()), []byte(resp.GetCaPem()), nil
}

// encodeOrphans 把孤儿记录转成线上格式。
func encodeOrphans(in []OrphanRecord) []*agentpb.OrphanRecord {
	if len(in) == 0 {
		return nil
	}
	out := make([]*agentpb.OrphanRecord, 0, len(in))
	for _, r := range in {
		out = append(out, &agentpb.OrphanRecord{
			Key: r.Key, RetainedPaths: r.RetainedPaths,
			Pack: r.Pack, Version: r.Version, RemovedAt: r.RemovedAt,
		})
	}
	return out
}
