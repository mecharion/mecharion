package packcmd

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/mecharion/mecharion/internal/pack"
)

// 输出格式常量，与 internal/cli 保持一致。
const (
	OutputTable = "table"
	OutputJSON  = "json"
	OutputYAML  = "yaml"
)

// exitError 携带自定义退出码。
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// ExitCode 返回该错误对应的退出码；非 exitError 返回 1。
func ExitCode(err error) int {
	if e, ok := err.(*exitError); ok {
		return e.code
	}
	return 1
}

func encode(w io.Writer, format string, v any) error {
	switch format {
	case OutputJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	case OutputYAML:
		enc := yaml.NewEncoder(w)
		defer enc.Close()
		return enc.Encode(v)
	}
	return fmt.Errorf("unknown output format %q", format)
}

// NewInspectCmd 构造 `mechpack inspect`。
func NewInspectCmd(output *string) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect [dir]",
		Short: "Show a Pack's contents, roles, profiles, and blob manifest",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			p, err := pack.Load(dir)
			if err != nil {
				return err
			}
			view := buildView(p)
			if *output == OutputJSON || *output == OutputYAML {
				return encode(c.OutOrStdout(), *output, view)
			}
			printInspect(c.OutOrStdout(), p, view)
			return nil
		},
	}
}

type inspectView struct {
	Name        string        `json:"name"        yaml:"name"`
	Version     string        `json:"version"     yaml:"version"`
	Revision    int           `json:"revision"    yaml:"revision"`
	Description string        `json:"description" yaml:"description,omitempty"`
	Platforms   []string      `json:"platforms"   yaml:"platforms"`
	Roles       []roleView    `json:"roles"       yaml:"roles"`
	Profiles    []profileView `json:"profiles"    yaml:"profiles,omitempty"`
	Requires    []depView     `json:"requires"    yaml:"requires,omitempty"`
	Exports     []exportView  `json:"exports"     yaml:"exports,omitempty"`
	Blobs       []blobView    `json:"blobs"       yaml:"blobs,omitempty"`
	Params      int           `json:"paramCount"  yaml:"paramCount"`
}

type roleView struct {
	Name        string `json:"name"        yaml:"name"`
	Cardinality string `json:"cardinality" yaml:"cardinality"`
	Runtime     string `json:"runtime"     yaml:"runtime,omitempty"`
	Quorum      bool   `json:"quorum"      yaml:"quorum,omitempty"`
	Description string `json:"description" yaml:"description,omitempty"`
}

type profileView struct {
	Name        string `json:"name"        yaml:"name"`
	Default     bool   `json:"default"     yaml:"default,omitempty"`
	MinNodes    int    `json:"minNodes"    yaml:"minNodes,omitempty"`
	Description string `json:"description" yaml:"description,omitempty"`
}

type depView struct {
	Name    string `json:"name"    yaml:"name"`
	Version string `json:"version" yaml:"version"`
	Scope   string `json:"scope"   yaml:"scope"`
}

type exportView struct {
	Name        string      `json:"name"        yaml:"name"`
	Role        string      `json:"role"        yaml:"role"`
	Description string      `json:"description" yaml:"description,omitempty"`
	Fields      []fieldView `json:"fields"      yaml:"fields,omitempty"`
}

// fieldView 是一个导出字段。
//
// Sensitive 由「该字段引用的参数是不是 secret」自动推导，Pack 不额外声明——
// 因此它不可能与实际不一致。展示它是为了让**消费方作者在没有提供方源码时**
// 也能看到契约：哪些字段带凭据、引用后自己的参数就得标 secret。
type fieldView struct {
	Name      string `json:"name"      yaml:"name"`
	Sensitive bool   `json:"sensitive" yaml:"sensitive,omitempty"`
}

type blobView struct {
	Name     string `json:"name"     yaml:"name"`
	Platform string `json:"platform" yaml:"platform"`
	Size     int64  `json:"size"     yaml:"size"`
	Filename string `json:"filename" yaml:"filename"`
}

