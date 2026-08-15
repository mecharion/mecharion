package mechd

import (
	"context"
	"sort"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/render"
	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/store"
)

// FormView 是一个组件的参数表单（23-web-ui §4.2）。
//
// 表单挂在 **role + configGroup** 上，不是挂在组件上：参数覆盖本来就是
// 按组走的（ADR-0021）。少了这两个坐标，界面回答不了「我改的是谁」，
// 而那是最常见一类误操作的来源——以为在改一台机器，实际改的是整个角色。
type FormView struct {
	Component string `json:"component"`
	Pack      string `json:"pack"`
	Version   string `json:"version"`
	Profile   string `json:"profile,omitempty"`

	// Roles 是这个 Pack 声明的全部角色，供界面切换。
	Roles []string `json:"roles"`
	Role  string   `json:"role"`
	// Groups 是当前角色下的配置组。
	Groups []GroupView `json:"groups,omitempty"`
	// Group 为空表示正在看角色级取值。
	Group string `json:"group,omitempty"`

	Params []render.FormParam `json:"params"`
	// Warnings 来自解析：defaultFrom 求值失败一类，不阻断但要说。
	Warnings []string `json:"warnings,omitempty"`
}

// GroupView 是一个配置组。成员要列出来——「影响哪几台机器」是选组时
// 唯一真正要看的信息。
type GroupView struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

// Form 组装一个组件的参数表单。
//
// **它跑一次真正的解析**，不走捷径：`from` 与 `defaultFrom` 的值依赖
// 拓扑（provider 的 exports 先算完）与放置结果，靠读声明是算不出来的。
// 而 `from` 参数是只读展示——展示需要值。
func (s *Service) Form(
	ctx context.Context, siteName, compName, role, group string,
) (*FormView, error) {
	site, err := s.resolveSite(ctx, siteName)
	if err != nil {
		return nil, err
	}
	comp, err := s.Repos.Components().GetByName(ctx, site.ID, compName)
	if err != nil {
		return nil, err
	}
	entry, err := s.Packs.Resolve(comp.Pack.Name, "="+comp.Pack.Version)
	if err != nil {
		return nil, faults.Permanentf("", "parsing Pack %s@%s: %w",
			comp.Pack.Name, comp.Pack.Version, err)
	}
	p := entry.Pack

	roles := roleNames(p)
	if role == "" {
		if len(roles) == 0 {
			return nil, faults.Permanentf("", "Pack %s declares no roles", p.Name)
		}
		role = roles[0]
	} else if !contains(roles, role) {
		return nil, faults.Permanentf("", "Pack %s has no role %q, available: %v", p.Name, role, roles)
	}

	groups, err := s.Repos.ConfigGroups().List(ctx, comp.ID)
	if err != nil {
		return nil, err
	}
	if group != "" && !hasGroup(groups, role, group) {
		return nil, faults.Permanentf("", "role %s has no config group %q", role, group)
	}

	view := &FormView{
		Component: comp.Name, Pack: comp.Pack.Name, Version: comp.Pack.Version,
		Profile: comp.Profile, Roles: roles, Role: role, Group: group,
	}
	for _, g := range groups {
		if g.Role == role {
			view.Groups = append(view.Groups, GroupView{Name: g.Name, Members: g.Members})
		}
	}
	sort.Slice(view.Groups, func(i, j int) bool {
		return view.Groups[i].Name < view.Groups[j].Name
	})

	derived, warnings := s.derivedValues(ctx, site, comp, p, role, group)
	view.Warnings = warnings

	view.Params = render.Form(render.FormRequest{
		Pack: p, Profile: comp.Profile, Role: role, Group: group,
		Overrides: overridesFrom(comp, groups),
		Derived:   derived,
		SecretSet: s.secretSetter(ctx, comp.ID),
	})
	return view, nil
}

