package ctlcmd

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	rootcli "github.com/mecharion/mecharion/internal/cli"
	"github.com/mecharion/mecharion/internal/mechd"
	"github.com/spf13/cobra"
)

// `mechctl config group *`（10-cli §4.4 · ADR-0021）。
//
// 这一族命令在设计里躺了很久：ADR-0021 在 2026-08-02 就写下了完整的命令面，
// 而 `config_groups` 表、解析链、多盘绑定的求值代码也都在——唯独没有任何
// 创建入口。第 7 步补的就是它。

type groupDetail struct {
	Name    string              `json:"name"`
	Role    string              `json:"role"`
	Members []string            `json:"members"`
	Params  map[string]any      `json:"params"`
	Paths   map[string][]string `json:"paths"`
}

func newConfigGroupCmd(f *ClientFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "group",
		Short: "Manage config groups (named subsets with different values within the same role)",
		Long: `A config group is a "named subset of instances sharing the same parameter
overrides" (ADR-0021).

**There's no such thing as an unnamed per-node override** in the model:
making one machine different requires a reason with a name attached.
` + "`config set --node`" + ` creates a group containing just that one machine for
you — same number of steps as an unnamed override, but the result is
named, enumerable, and diffable.`,
	}
	cmd.AddCommand(
		newGroupListCmd(f),
		newGroupCreateCmd(f),
		newGroupSetCmd(f),
		newGroupMoveCmd(f),
		newGroupRemoveCmd(f),
		newGroupDiffCmd(f),
	)
	return cmd
}

func groupsPath(component, role string) string {
	return mechd.APIPrefix + "/components/" + seg(component) + "/groups" + query(map[string]string{"role": role})
}

func groupPath(component, group string) string {
	return mechd.APIPrefix + "/components/" + seg(component) + "/groups/" + seg(group)
}

func fetchGroups(f *ClientFlags, component, role string) ([]groupDetail, error) {
	cli, err := f.client()
	if err != nil {
		return nil, validationErr(err)
	}
	var out struct {
		Groups []groupDetail `json:"groups"`
	}
	if err := cli.Do("GET", groupsPath(component, role), nil, &out); err != nil {
		return nil, err
	}
	return out.Groups, nil
}

// ── list ────────────────────────────────────────────────────────────────

func newGroupListCmd(f *ClientFlags) *cobra.Command {
	var component, role string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List config groups",
		RunE: func(c *cobra.Command, _ []string) error {
			groups, err := fetchGroups(f, component, role)
			if err != nil {
				return err
			}
			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(c.OutOrStdout(), map[string]any{"groups": groups})
			case rootcli.OutputYAML:
				return writeYAML(c.OutOrStdout(), map[string]any{"groups": groups})
			}
			w := c.OutOrStdout()
			if len(groups) == 0 {
				fmt.Fprintln(w, "No config groups — all of this component's instances use role-level values.")
				return nil
			}
			for _, g := range groups {
				fmt.Fprintf(w, "%s  (role %s, %d machine(s))\n", g.Name, g.Role, len(g.Members))
				fmt.Fprintf(w, "  members: %s\n", strings.Join(g.Members, ", "))
				for _, k := range sortedAny(g.Params) {
					fmt.Fprintf(w, "  param: %s = %v\n", k, g.Params[k])
				}
				for _, k := range sortedPaths(g.Paths) {
					fmt.Fprintf(w, "  path: %s → %s\n", k, strings.Join(g.Paths[k], ", "))
				}
				fmt.Fprintln(w)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&component, "component", "c", "", "Component name (required)")
	cmd.Flags().StringVarP(&role, "role", "r", "", "Only show this role")
	_ = cmd.MarkFlagRequired("component")
	return cmd
}

// ── create ──────────────────────────────────────────────────────────────

