// Package render 把「Pack + 用户输入 + 节点事实」变成每个 RoleInstance 的
// ResolvedSpec。
//
// 设计见 docs/design/15-render-pipeline.md。这是 mechd 里最长的一段逻辑，
// 也是最需要**顺序正确**的一段——每一步都依赖前一步的产出，而依赖关系
// 不是显然的：
//
//	① 参数链（声明默认 → Component → Role → ConfigGroup）
//	② defaultFrom      需要 Node.Facts，逐实例
//	③ generate         需要 SecretVault，仅首次
//	④ paths            需要 ①–③ 与 Node.Roots
//	⑤ topology         需要**全部实例**的 ④ 都已完成
//	⑥ from             需要 ⑤ 与依赖绑定
//	⑦ 渲染 resources / workload / health / hooks
//	⑧ 封装：notify 最终动作、密钥占位、Seal
//
// ⑤ 是两趟遍历的分界：拓扑里每个对等实例都带着**它自己那台节点上**解析出的
// 路径，因此必须等所有实例都走完 ④ 才建得起来。
//
// 除 generate 首次生成与 ordinal 首次分配外，**整条管线是纯函数**——
// 这不是调试便利，是事故复盘的第一手材料：「为什么这台机器上是这份配置」
// 必须能离线回答。
package render

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/spec"
)

// Request 是一次解析请求，覆盖一个 Component 的全部实例。
type Request struct {
	Site      spec.SiteRef
	Component string
	Pack      *pack.Pack
	PackRef   spec.PackRef
	Profile   string

	// Instances 是放置的结果，ordinal 已分配。
	Instances []Instance
	Overrides Overrides

	// Requires 是**已绑定**的依赖：依赖名 → 绑定结果。
	//
	// 绑定与 provider 的 exports 求值都发生在本次调用之前——那需要
	// 按 requires 图拓扑排序先跑完 provider 的管线（15-render-pipeline §5）。
	// 让 Render 只负责一个 Component，是为了让那个顺序显式可见。
	Requires map[string]Binding

	Secrets   SecretStore
	Blobs     []spec.BlobRef
	Reconcile spec.ReconcileOptions

	// DriftPolicy 是站点侧对 Pack 声明的覆盖，**只能放松不能收紧**
	// （06-state-and-drift §4.2）。空串表示没有覆盖。
	//
	// 它在这里而不是在 mechlet 侧生效：规格里的 driftPolicy 是**已经算好
	// 的最终值**，mechlet 不做任何判断（ADR-0006）。
	DriftPolicy string

	// Previous 是上一版规格，键为 InstanceRef.Key()。
	// 用于算 notify 的最终动作——只有 mechd 知道哪些参数变了。
	Previous map[string]*spec.ResolvedSpec

	// DoneOnce 记录已执行过的 scope:once hook，键为 "<point>/<script>"。
	DoneOnce map[string]bool
}

// Instance 是一个待渲染的角色实例。
type Instance struct {
	Role        string
	Ordinal     int
	ConfigGroup string
	Node        Node
	// PathBindings 是 ConfigGroup 上按卷名做的多盘绑定：路径名 → 卷名列表。
	PathBindings map[string][]string
}

// Key 是实例在 Previous / 结果映射中的键。
func (i Instance) Key() string { return i.Role + "@" + i.Node.Name }

// Node 是渲染需要的节点信息。
type Node struct {
	Name    string
	Address string
	Labels  map[string]string
	Roots   map[string]string
	Volumes map[string]Volume
	// Facts 是**放置时快照**，不是实时值（spec §9.4.1）。
	Facts map[string]any
}

// Volume 是节点上的一块存储。
type Volume struct {
	Path  string `json:"path"`
	Class string `json:"class,omitempty"`
}

// Overrides 是用户在三个层级上给出的参数取值。
//
// **不存在第四层**：单节点的差异表现为一个只含该节点的 ConfigGroup。
// 允许无名的 per-node 覆盖，就是允许配置雪花化（ADR-0021）。
type Overrides struct {
	Component map[string]any
	Role      map[string]map[string]any
	Group     map[string]map[string]any
}

// Binding 是一条依赖的解析结果。
type Binding struct {
	Pack      string
	Component string
	Version   string
	Scope     pack.DepScope
	// Paths 仅 scope:node 时有意义。
	Paths map[string][]string
	// Exports 是已求值的导出。
	Exports map[string]Export
	// Topology 是 provider 的实例快照，仅 scope:site 时需要。
	Topology []Peer
}

