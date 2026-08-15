package mechletcmd

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mecharion/mecharion/internal/ident"
	"github.com/mecharion/mecharion/internal/mechd"
	"github.com/mecharion/mecharion/internal/store"
	"github.com/mecharion/mecharion/internal/vault"
	"github.com/mecharion/mecharion/internal/version"
)

// 安装布局（04-paths-and-storage）。
const (
	// DefaultPrefix 是 Mecharion 自身的安装根。
	//
	// **不放 /opt/mecharion**：那里只放被管理的组件，两者混在一起会让
	// 「哪些是我装的、哪些是它自己的」变成一个需要文档解释的问题。
	DefaultPrefix = "/usr/local/lib/mecharion"
	// BinLinkDir 是命令软链的落点。
	//
	// **/usr/bin 而不是 /usr/local/bin**：RHEL 的 sudo secure_path
	// 不含后者，于是 `sudo mechctl` 会 command not found，而直接 root
	// 下又是好的——这类「有时能用有时不能」最消耗人。
	BinLinkDir = "/usr/bin"
	// UnitDir 是 systemd unit 的落点。
	UnitDir = "/etc/systemd/system"
)

// binaries 是要安装的四个命令。
var binaries = []string{"mechd", "mechlet", "mechctl", "mechpack"}

// NewInstallCmd 构造 `mechlet install`。
func NewInstallCmd() *cobra.Command {
	var (
		standalone bool
		prefix     string
		dataDir    string
		confDir    string
		node       string
		httpAddr   string
		linkDir    string
		force      bool
		join       string
		token      string
		caHash     string
	)

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install Mecharion",
		Long: `Install Mecharion itself onto this machine.

--standalone sets up a fully functional single machine: mechd and mechlet
run on the same machine over a unix socket, with no network configuration
needed.

Standalone and multi-node go through **the exact same** path, differing
only in the transport layer and in placement being trivial — no part of
the logic is standalone-only.`,
		Example: `  mechlet install --standalone
  mechlet install --join https://mechd-1:8443 --token m7n_join_… --ca-hash sha256:…`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			switch {
			case standalone && join != "":
				return fmt.Errorf(
					"--standalone and --join are mutually exclusive: the former is itself a control plane, the latter joins someone else's")
			case join != "":
				return runInstall(c, installOptions{
					Prefix: prefix, DataDir: dataDir, ConfDir: confDir,
					LinkDir: linkDir, Node: node, Force: force,
					Join: joinOptions{
						Server: join, Token: token, CAHash: caHash,
						Node: node, ConfDir: confDir,
					},
				})
			case standalone:
				return runInstall(c, installOptions{
					Prefix: prefix, DataDir: dataDir, ConfDir: confDir,
					LinkDir: linkDir,
					Node:    node, HTTPAddr: httpAddr, Force: force,
				})
			}
			return fmt.Errorf("either --standalone (set up a single machine), " +
				"or --join <mechd address> (join an existing cluster)")
		},
	}

	f := cmd.Flags()
	f.BoolVar(&standalone, "standalone", false, "Set up a self-sufficient single machine")
	f.StringVar(&prefix, "prefix", DefaultPrefix, "Install root for Mecharion itself")
	f.StringVar(&dataDir, "data-dir", DefaultDataDir, "Data directory")
	f.StringVar(&confDir, "conf-dir", "/etc/mecharion", "Config and key directory")
	f.StringVar(&node, "node", "", "This node's name, defaults to the hostname")
	f.StringVar(&httpAddr, "http", "0.0.0.0:8443", "mechd's HTTP API listen address")
	f.StringVar(&linkDir, "link-dir", BinLinkDir,
		"Where command symlinks are placed (usually only relevant for packaging and chroots)")
	f.BoolVar(&force, "force", false, "Overwrite an existing installation")
	f.StringVar(&join, "join", "", "Join an existing cluster: mechd's HTTPS address")
	f.StringVar(&token, "token", "", "join token (mechctl node token create)")
	f.StringVar(&caHash, "ca-hash", "",
		"mechd's CA public-key fingerprint, delivered out-of-band together with the token")
	return cmd
}