func newGroupCreateCmd(f *ClientFlags) *cobra.Command {
	var component, role string
	var nodes, sets, setFiles, paths []string
	var dryRun, yes bool

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new config group",
		Example: `  mechctl config group create ssd-nodes -c hdfs -r datanode --nodes n21,n22
  mechctl config group create 12-disk -c hdfs -r datanode --nodes n7,n8 \
      --path dataDirs=data1,data2`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			params, err := parseSets(sets, setFiles)
			if err != nil {
				return validationErr(err)
			}
			binds, err := parsePathBindings(paths)
			if err != nil {
				return validationErr(err)
			}
			return applyGroup(c, f, component, args[0], role, nodes, params, binds, dryRun, yes)
		},
	}
	cmd.Flags().StringVarP(&component, "component", "c", "", "Component name (required)")
	cmd.Flags().StringVarP(&role, "role", "r", "", "Role (required)")
	cmd.Flags().StringSliceVar(&nodes, "nodes", nil, "Member nodes, comma-separated")
	cmd.Flags().StringArrayVar(&sets, "set", nil, "Parameter override <param>=<value>")
	cmd.Flags().StringArrayVar(&setFiles, "set-file", nil, "Read from a file <param>=@<path>")
	cmd.Flags().StringArrayVar(&paths, "path", nil, "Multi-disk binding <path-name>=<vol1>,<vol2>")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Only show what would happen")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation")
	_ = cmd.MarkFlagRequired("component")
	_ = cmd.MarkFlagRequired("role")
	return cmd
}

// ── set ─────────────────────────────────────────────────────────────────

func newGroupSetCmd(f *ClientFlags) *cobra.Command {
	var component, role string
	var sets, setFiles, paths []string
	var dryRun, yes bool

	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Change a config group's parameters or path bindings",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			// 成员保持不变：改参数不该顺手动成员。
			// PUT 要求完整的组，因此先读回来。
			cur, err := findGroupByName(f, component, role, args[0])
			if err != nil {
				return err
			}
			params, err := parseSets(sets, setFiles)
			if err != nil {
				return validationErr(err)
			}
			merged := map[string]any{}
			for k, v := range cur.Params {
				merged[k] = v
			}
			for k, v := range params {
				merged[k] = v
			}
			binds := cur.Paths
			if len(paths) > 0 {
				if binds, err = parsePathBindings(paths); err != nil {
					return validationErr(err)
				}
			}
			return applyGroup(c, f, component, args[0], role,
				cur.Members, merged, binds, dryRun, yes)
		},
	}
	cmd.Flags().StringVarP(&component, "component", "c", "", "Component name (required)")
	cmd.Flags().StringVarP(&role, "role", "r", "", "Role (required)")
	cmd.Flags().StringArrayVar(&sets, "set", nil, "Parameter override <param>=<value>")
	cmd.Flags().StringArrayVar(&setFiles, "set-file", nil, "Read from a file <param>=@<path>")
	cmd.Flags().StringArrayVar(&paths, "path", nil, "Multi-disk binding <path-name>=<vol1>,<vol2> (replaces the whole set if given)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Only show what would happen")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation")
	_ = cmd.MarkFlagRequired("component")
	_ = cmd.MarkFlagRequired("role")
	return cmd
}

// ── move ────────────────────────────────────────────────────────────────

