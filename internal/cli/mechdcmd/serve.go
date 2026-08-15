// Package mechdcmd 实现 mechd 的子命令。
package mechdcmd

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/mecharion/mecharion/internal/mechd"
	"github.com/mecharion/mecharion/internal/packindex"
	"github.com/mecharion/mecharion/internal/pki"
	"github.com/mecharion/mecharion/internal/protocol"
	"github.com/mecharion/mecharion/internal/protocol/agentpb"
	"github.com/mecharion/mecharion/internal/store"
	"github.com/mecharion/mecharion/internal/vault"
)

// 默认路径。
const (
	// DefaultDataDir 是 mechd 的数据目录。
	DefaultDataDir = "/var/lib/mecharion/mechd"
	// DefaultConfDir 是配置与密钥目录。
	DefaultConfDir = "/etc/mecharion"
	// DefaultSocket 是 mechlet 拨入的 unix socket。
	//
	// 单机形态走 unix socket 且不用 mTLS：文件系统权限已经表达了
	// 「谁能连」，再加一层证书是纯粹的运维负担（01-architecture §2.3）。
	DefaultSocket = "/run/mecharion/mechd.sock"
	// DefaultHTTPListen 是 HTTP API 的监听地址。
	//
	// **默认 0.0.0.0**：边缘现场的常见需求是「拿笔记本连门店那台机看 UI」，
	// 绑回环会让这件事做不了（08-security §3.2）。
	DefaultHTTPListen = "0.0.0.0:8443"
	// DefaultGRPCListen 是**远程** mechlet 拨入的 mTLS 地址（M7）。
	//
	// 默认开着。它要求客户端出示一张本机 CA 签过的证书，握手就把没有
	// 证书的人挡在门外——暴露面是一次 TLS 握手，而换来的是「多节点开箱
	// 即用」。默认关掉的话，每套多节点部署都要先改一次配置，而**那一步
	// 出错时的症状是节点连不上**，比这点暴露面贵得多。
	//
	// 不需要时用 `--grpc ""` 关掉；单机形态本来也没有远程节点会来连。
	DefaultGRPCListen = "0.0.0.0:8444"
)

// ExitConfig 表示配置或环境问题。
const ExitConfig = 3

