package ctlcmd

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	rootcli "github.com/mecharion/mecharion/internal/cli"
	"github.com/mecharion/mecharion/internal/mechd"
	"github.com/spf13/cobra"
)

// NewOrphansCmd 构造 `mechctl orphans`（10-cli §4.7）。
//
// 它是 `component remove` 保留数据目录之后的**配套**：10-cli §4.3 明写
// 「保留而不提供发现机制等于把问题推给未来」。没有这个名词，默认保留
// 就等于在每台机器上悄悄堆下没人知道来历的目录。
func NewOrphansCmd(flags *ClientFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "orphans",
		Short: "Discover and clean up unclaimed leftovers on machines",
		Long: `List things that are still on a machine but that the control plane doesn't
recognize, and offer a way to clean them up.

Orphans come in two kinds, handled completely differently:

  has paths     **data leftovers** from remove. purge just deletes those
                directories.
  no paths      no longer in the dispatch, but the machine **still has it
                installed and possibly still running**. purge can't stop
                it — that requires re-registering it, or handling it
                manually on the machine.

**Orphans are never cleaned up automatically** (20-continuous-reconcile
§2.4): "mechd missed a dispatch" and "the user really deleted this
component" can't be told apart on the node side, and uninstallation is
irreversible.`,
	}
	cmd.AddCommand(newOrphansListCmd(flags), newOrphansPurgeCmd(flags))
	return cmd
}

func newOrphansListCmd(f *ClientFlags) *cobra.Command {
	var node string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all orphans",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			var out struct {
				Orphans []mechd.OrphanEntry `json:"orphans"`
			}
			path := mechd.APIPrefix + "/orphans" + query(map[string]string{"node": node})
			if err := cli.Do("GET", path, nil, &out); err != nil {
				return err
			}
			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(c.OutOrStdout(), out)
			case rootcli.OutputYAML:
				return writeYAML(c.OutOrStdout(), out)
			}
			printOrphans(c, out.Orphans)
			return nil
		},
	}
	cmd.Flags().StringVar(&node, "node", "", "Only show a single machine")
	return cmd
}

func printOrphans(c *cobra.Command, list []mechd.OrphanEntry) {
	w := c.OutOrStdout()
	if len(list) == 0 {
		fmt.Fprintln(w, "No orphans.")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tINSTANCE\tKIND\tSINCE\tPATHS")
	for _, o := range list {
		kind, paths := "data leftover", strings.Join(o.Paths, " ")
		if o.Installed() {
			// **两类必须在列表上就分得开。** 混在一起会让人以为 purge
			// 能停掉一个还在跑的服务。
			kind, paths = "still installed", "(entire instance, not just directories)"
		}
		if o.PurgeRequested {
			kind += " · pending purge"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			o.Node, o.Instance, kind, since(o.FirstSeen), paths)
	}
	_ = tw.Flush()

	fmt.Fprintf(w, "\nClean up: mechctl orphans purge <node> <instance>\n")
}

// since 把一个时刻显示成「多久以前」。
//
// 绝对时间戳要人自己做减法，而这一列要回答的正是「它在那儿放多久了」
// ——那个数字才决定要不要清。
func since(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func newOrphansPurgeCmd(f *ClientFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "purge <node> <instance>",
		Short: "Clean up the directories left behind by an orphan",
		Long: `Delete the data directory an orphan left behind on a machine. **Cannot be
undone.**

**Both node and instance key are required**: the same instance key can
appear on multiple machines (a three-replica component leaves three copies
of data after remove), and giving only the key would wipe all three at
once.

It doesn't delete anything on the spot — it records an intent that's
carried to the node by the dispatch, since that machine may well be
unreachable right now, and a cleanup that only runs while online would
exclude exactly the machines that need it most. The node executes it the
next time it connects.`,
		Example: `  mechctl orphans purge node-2 pg-main__primary`,
		Args:    cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			node, inst := args[0], args[1]

			// **先把要删的东西列出来。** 这条命令的后果无法从命令行看出来
			// ——那几个路径下面可能是 12 TB 的数据。
			var list struct {
				Orphans []mechd.OrphanEntry `json:"orphans"`
			}
			if err := cli.Do("GET",
				mechd.APIPrefix+"/orphans"+query(map[string]string{"node": node}), nil, &list); err != nil {
				return err
			}
			var target *mechd.OrphanEntry
			for i := range list.Orphans {
				if list.Orphans[i].Instance == inst {
					target = &list.Orphans[i]
					break
				}
			}
			if target == nil {
				return validationErr(fmt.Errorf(
					"no orphan named %s on %s\n  see what's there with mechctl orphans list",
					inst, node))
			}

			out := c.OutOrStdout()
			fmt.Fprintf(out, "This will delete the directories %s left behind on %s:\n", inst, node)
			for _, p := range target.Paths {
				fmt.Fprintf(out, "  %s\n", p)
			}
			fmt.Fprintln(out)

			// 二档确认：与 component remove 同一档——它删的是真实数据，
			// 而且没有任何撤销。**`-y` 跳不过去。**
			if err := confirmName(c,
				"These directories will be permanently deleted and cannot be recovered.", inst); err != nil {
				return err
			}

			if err := cli.Do("DELETE",
				mechd.APIPrefix+"/orphans/"+seg(node)+"/"+seg(inst),
				mechd.PurgeOrphanBody{Confirm: inst}, nil); err != nil {
				return err
			}
			fmt.Fprintf(out, "✓ cleanup intent dispatched; will run the next time %s connects\n", node)
			return nil
		},
	}
	return cmd
}
