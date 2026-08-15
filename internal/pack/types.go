// Package pack 实现 pack/v1 规范的解析、校验与组装。
//
// 规范见 docs/spec/pack-v1.md。本包中每条校验规则的编号（R01…R45）
// 与规范 §19「校验规则汇总」一一对应。
package pack

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// SchemaV1 是本实现支持的唯一 schema 标识。
const SchemaV1 = "pack/v1"

// Pack 是 pack.yaml 的完整结构。
type Pack struct {
	Schema      string       `yaml:"schema"`
	Name        string       `yaml:"name"`
	Version     string       `yaml:"version"`
	Revision    int          `yaml:"revision"`
	Description string       `yaml:"description"`
	Homepage    string       `yaml:"homepage"`
	License     string       `yaml:"license"`
	Maintainers []Maintainer `yaml:"maintainers"`
	Keywords    []string     `yaml:"keywords"`
	Platforms   []string     `yaml:"platforms"`

	UpgradePolicy *UpgradePolicy    `yaml:"upgradePolicy"`
	Requires      *Requires         `yaml:"requires"`
	Exports       map[string]Export `yaml:"exports"`
	Blobs         map[string]Blob   `yaml:"blobs"`
	Params        map[string]Param  `yaml:"params"`
	Paths         map[string]Path   `yaml:"paths"`
	Shared        *Shared           `yaml:"shared"`
	Roles         []Role            `yaml:"roles"`
	Placement     []Placement       `yaml:"placement"`
	Profiles      []Profile         `yaml:"profiles"`
	Hooks         Hooks             `yaml:"hooks"`

	// Dir 是 Pack 的源目录，解析时填入，用于定位 templates/ files/ hooks/。
	Dir string `yaml:"-"`
	// Doc 保留原始 YAML 节点树，供 lint 报告行号。
	Doc *yaml.Node `yaml:"-"`
}

// Maintainer 是维护者信息。
type Maintainer struct {
	Name  string `yaml:"name"`
	Email string `yaml:"email"`
}

// UpgradePolicy 声明升级兼容范围。
type UpgradePolicy struct {
	Compatible string `yaml:"compatible"`
}

// ── requires ────────────────────────────────────────────────────────────

// Requires 是前置条件。
type Requires struct {
	OS         *OSRequire        `yaml:"os"`
	Capability map[string]string `yaml:"capability"`
	Packs      []PackRequire     `yaml:"packs"`
	Resources  *ResourceRequire  `yaml:"resources"`
	Mecharion  string            `yaml:"mecharion"`
}

// OSRequire 是操作系统前置条件。
type OSRequire struct {
	Family     []string `yaml:"family"`
	MinVersion string   `yaml:"minVersion"`
	Glibc      string   `yaml:"glibc"`
	Kernel     string   `yaml:"kernel"`
	Cgroup     string   `yaml:"cgroup"`
}

// DepScope 是 Pack 依赖的作用域。
type DepScope string

const (
	// ScopeNode 表示依赖是本机磁盘上的文件，必须与本 Pack 的角色同节点。
	ScopeNode DepScope = "node"
	// ScopeSite 表示依赖是网络可达的服务，同 Site 内存在即可。
	ScopeSite DepScope = "site"
)

// PackRequire 是对另一个 Pack 的依赖。
type PackRequire struct {
	Name    string   `yaml:"name"`
	Version string   `yaml:"version"`
	Scope   DepScope `yaml:"scope"`
}

// EffectiveScope 返回生效的 scope（缺省为 node）。
func (p PackRequire) EffectiveScope() DepScope {
	if p.Scope == "" {
		return ScopeNode
	}
	return p.Scope
}

// ResourceRequire 是资源下限。
type ResourceRequire struct {
	Memory   string `yaml:"memory"`
	DiskFree string `yaml:"diskFree"`
	CPU      int    `yaml:"cpu"`
}

// ── exports ─────────────────────────────────────────────────────────────

// Export 是对外导出的连接点。
type Export struct {
	Description string `yaml:"description"`
	Role        string `yaml:"role"`
	Port        string `yaml:"port"`
	Separator   string `yaml:"separator"`
	Format      string `yaml:"format"`

	// Fields 是具名字段，供消费方自行组装（规范 §5.4）。
	//
	// 带凭据的连接无法用 Format 表达——提供方没法知道消费方要 libpq DSN、
	// JDBC URL 还是拆开的字段。让它猜，等于把消费方的实现细节写进提供方。
	Fields map[string]string `yaml:"fields"`
}

