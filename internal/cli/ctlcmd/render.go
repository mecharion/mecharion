// Package ctlcmd 实现 mechctl 的子命令。
package ctlcmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/render"
	"github.com/mecharion/mecharion/internal/spec"
)

// ExitValidation 表示校验失败，对齐 docs/design/10-cli.md §6。
const ExitValidation = 3

// Plan 是离线渲染的输入。
//
// 它把 mechd 从库里读出来的那些东西写成一个文件：站点、节点、放置结果、
// 参数覆盖、依赖绑定。字段与 render.Request 一一对应，**刻意不做任何推断**——
// 这条命令的价值在于「同样的输入必然得到同样的输出」，一旦开始猜就不成立了。
type Plan struct {
	Site      SiteSpec          `yaml:"site"`
	Component string            `yaml:"component"`
	Pack      string            `yaml:"pack"`
	Profile   string            `yaml:"profile"`
	Nodes     []NodeSpec        `yaml:"nodes"`
	Instances []InstanceSpec    `yaml:"instances"`
	Params    ParamLayers       `yaml:"params"`
	Requires  map[string]DepRef `yaml:"requires"`
}

// SiteSpec 是站点信息。
type SiteSpec struct {
	Name   string            `yaml:"name"`
	Kind   string            `yaml:"kind"`
	Labels map[string]string `yaml:"labels"`
}

// NodeSpec 是一台节点。
type NodeSpec struct {
	Name    string            `yaml:"name"`
	Address string            `yaml:"address"`
	Labels  map[string]string `yaml:"labels"`
	Roots   map[string]string `yaml:"roots"`
	Volumes []VolumeSpec      `yaml:"volumes"`
	Facts   map[string]any    `yaml:"facts"`
}

// VolumeSpec 是节点上的一块存储。
type VolumeSpec struct {
	Name  string `yaml:"name"`
	Path  string `yaml:"path"`
	Class string `yaml:"class"`
}

// InstanceSpec 是一个已放置的角色实例。
type InstanceSpec struct {
	Role        string              `yaml:"role"`
	Node        string              `yaml:"node"`
	Ordinal     int                 `yaml:"ordinal"`
	ConfigGroup string              `yaml:"configGroup"`
	Paths       map[string][]string `yaml:"paths"`
}

// ParamLayers 是三层参数覆盖。
type ParamLayers struct {
	Component map[string]any            `yaml:"component"`
	Role      map[string]map[string]any `yaml:"role"`
	Group     map[string]map[string]any `yaml:"group"`
}

// DepRef 是一条已绑定的依赖。
type DepRef struct {
	Pack      string              `yaml:"pack"`
	Component string              `yaml:"component"`
	Version   string              `yaml:"version"`
	Scope     string              `yaml:"scope"`
	Paths     map[string][]string `yaml:"paths"`
	Exports   map[string]DepPort  `yaml:"exports"`
	Topology  []PeerSpec          `yaml:"topology"`
}

// DepPort 是一条已求值的导出。
type DepPort struct {
	Value     string            `yaml:"value"`
	Fields    map[string]string `yaml:"fields"`
	Sensitive []string          `yaml:"sensitive"`
}

// PeerSpec 是依赖的一个实例。
type PeerSpec struct {
	Node    string              `yaml:"node"`
	Address string              `yaml:"address"`
	Ordinal int                 `yaml:"ordinal"`
	Role    string              `yaml:"role"`
	Labels  map[string]string   `yaml:"labels"`
	Paths   map[string][]string `yaml:"paths"`
}

