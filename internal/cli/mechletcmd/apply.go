// Package mechletcmd 实现 mechlet 的子命令。
package mechletcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/mecharion/mecharion/internal/reclaim"
	"github.com/mecharion/mecharion/internal/reconcile"
	"github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/runtime/docker"
	"github.com/mecharion/mecharion/internal/runtime/systemd"
	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/state"
)

// 退出码，对齐 docs/design/10-cli.md §6。
const (
	// ExitValidation 表示校验失败。
	ExitValidation = 3
	// ExitReconcile 表示调和失败。
	ExitReconcile = 4
)

// DefaultDataDir 是 mechlet 的数据目录默认值。
const DefaultDataDir = "/var/lib/mecharion"

// NewApplyCmd 构造 `mechlet apply`。
//
// 这是 M2 的调试入口：直接喂一份已解析规格，绕开 mechd。它读的结构与
// mechd 下发的**完全相同**，走的也是同一个 reconciler——不是分叉，
// 是同一条路径的另一种输入来源（docs/design/25-roadmap.md）。
func NewApplyCmd(output *string) *cobra.Command {
	var (
		file    string
		dataDir string
		dryRun  bool
	)

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Reconcile local state against an already-resolved spec",
		Long: `Read an already-resolved spec (ResolvedSpec) and reconcile the local machine
against it.

This is a debugging entry point. In normal use, the spec is dispatched by
mechd, going through the same reconciler.

  -f -   read from stdin`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			if file == "" {
				return fmt.Errorf("spec file required, use -f")
			}
			return runApply(c, file, dataDir, dryRun, *output)
		},
	}

	f := cmd.Flags()
	f.StringVarP(&file, "file", "f", "", "Path to a resolved spec file, `-` means stdin")
	f.StringVar(&dataDir, "data-dir", DefaultDataDir, "Data directory")
	f.BoolVar(&dryRun, "dry-run", false, "Only resolve and validate the spec, don't touch the machine")
	return cmd
}

func runApply(c *cobra.Command, file, dataDir string, dryRun bool, output string) error {
	out := c.OutOrStdout()

	s, err := loadSpec(c, file)
	if err != nil {
		return exitWith(ExitValidation, err)
	}

	// digest 是 generation 的身份，规格没带就当场算一份。手写的调试规格
	// 不该被迫先算一遍 sha256，但**内容与 digest 不符必须报错**——那说明
	// 传输途中被改过，或者下发方算错了。
	if s.Digest == "" {
		if err := spec.Seal(s); err != nil {
			return exitWith(ExitValidation, err)
		}
		slog.Debug("spec had no digest, computed it in place", "digest", s.Digest[:12])
	} else if err := spec.VerifyDigest(s); err != nil {
		return exitWith(ExitValidation, err)
	}

	if dryRun {
		return printDryRun(out, s, output)
	}

	store, err := state.New(filepath.Join(dataDir, "mechlet"))
	if err != nil {
		return err
	}
	r := &reconcile.Reconciler{
		Store:    store,
		Runtimes: runtime.NewRegistry(systemd.New(), docker.New(), docker.NewCompose()),
		BlobDir:  filepath.Join(dataDir, "blobs"),
		PackDir:  filepath.Join(dataDir, "packs"),
		Log:      slog.Default(),
	}

	s = attachDebugSecrets(s)

	rep, rerr := r.Reconcile(c.Context(), s)
	// 无论成败都把报告打出来——失败时它才是最有价值的东西，
	// 里面记着走到哪一步、哪个资源、差异是什么。
	if perr := printReport(out, rep, output); perr != nil {
		return perr
	}

	// 回收也在这里做一遍：apply 会 prune 掉超出保留数的 generation，
	// 而**一台只用 apply 的机器上没有常驻 agent**——不在这里收，
	// 那些镜像与载荷就再没有第二个人会来收。
	got := reclaim.Run(c.Context(), reclaim.Options{
		State: store, Runtimes: r.Runtimes,
		BlobDir: r.BlobDir, Log: slog.Default(),
	})
	printReclaimed(out, got, output)

	if rerr != nil {
		return exitWith(ExitReconcile, rerr)
	}
	return nil
}

