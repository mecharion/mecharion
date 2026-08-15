package mechd

import (
	"context"
	"sort"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/store"
)

// ApplyRequest 是一次 `mechctl apply -f`。
type ApplyRequest struct {
	Site string
	Doc  *ApplyDoc
	// Secrets 是 setFile 读出来的明文，键为 "<组件名>/<参数名>"。
	//
	// **由 CLI 读文件、服务端只收值**：路径是客户端的概念，mechd 上
	// 那个路径多半不存在（它可能在另一台机器上）。这与
	// `deploy --set-file` 是同一条路。
	Secrets map[string]any
	DryRun  bool
	Actor   string
}

// ApplyResult 是一次 apply 的结果。
type ApplyResult struct {
	// Components 逐个说明发生了什么，顺序与文件一致。
	Components []ApplyComponentResult `json:"components"`
	// Groups 是配置组的结果。
	Groups []ApplyGroupResult `json:"groups,omitempty"`
	// Extra 是**集群里有、文件里没有**的组件。
	//
	// **apply 不删它们**（§4 第 14 条）：一份写漏了的文件不该删掉线上
	// 组件。但差异也不能隐形——不列出来的话，一份写漏的文件与一份写对
	// 的文件输出一模一样。
	Extra  []string `json:"extra,omitempty"`
	DryRun bool     `json:"dryRun,omitempty"`
}

// ApplyComponentResult 是一个组件的处置结果。
type ApplyComponentResult struct {
	Name string `json:"name"`
	// Action 取 created | updated | unchanged | failed。
	Action string `json:"action"`
	// Instances 是这个组件最终的实例数。
	Instances int    `json:"instances,omitempty"`
	Error     string `json:"error,omitempty"`
	// Removing 表示它正在被移除，本次跳过了。
	Removing bool `json:"removing,omitempty"`
}

// ApplyGroupResult 是一个配置组的处置结果。
type ApplyGroupResult struct {
	Component string `json:"component"`
	Role      string `json:"role"`
	Name      string `json:"name"`
	Action    string `json:"action"`
	Error     string `json:"error,omitempty"`
}

// Apply 把集群收敛到一份声明文件描述的状态（10-cli §5）。
//
// **它是 deploy 的编排，不是第二条部署路径。** 每个组件都走
// `Deploy(--update)`——那条路上的放置校验、依赖拓扑、渲染、下发一个都
// 不能少，而另写一套迟早会与它分叉。
//
// **不做删除**（§4 第 14 条）：声明式不等于「文件里没有就删」。一份写漏
// 了的文件不该删掉线上组件；删除必须走显式的 `component remove`。
func (s *Service) Apply(ctx context.Context, req ApplyRequest) (*ApplyResult, error) {
	if req.Doc == nil {
		return nil, faults.Permanentf("", "no declared content")
	}
	site, err := s.resolveSite(ctx, req.Site)
	if err != nil {
		return nil, err
	}
	if err := checkSiteMatches(req.Doc.Site, site); err != nil {
		return nil, err
	}

	out := &ApplyResult{DryRun: req.DryRun}

	// ── ① 组件 ──
	//
	// **顺序照文件写的来**，不重排。依赖的解析顺序由 Deploy 内部按
	// requires 拓扑排序处理（那是它已有的能力），而这里若自作主张重排，
	// 出错时的输出顺序就与人写的文件对不上了。
	for _, c := range req.Doc.Components {
		out.Components = append(out.Components,
			s.applyComponent(ctx, site, c, req))
	}

	// ── ② 配置组 ──
	//
	// 排在组件之后：配置组挂在组件上，组件还不存在时它无处可挂。
	for _, g := range req.Doc.Groups {
		out.Groups = append(out.Groups, s.applyGroup(ctx, g, req))
	}

	// ── ③ 多余的组件：列出来，不动它们 ──
	if out.Extra, err = s.extraComponents(ctx, site, req.Doc); err != nil {
		// 算不出来不该让整次 apply 失败——它只是一条提示
		s.log().Warn("could not compute which components are missing from the file", "err", err)
	}
	return out, nil
}

