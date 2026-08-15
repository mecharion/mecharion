package ctlcmd

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/mecharion/mecharion/internal/pki"
)

// bootstrapDir 是二进制在目标机器上的落脚点。
//
// **不放 /tmp。** 加固过的机器普遍把 /tmp 挂成 noexec（CIS 基线就这么
// 要求），推过去的二进制在那里根本执行不了，而症状是一个 126 退出码，
// 看不出是挂载选项的问题。
//
// 放在安装根下面：那里如果也不可执行，Mecharion 本来就装不上——
// 失败发生在正确的地方，而不是在一个临时目录上。装完即删。
const bootstrapDir = "/usr/local/lib/mecharion/.bootstrap"

// binaries 是要推过去的四个命令。
//
// **四个一起推，不是只推 mechlet**。`mechlet install` 要求四个在同一个
// 目录里，并且会明确拒绝装一半——「装一半的 Mecharion 会在某个命令上
// 突然缺失」。设计文档早先写的「推送单个静态二进制」是个愿望，
// 与 install 的实际要求对不上（22-multi-node §6.5）。
var bootstrapBinaries = []string{"mechlet", "mechd", "mechctl", "mechpack"}

// newNodeBootstrapCmd 构造 `mechctl node bootstrap`。
//
// **SSH 只用在这一条命令里，之后再不使用**（ADR-0001）。那正是 Agent 模式
// 相对 Ansible 的安全收益：长期暴露面从「SSH 常开」降为「零入站端口」。
func newNodeBootstrapCmd(f *ClientFlags) *cobra.Command {
	var (
		token    string
		caHash   string
		joinURL  string
		identity string
		nodeName string
		timeout  time.Duration
		// 透传给远端 install 的安装布局。多数时候不用改——留着是因为
		// 「数据目录放大盘上」是个真实需求，而 bootstrap 是唯一能表达它
		// 的时刻（装完再改要停服务搬数据）。
		prefix  string
		confDir string
		dataDir string
		linkDir string
	)
	cmd := &cobra.Command{
		Use:   "bootstrap ssh://[user@]host[:port]",
		Short: "Turn a machine into a managed node over SSH",
		Long: `A one-time SSH push: the four binaries plus a mechlet install --join.

**SSH is used exactly once.** After that, mechlet dials out to mechd on its
own — the machine never opens an inbound port. That's the core security
benefit of the agent model over Ansible.

Only public-key auth is supported: password auth means either putting it on
the command line (which ends up in shell history) or typing it interactively
(which doesn't work in scripts) — neither is a good default.`,
		Example: "  mechctl node bootstrap ssh://root@store-042 --token m7n_join_…",
		Args:    cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			target, err := parseSSHTarget(args[0])
			if err != nil {
				return validationErr(err)
			}
			if token == "" {
				return validationErr(fmt.Errorf(
					"--token cannot be empty — generate one with mechctl node token create"))
			}

			// CA 指纹与加入地址都能从本机已有的配置推出来。
			//
			// **不让运维再粘一遍**：他此刻正对着 mechd 说话，mechctl 手上
			// 就有那份 CA 与那个地址。多要一次的唯一后果是多一次贴错。
			if caHash == "" {
				caHash, err = localCAHash(f)
				if err != nil {
					return validationErr(fmt.Errorf(
						"couldn't derive the CA fingerprint, pass --ca-hash explicitly: %w", err))
				}
			}
			if joinURL == "" {
				joinURL = serverOf(f)
			}
			if nodeName == "" {
				nodeName = target.host
			}

			out := c.OutOrStdout()
			fmt.Fprintf(out, "[1/3] connecting to %s\n", target.addr)
			cli, err := dialSSH(target, identity, timeout)
			if err != nil {
				return err
			}
			defer cli.Close()

			fmt.Fprintf(out, "[2/3] pushing binaries to %s\n", bootstrapDir)
			if err := pushBinaries(cli, out); err != nil {
				return err
			}
			// **defer 而不是成功路径上删一次。** 装失败时更要清干净：
			// 那条命令行里带着 token，而留下的二进制会让下一次排查以为
			// 「装过了」。早先写在成功路径上，于是每次失败都留一堆东西。
			defer func() {
				if _, cerr := runSSH(cli, "rm -rf "+bootstrapDir); cerr != nil {
					fmt.Fprintf(out, "  (failed to clean up %s, remove it manually)\n", bootstrapDir)
				}
			}()

			fmt.Fprintf(out, "[3/3] running install --join on the target machine\n")
			install := fmt.Sprintf(
				"%s/mechlet install --join %s --token %s --ca-hash %s --node %s",
				bootstrapDir, shellQuote(joinURL), shellQuote(token),
				shellQuote(caHash), shellQuote(nodeName))
			for flag, v := range map[string]string{
				"--prefix": prefix, "--conf-dir": confDir, "--data-dir": dataDir,
				"--link-dir": linkDir,
			} {
				if v != "" {
					install += " " + flag + " " + shellQuote(v)
				}
			}
			body, err := runSSH(cli, install)
			fmt.Fprint(out, indent(body))
			if err != nil {
				return fmt.Errorf("remote install failed: %w%s", err, execHint(err, body))
			}

			fmt.Fprintf(out, "\n%s has joined. SSH is done; it won't be used again.\n", nodeName)
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&token, "token", "", "join token")
	fl.StringVar(&caHash, "ca-hash", "", "mechd's CA public-key fingerprint, defaults to deriving it from the local CA")
	fl.StringVar(&joinURL, "join-url", "", "mechd address for the target machine to connect to, defaults to --server")
	fl.StringVar(&identity, "identity", "", "SSH private key file, defaults to trying ~/.ssh/id_ed25519 then id_rsa")
	fl.StringVar(&nodeName, "node", "", "Node name, defaults to the hostname")
	fl.DurationVar(&timeout, "timeout", 2*time.Minute, "SSH timeout")
	fl.StringVar(&prefix, "prefix", "", "Remote install root, defaults to mechlet's default")
	fl.StringVar(&confDir, "conf-dir", "", "Remote config and certificate directory")
	fl.StringVar(&dataDir, "data-dir", "", "Remote data directory")
	fl.StringVar(&linkDir, "link-dir", "", "Where remote command symlinks are placed")
	return cmd
}

