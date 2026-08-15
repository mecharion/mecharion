package ctlcmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	rootcli "github.com/mecharion/mecharion/internal/cli"
	"github.com/mecharion/mecharion/internal/mechd"
)

// ClientFlags 是全部需要连 mechd 的命令共用的 flag。
type ClientFlags struct {
	Server   string
	Token    string
	CAFile   string
	Insecure bool
	Site     string
	// Local 为 true 时不连 mechd，改连本机 mechlet 的只读诊断 socket
	// （ADR-0026、10-cli §1.5）。
	Local       bool
	LocalSocket string

	// Global 是根命令共享的全局标志，含唯一一份 --output。
	//
	// **这里不再自己持有一份 Output**：此前每个名词命令的 ClientFlags
	// 都各自注册一次 `-o/--output`，而 cobra 解析同名 flag 时按「离目标
	// 命令最近的祖先」取值——名词命令自己的定义离叶子命令更近，永远
	// 遮蔽掉根命令那份（含它的取值校验）。结果是两套独立的 `-o`：
	// 根命令的校验对任何真实子命令调用都是死代码，`table`/`yaml` 传给
	// 子命令时被子命令那份「只认识 json，其余一律当 text」的逻辑吃掉，
	// 不报错也不生效。改成子命令都读同一份根 flag，物理上只有一处
	// 能注册它，也就不可能再分叉。
	Global *rootcli.GlobalFlags
}

// Bind 把 flag 挂到命令上。
func (f *ClientFlags) Bind(cmd *cobra.Command) {
	p := cmd.PersistentFlags()
	p.StringVar(&f.Server, "server", "", "mechd address, defaults to "+DefaultServer)
	p.StringVar(&f.Token, "token", "", "Auth token, defaults to reading "+DefaultTokenFile)
	p.StringVar(&f.CAFile, "ca-file", "", "CA certificate, defaults to reading "+DefaultCAFile)
	p.BoolVar(&f.Insecure, "insecure-skip-verify", false,
		"Skip certificate verification (troubleshooting only — it makes MITM attacks undetectable)")
	p.StringVar(&f.Site, "site", "", "Site name")
	p.BoolVar(&f.Local, "local", false,
		"Connect directly to the local mechlet's read-only diagnostic view, bypassing mechd (only when mechd is unreachable)")
	p.StringVar(&f.LocalSocket, "local-socket", "",
		"Socket path for --local, defaults to "+DefaultLocalSocket)
}

// output 返回当前生效的输出格式，读根命令的唯一一份 --output。
// 取值已经在根命令的 PersistentPreRunE 里校验过
// （table|json|yaml），这里不用再判一次非法值。
func (f *ClientFlags) output() string {
	if f.Global != nil && f.Global.Output != "" {
		return f.Global.Output
	}
	return rootcli.OutputTable
}

// writeYAML 是 writeJSON 的 YAML 版本：-o json 和 -o yaml 都真的实现了。
func writeYAML(w io.Writer, v any) error {
	enc := yaml.NewEncoder(w)
	defer enc.Close()
	return enc.Encode(v)
}

func (f *ClientFlags) client() (*Client, error) {
	if f.Insecure {
		fmt.Fprintln(os.Stderr,
			"Warning: --insecure-skip-verify is enabled, certificates are not verified")
	}
	return NewClient(ClientConfig{
		Server: f.Server, Token: f.Token, CAFile: f.CAFile,
		Insecure: f.Insecure, Site: f.Site,
		Local: f.Local, LocalSocket: f.LocalSocket,
	})
}

// NewComponentCmd 构造 `mechctl component`。
//
// **flags 由调用方传入、只在根命令 Bind 过一次**（与 `-o/--output`
// 统一成一份的理由相同）：`--local`/`--server`/`--site` 等此前各名词
// 命令各自 Bind 一份，cobra 在还没找到子命令之前无法识别 `mechctl --local
// component ...` 里的 `--local`，会把 `component` 误吞成它的值——这正是
// 文档 10-cli §1.5 示例失败的根因。
func NewComponentCmd(flags *ClientFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "component",
		Short: "Manage Components",
	}

	cmd.AddCommand(
		newDeployCmd(flags),
		newStatusCmd(flags),
		newDiffCmd(flags),
		newAckDriftCmd(flags),
		newRunStateCmd(flags, "stopped"),
		newRunStateCmd(flags, "running"),
		newSetDriftPolicyCmd(flags),
		newSetRolloutCmd(flags),
		newUpgradeCmd(flags),
		newRollbackCmd(flags),
		newListCmd(flags),
		newRemoveCmd(flags),
		newRestartCmd(flags),
		NewRenderCmd(), // 离线，不连 mechd
	)
	return cmd
}

