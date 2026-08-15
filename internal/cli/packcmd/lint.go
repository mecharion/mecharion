// Package packcmd 实现 mechpack 的子命令。
package packcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/packindex"
)

// 退出码，对齐 docs/design/10-cli.md §6。
const (
	// ExitValidation 表示校验失败。
	ExitValidation = 3
)

// NewLintCmd 构造 `mechpack lint`。
func NewLintCmd(output *string) *cobra.Command {
	var hermetic bool
	var strict bool

	cmd := &cobra.Command{
		Use:   "lint [dir...]",
		Short: "Validate that a Pack conforms to the pack/v1 spec",
		Long: `Validate one or more Pack directories.

Validates the current directory when no arguments are given. Rule numbers
(R01…R45) correspond to spec §19.

  --hermetic  also check offline constraints: scan hooks/ and command resources for external dependency calls
  --strict    treat warnings as failures too`,
		Args: cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error {
			dirs := args
			if len(dirs) == 0 {
				dirs = []string{"."}
			}
			return runLint(c, dirs, hermetic, strict, *output)
		},
	}

	cmd.Flags().BoolVar(&hermetic, "hermetic", false, "Check offline constraints (spec §17)")
	cmd.Flags().BoolVar(&strict, "strict", false, "Treat warnings as failures")
	return cmd
}

type lintReport struct {
	Dir      string         `json:"dir"      yaml:"dir"`
	Pack     string         `json:"pack"     yaml:"pack"`
	Version  string         `json:"version"  yaml:"version"`
	OK       bool           `json:"ok"       yaml:"ok"`
	Findings []pack.Finding `json:"findings" yaml:"findings"`
}

func runLint(c *cobra.Command, dirs []string, hermetic, strict bool, output string) error {
	out := c.OutOrStdout()
	expanded := expandDirs(dirs)
	reports := make([]lintReport, 0, len(expanded))

	// 用**本次要校验的这批 Pack**建索引，让跨 Pack 引用（规则 43）可以核对。
	// 只校验单个 Pack 时索引里只有它自己，依赖解析不出来，那条规则自然降级
	// 为警告——这正是它该有的行为：依赖方完全可能单独发布。
	index := packindex.New()
	for _, dir := range expanded {
		if p, err := pack.Load(dir); err == nil {
			index.Add(p, dir)
		}
	}

	totalErr, totalWarn := 0, 0

	for _, dir := range expanded {
		rep := lintReport{Dir: dir}

		p, err := pack.Load(dir)
		if err != nil {
			rep.Findings = []pack.Finding{{
				Rule: "R00", Severity: pack.SevError, Path: dir, Message: err.Error(),
			}}
			totalErr++
			reports = append(reports, rep)
			continue
		}
		rep.Pack, rep.Version = p.Name, p.Version

		res := pack.Lint(p, pack.Options{Hermetic: hermetic, Resolver: index})
		rep.Findings = res.Findings
		rep.OK = res.OK() && (!strict || len(res.Warnings()) == 0)
		totalErr += len(res.Errors())
		totalWarn += len(res.Warnings())
		reports = append(reports, rep)
	}

	if output == OutputJSON || output == OutputYAML {
		if err := encode(out, output, reports); err != nil {
			return err
		}
	} else {
		printLintText(c, reports, hermetic)
	}

	if totalErr > 0 || (strict && totalWarn > 0) {
		c.SilenceUsage = true
		return &exitError{code: ExitValidation, msg: summarize(len(reports), totalErr, totalWarn)}
	}
	return nil
}

func printLintText(c *cobra.Command, reports []lintReport, hermetic bool) {
	out := c.OutOrStdout()
	for _, rep := range reports {
		name := rep.Pack
		if name == "" {
			name = filepath.Base(rep.Dir)
		}

		var errs, warns int
		for _, f := range rep.Findings {
			if f.Severity == pack.SevError {
				errs++
			} else {
				warns++
			}
		}

		status := "✓"
		if errs > 0 {
			status = "✗"
		} else if warns > 0 {
			status = "!"
		}
		fmt.Fprintf(out, "%s %-16s %s\n", status, name, rep.Dir)

		for _, f := range rep.Findings {
			fmt.Fprintf(out, "  %s\n", f.String())
		}
		if len(rep.Findings) > 0 {
			fmt.Fprintln(out)
		}
	}

	var okCount, errCount, warnCount int
	for _, rep := range reports {
		hasErr := false
		for _, f := range rep.Findings {
			if f.Severity == pack.SevError {
				hasErr = true
				errCount++
			} else {
				warnCount++
			}
		}
		if !hasErr {
			okCount++
		}
	}
	mode := ""
	if hermetic {
		mode = " (including hermetic checks)"
	}
	fmt.Fprintf(out, "%d/%d Pack(s) passed%s", okCount, len(reports), mode)
	if errCount > 0 || warnCount > 0 {
		fmt.Fprintf(out, "  —  %d error(s), %d warning(s)", errCount, warnCount)
	}
	fmt.Fprintln(out)
}

func summarize(n, errs, warns int) string {
	return fmt.Sprintf("validation failed: %d error(s), %d warning(s) across %d Pack(s)", errs, warns, n)
}

// expandDirs 把参数展开为 Pack 目录：
// 若参数本身含 pack.yaml 则直接采用，否则把其一级子目录中含 pack.yaml 的收进来。
func expandDirs(args []string) []string {
	var out []string
	for _, a := range args {
		if isPackDir(a) {
			out = append(out, a)
			continue
		}
		entries, err := os.ReadDir(a)
		if err != nil {
			out = append(out, a) // 让后续 Load 报出可读的错误
			continue
		}
		found := false
		var subs []string
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			sub := filepath.Join(a, e.Name())
			if isPackDir(sub) {
				subs = append(subs, sub)
				found = true
			}
		}
		if found {
			sort.Strings(subs)
			out = append(out, subs...)
		} else {
			out = append(out, a)
		}
	}
	return out
}

func isPackDir(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, pack.PackFile))
	return err == nil && !st.IsDir()
}
