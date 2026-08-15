package mechletcmd

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/mecharion/mecharion/internal/agent"
	"github.com/mecharion/mecharion/internal/pki"
	"github.com/mecharion/mecharion/internal/protocol"
	"github.com/mecharion/mecharion/internal/reconcile"
	rt "github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/runtime/docker"
	"github.com/mecharion/mecharion/internal/runtime/systemd"
	"github.com/mecharion/mecharion/internal/state"
	"github.com/mecharion/mecharion/internal/vault"
	"github.com/mecharion/mecharion/internal/version"
)

// DefaultUpstream 是单机形态下 mechd 的地址。
const DefaultUpstream = "unix:///run/mecharion/mechd.sock"

// NewAgentCmd 构造 `mechlet agent`。
func NewAgentCmd() *cobra.Command {
	var (
		dataDir  string
		upstream string
		node     string
		confDir  string
		interval time.Duration
		// renewBefore 只为测试而存在：证书有效期一年、阈值 30 天，
		// 等一年才能验一次轮换是不现实的。生产上不需要动它。
		renewBefore time.Duration
	)

	var localSocket string
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Connect to mechd and continuously reconcile local state",
		Long: `Run as a long-lived process: dial mechd, receive the desired state, and turn
it into the machine's actual state.

**mechlet dials out**, the machine opens no inbound port (network-wise —
the local unix socket does still expose a read-only diagnostic endpoint,
see --local-socket).

When mechd is unreachable, it keeps self-healing against the last known
desired state, reconnecting with exponential backoff — **it doesn't use
systemd's Requires= to tie itself to mechd**, which would drag every
mechlet down with it on a single mechd restart.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runAgent(c, dataDir, confDir, upstream, node,
				interval, renewBefore, localSocket)
		},
	}

	f := cmd.Flags()
	f.StringVar(&dataDir, "data-dir", DefaultDataDir, "Data directory")
	f.StringVar(&upstream, "upstream", DefaultUpstream, "mechd address")
	f.StringVar(&confDir, "conf-dir", "/etc/mecharion", "Config and certificate directory")
	f.StringVar(&node, "node", "", "This node's name, defaults to the hostname")
	// 缺省由 mechd 下发。本机覆盖是给两种情况留的：资源紧张的节点想调稀，
	// 以及测试要把等待压到秒级。
	f.DurationVar(&interval, "reconcile-interval", 0, "Override the reconcile interval mechd dispatches")
	f.DurationVar(&renewBefore, "cert-renew-before", 0,
		"How long before expiry to renew the certificate, defaults to 30 days")
	f.StringVar(&localSocket, "local-socket", DefaultLocalSocket,
		"Unix socket path for mechctl --local's read-only diagnostic endpoint")
	return cmd
}

func runAgent(
	c *cobra.Command, dataDir, confDir, upstream, node string,
	interval, renewBefore time.Duration, localSocket string,
) error {
	ctx, stop := signal.NotifyContext(c.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if node == "" {
		h, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("getting hostname: %w", err)
		}
		node = h
	}

	st, err := state.New(filepath.Join(dataDir, "mechlet"))
	if err != nil {
		return err
	}

	caps, err := probeCapabilities(ctx)
	if err != nil {
		return err
	}

	dialOpts, err := upstreamCreds(upstream, confDir)
	if err != nil {
		return err
	}
	cli, err := protocol.Dial(protocol.ClientOptions{
		Target: upstream, Node: node,
		AgentVersion: version.Version,
		Capabilities: caps,
		Log:          slog.Default(),
		DialOptions:  dialOpts,
	})
	if err != nil {
		return err
	}
	defer cli.Close()

	r := &reconcile.Reconciler{
		Store:    st,
		Runtimes: rt.NewRegistry(systemd.New(), docker.New(), docker.NewCompose()),
		BlobDir:  filepath.Join(dataDir, "blobs"),
		PackDir:  filepath.Join(dataDir, "packs"),
		Log:      slog.Default(),
	}

	// 本机的期望状态副本 + 密钥保管库。没有它就没有周期调和，
	// 而周期调和是常驻 Agent 相对 Ansible 的核心价值（ADR-0033）。
	fv, err := vault.OpenFile(filepath.Join(dataDir, "vault"), vault.FileOptions{})
	if err != nil {
		return err
	}
	desired, err := agent.NewDesiredStore(filepath.Join(dataDir, "desired"), fv)
	if err != nil {
		return err
	}

	a := agent.New(agent.Options{
		Client: cli, Reconciler: r, State: st, Node: node,
		BlobDir: filepath.Join(dataDir, "blobs"),
		Log:     slog.Default(),
		Facts:   collectFacts,
		Desired: desired,

		ReconcileInterval: interval,
		Certs:             agentCerts(upstream, confDir),
		RenewBefore:       renewBefore,
	})

	if localSocket != "" {
		lis, err := listenLocalUnix(localSocket)
		if err != nil {
			return fmt.Errorf("starting --local read-only diagnostic endpoint: %w", err)
		}
		srv := &http.Server{Handler: localStatusHandler(a, node)}
		go func() {
			if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("--local read-only diagnostic endpoint exited unexpectedly", "err", err)
			}
		}()
		go func() {
			<-ctx.Done()
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutCtx)
		}()
	}

	slog.Info("mechlet agent started", "node", node, "upstream", upstream)
	err = a.Run(ctx)
	if ctx.Err() != nil {
		return nil // 收到停止信号，正常退出
	}
	return err
}

// probeCapabilities 探测本机的运行时能力。
//
// capabilities 是事实的一部分——一套采集机制、一个命名空间
// （spec §9.4.2）。`requires.capability` 是针对它的匹配器。
func probeCapabilities(ctx context.Context) ([]protocol.Capability, error) {
	probe, err := systemd.New().Probe(ctx)
	if err != nil {
		// 探测失败不该拦住注册：一个探不出 systemd 的节点仍然应当
		// 在册并可被诊断，只是放置时会被 requires.capability 拒绝
		slog.Warn("failed to probe systemd capability", "err", err)
	}
	c := protocol.Capability{
		Name: "systemd", Available: probe.Available,
		Version: probe.Version, Detail: map[string]string{},
	}
	if probe.Reason != "" {
		c.Detail["reason"] = probe.Reason
	}
	return []protocol.Capability{c}, nil
}

// collectFacts 采集本机事实。
//
// **只报最基本的几项**（架构、系统、CPU 核数）——`defaultFrom` 主要用
// 内存与 CPU。文件系统、网卡、facts.d 自定义事实目前都还没采集，没有
// 排期，见 [ADR-0023](../../../docs/adr/0023-node-facts.md)。
func collectFacts() ([]byte, error) {
	facts := map[string]any{
		"arch": runtime.GOARCH,
		"os":   map[string]any{"family": osFamily()},
		"cpu":  map[string]any{"cores": runtime.NumCPU()},
	}
	if h, err := os.Hostname(); err == nil {
		facts["hostname"] = h
	}
	if total := memTotal(); total > 0 {
		facts["memory"] = map[string]any{"total": total}
	}
	return json.Marshal(facts)
}

// upstreamCreds 决定这条上行连接怎么建。
//
// **传输形态由地址决定，不另设开关**：一个 `--mtls` 之类的布尔量迟早会与
// 地址对不上（unix socket 配 mTLS、TCP 忘了开），而那种错配的症状是
// 一句连不上，看不出是配置问题。
//
//	unix://…   明文。socket 权限已经是内核强制的对端身份（ADR-0026）
//	其它        mTLS。证书在 <conf-dir>/pki 下，由 join 或 `mechd ca issue` 放好
func upstreamCreds(upstream, confDir string) ([]grpc.DialOption, error) {
	if strings.HasPrefix(upstream, "unix:") || strings.HasPrefix(upstream, "/") {
		return nil, nil // Dial 的缺省就是明文
	}

	dir := pki.Dir(confDir)
	certFile := filepath.Join(dir, "node.crt")
	keyFile := filepath.Join(dir, "node.key")
	caFile := filepath.Join(dir, "ca.crt")

	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		return nil, fmt.Errorf(
			"connecting to a remote mechd requires a local certificate, but couldn't read %s:\n  %w\n"+
				"  join the cluster with mechctl node bootstrap or mechlet install --join",
			certFile, err)
	}
	pool, err := pki.CAPool(caFile)
	if err != nil {
		return nil, err
	}
	return []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    pool,
			// **每次握手都从盘上读当前证书**，而不是把它固化进配置。
			//
			// 证书会被续期换掉（agent.renewCerts）。固化的话，换完必须
			// 重启 mechlet 才生效——而重启一个正在维持一堆组件的 agent
			// 只为了换张证书，是个没必要的风险。
			//
			// 旧连接继续用旧证书跑到它自己断开为止，那没问题：能触发
			// 续期就说明旧证书至少还有 30 天。
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				pair, err := tls.LoadX509KeyPair(certFile, keyFile)
				if err != nil {
					return nil, fmt.Errorf("reading local certificate %s: %w", certFile, err)
				}
				return &pair, nil
			},
			// ServerName 留空：走地址里的主机名，而 mechd 的服务端证书
			// SAN 里正是那个名字。写死会让「换个地址连同一台机」失败。
		})),
	}, nil
}

// agentCerts 返回本机证书的位置；单机形态下为空。
//
// 判据与 upstreamCreds 用**同一个**：地址决定形态。两处各判一次迟早会
// 分叉，而分叉的症状是「明明配了证书却不续期」。
func agentCerts(upstream, confDir string) agent.CertPaths {
	if strings.HasPrefix(upstream, "unix:") || strings.HasPrefix(upstream, "/") {
		return agent.CertPaths{}
	}
	return agent.NodeCertPaths(confDir)
}