// Export 是一条已求值的导出。
type Export struct {
	// Value 是 format 形态的结果。
	Value string
	// Fields 是 fields 形态的结果。两者互斥。
	Fields map[string]string
	// SensitiveFields 标出哪些字段引用了 secret 参数。
	//
	// 这份标记**只有 mechd 算得出**：lint 一次只看得见一个 Pack，
	// 而消费方可能来自别处、单独发布（spec §5.4）。
	SensitiveFields map[string]bool
}

// Peer 是拓扑中的一个对等实例。
type Peer struct {
	Node    string
	Address string
	Ordinal int
	Role    string
	Labels  map[string]string
	Paths   map[string][]string
}

// SecretStore 是密钥的生成与读取入口。
type SecretStore interface {
	// Ensure 返回 (component, param) 对应的密钥，**不存在时才生成**。
	//
	// 每轮调和都重新生成会让密码每 60 秒换一次，服务永远连不上——
	// 固化不是优化，是正确性（16-secrets §2）。
	Ensure(component, param string, g pack.Generate) (StoredSecret, error)

	// Store 固化一个用户给的敏感值。
	//
	// 用户给的口令同样要进 Vault：否则它在 Params 里虽被抹空，明文却仍留在
	// 渲染出的配置内容里，随规格进归档、审计与 diff。值相同则版本号不变，
	// 否则每次渲染都会 digest 变一次。
	Store(component, param, value string) (StoredSecret, error)
}

// StoredSecret 是一条已固化的密钥。
type StoredSecret struct {
	ID      string
	Version int
	Value   string
}

// Result 是一次解析的产出。
type Result struct {
	// Specs 是每个实例的规格，键为 Instance.Key()。
	Specs map[string]*spec.ResolvedSpec
	// Order 是实例键的稳定顺序（角色名、ordinal）。
	Order []string
	// Secrets 是本次用到的密钥明文，随 gRPC 消息单独下发，**不落盘**。
	Secrets map[string]string
	// Exports 是本组件对外提供的连接点，供消费方的解析使用。
	//
	// 渲染一个组件同时产出「它是什么」与「它对外提供什么」——
	// 后者本就是前者的函数，分成两次算只会让两者可能不一致。
	Exports map[string]Export
	// Warnings 是不阻断的问题。
	Warnings []string
}

// Spec 按实例取规格。
func (r *Result) Spec(role, node string) *spec.ResolvedSpec {
	return r.Specs[Instance{Role: role, Node: Node{Name: node}}.Key()]
}

// ── 内部状态 ────────────────────────────────────────────────────────────

type run struct {
	req Request
	eng *Engine

	// secrets 是本次涉及的密钥：参数名 → 密钥。
	secrets map[string]StoredSecret
	// tainted 是被敏感传播标记的参数名。
	tainted  map[string]bool
	warnings []string
}

type instance struct {
	Instance
	ctx    *Ctx
	paths  map[string]spec.PathValue
	params map[string]any
}

func (r *run) warnf(format string, args ...any) {
	r.warnings = append(r.warnings, fmt.Sprintf(format, args...))
}

// Render 解析一个 Component 的全部实例。
func Render(req Request) (*Result, error) {
	if req.Pack == nil {
		return nil, fmt.Errorf("rendering %s: missing Pack", req.Component)
	}
	if len(req.Instances) == 0 {
		return nil, fmt.Errorf("rendering %s: no instances to render", req.Component)
	}
	eng, err := NewEngine(req.Pack)
	if err != nil {
		return nil, err
	}
	r := &run{
		req:     req,
		eng:     eng,
		secrets: map[string]StoredSecret{},
		tainted: map[string]bool{},
	}

	// ── 第一趟：参数与路径 ──
	insts := make([]*instance, 0, len(req.Instances))
	for _, in := range req.Instances {
		it, err := r.pass1(in)
		if err != nil {
			return nil, err
		}
		insts = append(insts, it)
	}

	// ── 拓扑：必须等全部实例的路径就绪 ──
	topo := r.buildTopology(insts)

	// ── 第二趟：from、渲染、封装 ──
	out := &Result{
		Specs:   map[string]*spec.ResolvedSpec{},
		Secrets: map[string]string{},
	}
	for _, it := range insts {
		s, err := r.pass2(it, topo)
		if err != nil {
			return nil, err
		}
		out.Specs[it.Key()] = s
		out.Order = append(out.Order, it.Key())
	}

	ex, err := r.evalExports(insts, topo)
	if err != nil {
		return nil, err
	}
	out.Exports = ex

	for _, sec := range r.secrets {
		out.Secrets[sec.ID] = sec.Value
	}
	out.Warnings = r.warnings
	sort.Strings(out.Order)
	return out, nil
}