// NewRenderCmd 构造 `mechctl component render`。
func NewRenderCmd() *cobra.Command {
	var planPath, packDir, outDir string
	var showWarnings bool

	cmd := &cobra.Command{
		Use:   "render",
		Short: "Resolve a ResolvedSpec offline, without connecting to any service",
		Long: `render resolves "Pack + user input + node facts" into a spec for each
RoleInstance.

It **doesn't touch any machine and doesn't connect to mechd**: it runs the
exact same pipeline as a real deployment, minus the "persist + dispatch"
steps. So it doubles as both a --dry-run tool and an incident postmortem
tool — "why is this machine running this config" must be answerable
offline.

Secrets use one-off values in offline mode: the spec holds references, not
plaintext, so the output can be shared directly — but its digest isn't
comparable to production's (see the note at the end of the output).`,
		Example: `  mechctl component render -f plan.yaml
  mechctl component render -f plan.yaml --pack ./examples/packs/zookeeper -o ./out`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if planPath == "" {
				return validationErr(fmt.Errorf("input file required, use -f"))
			}
			res, err := runRender(planPath, packDir)
			if err != nil {
				return err
			}
			return emit(cmd, res, outDir, showWarnings)
		},
	}
	cmd.Flags().StringVarP(&planPath, "file", "f", "", "Render input file (YAML)")
	cmd.Flags().StringVar(&packDir, "pack", "", "Pack directory, overrides the pack field in the input file")
	// 不给 -o 简写：它在根命令上已经是 --output（输出格式，
	// 全局唯一一份）。抢过来会在**命令树合并时 panic**——而单独构造这个
	// 命令的单测看不到那一步，只有走真正的 mechctl 才会撞上，
	// 见 TestRenderCommandWiresIntoRealTree。
	cmd.Flags().StringVar(&outDir, "out", "", "Write each instance's spec to this directory, defaults to printing to stdout")
	cmd.Flags().BoolVar(&showWarnings, "warnings", true, "Print warnings")
	return cmd
}

func runRender(planPath, packOverride string) (*render.Result, error) {
	raw, err := os.ReadFile(planPath)
	if err != nil {
		return nil, err
	}
	var plan Plan
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true) // 拼错的字段必须报错——静默忽略会让人以为设置生效了
	if err := dec.Decode(&plan); err != nil {
		return nil, validationErr(fmt.Errorf("%s: %w", planPath, err))
	}

	dir := plan.Pack
	if packOverride != "" {
		dir = packOverride
	}
	if dir == "" {
		return nil, validationErr(fmt.Errorf("%s: no pack directory specified", planPath))
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(filepath.Dir(planPath), dir)
	}
	p, err := pack.Load(dir)
	if err != nil {
		return nil, validationErr(err)
	}

	req, err := plan.toRequest(p)
	if err != nil {
		return nil, validationErr(err)
	}
	res, err := render.Render(req)
	if err != nil {
		return nil, validationErr(err)
	}
	return res, nil
}

// toRequest 把输入文件变成 render.Request。
func (plan *Plan) toRequest(p *pack.Pack) (render.Request, error) {
	nodes := map[string]render.Node{}
	for _, n := range plan.Nodes {
		vols := map[string]render.Volume{}
		for _, v := range n.Volumes {
			vols[v.Name] = render.Volume{Path: v.Path, Class: v.Class}
		}
		nodes[n.Name] = render.Node{
			Name: n.Name, Address: n.Address,
			Labels: n.Labels, Roots: n.Roots, Volumes: vols, Facts: n.Facts,
		}
	}

	var insts []render.Instance
	for i, in := range plan.Instances {
		n, ok := nodes[in.Node]
		if !ok {
			return render.Request{}, fmt.Errorf(
				"instances[%d] is placed on node %q, but nodes doesn't have it\n  declared nodes: %s",
				i, in.Node, strings.Join(sortedNodeNames(nodes), ", "))
		}
		insts = append(insts, render.Instance{
			Role: in.Role, Ordinal: in.Ordinal,
			ConfigGroup: orDefault(in.ConfigGroup, "default"),
			Node:        n, PathBindings: in.Paths,
		})
	}

	reqs := map[string]render.Binding{}
	for name, d := range plan.Requires {
		b := render.Binding{
			Pack: orDefault(d.Pack, name), Component: d.Component,
			Version: d.Version, Scope: pack.DepScope(orDefault(d.Scope, "node")),
			Paths: d.Paths,
		}
		if len(d.Exports) > 0 {
			b.Exports = map[string]render.Export{}
			for ename, e := range d.Exports {
				ex := render.Export{Value: e.Value, Fields: e.Fields}
				if len(e.Sensitive) > 0 {
					ex.SensitiveFields = map[string]bool{}
					for _, f := range e.Sensitive {
						ex.SensitiveFields[f] = true
					}
				}
				b.Exports[ename] = ex
			}
		}
		for _, pr := range d.Topology {
			b.Topology = append(b.Topology, render.Peer{
				Node: pr.Node, Address: pr.Address, Ordinal: pr.Ordinal,
				Role: pr.Role, Labels: pr.Labels, Paths: pr.Paths,
			})
		}
		reqs[name] = b
	}

	return render.Request{
		Site: spec.SiteRef{
			Name: plan.Site.Name, Kind: plan.Site.Kind, Labels: plan.Site.Labels,
		},
		Component: orDefault(plan.Component, p.Name),
		Pack:      p,
		PackRef:   spec.PackRef{Name: p.Name, Version: p.Version, Revision: p.Revision},
		Profile:   plan.Profile,
		Instances: insts,
		Overrides: render.Overrides{
			Component: plan.Params.Component,
			Role:      plan.Params.Role,
			Group:     plan.Params.Group,
		},
		Requires: reqs,
		Secrets:  &ephemeralSecrets{values: map[string]string{}},
		// 载荷必须填：少了它，任何带 archive 的 Pack 都会渲染出一份
		// 「看着没问题、装上去报『规格中没有名为 main 的 blob』」的规格。
		// 与 mechd 共用同一个函数，两处不会分叉。
		Blobs: render.BlobsFor(p, render.DefaultPlatform),
	}, nil
}

