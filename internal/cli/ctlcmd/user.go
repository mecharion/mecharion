package ctlcmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	rootcli "github.com/mecharion/mecharion/internal/cli"
	"github.com/mecharion/mecharion/internal/mechd"
)

// NewUserCmd 构造 `mechctl user`。
//
// **只有一个账号 `admin`，而且它不是在这里建的**——口令由本人在首次访问
// Web UI 时设定（[ADR-0037](../../../docs/adr/0037-login-is-full-privilege.md)）。
// 那是为了无人值守部署：离线场景下脚本装完工具与组件包，人只在最后打开
// 一次浏览器就拿到管理能力。
//
// 因此这组命令是**服务器侧的补救通道**，不是日常入口：口令忘了、
// 或者要把机器交给下一任。
func NewUserCmd(flags *ClientFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage the Web UI's admin account",
		Long: `Manage the Web UI's admin account.

**There's exactly one account, always named admin, and no way to add
more.** Its password is set by a person the first time they visit the Web
UI, not created here.

This group of commands is a server-side recovery channel, not a daily
entry point: the password was forgotten, or the machine is being handed
off to the next owner.

**The password never goes through a command-line argument**: the command
line ends up in shell history and in the ps output visible to any user on
the same machine. Either type it interactively, or use --password-file to
read it from a file.

**Logging in means full privilege**: there's no role separation, anyone
who can log in can do anything.`,
	}
	cmd.AddCommand(
		newUserShowCmd(flags),
		newUserBootstrapCmd(flags),
		newUserPasswdCmd(flags),
		newUserResetCmd(flags),
	)
	return cmd
}

// newUserBootstrapCmd 是**无人值守场景**的首次初始化入口。
//
// 浏览器那条路径要求人手动把初始化令牌从终端输出或 admin.token 文件
// 抄进 Web UI（ADR-0039）——这一步在人工场景下没问题，但脚本化部署里
// 没有人去抄。这条命令把它接上自动化：`mechctl` 本机零配置就能读到
// 与初始化令牌同一个值（DefaultTokenFile），因此脚本只需要在
// `mechlet install --standalone` 之后紧接着跑这一条命令，口令走
// `--password-file`，全程不需要人打开浏览器。
func newUserBootstrapCmd(f *ClientFlags) *cobra.Command {
	var pwFile string
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Complete first-time initialization (unattended, no browser needed)",
		Long: `Complete first-time initialization, with the exact same effect as setting
the password by opening the Web UI in a browser.

For unattended deployments: right after a script installs mechd, running
this command **on the same machine** sets the admin password without
anyone having to go find the one-time initialization token — the local
machine can read /etc/mecharion/admin.token with zero configuration, and
this command uses that exact same value (ADR-0039). For remote execution,
give it explicitly with --token.

Errors (409) if already initialized — the same "one-time" rule as the
browser path, and it can't be bypassed just by using a different entry
point.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			pw, err := readPassword(c, pwFile)
			if err != nil {
				return err
			}
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			var out mechd.AdminView
			if err := cli.Do("POST", mechd.APIPrefix+"/auth/bootstrap",
				mechd.BootstrapBody{Password: pw, Token: cli.Token()}, &out); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "Initialization complete, %s can now log into the Web UI\n", out.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&pwFile, "password-file", "",
		"Read the password from a file (for scripts; interactive input otherwise)")
	return cmd
}

func newUserShowCmd(f *ClientFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show whether the admin account has been initialized",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			var v mechd.AdminView
			if err := cli.Do("GET", mechd.APIPrefix+"/admin", nil, &v); err != nil {
				return err
			}
			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(c.OutOrStdout(), v)
			case rootcli.OutputYAML:
				return writeYAML(c.OutOrStdout(), v)
			}
			w := c.OutOrStdout()
			if !v.Initialized {
				// **这条要说得重**：未初始化意味着任何能打开 UI 的人
				// 都能成为管理员
				fmt.Fprintf(w, "Account   %s (**not yet initialized**)\n", v.Name)
				fmt.Fprintln(w, "Hint      open the Web UI in a browser to set a password.")
				fmt.Fprintln(w, "          Until then, anyone who can reach that address can complete initialization.")
				return nil
			}
			fmt.Fprintf(w, "Account         %s\n", v.Name)
			fmt.Fprintf(w, "Initialized at  %s\n", v.CreatedAt)
			fmt.Fprintf(w, "Password set    %s\n", v.UpdatedAt)
			return nil
		},
	}
}

func newUserPasswdCmd(f *ClientFlags) *cobra.Command {
	var pwFile string
	cmd := &cobra.Command{
		Use:   "passwd",
		Short: "Reset the admin password",
		Long: `Reset the admin password.

**There's no self-service "forgot password" recovery** — that would need
an email channel, which is an external dependency (zero external
dependencies at deploy time is a hard line for this project). If it's
forgotten, reset it with this command on the server.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			pw, err := readPassword(c, pwFile)
			if err != nil {
				return err
			}
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			var out struct{}
			if err := cli.Do("POST", mechd.APIPrefix+"/admin/password",
				mechd.PasswordBody{Password: pw}, &out); err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(), "Updated the admin password")
			return nil
		},
	}
	cmd.Flags().StringVar(&pwFile, "password-file", "",
		"Read the password from a file (for scripts; interactive input otherwise)")
	return cmd
}