func newGroupMoveCmd(f *ClientFlags) *cobra.Command {
	var component, role, to string
	var dryRun, yes bool

	cmd := &cobra.Command{
		Use:   "move <node>",
		Short: "Move a machine into a different config group",
		Long: `Move a machine from its current group to the one given by --to.

**This is a real config change**: the machine's values switch from one
group to another, its config files change, and parameters marked
restartRequired will restart the service.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			node := args[0]
			groups, err := fetchGroups(f, component, role)
			if err != nil {
				return err
			}
			var target *groupDetail
			for i := range groups {
				if groups[i].Name == to {
					target = &groups[i]
				}
			}
			if target == nil {
				return validationErr(fmt.Errorf(
					"no config group %q under role %s\n  existing groups: %s",
					to, role, strings.Join(groupNames(groups), ", ")))
			}
			for _, m := range target.Members {
				if m == node {
					fmt.Fprintf(c.OutOrStdout(), "%s is already in %s.\n", node, to)
					return nil
				}
			}

			// 先把它从原来的组里摘出来——服务端会拒绝重叠成员，
			// 因此顺序不能反过来。
			for i := range groups {
				g := &groups[i]
				if g.Name == to {
					continue
				}
				for j, m := range g.Members {
					if m != node {
						continue
					}
					left := append(append([]string{}, g.Members[:j]...), g.Members[j+1:]...)
					if err := putGroup(f, component, g.Name, role,
						left, g.Params, g.Paths, false); err != nil {
						return err
					}
					fmt.Fprintf(c.OutOrStdout(), "Moved %s out of %s.\n", node, g.Name)
				}
			}
			return applyGroup(c, f, component, to, role,
				append(target.Members, node), target.Params, target.Paths, dryRun, yes)
		},
	}
	cmd.Flags().StringVarP(&component, "component", "c", "", "Component name (required)")
	cmd.Flags().StringVarP(&role, "role", "r", "", "Role (required)")
	cmd.Flags().StringVar(&to, "to", "", "Target config group (required)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Only show what would happen")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation")
	_ = cmd.MarkFlagRequired("component")
	_ = cmd.MarkFlagRequired("role")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

// ── remove ──────────────────────────────────────────────────────────────

func newGroupRemoveCmd(f *ClientFlags) *cobra.Command {
	var component, role string
	var dryRun, yes bool

	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Delete a config group, members fall back to role-level values",
		Long: `Delete a config group.

**This isn't cleanup, it's a real config change**: member machines' config
files revert to role-level values, their digest changes, and parameters
marked restartRequired will restart the service. So it goes through the
same preview-and-confirm flow as ` + "`config set`" + `.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			path := groupPath(component, args[0]) + query(map[string]string{"role": role})

			var preview setParamsResp
			if err := cli.Do("DELETE", appendQuery(path, url.Values{"dryRun": {"true"}}), nil, &preview); err != nil {
				return err
			}
			printPreview(c, preview)
			if dryRun {
				return nil
			}
			if err := confirmYN(c,
				fmt.Sprintf("Delete config group %s, its members will fall back to role-level values, continue?", args[0]),
				yes); err != nil {
				return err
			}
			var out setParamsResp
			if err := cli.Do("DELETE", path, nil, &out); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "Deleted config group %s.\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVarP(&component, "component", "c", "", "Component name (required)")
	cmd.Flags().StringVarP(&role, "role", "r", "", "Role (required)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Only show what would happen")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation")
	_ = cmd.MarkFlagRequired("component")
	_ = cmd.MarkFlagRequired("role")
	return cmd
}

// ── diff ────────────────────────────────────────────────────────────────

func newGroupDiffCmd(f *ClientFlags) *cobra.Command {
	var component, role string
	cmd := &cobra.Command{
		Use:   "diff <a> <b>",
		Short: "Compare the values of two config groups",
		Long: `Compare two config groups. Use ` + "`default`" + ` to refer to role-level values
(the implicit group).

This command answers one of the core questions ADR-0021 set out to solve:
"how many config shapes does this cluster have, and why do they differ".`,
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			groups, err := fetchGroups(f, component, role)
			if err != nil {
				return err
			}
			a, err := pickGroup(groups, args[0])
			if err != nil {
				return err
			}
			b, err := pickGroup(groups, args[1])
			if err != nil {
				return err
			}

			w := c.OutOrStdout()
			keys := map[string]bool{}
			for k := range a.Params {
				keys[k] = true
			}
			for k := range b.Params {
				keys[k] = true
			}
			names := make([]string, 0, len(keys))
			for k := range keys {
				names = append(names, k)
			}
			sort.Strings(names)

			fmt.Fprintf(w, "%-22s %-20s %-20s\n", "PARAMETER", args[0], args[1])
			same := true
			for _, k := range names {
				av, aok := a.Params[k]
				bv, bok := b.Params[k]
				if aok && bok && fmt.Sprint(av) == fmt.Sprint(bv) {
					continue
				}
				same = false
				fmt.Fprintf(w, "%-22s %-20s %-20s\n", k, cell(av, aok), cell(bv, bok))
			}
			if same {
				fmt.Fprintln(w, "(parameter values are identical)")
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&component, "component", "c", "", "Component name (required)")
	cmd.Flags().StringVarP(&role, "role", "r", "", "Role (required)")
	_ = cmd.MarkFlagRequired("component")
	_ = cmd.MarkFlagRequired("role")
	return cmd
}

// pickGroup 按名字挑一个组；`default` 指代角色级取值。
func pickGroup(groups []groupDetail, name string) (groupDetail, error) {
	if name == "default" {
		// 隐式组：没有任何覆盖。它不在列表里，因为它不是一个对象。
		return groupDetail{Name: "default"}, nil
	}
	for _, g := range groups {
		if g.Name == name {
			return g, nil
		}
	}
	return groupDetail{}, validationErr(fmt.Errorf(
		"no config group %q\n  existing groups: %s (default refers to role-level values)",
		name, strings.Join(groupNames(groups), ", ")))
}

func cell(v any, ok bool) string {
	if !ok {
		return "—"
	}
	return fmt.Sprint(v)
}

// ── 共用 ────────────────────────────────────────────────────────────────

// applyGroup 建组 / 改组：先干跑再确认，与 config set 同一套。
func applyGroup(
	c *cobra.Command, f *ClientFlags, component, name, role string,
	members []string, params map[string]any, paths map[string][]string,
	dryRun, yes bool,
) error {
	var preview setParamsResp
	if err := putGroupInto(f, component, name, role, members, params, paths, true, &preview); err != nil {
		return err
	}
	printPreview(c, preview)
	if dryRun {
		return nil
	}
	if preview.Effect != "none" {
		if err := confirmYN(c, effectPrompt(preview), yes); err != nil {
			return err
		}
	}
	if err := putGroup(f, component, name, role, members, params, paths, false); err != nil {
		return err
	}
	fmt.Fprintf(c.OutOrStdout(), "Saved config group %s (%d machine(s)).\n", name, len(members))
	return nil
}

func putGroup(
	f *ClientFlags, component, name, role string,
	members []string, params map[string]any, paths map[string][]string, dry bool,
) error {
	var out setParamsResp
	return putGroupInto(f, component, name, role, members, params, paths, dry, &out)
}

func putGroupInto(
	f *ClientFlags, component, name, role string,
	members []string, params map[string]any, paths map[string][]string,
	dry bool, out *setParamsResp,
) error {
	cli, err := f.client()
	if err != nil {
		return validationErr(err)
	}
	return cli.Do("PUT", groupPath(component, name), map[string]any{
		"role": role, "members": members,
		"params": params, "paths": paths, "dryRun": dry,
	}, out)
}

func findGroupByName(f *ClientFlags, component, role, name string) (groupDetail, error) {
	groups, err := fetchGroups(f, component, role)
	if err != nil {
		return groupDetail{}, err
	}
	for _, g := range groups {
		if g.Name == name {
			return g, nil
		}
	}
	return groupDetail{}, validationErr(fmt.Errorf(
		"no config group %q under role %s\n  existing groups: %s",
		name, role, strings.Join(groupNames(groups), ", ")))
}

func groupNames(groups []groupDetail) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, g.Name)
	}
	sort.Strings(out)
	return out
}

// parsePathBindings 解析 `--path <路径名>=<卷1>,<卷2>`。
func parsePathBindings(specs []string) (map[string][]string, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := map[string][]string{}
	for _, s := range specs {
		name, vols, ok := strings.Cut(s, "=")
		if !ok || name == "" || vols == "" {
			return nil, fmt.Errorf(
				"--path syntax is <path-name>=<vol1>,<vol2>, got %q", s)
		}
		var list []string
		for _, v := range strings.Split(vols, ",") {
			if v = strings.TrimSpace(v); v != "" {
				list = append(list, v)
			}
		}
		out[name] = list
	}
	return out, nil
}

func sortedAny(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedPaths(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
