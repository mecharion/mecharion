package ctlcmd

import (
	"fmt"
	"sort"
	"strings"

	rootcli "github.com/mecharion/mecharion/internal/cli"
	"github.com/mecharion/mecharion/internal/mechd"
	"github.com/spf13/cobra"
)

// NewConfigCmd 构造 `mechctl config`（10-cli §4.4）。
//
// **为什么改配置不用 `component deploy --update --set`**：`deploy` 会
// 重算放置，因而要求指定节点。于是一次「把日志级别改成 debug」被迫重述
// 整个拓扑，而重述拓扑时少写一个节点名就是一次缩容——`--allow-remove`
// 拦得住，但那道闸门是给「确实要缩容」准备的，让日常改配置反复去撞它，
// 迟早有人习惯性地把它加上（23-web-ui §4.3 ①）。
//
// `deploy --update` 仍然保留：它对「同时改拓扑与参数」是对的。
//
// 组那一半（`--node` 自动建组、`config group *`、`config diff`）**不在
// 这一步**——它们全都要 ConfigGroup，而 ConfigGroup 现在还没有任何创建
// 入口。整条链路见 23-web-ui 第 7 步。
func NewConfigCmd(flags *ClientFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "View and change a Component's parameters",
	}
	cmd.AddCommand(
		newConfigGetCmd(flags),
		newConfigSetCmd(flags),
		newConfigExplainCmd(flags),
		newConfigGroupCmd(flags),
	)
	return cmd
}

// formResp 是 GET /components/{name}/params 的响应。
type formResp struct {
	Component string      `json:"component"`
	Pack      string      `json:"pack"`
	Version   string      `json:"version"`
	Profile   string      `json:"profile"`
	Roles     []string    `json:"roles"`
	Role      string      `json:"role"`
	Params    []formParam `json:"params"`
	Warnings  []string    `json:"warnings"`
}

type formParam struct {
	Name            string `json:"name"`
	Type            string `json:"type"`
	Description     string `json:"description"`
	Unit            string `json:"unit"`
	Group           string `json:"group"`
	Advanced        bool   `json:"advanced"`
	Required        bool   `json:"required"`
	Default         any    `json:"default"`
	Value           any    `json:"value"`
	Source          string `json:"source"`
	Set             bool   `json:"set"`
	ReadOnly        bool   `json:"readOnly"`
	Immutable       bool   `json:"immutable"`
	Sensitive       bool   `json:"sensitive"`
	RestartRequired bool   `json:"restartRequired"`
	ReloadRequired  bool   `json:"reloadRequired"`
	Pending         bool   `json:"pending"`
}

// setParamsResp 是 PATCH /components/{name}/params 的响应。
type setParamsResp struct {
	Component string        `json:"component"`
	Changed   []paramChange `json:"changed"`
	Effect    string        `json:"effect"`
	Restarted []string      `json:"restarted"`
	Reloaded  []string      `json:"reloaded"`
	Warnings  []string      `json:"warnings"`
	DryRun    bool          `json:"dryRun"`
	// CreatedGroup 非空表示服务端顺手建了一个只含一台机器的配置组。
	CreatedGroup string `json:"createdGroup"`
}

type paramChange struct {
	Role string `json:"role"`
	Node string `json:"node"`
	From string `json:"from"`
	To   string `json:"to"`
}

// ── get ─────────────────────────────────────────────────────────────────

func newConfigGetCmd(f *ClientFlags) *cobra.Command {
	var component, role string
	cmd := &cobra.Command{
		Use:   "get",
		Short: "List a Component's parameter values and their sources",
		RunE: func(c *cobra.Command, _ []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			var out formResp
			if err := cli.Do("GET", configPath(component, role), nil, &out); err != nil {
				return err
			}
			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(c.OutOrStdout(), out)
			case rootcli.OutputYAML:
				return writeYAML(c.OutOrStdout(), out)
			}
			printForm(c, out)
			return nil
		},
	}
	cmd.Flags().StringVarP(&component, "component", "c", "", "Component name (required)")
	cmd.Flags().StringVarP(&role, "role", "r", "", "Role, defaults to the first role")
	_ = cmd.MarkFlagRequired("component")
	return cmd
}

func printForm(c *cobra.Command, v formResp) {
	out := c.OutOrStdout()
	fmt.Fprintf(out, "%s  %s %s", v.Component, v.Pack, v.Version)
	if v.Profile != "" {
		fmt.Fprintf(out, "  profile %s", v.Profile)
	}
	fmt.Fprintf(out, "  role %s\n\n", v.Role)

	group := ""
	for _, p := range v.Params {
		if g := orDefault(p.Group, "General"); g != group {
			group = g
			fmt.Fprintf(out, "[%s]\n", group)
		}
		fmt.Fprintf(out, "  %-22s %-14s %-12s %s\n",
			p.Name, p.Type, sourceLabel(p.Source), valueText(p))
	}
	for _, w := range v.Warnings {
		fmt.Fprintf(out, "\nWarning: %s\n", w)
	}
}