// ── SSH ─────────────────────────────────────────────────────────────────

type sshTarget struct {
	user, host, addr string
}

func parseSSHTarget(raw string) (sshTarget, error) {
	if !strings.HasPrefix(raw, "ssh://") {
		return sshTarget{}, fmt.Errorf(
			"the target must be written as ssh://[user@]host[:port], got %q", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return sshTarget{}, err
	}
	t := sshTarget{user: "root", host: u.Hostname()}
	if u.User != nil && u.User.Username() != "" {
		t.user = u.User.Username()
	}
	if t.host == "" {
		return sshTarget{}, fmt.Errorf("no hostname in target: %q", raw)
	}
	port := u.Port()
	if port == "" {
		port = "22"
	}
	t.addr = net.JoinHostPort(t.host, port)
	return t, nil
}

func dialSSH(t sshTarget, identity string, timeout time.Duration) (*ssh.Client, error) {
	signer, used, err := loadIdentity(identity)
	if err != nil {
		return nil, validationErr(err)
	}
	cfg := &ssh.ClientConfig{
		User:    t.user,
		Auth:    []ssh.AuthMethod{ssh.PublicKeys(signer)},
		Timeout: timeout,
		// **不校验主机密钥。**
		//
		// 这里必须诚实：bootstrap 是与一台**素未谋面**的机器的第一次接触，
		// 本机没有它的 known_hosts 条目，也没有第二条带外通道来核对指纹。
		//
		// 真正的信任锚在后面那一步——目标机器拿 --ca-hash 校验 mechd，
		// 而那个指纹是带外交付的。即便这条 SSH 被中间人接管，他也拿不到
		// 一张能通过指纹校验的 mechd 身份。
		//
		// 代价是：中间人可以让「这台机器」变成他的机器（推送的二进制落到
		// 别处）。要排除这一点，请自己先建立 known_hosts 再用 --identity。
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // 见上
	}
	cli, err := ssh.Dial("tcp", t.addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("SSH connect to %s (key %s): %w", t.addr, used, err)
	}
	return cli, nil
}

// loadIdentity 读一把私钥，返回它与实际用的路径。
func loadIdentity(path string) (ssh.Signer, string, error) {
	candidates := []string{path}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, "", err
		}
		candidates = []string{
			filepath.Join(home, ".ssh", "id_ed25519"),
			filepath.Join(home, ".ssh", "id_rsa"),
		}
	}
	var tried []string
	for _, p := range candidates {
		body, err := os.ReadFile(p)
		if err != nil {
			tried = append(tried, p)
			continue
		}
		signer, err := ssh.ParsePrivateKey(body)
		if err != nil {
			// 带口令的私钥在这里要说清楚，否则「解析失败」看起来像文件坏了
			return nil, p, fmt.Errorf(
				"read private key %s: %w\n  password-protected keys aren't supported yet, use a key without a passphrase", p, err)
		}
		return signer, p, nil
	}
	return nil, "", fmt.Errorf(
		"no usable SSH private key found (tried %s)\n  specify one with --identity",
		strings.Join(tried, ", "))
}

