// mechctl 是 Mecharion 的命令行入口。
//
// 常规操作**始终连 mechd**——单机形态下也是如此（ADR-0026：单机是
// mechd + mechlet 同机部署，不是「没有 mechd」）。`--local` 是另一条、
// 范围小得多的路径：直连本机 mechlet 的 unix socket，只读，只用于
// mechd 不可达时的现场诊断，命令面不与常规路径对等。
package main

import (
	"fmt"
	"os"

	"github.com/mecharion/mecharion/internal/cli"
	"github.com/mecharion/mecharion/internal/cli/ctlcmd"
	"github.com/mecharion/mecharion/internal/logging"
)

func main() {
	root, gf := cli.NewRoot(cli.Options{
		Name:  "mechctl",
		Short: "Mecharion command-line tool",
		Long: `mechctl is the command-line entry point for Mecharion (m7n).

Normal operations go through mechd to manage the whole Site — even in the
standalone form, where mechd and mechlet are deployed on the same machine
(mechlet install --standalone).

--local connects directly to the local mechlet's read-only diagnostic view.
It's for on-site troubleshooting when mechd is unreachable (e.g. component
status), not a normal entry point, and its command surface isn't equivalent.`,
		DefaultLogFormat: logging.FormatText,
	})

	// flags 是全部名词命令共用的**唯一**一份 ClientFlags（--output、
	// --local/--server/--token/--ca-file/--insecure-skip-verify/
	// --site/--local-socket 都在这一份上统一处理）：此前每个名词命令各自
	// `Bind` 一份，cobra 在还没找到子命令之前无法识别 `mechctl --local
	// component ...` 里的 `--local`，会把 `component` 误吞成它的值——
	// 只在根命令 Bind 一次，物理上不可能再分叉，`mechctl --local
	// component status` 与 `mechctl component status --local` 都能用。
	flags := &ctlcmd.ClientFlags{Global: gf}
	flags.Bind(root)
	root.AddCommand(ctlcmd.NewApplyCmd(flags))
	root.AddCommand(ctlcmd.NewBackupCmd(flags))
	root.AddCommand(ctlcmd.NewComponentCmd(flags))
	root.AddCommand(ctlcmd.NewConfigCmd(flags))
	root.AddCommand(ctlcmd.NewNodeCmd(flags))
	root.AddCommand(ctlcmd.NewOrphansCmd(flags))
	root.AddCommand(ctlcmd.NewPackCmd(flags))
	root.AddCommand(ctlcmd.NewRolloutCmd(flags))
	root.AddCommand(ctlcmd.NewUserCmd(flags))

	// 命令结构是**名词优先**：mechctl <名词> <动词> [参数]（ADR-0025）。
	// 顶层只有 apply / deploy / version / completion 四个命令——
	// 其余动词必须留在名词之下，因为 remove / list / status 等对多个名词
	// 都成立，顶层形式无法消歧。
	//
	// 还没做的（10-cli 里已定，代码里没有）：
	//
	//	site                站点管理——单 Site 之外还没有真实需求
	//	node facts|exec|logs
	//	pack list|show|pull   查询注册表；upload 已经做了
	//	config diff --from/--to   跨 Pack 版本比对（组间 diff 已有）
	//	completion 的动态补全      要向 mechd 查询名字
	//
	// 完整命令面见 docs/design/10-cli.md

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(ctlcmd.ExitCodeOf(err))
	}
}
