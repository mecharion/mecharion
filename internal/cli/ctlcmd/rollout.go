package ctlcmd

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	rootcli "github.com/mecharion/mecharion/internal/cli"
	"github.com/mecharion/mecharion/internal/mechd"
)

// NewRolloutCmd 构造 `mechctl rollout`。
//
// 单机形态下「分批」退化成一批，但 Rollout 不只是分批：它给「升级到一半」
// 这件事一个名字。没有它，运维只能看到「收敛没收敛」，而他想问的是
// **「这次升级怎么样了、能不能停下、要不要退回去」**（22-upgrade §2.6）。
func NewRolloutCmd(flags *ClientFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rollout",
		Short: "View and control a version change in progress",
	}
	cmd.AddCommand(
		newRolloutStatusCmd(flags),
		newRolloutHistoryCmd(flags),
		newRolloutActionCmd(flags, "pause"),
		newRolloutActionCmd(flags, "resume"),
		newRolloutActionCmd(flags, "abort"),
	)
	return cmd
}

func newRolloutStatusCmd(f *ClientFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "status <component>",
		Short: "Show the version change currently in progress",
		Long: `Shows the most recent one when nothing is in progress.

**This is deliberate**: an operator runs this command mostly because they
just did an upgrade, and "no rollout in progress" as an answer says
nothing useful.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			var v mechd.RolloutView
			if err := cli.Do("GET",
				mechd.APIPrefix+"/components/"+seg(args[0])+"/rollout", nil, &v); err != nil {
				return err
			}
			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(c.OutOrStdout(), v)
			case rootcli.OutputYAML:
				return writeYAML(c.OutOrStdout(), v)
			}
			printRollout(c, args[0], v)
			return nil
		},
	}
}

func newRolloutHistoryCmd(f *ClientFlags) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "history <component>",
		Short: "List past version changes",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			var out struct {
				Rollouts []mechd.RolloutView `json:"rollouts"`
			}
			path := fmt.Sprintf("%s/components/%s/rollout/history?limit=%d",
				mechd.APIPrefix, args[0], limit)
			if err := cli.Do("GET", path, nil, &out); err != nil {
				return err
			}
			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(c.OutOrStdout(), out)
			case rootcli.OutputYAML:
				return writeYAML(c.OutOrStdout(), out)
			}
			w := tabwriter.NewWriter(c.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "STARTED\tACTION\tFROM\tTO\tSTATE\tREASON")
			for _, v := range out.Rollouts {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					v.StartedAt, v.Kind, orDash(v.From), v.To,
					describeRolloutState(v.State), orDash(v.Reason))
			}
			return w.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "Maximum number of entries to list")
	return cmd
}

func newRolloutActionCmd(f *ClientFlags, action string) *cobra.Command {
	short := map[string]string{
		"pause":  "Freeze the gate (not the same as pausing dispatch)",
		"resume": "Resume gating",
		"abort":  "Abort and roll back to the starting version",
	}[action]
	long := map[string]string{
		"pause": `Freeze the "fail if it doesn't converge within this long" clock, and
**stop at the current batch**.

A batch that's already been released keeps running to completion, but the
next batch won't be released. It says "I'm investigating, don't proceed
yet."`,
		"resume": `Resume progress.

When halted by a failure, it picks up from **the batch that didn't pass the
gate**: that batch waits for the gate again, and no completed batch is
redone.

It doesn't check whether the machine has actually been fixed — mechd can't
tell, and you know better than it does. If it wasn't fixed, the next gate
check will stop it again after another batch timeout.`,
		"abort": `Abort this change, and **actually roll back to the starting version**.

Not just marking the record as aborted — that would leave an operator
thinking the world is back to pre-upgrade while the machines are still
running the new version.`,
	}[action]

	var yes bool
	cmd := &cobra.Command{
		Use:   action + " <component>",
		Short: short,
		Long:  long,
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			// 只有 abort 需要确认：pause / resume 都可以再敲一次撤销，
			// 而 abort 会真的把机器退回去（10-cli §7）。
			if action == "abort" {
				if err := confirmYN(c,
					fmt.Sprintf("This will abort %s's version change and roll it back to the starting version.", args[0]),
					yes); err != nil {
					return err
				}
			}
			var v mechd.RolloutView
			if err := cli.Do("POST",
				mechd.APIPrefix+"/components/"+seg(args[0])+"/rollout/"+action,
				struct{}{}, &v); err != nil {
				return err
			}
			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(c.OutOrStdout(), v)
			case rootcli.OutputYAML:
				return writeYAML(c.OutOrStdout(), v)
			}
			printRollout(c, args[0], v)
			return nil
		},
	}
	if action == "abort" {
		cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation")
	}
	return cmd
}

func printRollout(c *cobra.Command, name string, v mechd.RolloutView) {
	w := c.OutOrStdout()
	fmt.Fprintf(w, "%s  %s %s → %s\n", name, v.Kind, orDash(v.From), v.To)
	fmt.Fprintf(w, "State     %s\n", describeRolloutState(v.State))
	if v.Reason != "" {
		fmt.Fprintf(w, "Reason    %s\n", v.Reason)
	}
	// 「第 N/共 M 批」是多节点升级里运维最想知道的一个数：它把
	// 「还要多久」从一个感觉变成一个数字。
	if v.Batches > 0 {
		fmt.Fprintf(w, "Batch     %d/%d", v.Batch, v.Batches)
		if v.Current != "" {
			fmt.Fprintf(w, " (working on %s)", v.Current)
		}
		fmt.Fprintln(w)
	}
	// 失败之后集群是混合版本的——**不说的话运维得一台台去看**
	if len(v.Mixed) > 0 {
		versions := make([]string, 0, len(v.Mixed))
		for ver := range v.Mixed {
			versions = append(versions, ver)
		}
		sort.Strings(versions)
		fmt.Fprintln(w, "Current distribution")
		for _, ver := range versions {
			fmt.Fprintf(w, "  %-10s %s\n", ver, strings.Join(v.Mixed[ver], ", "))
		}
	}
	if len(v.Skipped) > 0 {
		// 不列的话，「为什么这台还是旧版」会变成一次排查
		fmt.Fprintf(w, "Skipped   %s (cordoned, restore with node uncordon)\n",
			strings.Join(v.Skipped, ", "))
	}
	fmt.Fprintf(w, "Started   %s\n", v.StartedAt)
	if v.EndedAt != "" {
		fmt.Fprintf(w, "Ended     %s\n", v.EndedAt)
	}
}

// describeRolloutState 把状态词换成一句人话。
//
// 状态输出是给现场的人看的，那时他多半正忙——一句「已失败（节点已回滚）」
// 比一个 `failed` 有用。
func describeRolloutState(s string) string {
	switch s {
	case "running":
		return "running"
	case "succeeded":
		return "succeeded"
	case "failed":
		return "failed"
	case "halted":
		// **说清是谁停的、以及怎么往前走**：运维看到这一行时多半正在处理
		// 一次故障，一个光秃秃的 halted 会让他先去查这个词是什么意思。
		return "halted due to a failure (fix it then rollout resume to continue, or rollout abort to roll back)"
	case "paused":
		return "gate frozen (resume with rollout resume)"
	case "aborted":
		return "aborted and rolled back"
	}
	return s
}
