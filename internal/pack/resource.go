package pack

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// 已知的资源类型。规范 §14。
const (
	ResFile        = "file"
	ResTemplate    = "template"
	ResDirectory   = "directory"
	ResSymlink     = "symlink"
	ResArchive     = "archive"
	ResUser        = "user"
	ResGroup       = "group"
	ResSysctl      = "sysctl"
	ResLimits      = "limits"
	ResHostsEntry  = "hosts_entry"
	ResMount       = "mount"
	ResTimer       = "timer"
	ResSystemdUnit = "systemd_unit"
	ResCommand     = "command"
	ResScript      = "script"
	ResPackage     = "package"
)

// KnownResourceTypes 是全部合法资源类型，顺序即文档中的呈现顺序。
var KnownResourceTypes = []string{
	ResFile, ResTemplate, ResDirectory, ResSymlink, ResArchive,
	ResUser, ResGroup,
	ResSysctl, ResLimits, ResHostsEntry, ResMount, ResTimer, ResSystemdUnit,
	ResCommand, ResScript,
	ResPackage,
}

// NonHermeticResourceTypes 是需要外部仓库、无法离线的资源类型。
var NonHermeticResourceTypes = map[string]string{
	ResPackage: "需要 OS 包仓库",
}

// Resource 是一条资源声明。YAML 中形如 `{ <type>: { …args… } }`。
type Resource struct {
	// Type 是资源类型，如 "template"。
	Type string
	// Args 是该类型的参数节点，按需解码。
	Args yaml.Node
	// Line 是该资源在 pack.yaml 中的行号。
	Line int

	// 通用字段，解析时从 Args 中提取（规范 §14.1）。
	ID          string
	When        string
	DriftPolicy string
	Notify      string
}

// UnmarshalYAML 把单键映射解成 Resource。
func (r *Resource) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: resource must be a mapping of the form `- <type>: {...}`", n.Line)
	}
	if len(n.Content) != 2 {
		var keys []string
		for i := 0; i < len(n.Content); i += 2 {
			keys = append(keys, n.Content[i].Value)
		}
		return fmt.Errorf("line %d: resource must have exactly one type key, got %d %v",
			n.Line, len(n.Content)/2, keys)
	}

	r.Type = n.Content[0].Value
	r.Args = *n.Content[1]
	r.Line = n.Line

	// 通用字段与类型参数混在同一层，单独取出
	var common struct {
		ID          string `yaml:"id"`
		When        string `yaml:"when"`
		DriftPolicy string `yaml:"driftPolicy"`
		Notify      string `yaml:"notify"`
	}
	if r.Args.Kind == yaml.MappingNode {
		if err := r.Args.Decode(&common); err != nil {
			return fmt.Errorf("line %d: parsing common fields of resource %s failed: %w", n.Line, r.Type, err)
		}
	}
	r.ID, r.When, r.DriftPolicy, r.Notify = common.ID, common.When, common.DriftPolicy, common.Notify
	return nil
}

// Arg 取出参数中的一个标量字段值；不存在时返回空串。
func (r Resource) Arg(key string) string {
	if r.Args.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(r.Args.Content); i += 2 {
		if r.Args.Content[i].Value == key {
			return r.Args.Content[i+1].Value
		}
	}
	return ""
}

// HasArg 报告参数中是否声明了某个字段（即使其值为空）。
func (r Resource) HasArg(key string) bool {
	if r.Args.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(r.Args.Content); i += 2 {
		if r.Args.Content[i].Value == key {
			return true
		}
	}
	return false
}

// ── hooks ───────────────────────────────────────────────────────────────

// HookScope 是 hook 的执行范围。
type HookScope string

const (
	// ScopePerInstance 表示该角色的每个实例都执行（默认）。
	ScopePerInstance HookScope = "perInstance"
	// ScopeOnce 表示整个 Component 内只执行一次。
	ScopeOnce HookScope = "once"
)

// LifecyclePoints 是全部合法的生命周期点。
var LifecyclePoints = []string{
	"preInstall", "postInstall",
	"preUpgrade", "postUpgrade",
	"preRemove", "postRemove",
	"preStart", "postStart",
	"preStop", "postStop",
}

// Hook 是一条 hook 声明。
type Hook struct {
	Script  string    `yaml:"script"`
	Args    []string  `yaml:"args"`
	Scope   HookScope `yaml:"scope"`
	When    string    `yaml:"when"`
	Timeout string    `yaml:"timeout"`
	User    string    `yaml:"user"`

	// Line 是该 hook 在 pack.yaml 中的行号。
	Line int `yaml:"-"`
}

// EffectiveScope 返回生效的 scope（缺省 perInstance）。
func (h Hook) EffectiveScope() HookScope {
	if h.Scope == "" {
		return ScopePerInstance
	}
	return h.Scope
}

// HookList 承载一个生命周期点上的一个或多个 hook。
// YAML 中可以是字符串简写、单个对象或对象列表。
type HookList []Hook

// UnmarshalYAML 支持三种写法。
func (hl *HookList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*hl = HookList{{Script: n.Value, Line: n.Line}}
		return nil
	case yaml.MappingNode:
		var h Hook
		if err := n.Decode(&h); err != nil {
			return err
		}
		h.Line = n.Line
		*hl = HookList{h}
		return nil
	case yaml.SequenceNode:
		out := make(HookList, 0, len(n.Content))
		for _, item := range n.Content {
			switch item.Kind {
			case yaml.ScalarNode:
				out = append(out, Hook{Script: item.Value, Line: item.Line})
			case yaml.MappingNode:
				var h Hook
				if err := item.Decode(&h); err != nil {
					return err
				}
				h.Line = item.Line
				out = append(out, h)
			default:
				return fmt.Errorf("line %d: hook list item must be a string or a mapping", item.Line)
			}
		}
		*hl = out
		return nil
	default:
		return fmt.Errorf("line %d: hook must be a string, mapping, or list", n.Line)
	}
}

// Hooks 是生命周期点到 hook 列表的映射，另含全局 timeout。
type Hooks struct {
	Points  map[string]HookList
	Timeout string
}

// UnmarshalYAML 把 hooks 段解析为 Points + Timeout。
func (h *Hooks) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: hooks must be a mapping", n.Line)
	}
	h.Points = map[string]HookList{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i].Value, n.Content[i+1]
		if key == "timeout" {
			h.Timeout = val.Value
			continue
		}
		var hl HookList
		if err := val.Decode(&hl); err != nil {
			return fmt.Errorf("hooks.%s: %w", key, err)
		}
		h.Points[key] = hl
	}
	return nil
}

// All 按生命周期点的固定顺序遍历全部 hook。
func (h Hooks) All(fn func(point string, hk Hook)) {
	for _, p := range LifecyclePoints {
		for _, hk := range h.Points[p] {
			fn(p, hk)
		}
	}
}
