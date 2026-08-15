package ctlcmd

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	rootcli "github.com/mecharion/mecharion/internal/cli"
	"github.com/mecharion/mecharion/internal/mechd"
)

// NewNodeCmd 构造 `mechctl node`。
func NewNodeCmd(flags *ClientFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Manage nodes",
	}
	cmd.AddCommand(
		newNodeAddCmd(flags),
		newNodeTokenCmd(flags),
		newNodeBootstrapCmd(flags),
		newNodeRemoveCmd(flags),
		newNodeRevokeCmd(flags, true),
		newNodeRevokeCmd(flags, false),
		newNodeCordonCmd(flags, true),
		newNodeCordonCmd(flags, false),
		newNodeListCmd(flags),
		newNodeShowCmd(flags),
	)
	return cmd
}

// nodeList 是 GET /nodes 的应答。
type nodeList struct {
	Nodes []mechd.NodeView `json:"nodes"`
}

func fetchNodes(f *ClientFlags) ([]mechd.NodeView, error) {
	cli, err := f.client()
	if err != nil {
		return nil, validationErr(err)
	}
	var out nodeList
	if err := cli.Do("GET", mechd.APIPrefix+"/nodes", nil, &out); err != nil {
		return nil, err
	}
	return out.Nodes, nil
}

func newNodeListCmd(f *ClientFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List nodes",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			nodes, err := fetchNodes(f)
			if err != nil {
				return err
			}
			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(c.OutOrStdout(), nodeList{Nodes: nodes})
			case rootcli.OutputYAML:
				return writeYAML(c.OutOrStdout(), nodeList{Nodes: nodes})
			}

			w := tabwriter.NewWriter(c.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NODE\tADDRESS\tSTATUS\tORPHANED INSTANCES")
			for _, n := range nodes {
				orphans := "-"
				if len(n.Orphans) > 0 {
					// 只给个数，明细在 show 里。**但数字必须在列表里出现**：
					// 一个只有 `node show <name>` 才看得到的问题，等于没人会
					// 发现它——而孤儿的典型来源恰好是「某次变更漏了一台机器」
					orphans = fmt.Sprintf("%d", len(n.Orphans))
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
					n.Name, orDash(n.Address), nodeState(n), orphans)
			}
			return w.Flush()
		},
	}
}

func newNodeShowCmd(f *ClientFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show details for a node",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			nodes, err := fetchNodes(f)
			if err != nil {
				return err
			}
			var found *mechd.NodeView
			for i := range nodes {
				if nodes[i].Name == args[0] {
					found = &nodes[i]
				}
			}
			if found == nil {
				return validationErr(fmt.Errorf("no node named %q", args[0]))
			}
			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(c.OutOrStdout(), found)
			case rootcli.OutputYAML:
				return writeYAML(c.OutOrStdout(), found)
			}

			out := c.OutOrStdout()
			fmt.Fprintf(out, "Node      %s\n", found.Name)
			fmt.Fprintf(out, "Address   %s\n", orDash(found.Address))
			fmt.Fprintf(out, "Status    %s\n", orDash(found.Status))
			if len(found.Labels) > 0 {
				var kv []string
				for k, v := range found.Labels {
					kv = append(kv, k+"="+v)
				}
				fmt.Fprintf(out, "Labels    %s\n", strings.Join(kv, " "))
			}

			if len(found.Orphans) == 0 {
				return nil
			}
			// 孤儿要说清「怎么办」，否则它只是一条让人不安的信息。
			// **绝不自动删**：卸载不可逆，而这里分辨不了「mechd 少发了一条」
			// 与「用户真的删了这个组件」（20-continuous-reconcile §2.4）。
			fmt.Fprintf(out, "\nOrphaned instances (%d) — still on the machine, but not in the dispatch\n",
				len(found.Orphans))
			for _, o := range found.Orphans {
				fmt.Fprintf(out, "  %-28s first seen %s\n", o.Instance, o.FirstSeen)
			}
			fmt.Fprintln(out,
				"\n  They won't be uninstalled automatically — \"dispatch missed one\" and "+
					"\"the component was really deleted\" can't be told apart on the node side.\n"+
					"  Use mechctl component remove once you've confirmed they should go.")
			return nil
		},
	}
}