// pushBinaries 把四个命令送到目标机器。
//
// 走 `cat > 文件` 而不是 sftp 子系统：后者不是所有 sshd 都开着，而
// 「bootstrap 在某些机器上莫名失败」是最难查的一类问题。
func pushBinaries(cli *ssh.Client, out interface{ Write([]byte) (int, error) }) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	srcDir := filepath.Dir(self)

	if _, err := runSSH(cli, "mkdir -p "+bootstrapDir); err != nil {
		return fmt.Errorf("creating directory on target machine: %w", err)
	}
	for _, name := range bootstrapBinaries {
		body, err := os.ReadFile(filepath.Join(srcDir, name))
		if err != nil {
			return fmt.Errorf(
				"couldn't find %s (should be in the same directory as mechctl, %s): %w\n"+
					"  the four binaries are released and installed together", name, srcDir, err)
		}
		sess, err := cli.NewSession()
		if err != nil {
			return err
		}
		sess.Stdin = bytes.NewReader(body)
		dst := bootstrapDir + "/" + name
		if err := sess.Run("cat > " + dst + " && chmod 0755 " + dst); err != nil {
			sess.Close()
			return fmt.Errorf("pushing %s: %w", name, err)
		}
		sess.Close()
		fmt.Fprintf(out, "      %s (%d KiB)\n", name, len(body)/1024)
	}
	return nil
}

// runSSH 跑一条远端命令，返回合并输出。
//
// **两个缓冲区，不是一个。** x/crypto/ssh 用两个 goroutine 分别拷贝
// stdout 与 stderr；把它们指向同一个 bytes.Buffer 是数据竞争，
// 而症状是**远端的错误行整条丢掉**——现场只剩一句「exited with status 1」。
// （os/exec 对同一个 writer 会串行化，ssh 不会；这个差别踩过一次。）
func runSSH(cli *ssh.Client, cmd string) (string, error) {
	sess, err := cli.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &stdout, &stderr
	err = sess.Run(cmd)
	return stdout.String() + stderr.String(), err
}

// ── 本机推导 ────────────────────────────────────────────────────────────

// localCAHash 由 mechctl 已经在用的那份 CA 算出指纹。
func localCAHash(f *ClientFlags) (string, error) {
	caFile := f.CAFile
	if caFile == "" {
		caFile = defaultPath(DefaultCAFile)
	}
	return pki.CAHashFromFile(caFile)
}

func serverOf(f *ClientFlags) string {
	if f.Server != "" {
		return f.Server
	}
	return DefaultServer
}

// shellQuote 把一个值安全地放进远端 shell 命令。
//
// token 与地址都来自用户输入，直接拼进命令行是一条注入路径——
// 而这条命令是**以 root 在别人的机器上执行**的。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func indent(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("      " + line + "\n")
	}
	return b.String()
}

// execHint 把「跑不起来」的退出码翻译成可操作的下一步。
//
// 126 / 127 是 shell 的约定，而它们对应的原因（noexec 挂载、缺少
// 解释器、路径不对）光看数字**一个都猜不出来**。远端命令又常常什么都
// 不打印，于是现场只剩一句「exited with status 126」。
func execHint(err error, body string) string {
	var code int
	if e, ok := err.(*ssh.ExitError); ok {
		code = e.ExitStatus()
	}
	switch code {
	case 126:
		return fmt.Sprintf(
			"\n  126 = the file is there but can't execute. The most common cause is the\n"+
				"  filesystem holding %s is mounted noexec, or an architecture\n"+
				"  mismatch (a binary for a different platform was pushed)",
			bootstrapDir)
	case 127:
		return fmt.Sprintf("\n  127 = couldn't find %s/mechlet, the push may have failed", bootstrapDir)
	}
	if strings.TrimSpace(body) == "" {
		return "\n  (no output from the remote side)"
	}
	return ""
}