// pass1 解析一个实例的参数与路径。
func (r *run) pass1(in Instance) (*instance, error) {
	it := &instance{Instance: in}
	decls := r.paramDecls(in.Role)

	static, err := r.resolveStaticParams(decls, it)
	if err != nil {
		return nil, err
	}

	ctx := r.baseCtx(in)
	ctx.Params = static
	it.ctx = ctx

	if err := r.resolveDefaultFrom(decls, ctx, it); err != nil {
		return nil, err
	}
	if err := r.resolveSecretParams(decls, ctx, it); err != nil {
		return nil, err
	}
	enrich(decls, ctx.Params)

	paths, err := r.resolvePaths(ctx, it)
	if err != nil {
		return nil, err
	}
	it.paths = paths
	it.params = ctx.Params
	return it, nil
}

// pass2 求值 from、渲染全部产物并封装。
func (r *run) pass2(it *instance, topo map[string]*TopologyCtx) (*spec.ResolvedSpec, error) {
	ctx := it.ctx
	ctx.Topology = topo[it.Key()]
	ctx.Requires = r.requiresCtx()

	decls := r.paramDecls(it.Role)
	if err := r.resolveFrom(decls, ctx, it); err != nil {
		return nil, err
	}
	enrich(decls, ctx.Params)
	if err := r.checkRequired(decls, ctx, it); err != nil {
		return nil, err
	}

	s := &spec.ResolvedSpec{
		SchemaVersion: spec.SchemaVersion,
		Site:          r.req.Site,
		Component:     r.req.Component,
		Role:          it.Role,
		ConfigGroup:   orDefault(it.ConfigGroup, "default"),
		Node: spec.NodeRef{
			Name:    it.Node.Name,
			Address: it.Node.Address,
			Labels:  it.Node.Labels,
			Roots:   it.Node.Roots,
		},
		Ordinal:   it.Ordinal,
		Pack:      r.req.PackRef,
		Profile:   r.req.Profile,
		Paths:     it.paths,
		Blobs:     r.req.Blobs,
		Topology:  topologySnapshot(topo[it.Key()]),
		Requires:  r.requiresSnapshot(),
		Reconcile: r.req.Reconcile.WithDefaults(),
	}

	s.Params = r.paramValues(decls, ctx)

	res, err := r.renderResources(ctx, it)
	if err != nil {
		return nil, err
	}
	s.Resources = res

	wl, composeRes, err := r.renderWorkload(ctx, it)
	if err != nil {
		return nil, err
	}
	s.Workload = wl
	if composeRes != nil {
		// compose 文件是流水线自动产出的。Pack 若自己也往同一个路径写，
		// 撞车了要报错而不是让两条资源互相覆盖——谁后写谁赢是最难查的那种 bug
		for _, existing := range s.Resources {
			if existing.ID == composeRes.ID {
				return nil, fmt.Errorf(
					"组件 %s@%s: 资源 id %q 与自动产出的 compose 文件冲突\n"+
						"  runtime=compose 时流水线已经会渲染它，不要再声明一条 template 资源",
					r.req.Component, it.Node.Name, composeRes.ID)
			}
		}
		s.Resources = append(s.Resources, *composeRes)
	}
	if s.Health, err = r.renderHealth(ctx, it); err != nil {
		return nil, err
	}
	if s.Hooks, err = r.renderHooks(ctx, it); err != nil {
		return nil, err
	}

	r.applyNotify(s, it, decls)

	if err := r.sealSecrets(s, decls); err != nil {
		return nil, err
	}
	if err := spec.Seal(s); err != nil {
		return nil, err
	}
	return s, nil
}

// baseCtx 构造实例的渲染上下文（不含 Params / Paths / Topology）。
func (r *run) baseCtx(in Instance) *Ctx {
	vols := map[string]any{}
	for name, v := range in.Node.Volumes {
		vols[name] = map[string]any{"path": v.Path, "class": v.Class, "Path": v.Path, "Class": v.Class}
	}
	return &Ctx{
		Pack: PackCtx{
			Name:     r.req.Pack.Name,
			Version:  r.req.Pack.Version,
			Revision: r.req.Pack.Revision,
		},
		Profile:   r.req.Profile,
		Site:      siteCtx(r.req.Site),
		Component: r.req.Component,
		Role:      in.Role,
		Node: NodeCtx{
			Name:    in.Node.Name,
			Address: in.Node.Address,
			Labels:  in.Node.Labels,
			Roots:   rootsWithDefaults(in.Node.Roots),
			Volumes: vols,
			Facts:   alias(in.Node.Facts),
		},
	}
}