// ── deploy ──────────────────────────────────────────────────────────────

func newDeployCmd(f *ClientFlags) *cobra.Command {
	var (
		component   string
		profile     string
		nodes       []string
		roles       []string
		sets        []string
		setFiles    []string
		setStdin    []string
		requires    []string
		update      bool
		allowRemove bool
		dryRun      bool
	)

	cmd := &cobra.Command{
		Use:   "deploy <pack>",
		Short: "Deploy a Component",
		Long: `Deploy a Pack to the given nodes.

The argument is the **Pack name**; the Component name defaults to it. The
same Pack can be deployed more than once in a Site (pg-main and pg-report),
in which case use -c to rename it explicitly.

  --set k=v          plain parameter
  --set-file k=@path sensitive parameter: read from a file, kept out of
                      shell history and ps output`,
		Example: `  mechctl component deploy go-webapp --nodes n1
  mechctl component deploy zookeeper -c zk-main --profile ensemble --role server=n1,n2,n3
  mechctl component deploy minio --nodes n1,n2,n3,n4 --set-file root_password=@/root/pw`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			roleMap, err := parseRoles(roles)
			if err != nil {
				return validationErr(err)
			}
			// **在发出请求之前**拦下用 --set 传的 secret（10-cli §4.3）。
			// 这里问的是 Pack 的声明——这个组件还不存在。
			if err := rejectPlaintextSecrets(c, f, args[0], "", profile, sets); err != nil {
				return err
			}
			setMap, err := parseSets(sets, setFiles)
			if err != nil {
				return validationErr(err)
			}
			piped, err := readStdinSecrets(c, setStdin)
			if err != nil {
				return err
			}
			for k, v := range piped {
				setMap[k] = v
			}

			body := mechd.DeployBody{
				Pack: args[0], Component: component, Profile: profile,
				Nodes: nodes, Roles: roleMap, Set: setMap,
				Require: mustPairs(requires),
				Update:  update, AllowRemove: allowRemove, DryRun: dryRun,
			}
			var out struct {
				Component string            `json:"component"`
				Instances []string          `json:"instances"`
				Digests   map[string]string `json:"digests"`
				Warnings  []string          `json:"warnings"`
				DryRun    bool              `json:"dryRun"`
			}
			if err := cli.Do("POST", mechd.APIPrefix+"/components", body, &out); err != nil {
				return err
			}

			w := c.OutOrStdout()
			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(w, out)
			case rootcli.OutputYAML:
				return writeYAML(w, out)
			}
			verb := "Deployed"
			if out.DryRun {
				verb = "Will deploy (--dry-run, nothing changed)"
			}
			fmt.Fprintf(w, "%s %s, %d instance(s) total\n", verb, out.Component, len(out.Instances))
			for _, k := range out.Instances {
				fmt.Fprintf(w, "  %-24s %s\n", k, short(out.Digests[k]))
			}
			printWarnings(c.ErrOrStderr(), out.Warnings)
			return nil
		},
	}

	fl := cmd.Flags()
	fl.StringVarP(&component, "component", "c", "", "Component name, defaults to the Pack name")
	fl.StringVar(&profile, "profile", "", "Deployment profile")
	fl.StringSliceVar(&nodes, "nodes", nil, "Node list (all required roles go on these nodes)")
	fl.StringArrayVar(&roles, "role", nil, "Role-to-node mapping, e.g. server=n1,n2,n3")
	fl.StringArrayVar(&sets, "set", nil, "Parameter override, e.g. port=9090")
	fl.StringArrayVar(&setFiles, "set-file", nil, "Read a parameter value from a file, e.g. password=@/root/pw")
	fl.StringArrayVar(&setStdin, "set-stdin", nil, setStdinUsage)
	fl.StringArrayVar(&requires, "require", nil, "Explicitly bind a dependency, e.g. zookeeper=zk-main")
	fl.BoolVar(&update, "update", false, "Allow changes to an existing Component")
	fl.BoolVar(&allowRemove, "allow-remove", false, "Allow scale-down (uninstalls instances)")
	fl.BoolVar(&dryRun, "dry-run", false, "Only resolve, don't persist or dispatch")
	return cmd
}