// NewServeCmd 构造 `mechd serve`。
func NewServeCmd() *cobra.Command {
	var (
		dataDir  string
		confDir  string
		packDir  string
		socket   string
		httpAddr string
		grpcAddr string
		insecure bool
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the control plane",
		Long: `Start mechd: the HTTP API (for people and the UI) and gRPC (for mechlet).

mechd **never performs any deployment action** — that's mechlet's job.
When it's unavailable, every node keeps reconciling against the last known
desired state.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runServe(c, serveOptions{
				DataDir: dataDir, ConfDir: confDir, PackDir: packDir,
				Socket: socket, HTTPAddr: httpAddr, GRPCAddr: grpcAddr,
				Insecure: insecure,
			})
		},
	}

	f := cmd.Flags()
	f.StringVar(&dataDir, "data-dir", DefaultDataDir, "Data directory (SQLite and blobs)")
	f.StringVar(&confDir, "conf-dir", DefaultConfDir, "Config and key directory")
	f.StringVar(&packDir, "pack-dir", "", "Local Pack collection directory, defaults to <data-dir>/packs")
	f.StringVar(&socket, "socket", DefaultSocket, "Unix socket mechlet dials into")
	f.StringVar(&httpAddr, "http", DefaultHTTPListen, "HTTP API listen address")
	f.StringVar(&grpcAddr, "grpc", DefaultGRPCListen,
		"mTLS listen address for remote mechlet; empty string disables it")
	f.BoolVar(&insecure, "insecure-http", false,
		"Serve plain HTTP (local debugging only; unacceptable for external listeners)")
	return cmd
}

type serveOptions struct {
	DataDir, ConfDir, PackDir string
	Socket, HTTPAddr          string
	GRPCAddr                  string
	Insecure                  bool
}

func runServe(c *cobra.Command, o serveOptions) error {
	ctx, stop := signal.NotifyContext(c.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	out := c.OutOrStdout()
	if o.PackDir == "" {
		o.PackDir = filepath.Join(o.DataDir, "packs")
	}

	// ── 存储 ──
	st, err := store.Open(ctx, store.Options{
		Path: filepath.Join(o.DataDir, "mechd.db"),
	})
	if err != nil {
		return exit(ExitConfig, err)
	}
	defer st.Close()

	v, err := vault.Open(ctx, st, vault.Options{
		KeyPath: filepath.Join(o.ConfDir, "secret.key"),
	})
	if err != nil {
		// 主密钥缺失但库里有密文时**拒绝启动并说清原因**，
		// 而不是静默把口令当空值（16-secrets §3）
		return exit(ExitConfig, err)
	}

	idx := packindex.New()
	if err := idx.AddDir(o.PackDir); err != nil && !os.IsNotExist(err) {
		return exit(ExitConfig, fmt.Errorf("loading Pack collection %s: %w", o.PackDir, err))
	}

	// ── 服务 ──
	//
	// 两者互相引用：Backend 要调 Service，Service 要用 Server 唤醒节点。
	// 分两步接线而不是加一个 setter：这个环是**真实的**（下发与被唤醒是
	// 同一件事的两端），把它显式写出来比藏进一个方法里更清楚。
	svc := &mechd.Service{
		Store: st, Repos: st.Repos(), Vault: v, Packs: idx,
		BlobDir: filepath.Join(o.DataDir, "blobs"),
		Log:     slog.Default(),
	}
	srv := protocol.NewServer(protocol.ServerOptions{
		Backend: &mechd.Backend{S: svc, ConfDir: o.ConfDir}, Log: slog.Default(),
	})
	svc.Notify = srv
	// 节点在不在线由这条长连接答，不由库里的字段答（22-multi-node §6.13）
	svc.Presence = srv
	// ad-hoc 命令通道（ADR-0038）：restart / 后续的 exec 走它，
	// 与期望状态那条流分开
	svc.Tasks = srv
	// 让正在看的浏览器知道「有东西变了」（23-web-ui §4.5.2）
	svc.Watch = mechd.NewHub()

	token, err := ensureToken(o.ConfDir, out)
	if err != nil {
		return exit(ExitConfig, err)
	}
	api := &mechd.API{
		S: svc, Auth: mechd.NewTokenAuth(token),
		ConfDir: o.ConfDir, PackDir: o.PackDir,
		// 首次初始化门禁复用同一个 token（ADR-0039）：知道它，就证明是
		// 刚装完这台机器、看得到上面这段输出或读得到 admin.token 文件的人。
		BootstrapTokenHash: mechd.HashToken(token),
		// HSTS 只在 HTTPS 模式下打开：--insecure-http 时打开
		// 没有意义，浏览器规范本就无视明文连接上收到的这个头。
		EnableHSTS: !o.Insecure,
	}

	// ── 监听 ──
	grpcSrv := grpc.NewServer(protocol.ServerKeepalive()...)
	agentpb.RegisterAgentServer(grpcSrv, srv)

	lis, err := listenUnix(o.Socket)
	if err != nil {
		return exit(ExitConfig, err)
	}
	defer lis.Close()

	// **刻意不设 WriteTimeout。**
	//
	// 它是对整个响应计时的，而 SSE（组件详情页的实时流，23-web-ui §4.5）
	// 的响应本来就没有尽头。设上之后症状是「页面开着开着就不更新了」，
	// 而且恰好在一个整数秒之后——那种现场极难联想到这里。
	//
	// 慢客户端的防护由 ReadHeaderTimeout（请求头）与 SSE 自己的心跳
	// 承担：写不进去时 Fprintf 会报错，那条流随即退出。
	httpSrv := &http.Server{
		Addr:              o.HTTPAddr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 远程节点的 mTLS 监听。
	//
	// **两个 grpc.Server 而不是一个**：传输安全是整个 Server 的属性，
	// 一个实例带不了两套。两者注册的是**同一个** srv——协议实现只有一份，
	// 差别只在「对端是谁」怎么得出（见 protocol.Server.nodeOf）。
	var grpcTLS *grpc.Server
	var tlsLis net.Listener
	if o.GRPCAddr != "" {
		cert, err := pki.EnsureServer(pki.Dir(o.ConfDir), o.HTTPAddr)
		if err != nil {
			return exit(ExitConfig, err)
		}
		creds, err := nodeTransportCreds(cert)
		if err != nil {
			return exit(ExitConfig, err)
		}
		tlsLis, err = net.Listen("tcp", o.GRPCAddr)
		if err != nil {
			return exit(ExitConfig, fmt.Errorf("listening on %s: %w", o.GRPCAddr, err))
		}
		defer tlsLis.Close()
		grpcTLS = grpc.NewServer(append(protocol.ServerKeepalive(), grpc.Creds(creds))...)
		agentpb.RegisterAgentServer(grpcTLS, srv)
	}

	errc := make(chan error, 3)
	go func() { errc <- grpcSrv.Serve(lis) }()
	if grpcTLS != nil {
		go func() { errc <- grpcTLS.Serve(tlsLis) }()
	}
	go func() {
		if o.Insecure {
			slog.Warn("HTTP API is running in plaintext",
				"addr", o.HTTPAddr,
				"hint", "local debugging only; unacceptable for external listeners")
			errc <- httpSrv.ListenAndServe()
			return
		}
		cert, err := pki.EnsureServer(pki.Dir(o.ConfDir), o.HTTPAddr)
		if err != nil {
			errc <- err
			return
		}
		errc <- httpSrv.ListenAndServeTLS(cert.CertFile, cert.KeyFile)
	}()

	fmt.Fprintf(out, "mechd is up\n  gRPC  %s\n", o.Socket)
	if o.GRPCAddr != "" {
		fmt.Fprintf(out, "  gRPC  %s (mTLS, for remote nodes)\n", o.GRPCAddr)
	}
	fmt.Fprintf(out, "  HTTP  %s\n  Pack  %s\n", o.HTTPAddr, o.PackDir)

	// **未初始化时每次启动都喊一声。**
	//
	// 在有人完成初始化之前，任何能访问这个地址的人都能成为管理员
	// （ADR-0037 记在案的代价）。装完到访问之间可能隔几小时甚至几天，
	// 而那段时间里这条警告是唯一提醒「窗口还开着」的东西。
	if done, err := svc.Initialized(ctx); err == nil && !done {
		slog.Warn("Web UI has not been initialized yet",
			"impact", "until someone sets a password, anyone who can reach this address can become the admin",
			"action", "open "+o.HTTPAddr+" in a browser to complete initialization soon")
	}

	select {
	case <-ctx.Done():
		fmt.Fprintln(out, "Received stop signal, shutting down…")
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}

	// 优雅关闭：mechd 退场不该影响已部署组件，但正在进行的 API 调用
	// 应当有机会收尾。
	//
	// **先 Drain 再 GracefulStop**，顺序要紧：Subscribe 是长连的服务端流，
	// 正常情况下永远不返回，而 GracefulStop 会等所有活跃 RPC 结束。少了
	// Drain，mechd 会一直挂到 systemd 的 TimeoutStopSec 再吃一发 SIGKILL
	// ——每次重启白等半分钟，而且这一行下面的活儿（关库、刷 WAL）根本
	// 走不到。
	srv.Drain()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	stopGRPC(grpcSrv, grpcTLS)
	return nil
}

// grpcStopGrace 是「等优雅关闭」的上限。
//
// 3 秒足够让一次进行中的一元调用收尾。Drain 之后订阅流会立刻退出，
// 正常情况下根本用不到这个超时。
const grpcStopGrace = 3 * time.Second

// stopGRPC 关掉 gRPC 监听，但**绝不无限期等下去**。
//
// Drain 才是让长连流收摊的机制；这里是**兜底**：关闭路径永远不该挂住。
// 今天卡住的是 Subscribe，明天可能是某个写歪了的 handler，而症状是一样的
// ——一次 SIGKILL，外加一个没干净关闭的数据库。
//
// 兜底用 Stop 打断阻塞中的 GracefulStop。grpc-go 没有把这个行为写进文档，
// 因此它只在**已经出问题、马上就要被 SIGKILL** 的情况下才跑；那时这个
// 赌注是划算的。
func stopGRPC(servers ...*grpc.Server) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for _, s := range servers {
			if s == nil {
				continue
			}
			wg.Add(1)
			go func(s *grpc.Server) { defer wg.Done(); s.GracefulStop() }(s)
		}
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(grpcStopGrace):
		slog.Warn("graceful shutdown timed out, forcing gRPC connections closed",
			"waited", grpcStopGrace.String())
		for _, s := range servers {
			if s != nil {
				s.Stop()
			}
		}
		<-done // Stop 会让上面那些 GracefulStop 返回，等它们收干净
	}
}

// nodeTransportCreds 是远程节点那条通道的传输凭证。
//
// **RequireAndVerifyClientCert**：没有证书、或者证书不是本机 CA 签的，
// 在握手阶段就被挡住，一个 RPC 都到不了业务层。这条通道的认证全部在这里
// ——业务层只负责回答「这张证书说自己是谁」（ADR-0034）。
func nodeTransportCreds(cert pki.CertPaths) (credentials.TransportCredentials, error) {
	pair, err := tls.LoadX509KeyPair(cert.CertFile, cert.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading server certificate: %w", err)
	}
	pool, err := pki.CAPool(cert.CAFile)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{pair},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}), nil
}

// listenUnix 建 unix socket，权限 0600。
//
// **文件系统权限就是这条通道的认证**：只有 root 能连，而 mechlet 本就
// 以 root 运行。加 mTLS 等于给一个已经关好的门再挂一把锁，代价是证书
// 的生成、分发与轮换（01-architecture §2.3）。
func listenUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// 上一次进程留下的 socket 文件会让 Listen 失败。它不是数据，
	// 删掉是安全的——真有另一个 mechd 在跑，下面的 Listen 仍会失败。
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		lis.Close()
		return nil, err
	}
	return lis, nil
}

// ensureToken 读出（或首次生成）初始 admin token。
//
// `0.0.0.0` 监听意味着**认证不能是可选的**。首次启动时生成并
// **只打印这一次**（08-security §3.3）。
func ensureToken(confDir string, out interface{ Write([]byte) (int, error) }) (string, error) {
	path := filepath.Join(confDir, "admin.token")
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := mechd.TokenPrefix + hex.EncodeToString(buf)

	if err := os.MkdirAll(confDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}

	fmt.Fprintf(out, `
mechd first-time startup complete.

  Initial admin token: %s

  This token is shown only once. It's also stored at %s (0600), but save it
  somewhere else right away.

  mechctl context set local --server https://<host>:8443 --token %s

  When you open the Web UI in a browser to complete initialization, this
  token is also the "initialization token" — they're the same value.

`, token, path, token)
	return token, nil
}

func exit(code int, err error) error { return &exitError{code: code, err: err} }

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// ExitCodeOf 把错误映射到退出码。
func ExitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return 1
}