func rootsWithDefaults(roots map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range DefaultRoots {
		out[k] = v
	}
	for k, v := range roots {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// ── 拓扑 ────────────────────────────────────────────────────────────────

// buildTopology 为每个实例建一份拓扑视图。
//
// 每份视图的 roles 内容相同，只有 Self / Ordinal 不同——所以对等条目
// 只构造一次并共享。
func (r *run) buildTopology(insts []*instance) map[string]*TopologyCtx {
	byRole := map[string][]*PeerCtx{}
	self := map[string]*PeerCtx{}

	for _, it := range insts {
		p := &PeerCtx{
			Node:    it.Node.Name,
			Name:    it.Node.Name,
			Address: it.Node.Address,
			Ordinal: it.Ordinal,
			Role:    it.Role,
			Labels:  it.Node.Labels,
			Paths:   pathsForCtx(it.paths),
		}
		byRole[it.Role] = append(byRole[it.Role], p)
		self[it.Key()] = p
	}
	for _, list := range byRole {
		sort.Slice(list, func(i, j int) bool { return list[i].Ordinal < list[j].Ordinal })
	}

	out := map[string]*TopologyCtx{}
	for _, it := range insts {
		out[it.Key()] = &TopologyCtx{
			Ordinal: it.Ordinal,
			Self:    self[it.Key()],
			roles:   byRole,
		}
	}
	return out
}

// pathsForCtx 把已解析路径变成模板可见的形状。
func pathsForCtx(paths map[string]spec.PathValue) map[string]any {
	out := map[string]any{}
	for name, pv := range paths {
		var v any = pv.Values
		if pv.Kind != string(pack.KindMulti) {
			v = pv.First()
		}
		out[name] = v
		if c := capitalize(name); c != name {
			out[c] = v
		}
	}
	return out
}

// topologySnapshot 把拓扑落成规格里的快照。
func topologySnapshot(t *TopologyCtx) spec.Topology {
	out := spec.Topology{Roles: map[string][]spec.Instance{}}
	if t == nil {
		return out
	}
	for _, role := range t.Roles() {
		for _, p := range t.roles[role] {
			paths := map[string][]string{}
			for k, v := range p.Paths {
				if c := capitalize(k); c == k && k != "" {
					continue // 只留原始拼写，别名不进快照
				}
				switch x := v.(type) {
				case string:
					paths[k] = []string{x}
				case []string:
					paths[k] = x
				}
			}
			out.Roles[role] = append(out.Roles[role], spec.Instance{
				Node:    p.Node,
				Address: p.Address,
				Ordinal: p.Ordinal,
				Labels:  p.Labels,
				Paths:   paths,
			})
		}
	}
	return out
}

// ── 依赖 ────────────────────────────────────────────────────────────────

func (r *run) requiresCtx() map[string]any {
	out := map[string]any{}
	for name, b := range r.req.Requires {
		d := &DepCtx{
			Component: b.Component,
			Version:   b.Version,
			Scope:     string(b.Scope),
			Exports:   map[string]any{},
		}
		if b.Scope != pack.ScopeSite {
			d.Paths = map[string]any{}
			for k, v := range b.Paths {
				var val any = v
				if len(v) == 1 {
					val = v[0]
				}
				d.Paths[k] = val
				if c := capitalize(k); c != k {
					d.Paths[c] = val
				}
			}
		}
		for ename, e := range b.Exports {
			if e.Fields != nil {
				f := map[string]any{}
				for k, v := range e.Fields {
					f[k] = v
					if c := capitalize(k); c != k {
						f[c] = v
					}
				}
				d.Exports[ename] = f
			} else {
				d.Exports[ename] = e.Value
			}
		}
		if len(b.Topology) > 0 {
			byRole := map[string][]*PeerCtx{}
			for _, p := range b.Topology {
				byRole[p.Role] = append(byRole[p.Role], &PeerCtx{
					Node: p.Node, Name: p.Node, Address: p.Address,
					Ordinal: p.Ordinal, Role: p.Role, Labels: p.Labels,
					Paths: peerPaths(p.Paths),
				})
			}
			d.topology = &TopologyCtx{roles: byRole}
		}
		out[name] = d
		if c := capitalize(name); c != name {
			out[c] = d
		}
	}
	return out
}

func peerPaths(m map[string][]string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		var val any = v
		if len(v) == 1 {
			val = v[0]
		}
		out[k] = val
		if c := capitalize(k); c != k {
			out[c] = val
		}
	}
	return out
}

// requiresSnapshot 把绑定落成规格里的记录。
//
// exports 拍平成 `<导出名>` 或 `<导出名>.<字段名>`——规格是给 mechlet 与
// 诊断看的平面结构，不需要再嵌一层。
func (r *run) requiresSnapshot() map[string]spec.RequireBinding {
	if len(r.req.Requires) == 0 {
		return nil
	}
	out := map[string]spec.RequireBinding{}
	for name, b := range r.req.Requires {
		rb := spec.RequireBinding{
			Pack:      b.Pack,
			Component: b.Component,
			Version:   b.Version,
			Scope:     string(b.Scope),
		}
		if b.Scope != pack.ScopeSite && len(b.Paths) > 0 {
			rb.Paths = map[string]string{}
			for k, v := range b.Paths {
				if len(v) > 0 {
					rb.Paths[k] = v[0]
				}
			}
		}
		for ename, e := range b.Exports {
			if rb.Exports == nil {
				rb.Exports = map[string]string{}
			}
			if e.Fields == nil {
				rb.Exports[ename] = e.Value
				continue
			}
			for f, v := range e.Fields {
				rb.Exports[ename+"."+f] = v
			}
		}
		out[name] = rb
	}
	return out
}

// ── 参数落盘形态 ────────────────────────────────────────────────────────

// paramValues 把已解析参数变成规格里的 ParamValue。
//
// 敏感参数的 Value 留空——值只走 SecretRefs。
func (r *run) paramValues(decls map[string]pack.Param, ctx *Ctx) map[string]spec.ParamValue {
	if len(ctx.Params) == 0 {
		return nil
	}
	out := map[string]spec.ParamValue{}
	for name, v := range ctx.Params {
		d := decls[name]
		pv := spec.ParamValue{Value: plainValue(v), Type: string(d.Type)}
		if d.IsSensitive() || r.tainted[name] {
			pv.Sensitive = true
			pv.Value = nil
		}
		out[name] = pv
	}
	return out
}

// ── 封装 ────────────────────────────────────────────────────────────────

// applyNotify 算出每条资源的**最终**通知动作（15-render-pipeline §7）。
//
// Pack 里写的是 `notify: reload`，但若这次变化的参数中有
// restartRequired: true 的，最终动作应当是 restart。这个判断只有 mechd
// 做得了——它持有上一版规格，知道哪些参数变了。mechlet 只做聚合去重与
// restart 吸收 reload，不判断该 restart 还是 reload。
func (r *run) applyNotify(s *spec.ResolvedSpec, it *instance, decls map[string]pack.Param) {
	prev := r.req.Previous[it.Key()]
	if prev == nil {
		return // 首次部署：资源全都要建，notify 无意义
	}

	var changed []string
	for name, d := range decls {
		if !d.RestartRequired {
			continue
		}
		now, old := s.Params[name], prev.Params[name]
		if now.Sensitive || old.Sensitive {
			continue // 敏感参数的值不在规格里，靠 SecretRefs 的版本号体现
		}
		if !sameValue(now.Value, old.Value) {
			changed = append(changed, name)
		}
	}
	if len(changed) == 0 {
		return
	}
	sort.Strings(changed)

	for i := range s.Resources {
		if s.Resources[i].Notify == "reload" {
			s.Resources[i].Notify = "restart"
		}
	}
	r.warnf("%s@%s: parameter %s change requires a restart, reload has been promoted to restart",
		r.req.Component, it.Node.Name, strings.Join(changed, ", "))
}

func sameValue(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

// sealSecrets 把规格中出现的密钥明文换成哨兵串。
//
// 做法是「序列化 → 字面量替换 → 反序列化」，与 ResolveGeneration 同一套
// 机制：明文可能出现在任何字符串字段（渲染出的 config 内容、envFile、
// exports 拼出的连接串……），逐字段处理既冗长又必然漏。
func (r *run) sealSecrets(s *spec.ResolvedSpec, decls map[string]pack.Param) error {
	values := map[string]StoredSecret{}
	for name, sec := range r.secrets {
		values[name] = sec
	}
	// 依赖导出里的敏感字段同样要遮起来：它们的明文正是 provider 的口令
	for dep, b := range r.req.Requires {
		for ename, e := range b.Exports {
			for f := range e.SensitiveFields {
				v, ok := e.Fields[f]
				if !ok || v == "" {
					continue
				}
				values[dep+"."+ename+"."+f] = StoredSecret{
					ID:      secretIDFor(dep, ename, f),
					Version: 0,
					Value:   v,
				}
			}
		}
	}
	if len(values) == 0 {
		return nil
	}

	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("serializing spec: %w", err)
	}
	text := string(b)

	if strings.Contains(text, spec.SecretPrefix) {
		return fmt.Errorf(
			"component %s@%s: rendered output contains the secret sentinel prefix %q -- "+
				"Pack templates or parameter values must not contain this string",
			r.req.Component, s.Node.Name, spec.SecretPrefix)
	}

	var refs []spec.SecretRef
	for _, name := range sortedSecretNames(values) {
		sec := values[name]
		if sec.Value == "" {
			continue
		}
		esc, err := json.Marshal(sec.Value)
		if err != nil {
			return err
		}
		lit := string(esc[1 : len(esc)-1])
		if !strings.Contains(text, lit) {
			continue // 该密钥没被这个实例用到，不必下发
		}
		// 遮蔽是**全文字面量替换**，短值会误伤无关字段（一个 3 字符的口令
		// 可能正好是某个路径片段）。仍然照做——泄漏比误伤更糟，而这么短的
		// 口令本就不该用——但必须说出来，否则现场会看到一份莫名其妙的配置。
		if len(sec.Value) < pack.MinGenerateLength {
			r.warnf("%s: parameter %s's value is only %d characters -- masking may accidentally redact "+
				"unrelated identical substrings in the spec; use at least %d characters for sensitive values",
				r.req.Component, name, len(sec.Value), pack.MinGenerateLength)
		}
		text = strings.ReplaceAll(text, lit, spec.SecretToken(sec.ID))
		refs = append(refs, spec.SecretRef{ID: sec.ID, Version: sec.Version, Param: name})
	}
	if len(refs) == 0 {
		return nil
	}

	var out spec.ResolvedSpec
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return fmt.Errorf("failed to parse spec after substituting secret placeholders: %w", err)
	}
	*s = out
	s.SecretRefs = refs
	return nil
}

