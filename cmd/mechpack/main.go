// mechpack 组装、校验与分发 Pack。
//
// 它**不构建你的软件**——构建由开发者用自己的工具链完成，
// mechpack 只把产物组装成 Pack。命令因此叫 assemble 而非 build。
// 详见 docs/adr/0015-offline-first-hermetic.md
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/mecharion/mecharion/internal/cli"
	"github.com/mecharion/mecharion/internal/cli/packcmd"
	"github.com/mecharion/mecharion/internal/logging"
)

func main() {
	root, gf := cli.NewRoot(cli.Options{
		Name:  "mechpack",
		Short: "Mecharion Pack tool",
		Long: `mechpack assembles, validates and distributes Packs.

It doesn't build your software — that's your own toolchain's job, mechpack
only assembles the artifacts. Hence the command is assemble, not build.`,
		DefaultLogFormat: logging.FormatText,
	})

	root.AddCommand(
		packcmd.NewInitCmd(),
		packcmd.NewAssembleCmd(&gf.Output),
		packcmd.NewBundleCmd(&gf.Output),
		packcmd.NewLintCmd(&gf.Output),
		packcmd.NewInspectCmd(&gf.Output),
	)

	// 没有 sign：Pack 签名/可信发布者校验决定不做，见 docs/adr/0040-pack-trust-is-operator-responsibility.md
	// （取代早先要求强制签名的 0016）。
	//
	// 还没做的：push（thick → thin，把 blob 入库；上传那条路已经在 mechd 侧做了）

	if err := root.Execute(); err != nil {
		var msg string
		if !errors.Is(err, os.ErrClosed) {
			msg = err.Error()
		}
		if msg != "" {
			fmt.Fprintln(os.Stderr, "Error:", msg)
		}
		os.Exit(packcmd.ExitCode(err))
	}
}