// ephemeralSecrets 是离线模式下的密钥来源。
//
// 值只活在本次进程里，**不写任何地方**。这样离线渲染既不需要主密钥，
// 也不会往磁盘上留下新的口令——一条用于复盘的命令不该有副作用。
//
// 代价是 digest 与生产环境不可比：那边的密钥版本号来自真实的 Vault。
// 这条限制在输出里明说，而不是让人自己发现。
type ephemeralSecrets struct{ values map[string]string }

func (e *ephemeralSecrets) Ensure(component, param string, g pack.Generate) (render.StoredSecret, error) {
	if _, ok := e.values[param]; !ok {
		e.values[param] = "OFFLINE-RENDER-" + strings.ToUpper(param)
	}
	return render.StoredSecret{ID: "offline." + param, Version: 0, Value: e.values[param]}, nil
}

func (e *ephemeralSecrets) Store(component, param, value string) (render.StoredSecret, error) {
	e.values[param] = value
	return render.StoredSecret{ID: "offline." + param, Version: 0, Value: value}, nil
}

// emit 输出结果。
func emit(cmd *cobra.Command, res *render.Result, outDir string, showWarnings bool) error {
	out := cmd.OutOrStdout()

	for _, key := range res.Order {
		s := res.Specs[key]
		blob, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return err
		}
		if outDir == "" {
			fmt.Fprintf(out, "# ── %s ──\n%s\n", key, blob)
			continue
		}
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return err
		}
		name := strings.ReplaceAll(key, "@", "_") + ".json"
		if err := os.WriteFile(filepath.Join(outDir, name), append(blob, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "  %s  %s\n", name, shortDigest(s.Digest))
	}

	if showWarnings && len(res.Warnings) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "\nWarnings (%d):\n", len(res.Warnings))
		for _, w := range res.Warnings {
			fmt.Fprintf(cmd.ErrOrStderr(), "  - %s\n", w)
		}
	}
	if len(res.Secrets) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"\nNote: offline mode uses one-off secrets, the spec holds only references, not plaintext.\n"+
				"      So the digest here isn't comparable to production's.\n")
	}
	return nil
}

func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

func sortedNodeNames(m map[string]render.Node) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// ── 退出码 ──────────────────────────────────────────────────────────────

type validationError struct{ err error }

func (e validationError) Error() string { return e.err.Error() }
func (e validationError) Unwrap() error { return e.err }

func validationErr(err error) error { return validationError{err} }

// ExitCodeOf 把错误映射到退出码。
func ExitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var v validationError
	if ok := asValidation(err, &v); ok {
		return ExitValidation
	}
	return 1
}

func asValidation(err error, target *validationError) bool {
	for err != nil {
		if v, ok := err.(validationError); ok {
			*target = v
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