// ── status ──────────────────────────────────────────────────────────────

func newStatusCmd(f *ClientFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "status [name]",
		Short: "Show a Component's status (with --local, show all local instances)",
		// --local 看的是「这台机器上有什么」，不是某一个 Component 跨节点
		// 的状态——本机通常没几个组件，直接列全部比再要求一个名字更直接，
		// 而且 mechlet 本来就不知道 Site 里其它节点在跑什么，给了名字也
		// 无法像 mechd 那样跨节点核对。
		Args: func(c *cobra.Command, args []string) error {
			if f.Local {
				return cobra.NoArgs(c, args)
			}
			return cobra.ExactArgs(1)(c, args)
		},
		RunE: func(c *cobra.Command, args []string) error {
			if f.Local {
				return runLocalStatus(c, f)
			}
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			var st mechd.StatusView
			if err := cli.Do("GET",
				mechd.APIPrefix+"/components/"+seg(args[0])+"/status", nil, &st); err != nil {
				return err
			}

			w := c.OutOrStdout()
			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(w, st)
			case rootcli.OutputYAML:
				return writeYAML(w, st)
			}
			fmt.Fprintf(w, "%s  (%s %s", st.Component, st.Pack, st.Version)
			if st.Profile != "" {
				fmt.Fprintf(w, ", profile %s", st.Profile)
			}
			fmt.Fprintf(w, ")\n")
			if st.Converged {
				fmt.Fprintln(w, "Converged")
			} else {
				fmt.Fprintln(w, "Not converged")
			}
			fmt.Fprintln(w)

			fmt.Fprintf(w, "%-10s %-10s %-4s %-10s %-10s %s\n",
				"ROLE", "NODE", "ORD", "WORKLOAD", "HEALTH", "CONVERGED")
			for _, in := range st.Instances {
				mark := "No"
				if in.Converged {
					mark = "Yes"
				} else if in.PendingVersion != "" {
					// **「在排队」与「出了问题」要分开**：两者在这一列
					// 里都是「否」，而一个只要等，一个要人去看。
					mark = "Queued (" + in.PendingVersion + ")"
				}
				fmt.Fprintf(w, "%-10s %-10s %-4d %-10s %-10s %s\n",
					in.Role, in.Node, in.Ordinal,
					orDash(in.Workload), orDash(in.Health), mark)
				// **重启次数要显示出来。** 一个每分钟崩一次又被拉起的
				// 服务，在「running / healthy」这两列里与健康的没有区别
				// ——这正是滚动升级的健康门禁要看重启计数的同一条理由。
				if in.Restarts > 0 {
					fmt.Fprintf(w, "           Restarted %d time(s)\n", in.Restarts)
				}
				// 「工作负载被拉起来过」这件事必须显示。那一轮资源全都没变，
				// 结果看起来只是 changed——不说清楚的话，一个每分钟崩一次
				// 又被拉起的服务在这张表里与健康的没有区别。
				if a := workloadActionText(in.WorkloadAction); a != "" {
					fmt.Fprintf(w, "           %s\n", a)
				}
				for _, d := range in.Drift {
					// **策略要和资源写在一起**：用户看到漂移的第一个问题是
					// 「它会不会被改回去」，而答案取决于 driftPolicy（含站点覆盖）。
					// 不说的话他得去翻 Pack 源码，再翻一遍站点配置。
					fmt.Fprintf(w, "           Drift: %s (%s)\n",
						d.Resource, describeDriftPolicy(d.Policy))
					if d.Detail != "" {
						fmt.Fprintf(w, "                 %s\n", d.Detail)
					}
				}
				if len(in.Suppressed) > 0 {
					fmt.Fprintf(w, "           Suppressed: %s\n",
						strings.Join(describeSuppressed(in.Suppressed), ", "))
				}
				if in.Got != "" && in.Got != in.Want {
					fmt.Fprintf(w, "           want %s, got %s\n",
						short(in.Want), short(in.Got))
				}
			}
			printWarnings(c.ErrOrStderr(), st.Warnings)
			return nil
		},
	}
}