type installOptions struct {
	Prefix, DataDir, ConfDir string
	LinkDir                  string
	Node, HTTPAddr           string
	Force                    bool
	// Join 非零表示这是一次「加入已有集群」，而不是装一台单机。
	Join joinOptions
}

// resolveNodeName 决定这次安装用哪个节点名。
//
// explicit 非空时**原样使用，不做任何改写**——用户明确写下的名字不该被
// 静默篡改。为空时退回主机名，但只转小写（对齐 kubelet 对节点名的
// 处理方式）：真实主机名常见大写或 FQDN 形式,而运行期标识符统一用
// RFC 1123 规则（09-naming-conventions.md §7）。
//
// **转完还不合法就报错，不再进一步改写**——例如主机名带下划线。
// 静默把 "_" 换成 "-" 看起来贴心，实际是在给这台机器起一个运维没有
// 在任何地方读到过的名字，下次对账对不上时无从查起（原则六：显式优于
// 隐式）。
func resolveNodeName(explicit string, hostname func() (string, error)) (string, error) {
	node := explicit
	if node == "" {
		h, err := hostname()
		if err != nil {
			return "", err
		}
		node = strings.ToLower(h)
	}
	if err := ident.Validate(ident.Node, node); err != nil {
		return "", fmt.Errorf("%w\n  specify a valid node name explicitly with --node", err)
	}
	return node, nil
}

func runInstall(c *cobra.Command, o installOptions) error {
	ctx := c.Context()
	out := c.OutOrStdout()

	if os.Geteuid() != 0 {
		return exitWith(ExitValidation, fmt.Errorf(
			"install requires root: it writes to /usr/bin, %s, and systemd units\n"+
				"  Mecharion **does not create a dedicated user** — managing systemd, sysctl, "+
				"and mount points inherently requires root, there's no way to drop privileges", UnitDir))
	}
	node, err := resolveNodeName(o.Node, os.Hostname)
	if err != nil {
		return exitWith(ExitValidation, err)
	}
	o.Node = node

	// 受管节点**不装控制面**：没有数据库、没有主密钥、没有 mechd 的 unit。
	// 它需要的是一张证书和一个 agent。
	//
	// 两条路共用前两步，是因为「二进制怎么装」与形态无关——那正是
	// generation + 原子切软链要保证的性质。
	steps := []struct {
		name string
		fn   func() error
	}{
		{"Install binaries and link them", func() error { return installBinaries(out, o) }},
	}
	if o.Join.Server != "" {
		o.Join.Node = o.Node
		steps = append(steps,
			struct {
				name string
				fn   func() error
			}{"Join the cluster", func() error { return runJoin(out, o.Join) }},
			struct {
				name string
				fn   func() error
			}{"Write systemd unit", func() error { return writeAgentUnit(out, o) }},
		)
	} else {
		steps = append(steps,
			struct {
				name string
				fn   func() error
			}{"Generate config", func() error { return writeConfigs(out, o) }},
			struct {
				name string
				fn   func() error
			}{"Initialize database and master key", func() error { return initStore(ctx, out, o) }},
			struct {
				name string
				fn   func() error
			}{"Register this node", func() error { return registerNode(ctx, out, o) }},
			struct {
				name string
				fn   func() error
			}{"Write systemd unit", func() error { return writeUnits(out, o) }},
		)
	}
	for i, s := range steps {
		fmt.Fprintf(out, "[%d/%d] %s\n", i+1, len(steps), s.name)
		if err := s.fn(); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}

	if o.Join.Server != "" {
		fmt.Fprintf(out, `
Install complete. This machine is a **managed node** — no control plane,
just the agent.

  systemctl enable --now mecharion-mechlet

It will dial out to %s; this machine opens no inbound port.
`, o.Join.Server)
		return nil
	}

	fmt.Fprintf(out, `
Install complete.

  systemctl enable --now mecharion-mechd mecharion-mechlet
  mechctl component list

The initial admin token is generated the first time mechd starts and
**printed only once** — see journalctl -u mecharion-mechd.

The master key is at %s — **back it up separately, don't put it in the
same backup as the database**.
`, filepath.Join(o.ConfDir, "secret.key"))
	return nil
}

