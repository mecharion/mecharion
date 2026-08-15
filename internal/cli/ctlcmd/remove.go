package ctlcmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/mecharion/mecharion/internal/mechd"
	"github.com/spf13/cobra"
)

// newRemoveCmd 是 `mechctl component remove`（10-cli §4.3）。
//
// **这是整个工具里最危险的操作**——它在 N 台机器上停进程、删文件。
// 因此这条命令的形态由两件事决定：先把后果说清楚，再要一个不可能
// 手滑输入的确认。
func newRemoveCmd(f *ClientFlags) *cobra.Command {
	var (
		keepConfig     bool
		purgeData      bool
		purgeUser      bool
		force          bool
		ignoreNotFound bool
		dryRun         bool
		yes            bool
	)

	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a component (stop processes, delete files)",
		Long: `Remove a Component: stop its processes, uninstall its workload, and delete
its directories on every machine it runs on.

What's deleted by default, what's kept:

  generation directory   deleted
  config directory       deleted        --keep-config keeps it
  data directory         **kept**       --purge-data deletes it too
  system user / group    kept           --purge-user deletes it too (dangerous)

The data directory is kept by default, following the same principle as
"upgrade never touches the data directory". Data left behind is registered
as an orphan, discoverable and cleanable with ` + "`mechctl orphans`" + `.

Removal completes **asynchronously, node by node**: when the command
returns, uninstallation has only just been dispatched, and the component
stays "Removing" until every instance reports it's been torn down.
An unreachable node will keep it stuck there — use --force to skip it in
that case; instances on that machine become orphans and won't clean
themselves up.`,
		Example: `  mechctl component remove pg-main
  mechctl component remove pg-main --purge-data      # delete the data too
  mechctl component remove pg-main --dry-run         # only see the impact
  mechctl component remove pg-main --force           # skip unreachable nodes
  mechctl component remove pg-main --ignore-not-found  # safe to call blindly from scripts`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			name := args[0]
			out := c.OutOrStdout()

			body := mechd.RemoveBody{
				KeepConfig: keepConfig, PurgeData: purgeData, PurgeUser: purgeUser,
				Force: force, IgnoreNotFound: ignoreNotFound,
			}

			// ── ① 先算影响面 ──
			//
			// **总是先干跑一次**，哪怕用户给了 -y。这条命令的后果无法从
			// 命令行本身看出来（几台机器？哪些目录？），而一次没看清后果
			// 的删除正是这条路上最贵的错误。
			preview := body
			preview.DryRun = true
			var dry mechd.RemoveResult
			if err := cli.Do("DELETE",
				mechd.APIPrefix+"/components/"+seg(name), preview, &dry); err != nil {
				return err
			}
			if dry.NotFound {
				fmt.Fprintf(out, "Component %s doesn't exist, nothing to remove\n", name)
				return nil
			}
			printImpact(out, dry.Impact)
			if dryRun {
				return nil
			}

			// ── ② 二档确认 ──
			//
			// **-y 不能跳过这一档**（10-cli §4.3）。别处的 -y 挡的是
			// 「手滑敲了回车」，这里挡的是「删错了对象」——而后者不可逆，
			// 一个 -y 不该同时买断这两件事。
			prompt := fmt.Sprintf("This is irreversible: %d instance(s) will be uninstalled.", dry.Impact.Instances)
			if err := confirmName(c, prompt, name); err != nil {
				return err
			}
			// **--purge-data 要再确认一次**（10-cli §7 的表：需输入
			// Component 名 **＋** 单独确认删除数据）。删掉进程与配置是
			// 可以重新部署回来的；删掉数据不是——两者不该由同一个动作买断。
			if purgeData {
				if err := confirmYN(c,
					fmt.Sprintf("This will also **delete the data directory** (%d path(s)), which cannot be undone. Are you sure?",
						len(dry.Impact.Deleted)), false); err != nil {
					return err
				}
			}
			body.Confirm = name

			// ── ③ 真的下发 ──
			var res mechd.RemoveResult
			if err := cli.Do("DELETE",
				mechd.APIPrefix+"/components/"+seg(name), body, &res); err != nil {
				return err
			}
			printRemoveResult(out, name, res)
			return nil
		},
	}

	fl := cmd.Flags()
	fl.BoolVar(&keepConfig, "keep-config", false, "Keep the config directory (deleted by default)")
	fl.BoolVar(&purgeData, "purge-data", false, "Delete the data directory too (kept by default)")
	fl.BoolVar(&purgeUser, "purge-user", false,
		"Delete the system user/group created by the Pack (dangerous: it may still own files elsewhere)")
	fl.BoolVar(&force, "force", false,
		"Skip unfinished nodes and delete the record anyway; instances on those machines become orphans")
	fl.BoolVar(&ignoreNotFound, "ignore-not-found", false,
		"Succeed silently if the component doesn't exist, safe to call blindly from scripts")
	fl.BoolVar(&dryRun, "dry-run", false, "Only print the impact, do nothing")
	fl.BoolVarP(&yes, "yes", "y", false,
		"Skip the general confirmation (**cannot** skip typing the component name or the data-deletion confirmation)")
	_ = yes // 这条命令的两档确认都不接受 -y，留着这个 flag 只为不打断肌肉记忆
	return cmd
}

// printImpact 把「这次会发生什么」打清楚。
func printImpact(w io.Writer, im mechd.RemovalImpact) {
	if im.Removing {
		fmt.Fprintf(w, "⚠ %s is already being removed (%d/%d done)\n",
			im.Component, im.Progress.Done, im.Progress.Total)
		if len(im.Progress.Pending) > 0 {
			fmt.Fprintf(w, "  pending: %s\n", strings.Join(im.Progress.Pending, " "))
		}
		fmt.Fprintln(w)
		return
	}

	fmt.Fprintf(w, "About to remove %s (%s %s)\n", im.Component, im.Pack, im.Version)
	fmt.Fprintf(w, "  %d instance(s) across %d machine(s): %s\n",
		im.Instances, len(im.Nodes), strings.Join(im.Nodes, " "))

	// **两边都列。** 只列要删的，人无从知道盘上还会剩什么；只列要留的，
	// 人无从判断这次删除到底有多狠。
	if len(im.Deleted) > 0 {
		fmt.Fprintf(w, "\n  will delete (on each machine):\n")
		for _, d := range im.Deleted {
			fmt.Fprintf(w, "    %s\n", d)
		}
	}
	if len(im.Retained) > 0 {
		fmt.Fprintf(w, "\n  will keep (registered as orphans, clean up with mechctl orphans):\n")
		for _, d := range im.Retained {
			fmt.Fprintf(w, "    %s\n", d)
		}
	}
	fmt.Fprintln(w)
}

func printRemoveResult(w io.Writer, name string, res mechd.RemoveResult) {
	switch {
	case res.Deleted:
		fmt.Fprintf(w, "✓ %s's record has been deleted\n", name)
		if len(res.Impact.Progress.Pending) > 0 {
			fmt.Fprintf(w, "  skipped %d unfinished instance(s), they will become orphans:\n    %s\n",
				len(res.Impact.Progress.Pending),
				strings.Join(res.Impact.Progress.Pending, " "))
			fmt.Fprintf(w, "  see mechctl orphans list\n")
		}
	default:
		fmt.Fprintf(w, "✓ removal dispatched, %s is being removed\n", name)
		fmt.Fprintf(w, "  uninstallation is proceeding asynchronously on each node; the record disappears once it's all done\n")
		fmt.Fprintf(w, "  see mechctl component list for progress\n")
	}
}