// secretIDFor 给依赖导出的敏感字段一个稳定 id。
//
// 稳定很重要：id 参与 digest，每次渲染换一个会让 digest 每次都变，
// 于是每次调和都产生新 generation。
func secretIDFor(dep, export, field string) string {
	return "dep-" + dep + "-" + export + "-" + field
}

func sortedSecretNames(m map[string]StoredSecret) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── 助手 ────────────────────────────────────────────────────────────────

func siteCtx(s spec.SiteRef) SiteCtx {
	return SiteCtx{Name: s.Name, Kind: s.Kind, Labels: s.Labels}
}

// DefaultPlatform 是尚未按节点 facts 分平台时用的平台。
//
// **目前只服务 linux/amd64 单机形态**，全部调用方（含 mechd 组装
// blob 请求）都直接用这个常量，按节点 facts 分平台还没有排期——
// 真做的话 Request.Blobs 也要按实例分组，而不是整份请求一份。
const DefaultPlatform = "linux/amd64"

// BlobsFor 取出 Pack 在某个平台上的载荷引用。
//
// **mechd 与 `mechctl component render` 必须共用它**：离线渲染这条命令的
// 全部价值在于「同样的输入必然得到同样的输出」，两处各算一遍载荷的话，
// 一处漏了就变成「离线渲染出来的规格装不上」——而那正是它要用来排除的
// 那类问题。
func BlobsFor(p *pack.Pack, platform string) []spec.BlobRef {
	if p == nil || len(p.Blobs) == 0 {
		return nil
	}
	if platform == "" {
		platform = DefaultPlatform
	}
	names := make([]string, 0, len(p.Blobs))
	for k := range p.Blobs {
		names = append(names, k)
	}
	sort.Strings(names)

	out := make([]spec.BlobRef, 0, len(names))
	for _, name := range names {
		entry, ok := p.Blobs[name][platform]
		if !ok {
			// 没有本平台的条目不是错误：一个纯配置的 Pack 可以只在
			// 某些平台上带二进制
			continue
		}
		out = append(out, spec.BlobRef{
			Name: name, SHA256: entry.SHA256, Size: entry.Size,
			Filename: entry.Filename, MediaType: entry.MediaType,
		})
	}
	return out
}
