// mechd 是 Mecharion 的控制面。
//
// **不是「可选」，是「可以与 mechlet 同机部署」**（ADR-0026）：多节点
// 形态下它跑在独立机器上，单机形态下 `mechlet install --standalone`
// 把它与 mechlet 装在同一台机器上——两种形态下功能完全一致，都有完整
// 的期望状态存储、blob 存储、Pack 注册、放置与拓扑解析、Rollout 编排、
// API 与 Web UI。
//
// mechd 不在数据面上：它不可用时各节点的 mechlet 仍按最后已知的期望
// 状态继续调和，只是暂时收不到新的变更。
package main

import (
	"fmt"
	"os"

	"github.com/mecharion/mecharion/internal/cli"
	"github.com/mecharion/mecharion/internal/cli/mechdcmd"
	"github.com/mecharion/mecharion/internal/logging"
)

func main() {
	root, _ := cli.NewRoot(cli.Options{
		Name:  "mechd",
		Short: "Mecharion control plane",
		Long: `mechd is the control plane for Mecharion (m7n).

Desired-state store, blob store, placement and topology resolution, rollout
orchestration, API and Web UI. In the standalone form it's deployed on the
same machine as mechlet (mechlet install --standalone), with functionality
identical to the multi-node form — not a reduced version (ADR-0026).`,
		DefaultLogFormat: logging.FormatJSON,
	})

	root.AddCommand(mechdcmd.NewServeCmd())
	root.AddCommand(mechdcmd.NewCACmd())

	// 还没做的：migrate（goose 迁移目前在 store.Open 里自动跑，
	// 没有独立入口）、backup

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(mechdcmd.ExitCodeOf(err))
	}
}