// printReclaimed 报告回收结果。
//
// 只在真的删了东西时才说话：一条每次 apply 都出现的「已回收 0 项」
// 会把有意义的那次淹掉。json / yaml 输出里不加——那里的结构是调和报告，
// 回收不属于它。
func printReclaimed(out io.Writer, got reclaim.Result, output string) {
	if output == "json" || output == "yaml" {
		return
	}
	if n := len(got.Images); n > 0 {
		fmt.Fprintf(out, "  reclaimed image(s) %s\n", strings.Join(got.Images, ", "))
	}
	if n := len(got.Blobs); n > 0 {
		fmt.Fprintf(out, "  reclaimed %d blob(s)\n", n)
	}
}

// loadSpec 从文件或标准输入读取规格。
func loadSpec(c *cobra.Command, file string) (*spec.ResolvedSpec, error) {
	var (
		data []byte
		err  error
	)
	if file == "-" {
		data, err = io.ReadAll(c.InOrStdin())
	} else {
		data, err = os.ReadFile(file)
	}
	if err != nil {
		return nil, fmt.Errorf("reading spec: %w", err)
	}

	// 规格的线上格式是 JSON。手写调试规格用 YAML 顺手得多，而 JSON 是
	// YAML 的子集，因此统一走 YAML → JSON 再解析，两种写法都能用。
	if !looksLikeJSON(data) {
		var node any
		if err := yaml.Unmarshal(data, &node); err != nil {
			return nil, fmt.Errorf("parsing spec: %w", err)
		}
		if data, err = json.Marshal(node); err != nil {
			return nil, fmt.Errorf("converting spec: %w", err)
		}
	}

	s, err := spec.Parse(data)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func looksLikeJSON(b []byte) bool {
	return strings.HasPrefix(strings.TrimLeft(string(b), " \t\r\n"), "{")
}

// printDryRun 展示解析结果，不改动机器。
func printDryRun(out io.Writer, s *spec.ResolvedSpec, output string) error {
	if output == "json" || output == "yaml" {
		return encode(out, s, output)
	}
	fmt.Fprintf(out, "Component  %s/%s (config group %s)\n", s.Component, s.Role, s.ConfigGroup)
	fmt.Fprintf(out, "Node       %s\n", s.Node.Name)
	fmt.Fprintf(out, "Pack       %s %s revision %d\n", s.Pack.Name, s.Pack.Version, s.Pack.Revision)
	fmt.Fprintf(out, "digest     %s\n", s.Digest)
	if s.Profile != "" {
		fmt.Fprintf(out, "Profile    %s\n", s.Profile)
	}

	fmt.Fprintf(out, "\nPaths (%d)\n", len(s.Paths))
	for _, name := range sortedPathNames(s) {
		p := s.Paths[name]
		fmt.Fprintf(out, "  %-12s %s", name, strings.Join(p.Values, ", "))
		if p.LinkInto != "" {
			fmt.Fprintf(out, "  → linked into %s", p.LinkInto)
		}
		fmt.Fprintln(out)
	}

	fmt.Fprintf(out, "\nResources (%d)\n", len(s.Resources))
	for _, r := range s.Resources {
		fmt.Fprintf(out, "  %-10s %s", r.Type, r.ID)
		if r.Notify != "" {
			fmt.Fprintf(out, "  notify=%s", r.Notify)
		}
		fmt.Fprintln(out)
	}

	if s.Workload != nil {
		fmt.Fprintf(out, "\nWorkload   runtime=%s\n", s.Workload.Runtime)
		if s.Workload.Systemd != nil {
			fmt.Fprintf(out, "  unit    %s\n", systemd.UnitName(s.Component, s.Role))
			fmt.Fprintf(out, "  exec    %s\n", s.Workload.Systemd.Exec)
		}
	}
	fmt.Fprintln(out, "\n(--dry-run: the machine was not changed)")
	return nil
}

// printReport 输出调和报告。
func printReport(out io.Writer, rep *reconcile.Report, output string) error {
	if rep == nil {
		return nil
	}
	if output == "json" || output == "yaml" {
		return encode(out, rep, output)
	}

	fmt.Fprintln(out, rep.Summary())

	for _, rr := range rep.Resources {
		if rr.Action == reconcile.ActionNone || rr.Action == "" {
			continue
		}
		fmt.Fprintf(out, "  %-9s %-10s %s\n", rr.Action, rr.Type, rr.ID)
		if rr.Reason != "" {
			fmt.Fprintf(out, "              unreadable: %s\n", rr.Reason)
		}
		for _, ch := range rr.Changes {
			fmt.Fprintf(out, "              %s\n", formatChange(ch))
		}
		if rr.Error != "" {
			fmt.Fprintf(out, "              error: %s\n", rr.Error)
		}
	}

	if len(rep.Absorbed) > 0 {
		fmt.Fprintf(out, "  notify %s absorbed by restart\n", strings.Join(rep.Absorbed, ", "))
	}
	if len(rep.Pruned) > 0 {
		fmt.Fprintf(out, "  reclaimed generation(s) %s\n", strings.Join(rep.Pruned, ", "))
	}
	if h := rep.Health; h != nil {
		if h.Healthy {
			fmt.Fprintf(out, "  health check passed (%s)\n", h.Probe)
		} else {
			fmt.Fprintf(out, "  health check failed (%s): %s\n", h.Probe, h.Error)
		}
	}
	if w := rep.Workload; w != nil {
		fmt.Fprintf(out, "  workload %s  %s\n", w.State, w.Native)
	}
	if rep.Error != "" {
		fmt.Fprintf(out, "\nfailed: %s\n", rep.Error)
	}
	return nil
}

func formatChange(ch reconcile.ChangeReport) string {
	if ch.Kind == "text" {
		return fmt.Sprintf("%s: content changed", ch.Field)
	}
	if ch.Got == "" {
		return fmt.Sprintf("%s: (none) → %s", ch.Field, ch.Want)
	}
	return fmt.Sprintf("%s: %s → %s", ch.Field, ch.Got, ch.Want)
}

func sortedPathNames(s *spec.ResolvedSpec) []string {
	out := make([]string, 0, len(s.Paths))
	for k := range s.Paths {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func encode(out io.Writer, v any, format string) error {
	if format == "yaml" {
		enc := yaml.NewEncoder(out)
		enc.SetIndent(2)
		defer enc.Close()
		return enc.Encode(v)
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// exitError 携带退出码。
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func exitWith(code int, err error) error {
	return &exitError{code: code, err: err}
}

// ExitCodeOf 取出错误对应的退出码；普通错误返回 1，nil 返回 0。
//
// 脚本靠退出码分流：3 = 规格有问题（改规格），4 = 调和失败（看报告），
// 1 = 其它。这是 10-cli.md §6 的约定。
func ExitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var e *exitError
	if errors.As(err, &e) {
		return e.code
	}
	return 1
}

// attachDebugSecrets 把规格里直接写着的敏感值收起来，供 hook 注入使用。
//
// 正常路径上敏感值不在规格里：它们随 gRPC 消息单独下发，规格里只有
// `@@m7n:secret:<id>@@` 引用（16-secrets §4）。
//
// 但 `mechlet apply -f` 是**没有 mechd 可问**的调试入口，因此允许规格里
// 直接带明文。这条路径刻意留下告警：它绕过了信封加密与「不落盘」那两条
// 保证，不该被当成常规用法。
func attachDebugSecrets(s *spec.ResolvedSpec) *spec.ResolvedSpec {
	byParam := map[string]string{}
	for name, p := range s.Params {
		if !p.Sensitive || p.Value == nil {
			continue
		}
		if str, ok := p.Value.(string); ok && str != "" {
			byParam[name] = str
		}
	}
	if len(byParam) == 0 {
		return s
	}
	names := make([]string, 0, len(byParam))
	for n := range byParam {
		names = append(names, n)
	}
	sort.Strings(names)
	slog.Warn("spec carries plaintext sensitive values, this is a debug path",
		"params", strings.Join(names, ","),
		"hint", "in real deployments secrets are dispatched separately by mechd, the spec only holds references")
	return s.WithSecrets(byParam)
}