// newNodeAddCmd 构造 `mechctl node add`。
//
// **登记与加入是两件事**：这条命令只在册子上留一行「这台机器属于本 Site」。
// 那台机器真正连上来还需要一张证书——常规路径是 join token
// （`mechctl node token create`），离线路径是 `mechd ca issue`。
//
// 分开是因为授权者不同：登记是控制面的管理动作，加入需要那台机器上有人。
// 也因此 `node add` 之后节点状态是 **reserved**（对外显示仍是
// pending）——它还没出现过，也从没发过证书；这正是它能被后续 Join
// 认领的原因（22-multi-node §6.16）。掉线的机器报 offline，
// 两者刻意分开：前者该去那台机器上敲 join，后者该去查它为什么掉了
// （22-multi-node §6.13）。
func newNodeAddCmd(f *ClientFlags) *cobra.Command {
	var address string
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Register a machine in the roster (doesn't mean it has connected yet)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			body := mechd.AddNodeBody{Name: args[0], Address: address, Site: f.Site}
			var v mechd.NodeView
			if err := cli.Do("POST", mechd.APIPrefix+"/nodes", body, &v); err != nil {
				return err
			}
			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(c.OutOrStdout(), v)
			case rootcli.OutputYAML:
				return writeYAML(c.OutOrStdout(), v)
			}
			fmt.Fprintf(c.OutOrStdout(),
				"Registered node %s (status %s — it hasn't connected yet)\n"+
					"  Next: join the cluster from that machine with a join token\n", v.Name, v.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&address, "address", "", "Node address, for display and diagnostics")
	return cmd
}

// newNodeTokenCmd 构造 `mechctl node token`。
//
// join token 是**带外交付**的凭据：运维自己拿到手上，再敲进那台机器。
// 那次交付本身就是授权——这条路上没有第二道人工闸门（ADR-0034），
// 因此 TTL、使用次数、可吊销、进审计是全部的限制手段。
func newNodeTokenCmd(f *ClientFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage node join credentials",
	}
	cmd.AddCommand(
		newTokenCreateCmd(f), newTokenListCmd(f), newTokenRevokeCmd(f))
	return cmd
}

func newTokenCreateCmd(f *ClientFlags) *cobra.Command {
	var (
		node string
		ttl  time.Duration
		uses int
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Generate a join credential (the plaintext is shown only once)",
		Long: `Generate a join token.

--node binds it to a node name: this token can only issue a certificate for
that name, it can't be used to impersonate another. Without a bound name,
it's first-come-first-served, meant to pair with --uses for bulk
provisioning (cloud-init / images) — **the cost is that whoever gets hold
of it can add a machine to the cluster**, so give it a short TTL.`,
		Example: "  mechctl node token create --node store-042 --ttl 30m",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			body := mechd.CreateJoinTokenBody{
				Site: f.Site, Node: node, Uses: uses,
			}
			if ttl > 0 {
				body.TTL = ttl.String()
			}
			var v mechd.JoinTokenView
			if err := cli.Do("POST",
				mechd.APIPrefix+"/nodes/tokens", body, &v); err != nil {
				return err
			}
			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(c.OutOrStdout(), v)
			case rootcli.OutputYAML:
				return writeYAML(c.OutOrStdout(), v)
			}
			w := c.OutOrStdout()
			fmt.Fprintf(w, "\n  token: %s\n\n", v.Token)
			fmt.Fprintf(w, "  This token is shown only once. Valid until %s, usable %d time(s).\n",
				v.Expires, v.MaxUses)
			if v.Node != "" {
				fmt.Fprintf(w, "  It's bound to node name %s, and can't be used to join under another name.\n", v.Node)
			} else {
				fmt.Fprintf(w, "  It is **not bound to a node name** — whoever gets hold of it can add a machine to the cluster.\n")
			}
			if v.JoinCommand != "" {
				fmt.Fprintf(w, "\nRun on the target machine:\n\n%s\n\n", v.JoinCommand)
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&node, "node", "", "Node name to bind to; first-come-first-served if omitted")
	fl.DurationVar(&ttl, "ttl", 0, "Validity period, defaults to 30m")
	fl.IntVar(&uses, "uses", 1, "Number of uses")
	return cmd
}