func buildView(p *pack.Pack) inspectView {
	v := inspectView{
		Name: p.Name, Version: p.Version, Revision: p.Revision,
		Description: p.Description, Platforms: p.Platforms,
		Params: len(p.AllParams()),
	}
	for _, r := range p.Roles {
		rv := roleView{
			Name: r.EffectiveName(), Cardinality: r.EffectiveCardinality(),
			Quorum: r.Quorum, Description: r.Description,
		}
		if r.Workload != nil {
			rv.Runtime = string(r.Workload.Runtime)
		}
		v.Roles = append(v.Roles, rv)
	}
	for _, pr := range p.Profiles {
		v.Profiles = append(v.Profiles, profileView{
			Name: pr.Name, Default: pr.Default, MinNodes: pr.MinNodes,
			Description: pr.Description,
		})
	}
	addDeps := func(r *pack.Requires) {
		if r == nil {
			return
		}
		for _, d := range r.Packs {
			v.Requires = append(v.Requires, depView{
				Name: d.Name, Version: d.Version, Scope: string(d.EffectiveScope()),
			})
		}
	}
	addDeps(p.Requires)
	for _, pr := range p.Profiles {
		addDeps(pr.Requires)
	}
	for _, name := range sortedKeys(p.Exports) {
		e := p.Exports[name]
		ev := exportView{Name: name, Role: e.Role, Description: e.Description}
		for _, f := range e.FieldNames() {
			ev.Fields = append(ev.Fields, fieldView{
				Name:      f,
				Sensitive: p.ExprCarriesSecret(e.Fields[f]),
			})
		}
		v.Exports = append(v.Exports, ev)
	}
	for _, bn := range sortedKeys(p.Blobs) {
		blob := p.Blobs[bn]
		for _, plat := range sortedKeys(blob) {
			e := blob[plat]
			v.Blobs = append(v.Blobs, blobView{
				Name: bn, Platform: plat, Size: e.Size, Filename: e.Filename,
			})
		}
	}
	return v
}

func printInspect(w io.Writer, p *pack.Pack, v inspectView) {
	fmt.Fprintf(w, "%s %s-%d\n", v.Name, v.Version, v.Revision)
	if v.Description != "" {
		fmt.Fprintf(w, "  %s\n", v.Description)
	}
	fmt.Fprintf(w, "\nPlatforms: %s\n", strings.Join(v.Platforms, ", "))

	fmt.Fprintf(w, "\nRoles (%d):\n", len(v.Roles))
	for _, r := range v.Roles {
		extra := r.Runtime
		if extra == "" {
			extra = "no workload"
		}
		if r.Quorum {
			extra += ", quorum"
		}
		fmt.Fprintf(w, "  %-20s %-8s %s\n", r.Name, r.Cardinality, extra)
	}

	if len(v.Profiles) > 0 {
		fmt.Fprintf(w, "\nProfiles (%d):\n", len(v.Profiles))
		for _, pr := range v.Profiles {
			mark := " "
			if pr.Default {
				mark = "*"
			}
			nodes := ""
			if pr.MinNodes > 0 {
				nodes = fmt.Sprintf("≥%d node(s)", pr.MinNodes)
			}
			fmt.Fprintf(w, " %s %-18s %-10s %s\n", mark, pr.Name, nodes, pr.Description)
		}
	}

	if len(v.Requires) > 0 {
		fmt.Fprintf(w, "\nDependencies (%d):\n", len(v.Requires))
		for _, d := range v.Requires {
			fmt.Fprintf(w, "  %-16s %-14s scope=%s\n", d.Name, d.Version, d.Scope)
		}
	}

	if len(v.Exports) > 0 {
		fmt.Fprintf(w, "\nExported connection points (%d):\n", len(v.Exports))
		for _, e := range v.Exports {
			fmt.Fprintf(w, "  %-16s role=%-12s %s\n", e.Name, e.Role, e.Description)
			for _, f := range e.Fields {
				mark := ""
				if f.Sensitive {
					mark = "  ← sensitive, parameters referencing it must declare type: secret"
				}
				fmt.Fprintf(w, "    %-14s%s\n", f.Name, mark)
			}
		}
	}

	if len(v.Blobs) > 0 {
		fmt.Fprintf(w, "\nBlobs (%d):\n", len(v.Blobs))
		for _, b := range v.Blobs {
			fmt.Fprintf(w, "  %-10s %-14s %10s  %s\n",
				b.Name, b.Platform, humanSize(b.Size), b.Filename)
		}
	}

	fmt.Fprintf(w, "\nParameters: %d\n", v.Params)
	_ = p
}

func humanSize(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