func describeSuppressed(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			out = append(out, "(entire instance)")
			continue
		}
		out = append(out, s)
	}
	return out
}

// ── diff ────────────────────────────────────────────────────────────────

func newDiffCmd(f *ClientFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "diff <name>",
		Short: "Compare desired state with actual state",
		Long: `Run the full resolution pipeline but **don't persist or dispatch**.

This is how "see what would happen" is implemented: not a separate preview
code path, but the same pipeline minus its last two steps — two separate
implementations would eventually drift apart, and an inconsistent preview
is worse than no preview at all.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			var d mechd.DiffView
			if err := cli.Do("GET",
				mechd.APIPrefix+"/components/"+seg(args[0])+"/diff", nil, &d); err != nil {
				return err
			}

			w := c.OutOrStdout()
			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(w, d)
			case rootcli.OutputYAML:
				return writeYAML(w, d)
			}
			if !d.Changed {
				fmt.Fprintf(w, "%s has no pending changes\n", d.Component)
			}
			for _, e := range d.Entries {
				switch e.Change {
				case "none":
					continue
				case "drift":
					fmt.Fprintf(w, "  drift   %s@%s  %s\n",
						e.Role, e.Node, strings.Join(e.Drift, ", "))
				default:
					fmt.Fprintf(w, "  %-6s  %s@%s  %s → %s\n",
						e.Change, e.Role, e.Node, short(e.Got), short(e.Want))
				}
			}
			printWarnings(c.ErrOrStderr(), d.Warnings)
			return nil
		},
	}
}

// ── ack-drift ───────────────────────────────────────────────────────────

func newAckDriftCmd(f *ClientFlags) *cobra.Command {
	var (
		role     string
		node     string
		resource string
		duration string
		reason   string
	)

	cmd := &cobra.Command{
		Use:   "ack-drift <name>",
		Short: "Acknowledge a temporary change, silencing its alert for now",
		Long: `Give a "temporary change" a name.

An operator fixing something at 3am changes a value — until now, that value
either gets reported as broken forever, or requires a full formal change.
This command's suppression **has an expiry** (alerting resumes automatically,
it never silently becomes permanent), **requires a reason** (goes into the
audit log), and **still detects** the drift (it just doesn't alert; status
keeps showing it as "Suppressed").