// FieldNames 返回字段名，按字典序。
func (e Export) FieldNames() []string {
	out := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── blobs ───────────────────────────────────────────────────────────────

// Blob 是一个载荷在各平台上的条目：平台键 → 条目。
type Blob map[string]BlobEntry

// BlobEntry 是单个平台的载荷描述。
type BlobEntry struct {
	SHA256    string `yaml:"sha256"`
	Size      int64  `yaml:"size"`
	Filename  string `yaml:"filename"`
	SourceURL string `yaml:"sourceUrl"`
	MediaType string `yaml:"mediaType"`
}

// MediaType 取值。
const (
	MediaTar           = "tar"
	MediaTarGz         = "tar.gz"
	MediaTarZst        = "tar.zst"
	MediaZip           = "zip"
	MediaRaw           = "raw"
	MediaDockerArchive = "docker-archive"
)

// ── paths ───────────────────────────────────────────────────────────────

// PathKind 是路径的重数。
type PathKind string

const (
	// KindSingle 是单一路径（默认）。
	KindSingle PathKind = "single"
	// KindMulti 是多路径，渲染为列表。
	KindMulti PathKind = "multi"
)

// PathLayout 是配置相对 generation 的布局。
type PathLayout string

const (
	// LayoutSeparate 是默认布局：物理存储在 generation 之外。
	LayoutSeparate PathLayout = "separate"
	// LayoutInline 让配置直接位于 generation 内，每次变更产生新 generation。
	LayoutInline PathLayout = "inline"
)

// Path 是一个路径声明。Default 可能是字符串或字符串列表，因此用 yaml.Node 承载。
type Path struct {
	Default  yaml.Node  `yaml:"default"`
	Kind     PathKind   `yaml:"kind"`
	Layout   PathLayout `yaml:"layout"`
	LinkInto string     `yaml:"linkInto"`
	DistDir  string     `yaml:"distDir"`
	Subpath  string     `yaml:"subpath"`
	Mode     string     `yaml:"mode"`
	Owner    string     `yaml:"owner"`
	Group    string     `yaml:"group"`
}

// EffectiveKind 返回生效的 kind（缺省为 single）。
func (p Path) EffectiveKind() PathKind {
	if p.Kind == "" {
		return KindSingle
	}
	return p.Kind
}

// EffectiveLayout 返回生效的 layout（缺省为 separate）。
func (p Path) EffectiveLayout() PathLayout {
	if p.Layout == "" {
		return LayoutSeparate
	}
	return p.Layout
}

// DefaultStrings 把 Default 归一化为字符串切片。
func (p Path) DefaultStrings() ([]string, error) {
	switch p.Default.Kind {
	case 0:
		return nil, nil
	case yaml.ScalarNode:
		return []string{p.Default.Value}, nil
	case yaml.SequenceNode:
		var out []string
		if err := p.Default.Decode(&out); err != nil {
			return nil, err
		}
		return out, nil
	default:
		return nil, fmt.Errorf("default must be a string or a list of strings")
	}
}

// DefaultIsList 报告 Default 是否写成了列表形式。
func (p Path) DefaultIsList() bool { return p.Default.Kind == yaml.SequenceNode }

// ── shared / roles ──────────────────────────────────────────────────────

// Shared 是各角色共有的部分。
type Shared struct {
	Resources []Resource `yaml:"resources"`
}

// Role 是 Pack 内定义的角色类型。
type Role struct {
	Name        string           `yaml:"name"`
	Description string           `yaml:"description"`
	Scope       string           `yaml:"scope"`
	Cardinality string           `yaml:"cardinality"`
	Quorum      bool             `yaml:"quorum"`
	Requires    []string         `yaml:"requires"`
	Params      map[string]Param `yaml:"params"`
	Paths       map[string]Path  `yaml:"paths"`
	Resources   []Resource       `yaml:"resources"`
	Workload    *Workload        `yaml:"workload"`
	Health      *Health          `yaml:"health"`
	Hooks       Hooks            `yaml:"hooks"`
}

// EffectiveName 返回角色名，单角色 Pack 省略时为 "default"。
func (r Role) EffectiveName() string {
	if r.Name == "" {
		return "default"
	}
	return r.Name
}

// EffectiveCardinality 返回 cardinality，缺省为 "0-N"。
func (r Role) EffectiveCardinality() string {
	if r.Cardinality == "" {
		return "0-N"
	}
	return r.Cardinality
}

// ── placement ───────────────────────────────────────────────────────────

// Enforcement 是放置约束的强制级别。
type Enforcement string

const (
	// EnforceRequired 表示违反则拒绝放置。
	EnforceRequired Enforcement = "required"
	// EnforcePreferred 表示违反时仅告警。
	EnforcePreferred Enforcement = "preferred"
)

// Placement 是角色之间的位置关系约束。
type Placement struct {
	AntiAffinity []string    `yaml:"antiAffinity"`
	Affinity     []string    `yaml:"affinity"`
	Scope        string      `yaml:"scope"`
	Enforcement  Enforcement `yaml:"enforcement"`
	Reason       string      `yaml:"reason"`
}

// EffectiveScope 返回 scope，缺省为 node。
func (p Placement) EffectiveScope() string {
	if p.Scope == "" {
		return "node"
	}
	return p.Scope
}

// EffectiveEnforcement 返回 enforcement，缺省为 required。
func (p Placement) EffectiveEnforcement() Enforcement {
	if p.Enforcement == "" {
		return EnforceRequired
	}
	return p.Enforcement
}

// Roles 返回该约束涉及的角色列表。
func (p Placement) Roles() []string {
	if len(p.AntiAffinity) > 0 {
		return p.AntiAffinity
	}
	return p.Affinity
}

// ── profiles ────────────────────────────────────────────────────────────

// Profile 是一个部署形态。
type Profile struct {
	Name        string                 `yaml:"name"`
	Description string                 `yaml:"description"`
	Default     bool                   `yaml:"default"`
	MinNodes    int                    `yaml:"minNodes"`
	UpgradeFrom []string               `yaml:"upgradeFrom"`
	Roles       map[string]ProfileRole `yaml:"roles"`
	Params      map[string]Param       `yaml:"params"`
	Requires    *Requires              `yaml:"requires"`
	Placement   []Placement            `yaml:"placement"`
}

// ProfileRole 是 profile 对某个角色的覆盖。
type ProfileRole struct {
	Enabled     *bool  `yaml:"enabled"`
	Cardinality string `yaml:"cardinality"`
}

// IsEnabled 报告该角色在本 profile 中是否启用（缺省为启用）。
func (p ProfileRole) IsEnabled() bool {
	return p.Enabled == nil || *p.Enabled
}
