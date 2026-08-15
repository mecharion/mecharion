// mechlet 是节点代理，也是 Mecharion 唯一的执行引擎。
//
// 读取本地期望状态、解析 Pack、物化组件、持续调和——mechd 断连时这些
// 都不受影响，机器按最后已知的期望状态继续自愈。这与「mechd 是可选的」
// 不是一回事：常规部署里 mechd 始终在场（多节点是独立机器，单机是同机
// 部署，见 docs/adr/0026-standalone-runs-mechd.md），mechlet 的自治说的
// 是「断连时不停摆」，不是「本来就不需要 mechd」。
//
// `agent` 子命令在监听上行连接之外，还在本机 unix socket 上开一个只读
// 诊断入口（--local-socket），供 mechctl --local 在 mechd 不可达时读取
// 本机实例状态。详见 docs/adr/0002-mechlet-as-sole-engine.md
package main

import (
	"fmt"
	"os"

	"github.com/mecharion/mecharion/internal/cli"
	"github.com/mecharion/mecharion/internal/cli/mechletcmd"
	"github.com/mecharion/mecharion/internal/logging"
)

func main() {
	root, gf := cli.NewRoot(cli.Options{
		Name:  "mechlet",
		Short: "Mecharion node agent",
		Long: `mechlet is the node agent and execution engine for Mecharion (m7n).

Resource reconciliation, runtime drivers, generation management, health
checks and drift detection all happen here. When mechd is unreachable, it
keeps self-healing against the last known desired state; mechctl --local can
read the local read-only diagnostic view at that point, but that's not the
normal way to operate — normal operations all go through mechd.`,
		DefaultLogFormat: logging.FormatJSON,
	})

	root.AddCommand(
		mechletcmd.NewApplyCmd(&gf.Output),
		mechletcmd.NewAgentCmd(),
		mechletcmd.NewInstallCmd(),
	)

	// 还没做的：probe（能力探测目前只在注册时上报，没有独立命令）。
	// 本机视角的实例状态现在有两条路：mechd 侧的完整视图（跨节点、对照
	// 期望状态），或 mechd 不可达时 mechctl --local component status
	// 读的本机只读诊断视图（agent 命令内置，不是这里的独立子命令）。

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(mechletcmd.ExitCodeOf(err))
	}
}
