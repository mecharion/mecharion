// Package logging 统一四个二进制的日志配置。
//
// 使用标准库 log/slog，不引入第三方日志库——这与「单静态二进制、依赖最小」
// 的目标一致，且 slog 的 Handler 接口足以支撑将来的结构化输出需求。
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Format 是日志输出格式。
type Format string

const (
	// FormatText 是人类可读格式，终端交互时的默认值。
	FormatText Format = "text"
	// FormatJSON 是结构化格式，mechd / mechlet 长驻运行时的默认值。
	FormatJSON Format = "json"
)

// Options 控制日志器的构造。
type Options struct {
	Level  string // debug | info | warn | error
	Format Format
	Out    io.Writer // nil 时用 os.Stderr
	// AddSource 在日志中带上源码位置，仅建议 debug 级别开启。
	AddSource bool
}

// ParseLevel 把字符串解析为 slog.Level。
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unknown log level %q (choices: debug|info|warn|error)", s)
	}
}

// New 按 Options 构造日志器。
func New(opts Options) (*slog.Logger, error) {
	lvl, err := ParseLevel(opts.Level)
	if err != nil {
		return nil, err
	}
	out := opts.Out
	if out == nil {
		out = os.Stderr
	}

	hOpts := &slog.HandlerOptions{
		Level:     lvl,
		AddSource: opts.AddSource,
	}

	var h slog.Handler
	switch opts.Format {
	case FormatJSON:
		h = slog.NewJSONHandler(out, hOpts)
	case FormatText, "":
		h = slog.NewTextHandler(out, hOpts)
	default:
		return nil, fmt.Errorf("unknown log format %q (choices: text|json)", opts.Format)
	}
	return slog.New(h), nil
}

// SetDefault 构造日志器并设为 slog 的全局默认。
func SetDefault(opts Options) (*slog.Logger, error) {
	l, err := New(opts)
	if err != nil {
		return nil, err
	}
	slog.SetDefault(l)
	return l, nil
}