func newTokenListCmd(f *ClientFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List join credentials (without plaintext)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			var out struct {
				Tokens []mechd.JoinTokenView `json:"tokens"`
			}
			if err := cli.Do("GET",
				mechd.APIPrefix+"/nodes/tokens", nil, &out); err != nil {
				return err
			}
			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(c.OutOrStdout(), out)
			case rootcli.OutputYAML:
				return writeYAML(c.OutOrStdout(), out)
			}
			w := tabwriter.NewWriter(c.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tBOUND NODE\tEXPIRES\tUSAGE\tUSABLE")
			for _, t := range out.Tokens {
				usable := "yes"
				if !t.Usable {
					usable = t.Reason
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%d/%d\t%s\n",
					t.ID, orDash(t.Node), t.Expires, t.Used, t.MaxUses, usable)
			}
			return w.Flush()
		},
	}
}

func newTokenRevokeCmd(f *ClientFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke a join credential",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			var out map[string]any
			path := mechd.APIPrefix + "/nodes/tokens/" + seg(args[0]) + "/revoke"
			if err := cli.Do("POST", path, struct{}{}, &out); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "Revoked credential %s\n", args[0])
			return nil
		},
	}
}

// newNodeRemoveCmd 构造 `mechctl node remove`。
//
// **它只改册子，不去那台机器上卸载任何东西**：mechd 不执行部署动作，
// 而且要删的机器很可能已经联系不上了（换硬件、退役、被回收）。
// 机器上留下的东西会在它下次上报时变成孤儿并被明确列出。
func newNodeRemoveCmd(f *ClientFlags) *cobra.Command {
	var force, yes bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a node from the roster",
		Long: `Remove a node from the roster.

**Doesn't uninstall anything on that machine.** Rejected by default while
it still has instances — removing a node that's still running components
would make those components vanish from the central view while they keep
running on the machine.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			name := args[0]
			// 确认分两档，与 10-cli §7 的表一一对应：
			//
			//	node remove           y/N     —— 仍有实例时服务端会直接拒绝，
			//	                                 这一档挡的只是手滑
			//	node remove --force   输名字  —— 它会连着实例一起抹掉，
			//	                                 而那些组件仍在机器上跑
			//
			// **--force 那一档 `-y` 跳不过去**：它挡的是「删错了对象」，
			// 与 -y 挡的不是同一件事。
			prompt := fmt.Sprintf("This will remove node %s from the roster (nothing on the machine will be uninstalled).", name)
			if force {
				err = confirmName(c, prompt+"\n  --force will also wipe its instances from the roster.", name)
			} else {
				err = confirmYN(c, prompt+" Are you sure?", yes)
			}
			if err != nil {
				return err
			}
			path := mechd.APIPrefix + "/nodes/" + seg(name)
			if force {
				path += "?force=true"
			}
			var out map[string]any
			if err := cli.Do("DELETE", path, nil, &out); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "Removed node %s\n", name)
			return nil
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&force, "force", false, "Remove it even if it still has instances (they become orphans)")
	fl.BoolVarP(&yes, "yes", "y", false, "Skip confirmation")
	return cmd
}

// newNodeRevokeCmd 构造 `mechctl node revoke` / `unrevoke`。
//
// **与 remove 是两件事**：remove 把节点从册子上抹掉（换硬件、退役），
// revoke 保留那一行但切断它——「这台机器被偷了/被攻破了，先断掉，
// 但我还要看它上面装过什么」。
//
// 吊销走应用层检查而不是 CRL（ADR-0034）：被吊销的证书**握手仍会成功**，
// 但任何 RPC 都会被拒。这条代价在那个 ADR 里写明了。
func newNodeRevokeCmd(f *ClientFlags, revoke bool) *cobra.Command {
	verb, short := "revoke", "Revoke a node's certificate (it can't connect, but stays in the roster)"
	if !revoke {
		verb, short = "unrevoke", "Restore a revoked node"
	}
	var yes bool
	cmd := &cobra.Command{
		Use:   verb + " <name>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			name := args[0]
			if revoke {
				if err := confirmYN(c, fmt.Sprintf(
					"This will revoke node %s's certificate, and it will lose connectivity immediately.", name), yes); err != nil {
					return err
				}
			}
			var out map[string]any
			path := mechd.APIPrefix + "/nodes/" + seg(name) + "/" + verb
			if err := cli.Do("POST", path, struct{}{}, &out); err != nil {
				return err
			}
			if revoke {
				fmt.Fprintf(c.OutOrStdout(),
					"Revoked node %s.\n"+
						"  Its certificate **handshake will still succeed**, but every RPC will be rejected —\n"+
						"  that's the cost of application-layer revocation (ADR-0034).\n", name)
			} else {
				fmt.Fprintf(c.OutOrStdout(), "Restored node %s\n", name)
			}
			return nil
		},
	}
	if revoke {
		cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation")
	}
	return cmd
}

// newNodeCordonCmd 构造 `mechctl node cordon` / `uncordon`。
//
// **与 revoke 是两件完全不同的事**，两者都有用，语义必须分清：
//
//	cordon  暂停调和，连接与上报照常，运行中的进程不动
//	        「我要手工调试这台机，别让 mechlet 把我的改动改回去」
//	revoke  切断证书，任何 RPC 都被拒
//	        「这台机器被偷了」
//
// cordon 的节点也**不进任何 Rollout 批次**（22-multi-node §2.7）——
// 它不只是个便利命令，是分批必须回答的一个输入。
func newNodeCordonCmd(f *ClientFlags, on bool) *cobra.Command {
	verb, short := "cordon", "Pause reconciliation on a node (running processes are left alone)"
	if !on {
		verb, short = "uncordon", "Resume reconciliation on a node"
	}
	return &cobra.Command{
		Use:   verb + " <name>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			name := args[0]
			var out map[string]any
			path := mechd.APIPrefix + "/nodes/" + seg(name) + "/" + verb
			if err := cli.Do("POST", path, struct{}{}, &out); err != nil {
				return err
			}
			if on {
				fmt.Fprintf(c.OutOrStdout(),
					"Paused reconciliation on %s.\n"+
						"  It's still connected and still reporting, running processes are unaffected —\n"+
						"  only the desired state stops landing on this machine, and it won't enter any Rollout batch.\n"+
						"  To resume: mechctl node uncordon %s\n", name, name)
			} else {
				fmt.Fprintf(c.OutOrStdout(), "Resumed reconciliation on %s\n", name)
			}
			return nil
		},
	}
}

// nodeState 把在线状态与 cordon / revoke 并成一列展示。
//
// **附在状态后面而不是另开两列**：一台机器可以既在线又被暂停调和，
// 而运维扫这张表时问的是「这台机现在什么情况」——三列分开看反而要
// 自己合。json 输出里它们仍是独立字段，脚本不受影响。
func nodeState(n mechd.NodeView) string {
	s := orDash(n.Status)
	switch {
	case n.Revoked:
		s += " (revoked)"
	case n.Cordoned:
		s += " (cordoned)"
	}
	return s
}