Without --resource, the whole instance is suppressed — for maintenance
windows on the whole machine.`,
		Example: `  mechctl component ack-drift web --resource template:app.yaml \
      --duration 4h --reason "adjusted log level during a 3am incident, pending change ticket #1234"`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			var out struct {
				Suppressed int `json:"suppressed"`
			}
			body := mechd.AckDriftBody{
				Role: role, Node: node, Resource: resource,
				Duration: duration, Reason: reason,
			}
			if err := cli.Do("POST",
				mechd.APIPrefix+"/components/"+seg(args[0])+"/ack-drift", body, &out); err != nil {
				return err
			}

			d, _ := time.ParseDuration(duration)
			fmt.Fprintf(c.OutOrStdout(),
				"Suppressed drift alerts for %d instance(s), resumes automatically after %s\n",
				out.Suppressed, d)
			return nil
		},
	}

	fl := cmd.Flags()
	fl.StringVar(&role, "role", "", "Only apply to this role")
	fl.StringVar(&node, "node", "", "Only apply to this node")
	fl.StringVar(&resource, "resource", "", "Only suppress this resource, defaults to suppressing the entire instance")
	fl.StringVar(&duration, "duration", "", "Suppression duration, e.g. 4h (required)")
	fl.StringVar(&reason, "reason", "", "Reason, goes into the audit log (required)")
	_ = cmd.MarkFlagRequired("duration")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

// ── list ────────────────────────────────────────────────────────────────

func newListCmd(f *ClientFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List Components",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			var out struct {
				Components []mechd.ComponentView `json:"components"`
			}
			if err := cli.Do("GET", mechd.APIPrefix+"/components", nil, &out); err != nil {
				return err
			}
			w := c.OutOrStdout()
			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(w, out)
			case rootcli.OutputYAML:
				return writeYAML(w, out)
			}
			if len(out.Components) == 0 {
				fmt.Fprintln(w, "(no Components)")
				return nil
			}
			// **「正在移除」必须在列表里就看得见。**
			//
			// 它不接受任何其它写操作，而若与正常组件长得一样，用户会先
			// 敲一条 config set，撞上「不接受其它写操作」之后才知道发生
			// 了什么。UI 那边有徽标，命令行这边一度漏了——是验收表第 7
			// 条的真机测试把它抓出来的。
			fmt.Fprintf(w, "%-16s %-16s %-10s %-10s %-6s %s\n",
				"NAME", "PACK", "VERSION", "PROFILE", "INSTANCES", "STATE")
			for _, c := range out.Components {
				state := "-"
				if c.Removing {
					state = "Removing"
				}
				fmt.Fprintf(w, "%-16s %-16s %-10s %-10s %-6d %s\n",
					c.Name, c.Pack, c.Version, orDash(c.Profile), c.Instances, state)
			}
			return nil
		},
	}
}

// ── 解析辅助 ────────────────────────────────────────────────────────────

// parseRoles 解析 --role server=n1,n2,n3。
func parseRoles(in []string) (map[string][]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := map[string][]string{}
	for _, s := range in {
		role, list, ok := strings.Cut(s, "=")
		if !ok || role == "" || list == "" {
			return nil, fmt.Errorf("--role syntax is <role>=<node1,node2>, got %q", s)
		}
		var nodes []string
		for _, n := range strings.Split(list, ",") {
			if n = strings.TrimSpace(n); n != "" {
				nodes = append(nodes, n)
			}
		}
		out[role] = nodes
	}
	return out, nil
}

// parseSets 解析 --set 与 --set-file。
//
// **敏感参数必须走 --set-file**：命令行会出现在同机任何用户的 ps 输出里，
// 也会进 shell 历史（spec §7.7）。这里不做类型判断（那要先拿到 Pack），
// 但给了一条不经过命令行的路。
func parseSets(sets, files []string) (map[string]any, error) {
	out := map[string]any{}
	for _, s := range sets {
		k, v, ok := strings.Cut(s, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--set syntax is <param>=<value>, got %q", s)
		}
		out[k] = coerceScalar(v)
	}
	for _, s := range files {
		k, path, ok := strings.Cut(s, "=")
		if !ok || k == "" || path == "" {
			return nil, fmt.Errorf("--set-file syntax is <param>=@<path>, got %q", s)
		}
		path = strings.TrimPrefix(path, "@")
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		// 去掉结尾换行：`echo secret > pw` 是最常见的造文件方式，
		// 而那个换行几乎从来不是口令的一部分
		out[k] = strings.TrimRight(string(b), "\r\n")
	}
	return out, nil
}

// coerceScalar 把命令行上的字符串还原成 JSON 里合适的类型。
//
// `--set port=9090` 应当是数字而不是 "9090"：参数的类型校验按声明的
// type 做，一个字符串会在 port 上直接报错。
func coerceScalar(s string) any {
	switch s {
	case "true":
		return true
	case "false":
		return false
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

func mustPairs(in []string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, s := range in {
		if k, v, ok := strings.Cut(s, "="); ok {
			out[k] = v
		}
	}
	return out
}

// ── 输出辅助 ────────────────────────────────────────────────────────────

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printWarnings(w io.Writer, warnings []string) {
	if len(warnings) == 0 {
		return
	}
	sorted := append([]string(nil), warnings...)
	sort.Strings(sorted)
	fmt.Fprintf(w, "\nWarnings (%d):\n", len(sorted))
	for _, s := range sorted {
		fmt.Fprintf(w, "  - %s\n", s)
	}
}

func short(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	if d == "" {
		return "-"
	}
	return d
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ── stop / start ────────────────────────────────────────────────────────

// newRunStateCmd 构造 `component stop` 与 `component start`。
//
// 两条命令改的是**期望**，不是执行一次动作。区别在下一个调和周期就能看到：
// 一次性的 `systemctl stop` 会被调和器当成漂移拉起来，而 `component stop`
// 之后它会一直停着——**包括有人手工把它启动起来时，也会被停回去**
// （20-continuous-reconcile §2.2）。
func newRunStateCmd(f *ClientFlags, state string) *cobra.Command {
	var (
		role string
		node string
	)

	verb, path, done := "Stop", "stop", "Stopped"
	long := `Change the component's desired run state to stopped.