// valueText 是一个参数在文本输出里的样子。
//
// **secret 永远不打印值**：CLI 的输出会进日志、进工单、进截图。
func valueText(p formParam) string {
	var marks []string
	if p.Immutable {
		marks = append(marks, "requires rebuild")
	}
	if p.RestartRequired {
		marks = append(marks, "restarts")
	}
	if p.ReloadRequired {
		marks = append(marks, "reloads")
	}
	if p.Advanced {
		marks = append(marks, "advanced")
	}

	var v string
	switch {
	case p.Pending:
		v = "(determined after deploy)"
	case p.Type == "secret" || p.Sensitive:
		if p.Set {
			v = "(set)"
		} else {
			v = "(not set)"
		}
	case p.Value == nil:
		v = "—"
	default:
		v = fmt.Sprint(p.Value)
	}
	if p.Unit != "" && !p.Pending {
		v += " " + p.Unit
	}
	if len(marks) > 0 {
		v += "  [" + strings.Join(marks, " ") + "]"
	}
	return v
}

func sourceLabel(s string) string {
	switch s {
	case "default":
		return "Pack default"
	case "component":
		return "component-level"
	case "role":
		return "role-level"
	case "group":
		return "group override"
	case "derived":
		return "derived"
	case "generated":
		return "engine-generated"
	case "defaultFrom":
		return "computed from facts"
	}
	return s
}

// ── set ─────────────────────────────────────────────────────────────────

func newConfigSetCmd(f *ClientFlags) *cobra.Command {
	var (
		component string
		role      string
		group     string
		node      string
		sets      []string
		setFiles  []string
		setStdin  []string
		unset     []string
		dryRun    bool
		yes       bool
	)
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Change a Component's parameters (doesn't change topology)",
		Long: `Change the parameters of an already-deployed Component.

  --set k=v          plain parameter
  --set-file k=@path sensitive parameter: read from a file, kept out of
                      shell history and ps output
  --unset k          remove the override, revert to the default

**It doesn't touch placement**: instance count and which nodes they're on
stay the same. To change topology at the same time, use ` +
			"`component deploy --update`" + `.`,
		Example: `  mechctl config set -c web --set log_level=debug
  mechctl config set -c minio --set-file root_password=@/root/pw
  mechctl config set -c web --unset log_level`,
		RunE: func(c *cobra.Command, _ []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			// **在发出请求之前**拦下用 --set 传的 secret（10-cli §4.3）
			if err := rejectPlaintextSecrets(c, f, "", component, "", sets); err != nil {
				return err
			}
			values, err := parseSets(sets, setFiles)
			if err != nil {
				return validationErr(err)
			}
			piped, err := readStdinSecrets(c, setStdin)
			if err != nil {
				return err
			}
			for k, v := range piped {
				values[k] = v
			}
			if len(values) == 0 && len(unset) == 0 {
				return validationErr(fmt.Errorf(
					"nothing to change: use --set / --set-file / --unset"))
			}

			body := map[string]any{
				"set": values, "unset": unset,
				"role": role, "group": group, "node": node,
			}

			// **先干跑一遍再问**：确认要问的是「会发生什么」，
			// 而那句话得先算出来。一个只说「确定吗」的确认没有价值。
			body["dryRun"] = true
			var preview setParamsResp
			if err := cli.Do("PATCH", configPath(component, ""), body, &preview); err != nil {
				return err
			}
			printPreview(c, preview)

			if dryRun {
				return nil
			}
			if len(preview.Changed) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "No changes, nothing was modified.")
				return nil
			}
			// 只有会重启 / reload 才需要确认：一次纯配置变更不该
			// 每次都拦一下，那会训练用户无脑按 y
			if preview.Effect != "none" {
				if err := confirmYN(c, effectPrompt(preview), yes); err != nil {
					return err
				}
			}

			body["dryRun"] = false
			var out setParamsResp
			if err := cli.Do("PATCH", configPath(component, ""), body, &out); err != nil {
				return err
			}
			if out.CreatedGroup != "" {
				// **不能静默**：用户敲的是「改这一台」，而模型里不存在无名的
				// per-node 覆盖（ADR-0021），于是凭空多出来一个对象。不说的话，
				// `config group list` 里会冒出一堆 node-* 组而没人记得来历。
				fmt.Fprintf(c.OutOrStdout(),
					"Created config group %s (member: %s)\n"+
						"  This is a group containing just one machine. If other machines should share the same config:\n"+
						"    mechctl config group move <node> --to %s -c %s -r %s\n",
					out.CreatedGroup, node, out.CreatedGroup, component, role)
			}
			fmt.Fprintf(c.OutOrStdout(), "Updated %s, spec changed for %d instance(s).\n",
				out.Component, len(out.Changed))
			return nil
		},
	}
	cmd.Flags().StringVarP(&component, "component", "c", "", "Component name (required)")
	cmd.Flags().StringVarP(&role, "role", "r", "", "Role (paired with --group / --node)")
	cmd.Flags().StringVar(&group, "group", "", "Change values for this config group")
	cmd.Flags().StringVar(&node, "node", "", "Only change this machine — automatically creates a group containing just it")
	cmd.Flags().StringArrayVar(&sets, "set", nil, "Parameter override <param>=<value>")
	cmd.Flags().StringArrayVar(&setFiles, "set-file", nil, "Read from a file <param>=@<path>")
	cmd.Flags().StringArrayVar(&setStdin, "set-stdin", nil, setStdinUsage)
	cmd.Flags().StringArrayVar(&unset, "unset", nil, "Remove the override, revert to the default")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Only show what would happen, change nothing")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation")
	_ = cmd.MarkFlagRequired("component")
	return cmd
}

