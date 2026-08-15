// Package cli 提供四个二进制共用的命令行骨架：全局标志、日志装配、
// 版本子命令与统一的输出格式。
package cli

import (
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/mecharion/mecharion/internal/logging"
)

// GlobalFlags 是所有二进制共有的全局标志值。
type GlobalFlags struct {
	LogLevel  string
	LogFormat string
	Output    string
}

// Options 描述一个二进制的根命令。
type Options struct {
	// Name 是二进制名，如 "mechctl"。
	Name string
	// Short 是一行简介。
	Short string
	// Long 是详细说明，可为空。
	Long string
	// DefaultLogFormat 决定该二进制的默认日志格式：
	// 交互式 CLI 用 text，长驻服务用 json。
	DefaultLogFormat logging.Format
}

// NewRoot 构造根命令，已装配全局标志、日志与 version 子命令。
// 返回的 GlobalFlags 在 PersistentPreRunE 之后才被填充完毕。
func NewRoot(opts Options) (*cobra.Command, *GlobalFlags) {
	gf := &GlobalFlags{}

	defFormat := opts.DefaultLogFormat
	if defFormat == "" {
		defFormat = logging.FormatText
	}

	cmd := &cobra.Command{
		Use:   opts.Name,
		Short: opts.Short,
		Long:  opts.Long,
		// 子命令自行处理错误展示，避免 cobra 重复打印
		SilenceUsage:  true,
		SilenceErrors: true,
		// 无子命令时打印帮助而非报错
		RunE: func(c *cobra.Command, args []string) error {
			return c.Help()
		},
		PersistentPreRunE: func(c *cobra.Command, args []string) error {
			_, err := logging.SetDefault(logging.Options{
				Level:     gf.LogLevel,
				Format:    logging.Format(gf.LogFormat),
				AddSource: gf.LogLevel == "debug",
			})
			if err != nil {
				return err
			}
			if gf.Output != "" && !isValidOutput(gf.Output) {
				return fmt.Errorf("unknown output format %q (choices: table|json|yaml)", gf.Output)
			}
			slog.Debug("starting", "component", opts.Name)
			return nil
		},
	}

	pf := cmd.PersistentFlags()
	pf.StringVar(&gf.LogLevel, "log-level", "info", "Log level: debug|info|warn|error")
	pf.StringVar(&gf.LogFormat, "log-format", string(defFormat), "Log format: text|json")
	// -o 对四个二进制一律提供：version 在任何一个上都输出结构化数据，
	// 且 mechd/mechlet 后续都会有只读的 status / inspect 类命令。
	pf.StringVarP(&gf.Output, "output", "o", OutputTable, "Output format: table|json|yaml")

	cmd.AddCommand(newVersionCmd(opts.Name, gf))
	return cmd, gf
}

// 输出格式常量。
const (
	OutputTable = "table"
	OutputJSON  = "json"
	OutputYAML  = "yaml"
)

func isValidOutput(s string) bool {
	switch s {
	case OutputTable, OutputJSON, OutputYAML:
		return true
	}
	return false
}