// applyComponent 收敛一个组件。
func (s *Service) applyComponent(
	ctx context.Context, site store.Site, c ApplyComp, req ApplyRequest,
) ApplyComponentResult {
	name := c.EffectiveName()
	res := ApplyComponentResult{Name: name}

	// 正在移除的组件**跳过而不是失败**。
	//
	// 一次 apply 撞上一个正在被删的组件是完全正常的（删除是异步的），
	// 让整份文件因此失败，会把「等它删完」变成「先删掉这一段再跑」。
	if cur, err := s.Repos.Components().GetByName(ctx, site.ID, name); err == nil && cur.Removing() {
		res.Action, res.Removing = "skipped", true
		return res
	}

	existed := false
	if _, err := s.Repos.Components().GetByName(ctx, site.ID, name); err == nil {
		existed = true
	}

	set := map[string]any{}
	for k, v := range c.Set {
		set[k] = v
	}
	// setFile 读出来的明文由 CLI 传进来
	for k, v := range req.Secrets {
		comp, param, ok := splitSecretKey(k)
		if ok && comp == name {
			set[param] = v
		}
	}

	dep, err := s.Deploy(ctx, DeployRequest{
		Site: req.Site, Pack: c.Pack, Component: c.Name, Profile: c.Profile,
		Roles: c.Roles, Nodes: c.Nodes, Set: set, Require: c.Require,
		// **总是 --update**：apply 的语义就是收敛到声明的状态。
		// 不加的话第二次 apply 会撞上「组件已存在，重复 deploy 默认拒绝」
		// ——而幂等正是这条命令的全部意义。
		Update: true,
		DryRun: req.DryRun,
		Actor:  req.Actor,
	})
	if err != nil {
		res.Action, res.Error = "failed", err.Error()
		return res
	}
	res.Instances = len(dep.Specs)
	switch {
	case !existed:
		res.Action = "created"
	default:
		res.Action = "updated"
	}
	return res
}

// applyGroup 收敛一个配置组。
func (s *Service) applyGroup(
	ctx context.Context, g ApplyGroup, req ApplyRequest,
) ApplyGroupResult {
	res := ApplyGroupResult{
		Component: g.Component, Role: g.Role, Name: g.Name, Action: "updated",
	}
	if req.DryRun {
		res.Action = "unchanged"
		return res
	}
	_, err := s.SaveGroup(ctx, GroupRequest{
		Site: req.Site, Component: g.Component, Role: g.Role, Name: g.Name,
		Members: g.Members, Params: g.Params, Paths: g.Paths,
		Actor: req.Actor,
	})
	if err != nil {
		res.Action, res.Error = "failed", err.Error()
	}
	return res
}

// extraComponents 返回**集群里有、文件里没有**的组件名。
//
// 它们不会被动，但必须被说出来：不列的话，一份写漏了的文件与一份写对
// 的文件输出一模一样，而那正是声明式最容易出的事故。
func (s *Service) extraComponents(
	ctx context.Context, site store.Site, doc *ApplyDoc,
) ([]string, error) {
	declared := map[string]bool{}
	for _, n := range doc.ComponentNames() {
		declared[n] = true
	}
	comps, err := s.Repos.Components().List(ctx, site.ID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range comps {
		if !declared[c.Name] {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// checkSiteMatches 核对声明里的站点名。
//
// **只核对，不改。** 建站点是安装时的事；一份文件把 kind 从 standalone
// 改成 cluster 也不该真的去改一个已存在的站点——那是一次迁移，不是一次
// 收敛，而把它伪装成收敛是这类工具最危险的做法。
func checkSiteMatches(want *ApplySite, got store.Site) error {
	if want == nil || want.Name == "" {
		return nil
	}
	if want.Name != got.Name {
		return faults.Permanentf("",
			"the apply doc's site is %q, but this mechd manages %q\n"+
				"  a site name mismatch usually means the file was used in the wrong place -- it will not switch automatically",
			want.Name, got.Name)
	}
	if want.Kind != "" && want.Kind != got.Kind {
		return faults.Permanentf("",
			"the apply doc declares site %s's kind as %q, but it is actually %q\n"+
				"  changing kind is a migration, not a reconciliation, and apply does not do that",
			got.Name, want.Kind, got.Kind)
	}
	return nil
}

// splitSecretKey 拆 "<组件>/<参数>"。
func splitSecretKey(k string) (comp, param string, ok bool) {
	for i := 0; i < len(k); i++ {
		if k[i] == '/' {
			return k[:i], k[i+1:], true
		}
	}
	return "", "", false
}