func printPreview(c *cobra.Command, v setParamsResp) {
	out := c.OutOrStdout()
	if len(v.Changed) == 0 {
		fmt.Fprintln(out, "No change to the spec.")
		return
	}
	fmt.Fprintf(out, "Will affect %d instance(s):\n", len(v.Changed))
	for _, ch := range v.Changed {
		fmt.Fprintf(out, "  %s@%-16s %s → %s\n",
			ch.Role, ch.Node, short(ch.From), short(ch.To))
	}
	switch v.Effect {
	case "restart":
		fmt.Fprintf(out, "\n**will restart the service** (%s)\n", strings.Join(v.Restarted, ", "))
	case "reload":
		fmt.Fprintf(out, "\nwill trigger a reload (%s)\n", strings.Join(v.Reloaded, ", "))
	default:
		fmt.Fprintln(out, "\nNo restart or reload needed.")
	}
	for _, w := range v.Warnings {
		fmt.Fprintf(out, "Warning: %s\n", w)
	}
}

func effectPrompt(v setParamsResp) string {
	if v.Effect == "restart" {
		return fmt.Sprintf("This will restart the service on %d instance(s), continue?", len(v.Changed))
	}
	return fmt.Sprintf("This will reload the service on %d instance(s), continue?", len(v.Changed))
}

// ── explain ─────────────────────────────────────────────────────────────

func newConfigExplainCmd(f *ClientFlags) *cobra.Command {
	var component, role string
	cmd := &cobra.Command{
		Use:   "explain <param>",
		Short: "Explain why a parameter currently has this value",
		Long: `Print a parameter's value and which layer it comes from.

This is the **necessary compensation** for the extra resolution layer
ConfigGroup adds (ADR-0021): once the chain gets longer, "why is this value
what it is" stops being obvious at a glance.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			var form formResp
			if err := cli.Do("GET", configPath(component, role), nil, &form); err != nil {
				return err
			}
			for _, p := range form.Params {
				if p.Name != args[0] {
					continue
				}
				printExplain(c, form, p)
				return nil
			}
			var names []string
			for _, p := range form.Params {
				names = append(names, p.Name)
			}
			sort.Strings(names)
			return validationErr(fmt.Errorf(
				"no parameter %q under role %s\n  declared parameters: %s",
				args[0], form.Role, strings.Join(names, ", ")))
		},
	}
	cmd.Flags().StringVarP(&component, "component", "c", "", "Component name (required)")
	cmd.Flags().StringVarP(&role, "role", "r", "", "Role, defaults to the first role")
	_ = cmd.MarkFlagRequired("component")
	return cmd
}

func printExplain(c *cobra.Command, form formResp, p formParam) {
	out := c.OutOrStdout()
	fmt.Fprintf(out, "  %s\n", valueText(p))
	fmt.Fprintf(out, "  ← %s\n", sourceLabel(p.Source))

	// 把没有生效的那几层也列出来：「它没被覆盖」与「这一层不存在」
	// 是两件不同的事，而只打印生效的那层分不出来。
	fmt.Fprintln(out)
	fmt.Fprintf(out, "    Role       %s\n", form.Role)
	fmt.Fprintf(out, "    Component  %s\n", form.Component)
	if p.Type == "secret" || p.Sensitive {
		fmt.Fprintln(out, "    Pack default  (secrets have no default)")
	} else if p.Default != nil {
		fmt.Fprintf(out, "    Pack default  %v\n", p.Default)
	} else {
		fmt.Fprintln(out, "    Pack default  (not declared)")
	}
	if p.Description != "" {
		fmt.Fprintf(out, "\n  %s\n", p.Description)
	}
	if p.ReadOnly {
		fmt.Fprintln(out, "\n  This parameter doesn't accept a value.")
	}
}

// configPath 拼出表单接口的路径。
func configPath(component, role string) string {
	return mechd.APIPrefix + "/components/" + seg(component) + "/params" + query(map[string]string{"role": role})
}