// ── ① 二进制 ────────────────────────────────────────────────────────────

// installBinaries 把四个命令装进版本目录并挂软链。
//
// **实体文件在版本目录里，PATH 上只有软链**：mechlet 要能升级正在运行的
// 自己，靠的是 generation + 原子切软链。把实体二进制直接放进 /usr/bin
// 会让这套机制整个没了——覆盖一个正在执行的文件既不原子也无法回滚。
func installBinaries(out io.Writer, o installOptions) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	srcDir := filepath.Dir(self)

	genDir := filepath.Join(o.Prefix, "generations",
		fmt.Sprintf("0001-%s", version.Version))
	binDir := filepath.Join(genDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}

	for _, name := range binaries {
		src := filepath.Join(srcDir, name)
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf(
				"couldn't find %s (should be in the same directory as mechlet, %s)\n"+
					"  → the four binaries are released and installed together: a half-installed "+
					"Mecharion would suddenly be missing some command",
				name, srcDir)
		}
		if err := copyExec(src, filepath.Join(binDir, name)); err != nil {
			return err
		}
	}

	// current 软链：运行期一律引用它
	current := filepath.Join(o.Prefix, "current")
	if err := replaceSymlink(current, genDir); err != nil {
		return err
	}

	linkDir := o.LinkDir
	if linkDir == "" {
		linkDir = BinLinkDir
	}
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		return err
	}
	for _, name := range binaries {
		link := filepath.Join(linkDir, name)
		target := filepath.Join(current, "bin", name)
		if err := replaceSymlink(link, target); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "      %s -> %s\n", BinLinkDir, current)
	return nil
}