This isn't "run stop once" — it **changes the desired state**: the
reconciler will keep enforcing it from now on. Use ` + "`mechctl component start`" + `
to restore it once the maintenance window is over — a manual
systemctl start on the machine isn't enough, the next reconcile will stop
it again.`
	if state == "running" {
		verb, path, done = "Start", "start", "Started"
		long = `Change the component's desired run state back to running.

The reconciler will bring up any instance that isn't running, and keep it
that way.`
	}

	cmd := &cobra.Command{
		Use:   path + " <name>",
		Short: verb + " a component (change desired run state)",
		Long:  long,
		Example: `  mechctl component ` + path + ` web
  mechctl component ` + path + ` web --node node-3   # target a single machine`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			var out struct {
				Instances int `json:"instances"`
			}
			body := mechd.RunStateBody{Role: role, Node: node}
			if err := cli.Do("POST",
				mechd.APIPrefix+"/components/"+seg(args[0])+"/"+path, body, &out); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "%s %d instance(s) of %s\n",
				done, out.Instances, args[0])
			return nil
		},
	}

	fl := cmd.Flags()
	fl.StringVar(&role, "role", "", "Only apply to this role")
	fl.StringVar(&node, "node", "", "Only apply to this node")
	return cmd
}

// ── set-drift-policy ────────────────────────────────────────────────────

// newSetDriftPolicyCmd 构造 `component set-drift-policy`。
//
// `driftPolicy` 写在 Pack 里，等于**Pack 作者决定了运维现场的临时修改能
// 不能活下来**——这个权责关系是反的，因此站点可以放松它
// （06-state-and-drift §4.2）。
func newSetDriftPolicyCmd(f *ClientFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set-drift-policy <name> <report|ignore|none>",
		Short: "Relax the drift policy declared by the Pack",
		Long: `Relax a Component's drift policy to the given level.

**Only relaxing is allowed, never tightening.** reconcile is the strictest
level; an override in that direction would only ever tighten it, so it's
rejected — a Pack author marking something report usually means that file
is meant to be editable, and a site policy has no reason to be stricter
than that.

  report   report only, don't revert
  ignore   don't compare
  none     clear the override, fall back to what the Pack declares

Resources already looser than the override are unaffected: if the Pack
marks a file ignore (because the application rewrites it itself), a report
override won't pull it back into alerting.`,
		Example: `  mechctl component set-drift-policy pg-main report
  mechctl component set-drift-policy pg-main none`,
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			policy := args[1]
			if policy == "none" {
				policy = ""
			}
			var out struct{}
			if err := cli.Do("POST",
				mechd.APIPrefix+"/components/"+seg(args[0])+"/drift-policy",
				mechd.DriftPolicyBody{Policy: policy}, &out); err != nil {
				return err
			}
			if policy == "" {
				fmt.Fprintf(c.OutOrStdout(), "Cleared the drift-policy override for %s\n", args[0])
			} else {
				fmt.Fprintf(c.OutOrStdout(), "Relaxed the drift policy for %s to %s\n", args[0], policy)
			}
			return nil
		},
	}
	return cmd
}

// ── set-rollout ─────────────────────────────────────────────────────────