func newUserResetCmd(f *ClientFlags) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Wipe the admin account, reopening the initialization window",
		Long: `Wipe the admin account, returning the Web UI to an "uninitialized" state.

Meant for handing a machine off to the next owner: they'll see the
initialization page when they open the UI, and set their own password.

**The danger**: between the wipe and the next completed initialization,
**anyone who can reach this address can become the administrator**. Don't
leave a machine reachable from the public internet sitting in this state
for long.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := confirmYN(c,
				"This will wipe the admin account. Until initialization is completed again, "+
					"anyone who can access the Web UI can become the administrator.", yes); err != nil {
				return err
			}
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			var out struct{}
			if err := cli.Do("POST", mechd.APIPrefix+"/admin/reset",
				struct{}{}, &out); err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(),
				"Wiped admin. The next person to open the Web UI will complete initialization.")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation")
	return cmd
}

// readPassword 取口令：优先文件，其次交互式（输两遍）。
//
// **没有 --password 这个选项，永远不会有**。命令行参数会进 shell 历史，
// 也会出现在同机任何用户的 `ps` 输出里——这与 `component deploy` 的敏感参数
// 必须走 `--set-file` 是同一条纪律（spec §7.7）。
func readPassword(c *cobra.Command, file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", validationErr(fmt.Errorf("read password file: %w", err))
		}
		// 去掉结尾换行：`echo pw > file` 是最常见的造文件方式，
		// 而那个换行几乎从不是口令的一部分
		return strings.TrimRight(string(b), "\r\n"), nil
	}

	in, ok := c.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		// **非交互环境下不从 stdin 裸读**：那样调用者多半会写成
		// `echo pw | mechctl …`——而那条命令本身就进了历史。
		return "", validationErr(errors.New(
			"couldn't read a password (non-interactive environment). Use --password-file to read from a file,\n" +
				"  don't use a pipe or --password: the former puts the password in that command, the latter in ps output"))
	}

	fmt.Fprint(c.ErrOrStderr(), "Password: ")
	pw, err := term.ReadPassword(int(in.Fd()))
	fmt.Fprintln(c.ErrOrStderr())
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}

	fmt.Fprint(c.ErrOrStderr(), "Confirm: ")
	again, err := term.ReadPassword(int(in.Fd()))
	fmt.Fprintln(c.ErrOrStderr())
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if string(pw) != string(again) {
		return "", validationErr(errors.New("the two entries didn't match"))
	}
	return string(pw), nil
}