// derivedValues 跑一次解析，取出 from / defaultFrom 算出来的值。
//
// **解析失败不是致命的**：一个依赖还没部署的组件解析不出 from，而此时
// 用户恰恰需要打开表单去配它。那种情况下 from 字段标成「部署后确定」，
// 其余字段照常显示——把整张表单变成一条错误信息才是最没用的反应。
func (s *Service) derivedValues(
	ctx context.Context, site store.Site, comp store.Component,
	p *pack.Pack, role, group string,
) (map[string]any, []string) {
	insts, byID, err := s.existingInstances(ctx, comp.ID)
	if err != nil || len(insts) == 0 {
		return nil, nil
	}
	inputs, err := s.freezeFacts(ctx, comp.ID, assignmentsOf(insts, byID), insts, false)
	if err != nil {
		s.log().Warn("failed to snapshot facts for form, from will show as pending",
			"component", comp.Name, "err", err)
		return nil, nil
	}
	res, err := s.renderComponent(ctx, site, comp, p, inputs, true)
	if err != nil {
		s.log().Warn("failed to render form, from will show as pending",
			"component", comp.Name, "err", err)
		return nil, []string{"some values could not be computed right now: " + err.Error()}
	}

	// 取哪一个实例的解析结果？**同角色同组内它们的取值相同**——
	// 逐实例才不同的是 defaultFrom（按机器事实算），而那正是这里要
	// 说清楚的：表单编辑的是一层覆盖，不是一台机器。
	sp := firstSpecOf(res, role, group)
	if sp == nil {
		return nil, res.Warnings
	}
	out := map[string]any{}
	for name, pv := range sp.Params {
		if pv.Sensitive {
			continue // 值本来就没带出来
		}
		out[name] = pv.Value
	}
	return out, res.Warnings
}

// firstSpecOf 按 Order 取该角色（可选该组）的第一个实例规格。
//
// 按 Order 而不是遍历 map：map 顺序随机会让 defaultFrom 那类逐实例取值
// 在每次刷新时跳来跳去，而它看上去像「配置在自己变」。
func firstSpecOf(res *render.Result, role, group string) *spec.ResolvedSpec {
	for _, key := range res.Order {
		sp := res.Specs[key]
		if sp == nil || sp.Role != role {
			continue
		}
		if group != "" && sp.ConfigGroup != group {
			continue
		}
		return sp
	}
	return nil
}

// secretSetter 报告某个 secret 参数在保管库里有没有值。
func (s *Service) secretSetter(ctx context.Context, compID int64) func(string) bool {
	rows, err := s.Vault.List(ctx, compID)
	if err != nil {
		s.log().Warn("failed to query secret collection, secrets will all show as unset", "err", err)
		return func(string) bool { return false }
	}
	set := map[string]bool{}
	for _, r := range rows {
		set[r.Param] = true
	}
	return func(name string) bool { return set[name] }
}

// roleNames 返回全部角色名。
//
// **必须走 EffectiveName**：单角色 Pack 可以省略 `name`，那时角色叫
// `default`。直接读 `r.Name` 会得到一个空名字——表单上角色下拉框空着，
// 而 RoleByName 用的是 EffectiveName，于是角色层的参数声明一条都合不进来。
// 这个缺陷是拿真集群跑出来的：go-webapp 正是这样一个省略了角色名的 Pack。
func roleNames(p *pack.Pack) []string {
	out := make([]string, 0, len(p.Roles))
	for _, r := range p.Roles {
		out = append(out, r.EffectiveName())
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func hasGroup(groups []store.ConfigGroup, role, name string) bool {
	for _, g := range groups {
		if g.Role == role && g.Name == name {
			return true
		}
	}
	return false
}

// PackForm 返回一个**尚未部署**的 Pack 的参数声明。
//
// 与 Form 的区别是它没有任何取值来源：没有实例、没有配置组、没有解析
// 结果，因此 `from` 与 `defaultFrom` 只能标成 Pending（「部署后确定」）。
//
// 它的第一个用途是让 `mechctl component deploy` **在发出请求之前**就知道
// 哪些参数是 secret——那条「不能用 --set 传明文」的规则必须在客户端执行，
// 因为服务端分辨不出一个值是 `--set` 还是 `--set-file` 传来的，而风险
// （shell 历史、ps 输出）本来就全在客户端（10-cli §4.3）。
func (s *Service) PackForm(
	ctx context.Context, packName, version, profile, role string,
) (*FormView, error) {
	if err := s.Packs.Reload(); err != nil {
		s.log().Warn("failed to rescan Pack collection, keeping the previous index", "err", err)
	}
	constraint := ""
	if version != "" {
		constraint = "=" + version
	}
	entry, err := s.Packs.Resolve(packName, constraint)
	if err != nil {
		return nil, faults.Permanentf("", "parsing Pack %s: %w", packName, err)
	}
	p := entry.Pack

	roles := roleNames(p)
	if role == "" {
		if len(roles) == 0 {
			return nil, faults.Permanentf("", "Pack %s declares no roles", p.Name)
		}
		role = roles[0]
	} else if !contains(roles, role) {
		return nil, faults.Permanentf("", "Pack %s has no role %q, available: %v", p.Name, role, roles)
	}

	return &FormView{
		Pack: p.Name, Version: p.Version, Profile: profile,
		Roles: roles, Role: role,
		Params: render.Form(render.FormRequest{
			Pack: p, Profile: profile, Role: role,
		}),
	}, nil
}