func copyExec(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	// 先写临时文件再 rename：直接覆盖一个可能正在执行的文件会得到
	// ETXTBSY，而半个文件比没有文件更糟
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, in); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// replaceSymlink 原子地把一个软链指向新目标。
func replaceSymlink(link, target string) error {
	tmp := link + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	// rename 覆盖软链是原子的；先删再建会留下一个窗口，
	// 那期间 `mechctl` 是 command not found
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ── ② 配置 ──────────────────────────────────────────────────────────────

func writeConfigs(out io.Writer, o installOptions) error {
	if err := os.MkdirAll(o.ConfDir, 0o700); err != nil {
		return err
	}

	mechdYAML := fmt.Sprintf(`# mechd config. Generated by mechlet install --standalone.
http:
  listen: %s
  tls:
    mode: self-signed      # self-signed | provided
grpc:
  socket: /run/mecharion/mechd.sock
dataDir: %s
packDir: %s
`, o.HTTPAddr, filepath.Join(o.DataDir, "mechd"), filepath.Join(o.DataDir, "packs"))

	mechletYAML := fmt.Sprintf(`# mechlet config. Generated by mechlet install --standalone.
node: %s
upstream: %s
dataDir: %s

# Path roots. Changing these **does not** move already-installed
# components — paths are fixed at first materialization, and every
# reconcile after that reads the fixed value (spec §8.7).
roots:
  opt:  /opt/mecharion
  etc:  /etc/mecharion
  data: /var/lib/mecharion
  logs: /var/log/mecharion
  run:  /run/mecharion

# Multiple disks are declared here, then bound by volume name via ConfigGroup (spec §8.6).
volumes: []
`, o.Node, DefaultUpstream, o.DataDir)

	for _, f := range []struct {
		name, body string
	}{
		{"mechd.yaml", mechdYAML},
		{"mechlet.yaml", mechletYAML},
	} {
		p := filepath.Join(o.ConfDir, f.name)
		if _, err := os.Stat(p); err == nil && !o.Force {
			fmt.Fprintf(out, "      %s already exists, keeping it (--force to overwrite)\n", p)
			continue
		}
		if err := os.WriteFile(p, []byte(f.body), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// ── ③ 存储与主密钥 ──────────────────────────────────────────────────────

func initStore(ctx context.Context, out io.Writer, o installOptions) error {
	for _, d := range []string{
		filepath.Join(o.DataDir, "mechd"),
		filepath.Join(o.DataDir, "mechlet"),
		filepath.Join(o.DataDir, "blobs"),
		filepath.Join(o.DataDir, "packs"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	st, err := store.Open(ctx, store.Options{
		Path: filepath.Join(o.DataDir, "mechd", "mechd.db"),
	})
	if err != nil {
		return err
	}
	defer st.Close()

	// 主密钥：首次生成。丢了它，运维手填的口令**真的没了**——
	// 引擎 generate 的可以重新生成（16-secrets §3）。
	if _, err := vault.Open(ctx, st, vault.Options{
		KeyPath: filepath.Join(o.ConfDir, "secret.key"),
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "      %s\n", filepath.Join(o.DataDir, "mechd", "mechd.db"))
	return nil
}

// ── ④ 注册节点 ──────────────────────────────────────────────────────────

// registerNode 建出默认站点并把本机登记为节点。
//
// **mechd 不凭空创建节点**（Backend.Register 会拒绝未在册的机器），
// 因此单机安装必须在这里先把自己登记上——否则 agent 一拨上来就被拒。
func registerNode(ctx context.Context, out io.Writer, o installOptions) error {
	st, err := store.Open(ctx, store.Options{
		Path: filepath.Join(o.DataDir, "mechd", "mechd.db"),
	})
	if err != nil {
		return err
	}
	defer st.Close()
	repos := st.Repos()

	site, err := repos.Sites().GetByName(ctx, mechd.DefaultSite)
	if err != nil {
		site, err = repos.Sites().Create(ctx, store.Site{
			Name: mechd.DefaultSite, Kind: "standalone",
		})
		if err != nil {
			return err
		}
	}

	if _, err := repos.Nodes().Upsert(ctx, store.Node{
		SiteID: site.ID, Name: o.Node, Address: "127.0.0.1",
		Roots: map[string]string{
			"opt": "/opt/mecharion", "etc": "/etc/mecharion",
			"data": "/var/lib/mecharion", "logs": "/var/log/mecharion",
			"run": "/run/mecharion",
		},
		Status: "unknown",
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "      site %s, node %s\n", site.Name, o.Node)
	return nil
}

// ── ⑤ systemd unit ──────────────────────────────────────────────────────

// writeUnits 写两个 unit。
//
// **不用 `Requires=mecharion-mechd`**：那会让 mechd 的一次重启把 mechlet
// 一起拖下水，而 mechlet 本就该在 mechd 不可达时继续自治。启动依赖用
// 重试解决（01-architecture §4.2）。
func writeUnits(out io.Writer, o installOptions) error {
	mechdUnit := fmt.Sprintf(`[Unit]
Description=Mecharion control plane (mechd)
Documentation=https://github.com/mecharion/mecharion
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s/mechd serve --data-dir %s --conf-dir %s
Restart=always
RestartSec=5s
# mechd isn't on the data plane: already-deployed components keep running when it exits
KillMode=mixed
TimeoutStopSec=30s

[Install]
WantedBy=multi-user.target
`, BinLinkDir, filepath.Join(o.DataDir, "mechd"), o.ConfDir)

	mechletUnit := fmt.Sprintf(`[Unit]
Description=Mecharion node agent (mechlet)
Documentation=https://github.com/mecharion/mecharion
After=network-online.target
Wants=network-online.target
# Deliberately **not** Requires=mecharion-mechd.service:
# that would drag mechlet down with every mechd restart, when mechlet is
# supposed to keep self-healing against the last known desired state
# while mechd is unreachable. Start ordering is expressed with After;
# if it can't connect, mechlet reconnects on its own with exponential backoff.
After=mecharion-mechd.service

[Service]
Type=simple
ExecStart=%s/mechlet agent --data-dir %s --node %s
Restart=always
RestartSec=5s
KillMode=mixed
TimeoutStopSec=60s

[Install]
WantedBy=multi-user.target
`, BinLinkDir, o.DataDir, o.Node)

	for _, u := range []struct{ name, body string }{
		{"mecharion-mechd.service", mechdUnit},
		{"mecharion-mechlet.service", mechletUnit},
	} {
		p := filepath.Join(UnitDir, u.name)
		if err := os.WriteFile(p, []byte(u.body), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "      %s\n", p)
	}

	if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
		// 不算失败：容器里可能没有跑着的 systemd，而 unit 已经写好了
		fmt.Fprintf(out, "      (systemctl daemon-reload didn't succeed, run it manually)\n")
	}
	return nil
}

// ── 卸载提示 ────────────────────────────────────────────────────────────

// UninstallHint 是 remove 命令实现前给用户的说明。
func UninstallHint(prefix string) string {
	return strings.Join([]string{
		"Manual uninstall:",
		"  systemctl disable --now mecharion-mechlet mecharion-mechd",
		"  rm -f " + UnitDir + "/mecharion-*.service",
		"  rm -f " + BinLinkDir + "/{mechd,mechlet,mechctl,mechpack}",
		"  rm -rf " + prefix,
		"  # data and config are deliberately not deleted — that's your desired state and secrets",
	}, "\n")
}

// writeAgentUnit 只写 mechlet 的 unit——受管节点上没有 mechd。
//
// 与 standalone 的那份**刻意分开写**而不是加一堆条件：两种形态的 unit
// 差别在 ExecStart 的参数、依赖关系与是否存在第二个 unit，用条件拼出来的
// 结果没人读得懂，而 unit 文件正是出事时第一个要被人读的东西。
func writeAgentUnit(out io.Writer, o installOptions) error {
	unit := fmt.Sprintf(`[Unit]
Description=Mecharion node agent (mechlet)
Documentation=https://github.com/mecharion/mecharion
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s/mechlet agent --data-dir %s --conf-dir %s --upstream %s --node %s
Restart=always
RestartSec=5s
KillMode=mixed
TimeoutStopSec=60s

[Install]
WantedBy=multi-user.target
`, BinLinkDir, o.DataDir, o.ConfDir, agentUpstream(o.Join.Server), o.Node)

	p := filepath.Join(UnitDir, "mecharion-mechlet.service")
	if err := os.WriteFile(p, []byte(unit), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "      %s\n", p)
	if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
		fmt.Fprintf(out, "      (systemctl daemon-reload didn't succeed, run it manually)\n")
	}
	return nil
}

// agentUpstream 把 join 用的 HTTPS 地址换成 agent 用的 gRPC 地址。
//
// 两者是**同一台机器的两个端口**：join 走 HTTP API（8443），之后的下发与
// 上报走 mTLS gRPC（8444）。让用户在加入时只填一个地址，是因为那时他手上
// 只有一张 token 和一条命令——再让他记住第二个端口没有意义。
func agentUpstream(joinServer string) string {
	host := strings.TrimPrefix(strings.TrimPrefix(joinServer, "https://"), "http://")
	host = strings.TrimSuffix(host, "/")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return net.JoinHostPort(host, DefaultGRPCPort)
}

// DefaultGRPCPort 与 mechd 的 --grpc 默认值对应。
const DefaultGRPCPort = "8444"