func newSetRolloutCmd(f *ClientFlags) *cobra.Command {
	var maxUnavailable, canary int
	cmd := &cobra.Command{
		Use:   "set-rollout <name>",
		Short: "Tune rolling-upgrade concurrency and canary batch size",
		Long: `Set how a Component's next change will be batched.

  --max-unavailable  how many instances to touch per batch (default 1)
  --canary           how many to touch in the first batch (default 1; 0
                      disables canary)

**Only affects the next change.** Batches are computed once, up front, when
a change starts, and persisted; turning these two knobs doesn't recompute
a change already in progress — recomputing mid-flight would change the
total batch count while it's running, and that's exactly what an operator
uses to judge how much longer it'll take.

**Roles that declare quorum aren't controlled by this**: their concurrency
is forced down to (N-1)/2. Quorum semantics are only known to the Pack
author — a --max-unavailable 2 shouldn't be able to cost a 3-node ZooKeeper
its majority.

With no arguments, just shows the current value.`,
		Example: `  mechctl component set-rollout kafka --max-unavailable 2
  mechctl component set-rollout kafka --canary 0
  mechctl component set-rollout kafka`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			path := mechd.APIPrefix + "/components/" + seg(args[0]) + "/rollout-policy"

			var v mechd.RolloutPolicyView
			// 一个参数都没给就是「读」——那是运维忘了当前值时最想敲的
			if !c.Flags().Changed("max-unavailable") && !c.Flags().Changed("canary") {
				if err := cli.Do("GET", path, nil, &v); err != nil {
					return err
				}
			} else {
				// **指针**：0 是 canary 的合法值（关掉金丝雀），
				// 用 0 当「没传」会让人永远关不掉它。
				body := struct {
					MaxUnavailable *int `json:"maxUnavailable,omitempty"`
					Canary         *int `json:"canary,omitempty"`
				}{}
				if c.Flags().Changed("max-unavailable") {
					body.MaxUnavailable = &maxUnavailable
				}
				if c.Flags().Changed("canary") {
					body.Canary = &canary
				}
				if err := cli.Do("POST", path, body, &v); err != nil {
					return err
				}
			}
			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(c.OutOrStdout(), v)
			case rootcli.OutputYAML:
				return writeYAML(c.OutOrStdout(), v)
			}
			w := c.OutOrStdout()
			fmt.Fprintf(w, "%s  %d per batch", v.Component, v.MaxUnavailable)
			if v.Canary > 0 {
				fmt.Fprintf(w, ", %d in the first batch", v.Canary)
			} else {
				fmt.Fprint(w, ", no canary")
			}
			fmt.Fprintln(w)
			// **仲裁角色不受这个旋钮控制，必须说出来。**
			//
			// 旋钮在 Component 上而 quorum 在角色上：用户设了 2 却看到
			// 每批只动 1 台，不说的话他会以为设置没生效，然后去翻日志。
			if len(v.QuorumRoles) > 0 {
				fmt.Fprintf(w, "Quorum roles  %s — capped at (N-1)/2, not controlled by the concurrency above\n",
					strings.Join(v.QuorumRoles, ", "))
			}
			if v.Note != "" {
				fmt.Fprintf(w, "Note          %s\n", v.Note)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&maxUnavailable, "max-unavailable", 1, "How many instances to touch per batch")
	cmd.Flags().IntVar(&canary, "canary", 1, "How many to touch in the first batch (0 disables canary)")
	return cmd
}

// describeDriftPolicy 把策略名换成一句人话。
//
// 直接印 "report" 要求读者记得三个词各是什么意思。**状态输出是给现场的人
// 看的，那时他多半正忙**——一句「不会自动改回」比一个术语有用。
func describeDriftPolicy(p string) string {
	switch p {
	case "report":
		return "report only, won't be reverted automatically"
	case "reconcile":
		return "reverted automatically"
	case "ignore":
		return "not compared"
	case "":
		return "unknown policy"
	}
	return p
}

// workloadActionText 说明上一轮对工作负载做了什么。
func workloadActionText(a string) string {
	switch a {
	case "restored":
		return "Workload wasn't running and has been brought back up — if this keeps happening, check why the service itself is exiting"
	case "stopped":
		return "Workload was running but the desired state is stopped, so it has been stopped again"
	}
	return ""
}

// sinceText 把一个 RFC3339 时刻说成「多久以前」。
//
// 绝对时间要求读者心算，而现场看状态的人正忙。**「3 分钟前」与「昨天」
// 是完全不同的两件事**，而那个差别恰好是判断「还在反复发生吗」的关键。
func sinceText(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return "unknown time"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minute(s) ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hour(s) ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d day(s) ago", int(d.Hours()/24))
}

