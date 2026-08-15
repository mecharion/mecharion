package mechd

import (
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mecharion/mecharion/internal/faults"
)

// ApplyDoc 是 `mechctl apply -f` 的声明文件（24-lifecycle §2.5）。
//
// **顶层按名词分段**，字段名与 `component deploy` 的参数一一对应——
// `roles` / `set` / `setFile` / `require` / `profile` 都是已有的概念，
// 学一遍就够。
//
// 它**不是** `render -f plan.yaml`：那份是解析管线的入参（实例已放置、
// ordinal 已分配、依赖已绑定），是低一层的东西。这里说的是「我想要
// 什么」，放置由 mechd 算。
type ApplyDoc struct {
	Site       *ApplySite   `yaml:"site,omitempty" json:"site,omitempty"`
	Components []ApplyComp  `yaml:"components,omitempty" json:"components,omitempty"`
	Groups     []ApplyGroup `yaml:"configGroups,omitempty" json:"configGroups,omitempty"`
}

// ApplySite 是站点声明。
//
// 目前只用来核对名字：建站点是安装时的事，而一份声明文件把 kind 改了
// 也不该真的去改一个已存在的站点——那是一次迁移，不是一次收敛。
type ApplySite struct {
	Name string `yaml:"name" json:"name"`
	Kind string `yaml:"kind,omitempty" json:"kind,omitempty"`
}

// ApplyComp 是一个组件的期望状态。
type ApplyComp struct {
	Name string `yaml:"name" json:"name"`
	Pack string `yaml:"pack" json:"pack"`
	// Version 可省，省了就是本地最新。
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
	Profile string `yaml:"profile,omitempty" json:"profile,omitempty"`
	// Roles 是 角色 → 节点名。单角色 Pack 可以用 Nodes 作糖。
	Roles map[string][]string `yaml:"roles,omitempty" json:"roles,omitempty"`
	Nodes []string            `yaml:"nodes,omitempty" json:"nodes,omitempty"`
	// Set 是参数覆盖。
	//
	// **secret 类型的参数不能写在这里**：这份文件会进版本库、会被传阅，
	// 而那正是 `--set` 在 CLI 上被禁掉的同一条理由（10-cli §4.3）。
	Set map[string]any `yaml:"set,omitempty" json:"set,omitempty"`
	// SetFile 是 参数名 → 文件路径，secret 的唯一入口。
	SetFile map[string]string `yaml:"setFile,omitempty" json:"setFile,omitempty"`
	// Require 是显式依赖绑定：依赖名 → Component 名。
	Require map[string]string `yaml:"require,omitempty" json:"require,omitempty"`
}

// ApplyGroup 是一个配置组。
type ApplyGroup struct {
	Component string              `yaml:"component" json:"component"`
	Role      string              `yaml:"role" json:"role"`
	Name      string              `yaml:"name" json:"name"`
	Members   []string            `yaml:"members,omitempty" json:"members,omitempty"`
	Params    map[string]any      `yaml:"params,omitempty" json:"params,omitempty"`
	Paths     map[string][]string `yaml:"paths,omitempty" json:"paths,omitempty"`
}

// ParseApplyDoc 解析一份声明文件。
//
// **拼错的字段直接报错**（KnownFields）。一个被静默忽略的字段是这类
// 文件最贵的失败方式：文件看起来说了一件事，系统做的是另一件，而两者
// 都不报错——`reqiure:` 打错一个字母，依赖就悄悄没绑上。
func ParseApplyDoc(data []byte) (*ApplyDoc, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)

	var doc ApplyDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, faults.Permanentf("", "parsing apply doc: %w", err)
	}
	// **拒绝多文档**：`---` 分隔的第二段会被静默丢掉，而写的人以为它生效了。
	var extra ApplyDoc
	if err := dec.Decode(&extra); err == nil {
		return nil, faults.Permanentf("", "apply doc contains multiple YAML documents (separated by `---`)\n"+
			"  this format is single-document: merge the content into one components / configGroups list")
	}

	if err := doc.Validate(); err != nil {
		return nil, err
	}
	return &doc, nil
}

// Validate 做结构性校验。
//
// 只查这一层能查的：名字、重复、必填。真正的语义校验（Pack 在不在、
// 节点存不存在、角色对不对）由 deploy 那条路完成——**不重复一遍**，
// 两套校验迟早会不一致，而不一致的校验比没有校验更糟。
func (d *ApplyDoc) Validate() error {
	if len(d.Components) == 0 && len(d.Groups) == 0 {
		return faults.Permanentf("", "apply doc has no components or configGroups")
	}

	seen := map[string]bool{}
	for i, c := range d.Components {
		switch {
		case c.Pack == "":
			return faults.Permanentf("", "components[%d] is missing pack", i)
		case c.Name == "" && c.Pack == "":
			return faults.Permanentf("", "components[%d] is missing name", i)
		}
		name := c.EffectiveName()
		if seen[name] {
			// 同名两次是**真的会出事**的：后一条会覆盖前一条，而写的人
			// 多半以为它们是两个组件
			return faults.Permanentf("", "component %s appears twice in the file", name)
		}
		seen[name] = true

		if len(c.Roles) > 0 && len(c.Nodes) > 0 {
			return faults.Permanentf("", "component %s gives both roles and nodes -- "+
				"nodes is just sugar for single-role Packs, the two are mutually exclusive", name)
		}
		for k := range c.Set {
			if _, dup := c.SetFile[k]; dup {
				return faults.Permanentf("", "component %s's parameter %s appears in both set and setFile",
					name, k)
			}
		}
	}

	gseen := map[string]bool{}
	for i, g := range d.Groups {
		if g.Component == "" || g.Role == "" || g.Name == "" {
			return faults.Permanentf("", "configGroups[%d] requires component, role, and name all to be present", i)
		}
		k := g.Component + "/" + g.Role + "/" + g.Name
		if gseen[k] {
			return faults.Permanentf("", "config group %s appears twice in the file", k)
		}
		gseen[k] = true
	}
	return nil
}

// EffectiveName 返回组件名；省略时等于 Pack 名（与 deploy 一致）。
func (c ApplyComp) EffectiveName() string {
	if c.Name != "" {
		return c.Name
	}
	return c.Pack
}

// ComponentNames 返回文件里声明的全部组件名，按字典序。
func (d *ApplyDoc) ComponentNames() []string {
	out := make([]string, 0, len(d.Components))
	for _, c := range d.Components {
		out = append(out, c.EffectiveName())
	}
	sort.Strings(out)
	return out
}
