package pack

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// PackFile 是 Pack 目录中清单文件的固定名字。
const PackFile = "pack.yaml"

// 约定的子目录名。
const (
	DirTemplates = "templates"
	DirFiles     = "files"
	DirHooks     = "hooks"
	DirBlobs     = "blobs"
)

// Load 从目录读取并解析 pack.yaml。
// 它只做**语法与结构**解析；语义校验由 Lint 负责。
func Load(dir string) (*Pack, error) {
	manifest := filepath.Join(dir, PackFile)
	data, err := os.ReadFile(manifest)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", manifest, err)
	}
	p, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", manifest, err)
	}
	p.Dir = dir
	return p, nil
}

// Parse 解析 pack.yaml 的字节内容。
func Parse(data []byte) (*Pack, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	// 空文件
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("file is empty")
	}

	var p Pack
	if err := doc.Content[0].Decode(&p); err != nil {
		return nil, err
	}
	p.Doc = doc.Content[0]

	// 常量默认值（规范 §1「只允许常量默认值，不允许从其他字段推导」）
	if p.Revision == 0 {
		p.Revision = 1
	}
	return &p, nil
}

// ── 行号定位 ────────────────────────────────────────────────────────────

// LineOf 返回给定字段路径在 pack.yaml 中的行号；找不到时返回 0。
// 路径支持映射键与数组下标，例如 LineOf("roles", "0", "cardinality")。
func (p *Pack) LineOf(path ...string) int {
	n := nodeAt(p.Doc, path...)
	if n == nil {
		return 0
	}
	return n.Line
}

func nodeAt(n *yaml.Node, path ...string) *yaml.Node {
	cur := n
	for _, seg := range path {
		if cur == nil {
			return nil
		}
		switch cur.Kind {
		case yaml.MappingNode:
			found := false
			for i := 0; i+1 < len(cur.Content); i += 2 {
				if cur.Content[i].Value == seg {
					cur = cur.Content[i+1]
					found = true
					break
				}
			}
			if !found {
				return nil
			}
		case yaml.SequenceNode:
			idx := -1
			if _, err := fmt.Sscanf(seg, "%d", &idx); err != nil || idx < 0 || idx >= len(cur.Content) {
				return nil
			}
			cur = cur.Content[idx]
		default:
			return nil
		}
	}
	return cur
}

// ── 目录内容 ────────────────────────────────────────────────────────────

// HasFile 报告 Pack 目录下某个相对路径是否存在。
func (p *Pack) HasFile(rel string) bool {
	if p.Dir == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(p.Dir, filepath.FromSlash(rel)))
	return err == nil && !st.IsDir()
}

// ReadFile 读取 Pack 某个子目录下的一个文件。
//
// rel 用斜杠分隔（与 ListDir 的返回一致），在 Windows 上转成本地分隔符。
func (p *Pack) ReadFile(sub, rel string) ([]byte, error) {
	if p.Dir == "" {
		return nil, fmt.Errorf("Pack has no source directory, cannot read %s/%s", sub, rel)
	}
	return os.ReadFile(filepath.Join(p.Dir, sub, filepath.FromSlash(rel)))
}

// ListDir 列出 Pack 某个子目录下的全部文件（相对该子目录，斜杠分隔）。
// 目录不存在时返回空切片而非错误。
func (p *Pack) ListDir(sub string) ([]string, error) {
	if p.Dir == "" {
		return nil, nil
	}
	root := filepath.Join(p.Dir, sub)
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	}
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out, err
}

// ── 便捷访问 ────────────────────────────────────────────────────────────

// RoleNames 返回全部角色名（已应用 default 兜底）。
func (p *Pack) RoleNames() []string {
	out := make([]string, 0, len(p.Roles))
	for _, r := range p.Roles {
		out = append(out, r.EffectiveName())
	}
	return out
}

// RoleByName 按名字查找角色。
func (p *Pack) RoleByName(name string) *Role {
	for i := range p.Roles {
		if p.Roles[i].EffectiveName() == name {
			return &p.Roles[i]
		}
	}
	return nil
}

// ProfileByName 按名字查找 profile。
func (p *Pack) ProfileByName(name string) *Profile {
	for i := range p.Profiles {
		if p.Profiles[i].Name == name {
			return &p.Profiles[i]
		}
	}
	return nil
}

// DefaultProfile 返回默认 profile；无 profiles 时返回 nil。
// 未显式标注 default 时取第一个（规范 §19 规则 16）。
func (p *Pack) DefaultProfile() *Profile {
	if len(p.Profiles) == 0 {
		return nil
	}
	for i := range p.Profiles {
		if p.Profiles[i].Default {
			return &p.Profiles[i]
		}
	}
	return &p.Profiles[0]
}

// AllResources 遍历 Pack 中的全部资源（shared + 各角色），
// 回调的 owner 形如 "shared" 或 "roles[namenode]"。
func (p *Pack) AllResources(fn func(owner string, idx int, r Resource)) {
	if p.Shared != nil {
		for i, r := range p.Shared.Resources {
			fn("shared", i, r)
		}
	}
	for _, role := range p.Roles {
		owner := fmt.Sprintf("roles[%s]", role.EffectiveName())
		for i, r := range role.Resources {
			fn(owner, i, r)
		}
	}
}