// ── upgrade / rollback ──────────────────────────────────────────────────

// newUpgradeCmd 构造 `component upgrade`。
func newUpgradeCmd(f *ClientFlags) *cobra.Command {
	var (
		version string
		force   bool
		dryRun  bool
	)
	cmd := &cobra.Command{
		Use:   "upgrade <name>",
		Short: "Move a Component to a different Pack version",
		Long: `Change the Pack version, through the exact same resolution pipeline as deploy.

The node side materializes a new generation, switches to it atomically, and
runs a health check. **If the new version fails to start or fails the
health check, it's automatically rolled back to the last known-good
version** — the service isn't lost.

Before upgrading, the engine checks whether the target version's
upgradePolicy.compatible includes the current version. Crossing a major
version (e.g. PostgreSQL 16 → 17) usually needs a data migration, in which
case a new Component should be created instead.`,
		Example: `  mechctl component upgrade web --version 1.3.0
  mechctl component upgrade web            # upgrade to the latest version available locally
  mechctl component upgrade web --version 1.3.0 --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			var out mechd.VersionChangeResponse
			body := mechd.UpgradeBody{Version: version, Force: force, DryRun: dryRun}
			if err := cli.Do("POST",
				mechd.APIPrefix+"/components/"+seg(args[0])+"/upgrade", body, &out); err != nil {
				return err
			}
			return printVersionChange(c, f, out, "Upgraded", "Will upgrade")
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&version, "version", "", "Target version, defaults to the latest version available locally")
	fl.BoolVar(&force, "force", false,
		"Skip the upgradePolicy.compatible check (goes into the audit log)")
	fl.BoolVar(&dryRun, "dry-run", false, "Only resolve, don't persist or dispatch")
	return cmd
}

// newRollbackCmd 构造 `component rollback`。
func newRollbackCmd(f *ClientFlags) *cobra.Command {
	var (
		version string
		dryRun  bool
	)
	cmd := &cobra.Command{
		Use:   "rollback <name>",
		Short: "Move a Component back to an older version",
		Long: `Go back to an older version, defaulting to the previous one.

**Usually a matter of seconds**: resolution is a pure function, so going
back to an old version reproduces the exact same digest it had back then —
the node hits an already-retained generation and only needs to switch a
symlink, no re-extraction needed. Versions beyond
reconcile.retainGenerations get re-materialized, which is slower.

Rollback **does not skip** the upgradePolicy check: going from 17 back to
16 faces the same data-directory problem as going from 16 up to 17 — the
direction doesn't change that.`,
		Example: `  mechctl component rollback web
  mechctl component rollback web --to-version 1.2.0`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			var out mechd.VersionChangeResponse
			body := mechd.RollbackBody{Version: version, DryRun: dryRun}
			if err := cli.Do("POST",
				mechd.APIPrefix+"/components/"+seg(args[0])+"/rollback", body, &out); err != nil {
				return err
			}
			return printVersionChange(c, f, out, "Rolled back", "Will roll back")
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&version, "to-version", "", "Target version, defaults to the previous version")
	fl.BoolVar(&dryRun, "dry-run", false, "Only resolve, don't persist or dispatch")
	return cmd
}

func printVersionChange(
	c *cobra.Command, f *ClientFlags, out mechd.VersionChangeResponse, doneVerb, willVerb string,
) error {
	w := c.OutOrStdout()
	switch f.output() {
	case rootcli.OutputJSON:
		return writeJSON(w, out)
	case rootcli.OutputYAML:
		return writeYAML(w, out)
	}
	verb := doneVerb
	if out.DryRun {
		verb = willVerb + " (--dry-run, nothing changed)"
	}
	fmt.Fprintf(w, "%s %s, %d instance(s) total\n", verb, out.Component, len(out.Instances))
	for _, k := range out.Instances {
		fmt.Fprintf(w, "  %-24s %s\n", k, short(out.Digests[k]))
	}
	if !out.DryRun {
		fmt.Fprintln(w,
			"\nThe node will materialize and switch to a new generation; use component status to check convergence.")
	}
	printWarnings(c.ErrOrStderr(), out.Warnings)
	return nil
}
