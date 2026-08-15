package ctlcmd

import (
	"fmt"
	"io"
	"text/tabwriter"

	rootcli "github.com/mecharion/mecharion/internal/cli"
	"github.com/mecharion/mecharion/internal/mechd"
	"github.com/spf13/cobra"
)

// newRestartCmd 是 `mechctl component restart`（06-state-and-drift）。
//
// **它不改期望状态**，只是把进程踢一下。因此它既不产生新 generation、
// 也不触发 Rollout——下一轮调和看到的仍是同一份规格。
//
// 命令走独立的 ad-hoc 通道（ADR-0038），因此结果是**逐节点**的：
// 「n3 不可达、未执行」是一句确定的话，而不是一个悬着的承诺。
func newRestartCmd(f *ClientFlags) *cobra.Command {
	var (
		role    string
		node    string
		timeout int
		yes     bool
	)

	cmd := &cobra.Command{
		Use:   "restart <name>",
		Short: "Restart a component's instances (doesn't change desired state)",
		Long: `Stop and bring back up a component's workload.

**It doesn't change the desired state**: the desired state stays the same,
all that changes is "kick it right now". So it doesn't produce a new
generation and doesn't trigger a rolling upgrade — the next reconcile still
sees the same spec.

By default it restarts **all** matching instances **at the same time** —
the service will see a brief interruption. Use --role / --node to narrow it
down.

An unreachable node is reported honestly as "unreachable, not executed": the
command doesn't queue up or catch up later. Restarting a machine that
reconnects after three days offline, just because it missed a restart, is
pure harm.`,
		Example: `  mechctl component restart web
  mechctl component restart web --node node-3
  mechctl component restart web --role replica`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			name := args[0]

			// 同时重启全部实例会让服务短暂中断——那要人点头。
			// 缩小到单台时不问：那本来就是「只动这一台」的意思。
			if role == "" && node == "" {
				if err := confirmYN(c,
					fmt.Sprintf("This will restart all instances of %s at the same time, causing a brief service interruption. Are you sure?", name),
					yes); err != nil {
					return err
				}
			}

			var res mechd.RestartResult
			if err := cli.Do("POST",
				mechd.APIPrefix+"/components/"+seg(name)+"/restart",
				mechd.RestartBody{Role: role, Node: node, TimeoutSeconds: timeout},
				&res); err != nil {
				return err
			}
			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(c.OutOrStdout(), res)
			case rootcli.OutputYAML:
				return writeYAML(c.OutOrStdout(), res)
			}
			printRestart(c.OutOrStdout(), res)
			if res.Failed() {
				// **部分失败要非零退出**：CI 会把一次半成功当成成功。
				return fmt.Errorf("some instance(s) failed to restart")
			}
			return nil
		},
	}

	fl := cmd.Flags()
	fl.StringVar(&role, "role", "", "Only restart this role")
	fl.StringVar(&node, "node", "", "Only restart instances on this machine")
	fl.IntVar(&timeout, "timeout", 0, "Seconds to wait for the result (default 30)")
	fl.BoolVarP(&yes, "yes", "y", false, "Skip confirmation")
	return cmd
}

func printRestart(w io.Writer, res mechd.RestartResult) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, o := range res.Instances {
		fmt.Fprintf(tw, "  %s@%s\t%s\n", o.Role, o.Node, restartLabel(o))
	}
	_ = tw.Flush()
}

// restartLabel 把一个结果说成人话。
//
// **unreachable 与 failed 必须看得出区别**：前者是「那台机器没连着，
// 命令根本没发出去」，后者是「发出去了但没成功」。运维的下一步完全
// 不同——一个去查网络，一个去看日志。
func restartLabel(o mechd.RestartOutcome) string {
	switch o.State {
	case "ok":
		return fmt.Sprintf("✓ restarted (%.1fs)", float64(o.Millis)/1000)
	case "unreachable":
		if o.Message != "" {
			return "✗ unreachable, not executed (" + o.Message + ")"
		}
		return "✗ unreachable, not executed"
	case "timeout":
		return "✗ timed out, result unknown"
	default:
		if o.Message != "" {
			return "✗ " + o.Message
		}
		return "✗ failed"
	}
}
