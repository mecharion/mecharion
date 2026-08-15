package ctlcmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	rootcli "github.com/mecharion/mecharion/internal/cli"
	"github.com/mecharion/mecharion/internal/mechd"
	"github.com/spf13/cobra"
)

// NewApplyCmd 构造 `mechctl apply -f`（10-cli §5）。
//
// 它是**声明式主干的入口**：接受一份可能同时含 Site、Component、
// ConfigGroup 的文件，因此不属于任何单一名词。
//
// **它不是第二条部署路径**：每个组件都走与 `component deploy --update`
// 完全相同的那条路。另写一套迟早会与它分叉，而分叉的部署路径意味着
// 「用 apply 装的」与「用 deploy 装的」会有微妙的不同。
func NewApplyCmd(flags *ClientFlags) *cobra.Command {
	var (
		file   string
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:   "apply -f <file>",
		Short: "Converge the cluster to a declared file",
		Long: `Read a declaration file and converge the cluster to the state it describes.

The file is organized by noun at the top level, with field names matching
` + "`component deploy`" + `'s parameters one for one:

  site:          site name and kind (**checked only, never changed**)
  components:    each component's pack / version / profile / roles / set / require
  configGroups:  config groups

Secret-typed parameters **can only go through setFile** (the value is read
from a file, never written into the declaration file) — this file goes into
version control and gets passed around, which is the same reason --set is
disallowed on deploy.

**apply never deletes.** Components that exist in the cluster but not in
the file are listed as a reminder, but left untouched — a file that missed
something shouldn't delete a live component. Deletion goes through ` +
			"`mechctl component remove`" + `.`,
		Example: `  mechctl apply -f site.yaml
  mechctl apply -f site.yaml --dry-run
  mechctl apply -f -                    # read from stdin`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if file == "" {
				return validationErr(fmt.Errorf("declaration file required, use -f"))
			}
			data, err := readDoc(c, file)
			if err != nil {
				return validationErr(err)
			}
			// **在客户端解析。** `setFile` 里的路径是客户端的概念——那个
			// 文件多半在这台机器上，而 mechd 可能在另一台。既然文件必须
			// 在这边读，YAML 也一起在这边解掉。
			doc, err := mechd.ParseApplyDoc(data)
			if err != nil {
				return validationErr(err)
			}
			secrets, err := readSetFiles(doc)
			if err != nil {
				return validationErr(err)
			}

			cli, err := flags.client()
			if err != nil {
				return validationErr(err)
			}
			var res mechd.ApplyResult
			if err := cli.Do("POST", mechd.APIPrefix+"/apply", mechd.ApplyBody{
				Doc: doc, Secrets: secrets, DryRun: dryRun,
			}, &res); err != nil {
				return err
			}
			switch flags.output() {
			case rootcli.OutputJSON:
				return writeJSON(c.OutOrStdout(), res)
			case rootcli.OutputYAML:
				return writeYAML(c.OutOrStdout(), res)
			}
			printApply(c.OutOrStdout(), res)
			return applyExit(res)
		},
	}

	fl := cmd.Flags()
	fl.StringVarP(&file, "file", "f", "", "Declaration file path, `-` means stdin")
	fl.BoolVar(&dryRun, "dry-run", false, "Only compute what would happen, change nothing")
	return cmd
}

func readDoc(c *cobra.Command, file string) ([]byte, error) {
	if file == "-" {
		return io.ReadAll(c.InOrStdin())
	}
	return os.ReadFile(file)
}

// readSetFiles 读出 setFile 指向的明文。
//
// **在客户端读**：那些路径（`/run/secrets/pg`）存在于敲命令的这台机器
// 上，mechd 上多半没有。这与 `deploy --set-file` 是同一条路。
func readSetFiles(doc *mechd.ApplyDoc) (map[string]any, error) {
	out := map[string]any{}
	for _, c := range doc.Components {
		for param, path := range c.SetFile {
			b, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("component %s's setFile.%s: %w",
					c.EffectiveName(), param, err)
			}
			// 去掉结尾换行：`echo secret > file` 会带一个，而它几乎从来
			// 不是口令的一部分。不去掉的症状是「口令明明对却连不上」。
			out[c.EffectiveName()+"/"+param] =
				strings.TrimRight(string(b), "\r\n")
		}
	}
	return out, nil
}

func printApply(w io.Writer, res mechd.ApplyResult) {
	if res.DryRun {
		fmt.Fprintln(w, "(dry run: nothing was changed)")
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range res.Components {
		switch {
		case c.Error != "":
			fmt.Fprintf(tw, "✗\t%s\t%s\n", c.Name, firstLineOf(c.Error))
		case c.Removing:
			fmt.Fprintf(tw, "·\t%s\tbeing removed, skipped this time\n", c.Name)
		default:
			fmt.Fprintf(tw, "✓\t%s\t%s (%d instance(s))\n",
				c.Name, actionLabel(c.Action), c.Instances)
		}
	}
	for _, g := range res.Groups {
		if g.Error != "" {
			fmt.Fprintf(tw, "✗\t%s/%s/%s\t%s\n",
				g.Component, g.Role, g.Name, firstLineOf(g.Error))
			continue
		}
		fmt.Fprintf(tw, "✓\t%s/%s/%s\tconfig group updated\n", g.Component, g.Role, g.Name)
	}
	_ = tw.Flush()

	// **多余的组件必须说出来。** 不说的话，一份写漏了的文件与一份写对的
	// 文件输出一模一样——而那正是声明式最容易出的事故。
	if len(res.Extra) > 0 {
		fmt.Fprintf(w, "\n⚠ These components exist in the cluster but not in this file, **left untouched**:\n")
		for _, n := range res.Extra {
			fmt.Fprintf(w, "    %s\n", n)
		}
		fmt.Fprintf(w, "  Declarative doesn't mean \"missing from the file means delete it\".\n")
		fmt.Fprintf(w, "  To actually delete: mechctl component remove <name>\n")
	}
}

func actionLabel(a string) string {
	switch a {
	case "created":
		return "Created"
	case "updated":
		return "Updated"
	default:
		return "Confirmed"
	}
}

// applyExit 让部分失败可以被脚本发现。
//
// **一个组件失败不该让其余的不执行**（它们彼此独立），但整条命令必须
// 以非零退出——否则 CI 会把一次半成功当成成功。
func applyExit(res mechd.ApplyResult) error {
	var bad []string
	for _, c := range res.Components {
		if c.Error != "" {
			bad = append(bad, c.Name)
		}
	}
	for _, g := range res.Groups {
		if g.Error != "" {
			bad = append(bad, g.Component+"/"+g.Role+"/"+g.Name)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("%d item(s) failed: %s", len(bad), strings.Join(bad, " "))
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