// AllHooks 遍历 Pack 中的全部 hook（顶层 + 各角色）。
func (p *Pack) AllHooks(fn func(owner, point string, hk Hook)) {
	p.Hooks.All(func(point string, hk Hook) { fn("hooks", point, hk) })
	for _, role := range p.Roles {
		owner := fmt.Sprintf("roles[%s].hooks", role.EffectiveName())
		role.Hooks.All(func(point string, hk Hook) { fn(owner, point, hk) })
	}
}

// AllParams 返回参数的合并视图：顶层 + 各角色 + 各 profile。
// 同名参数以先出现者为准，返回值仅用于「是否声明过」这类存在性判断。
func (p *Pack) AllParams() map[string]Param {
	out := map[string]Param{}
	add := func(m map[string]Param) {
		for k, v := range m {
			if _, ok := out[k]; !ok {
				out[k] = v
			}
		}
	}
	add(p.Params)
	for _, r := range p.Roles {
		add(r.Params)
	}
	for _, pr := range p.Profiles {
		add(pr.Params)
	}
	return out
}

// ExprCarriesSecret 报告一个表达式的取值是否会携带敏感信息。
//
// 判据是「它引用的参数里有没有 secret」——**推导而非声明**，因此不可能
// 与实际不一致。用于 `mechpack inspect` 展示导出契约，也是 mechd 在绑定
// 时自动传播敏感标记的依据（规范 §5.4）。
func (p *Pack) ExprCarriesSecret(expr string) bool {
	if expr == "" {
		return false
	}
	all := p.AllParams()
	for _, name := range ExprReferencedParams(expr) {
		if pv, ok := all[name]; ok && pv.IsSensitive() {
			return true
		}
	}
	return false
}

// EffectiveRole 是某个 profile 生效时一个角色的样子。
type EffectiveRole struct {
	Name        string
	Cardinality string
	Quorum      bool
	Requires    []string
	// Enabled 为 false 时该角色在本形态下不部署。
	Enabled bool
}

// RolesForProfile 返回某个 profile 生效时的角色集合，按声明顺序。
//
// profile 能覆盖的只有 [§13.3](../spec/pack-v1.md#133-profile-可以覆盖的五项)
// 列出的那几项——这里处理其中的 `cardinality` 与 `enabled`。
// **形态在 mechd 放置阶段被解析掉**，引擎侧不存在 profile 概念（ADR-0022）。
func (p *Pack) RolesForProfile(profile string) []EffectiveRole {
	var pr *Profile
	if profile != "" {
		pr = p.ProfileByName(profile)
	}
	out := make([]EffectiveRole, 0, len(p.Roles))
	for _, r := range p.Roles {
		e := EffectiveRole{
			Name:        r.EffectiveName(),
			Cardinality: r.Cardinality,
			Quorum:      r.Quorum,
			Requires:    r.Requires,
			Enabled:     true,
		}
		if pr != nil {
			if ov, ok := pr.Roles[e.Name]; ok {
				if ov.Cardinality != "" {
					e.Cardinality = ov.Cardinality
				}
				e.Enabled = ov.IsEnabled()
			}
		}
		out = append(out, e)
	}
	return out
}

// PlacementForProfile 返回某个 profile 生效时的放置约束。
//
// profile 的 placement 是**追加**而非替换——形态特有的约束（HA 下 NN 与
// SNN 不得同机）叠加在通用约束之上，而不是把后者覆盖掉。
func (p *Pack) PlacementForProfile(profile string) []Placement {
	out := append([]Placement(nil), p.Placement...)
	if profile == "" {
		return out
	}
	if pr := p.ProfileByName(profile); pr != nil {
		out = append(out, pr.Placement...)
	}
	return out
}

// CardinalityBounds 返回 cardinality 的下限与上限（上限 -1 表示 N）。
func CardinalityBounds(s string) (lo, hi int, ok bool) { return cardinalityBounds(s) }

// ParamsForProfile 返回某个 profile 生效时可见的参数集合。
// profile 为空表示未声明 profiles 的情形。
func (p *Pack) ParamsForProfile(profile string) map[string]Param {
	out := map[string]Param{}
	for k, v := range p.Params {
		out[k] = v
	}
	for _, r := range p.Roles {
		for k, v := range r.Params {
			out[k] = v
		}
	}
	if profile == "" {
		return out
	}
	if pr := p.ProfileByName(profile); pr != nil {
		for k, v := range pr.Params {
			out[k] = v
		}
	}
	return out
}

// ProfileNames 返回全部 profile 名；无 profiles 时返回 [""]，
// 便于「对每个形态各校验一遍」的循环统一处理。
func (p *Pack) ProfileNames() []string {
	if len(p.Profiles) == 0 {
		return []string{""}
	}
	out := make([]string, 0, len(p.Profiles))
	for _, pr := range p.Profiles {
		out = append(out, pr.Name)
	}
	return out
}

// HookScriptPath 把 hook 的 script 归一成 Pack 内的相对路径。
//
// 规范允许两种写法：`hooks/nn-format.sh` 与 `nn-format.sh`（spec §16.3）。
// **两处必须用同一个函数**——lint 曾经自己 TrimPrefix、渲染却无条件再
// 拼一次 `hooks/`，于是写全路径的 Pack 能过 lint，部署时却报
// `hooks/hooks/nn-format.sh: no such file`。规则写两遍就一定会分叉。
func HookScriptPath(script string) string {
	return path.Join(DirHooks, strings.TrimPrefix(script, DirHooks+"/"))
}
