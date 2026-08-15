package pack

import (
	"fmt"
	"strconv"
	"strings"
)

// ── R16–R21 形态 ────────────────────────────────────────────────────────

func (l *linter) checkProfiles() {
	p := l.p
	if len(p.Profiles) == 0 {
		return
	}

	seen := map[string]bool{}
	defaults := 0
	for i, pr := range p.Profiles {
		where := fmt.Sprintf("profiles[%d]", i)
		line := p.LineOf("profiles", strconv.Itoa(i))

		if pr.Name == "" {
			l.err("R16", where+".name", line, "profile is missing name", "")
		} else if seen[pr.Name] {
			l.err("R16", where+".name", line,
				fmt.Sprintf("profile name %q is duplicated", pr.Name), "")
		}
		seen[pr.Name] = true
		if pr.Default {
			defaults++
		}

		// R17：引用的角色必须存在
		for _, rn := range sortedKeys(pr.Roles) {
			if !l.roleSet[rn] {
				l.err("R17", fmt.Sprintf("%s.roles.%s", where, rn), line,
					fmt.Sprintf("profile %q references role %q, which does not exist", pr.Name, rn),
					"defined roles: "+quoteList(p.RoleNames()))
			}
			if c := pr.Roles[rn].Cardinality; c != "" && !validCardinality(c) {
				l.err("R11", fmt.Sprintf("%s.roles.%s.cardinality", where, rn), line,
					fmt.Sprintf("cardinality %q has invalid syntax", c), "")
			}
		}

		// R18：placement 不得引用在本 profile 中被关掉的角色
		//
		// 这类声明不会报错，只会**静默失效**——用户以为反亲和还在守着，
		// 实际上那个角色在这个形态下根本不存在。
		for i, pl := range pr.Placement {
			for _, rn := range append(append([]string{}, pl.AntiAffinity...), pl.Affinity...) {
				if role, ok := pr.Roles[rn]; ok && !role.IsEnabled() {
					l.err("R18", fmt.Sprintf("%s.placement[%d]", where, i), line,
						fmt.Sprintf("profile %q's placement constraint references role %q, "+
							"but that role has enabled: false in this profile", pr.Name, rn),
						"the constraint will silently have no effect. Remove it, or enable the role in this profile")
				}
			}
		}

		// R19：可满足性
		l.checkProfileSatisfiable(where, line, pr)

		// R20：upgradeFrom
		for _, from := range pr.UpgradeFrom {
			if from == pr.Name {
				l.err("R20", where+".upgradeFrom", line,
					fmt.Sprintf("profile %q's upgradeFrom includes itself", pr.Name), "")
				continue
			}
			if p.ProfileByName(from) == nil {
				l.err("R20", where+".upgradeFrom", line,
					fmt.Sprintf("upgradeFrom references profile %q, which does not exist", from),
					"defined profiles: "+quoteList(p.ProfileNames()))
			}
		}
	}

	if defaults > 1 {
		l.err("R16", "profiles", p.LineOf("profiles"),
			fmt.Sprintf("%d profiles are marked default, at most one is allowed", defaults),
			"when unmarked, the first one is treated as default")
	}

	if cyc := findProfileCycle(p); cyc != nil {
		l.err("R20", "profiles", p.LineOf("profiles"),
			"upgradeFrom cycle: "+strings.Join(cyc, " → "), "")
	}
}

// checkProfileSatisfiable 校验「存在一个满足全部约束的放置方案」（R19）。
//
// 规模很小（角色数 <20），因此直接用「必需实例数下限 vs 可用节点数上限」
// 这一必要条件判断，而非完整的约束满足求解。
func (l *linter) checkProfileSatisfiable(where string, line int, pr Profile) {
	enabled := l.enabledRoles(pr)

	// 每个启用角色的实例数下限
	lowerBound := map[string]int{}
	for rn := range enabled {
		card := ""
		if o, ok := pr.Roles[rn]; ok && o.Cardinality != "" {
			card = o.Cardinality
		} else if r := l.p.RoleByName(rn); r != nil {
			card = r.EffectiveCardinality()
		}
		if lo, _, ok := cardinalityBounds(card); ok {
			lowerBound[rn] = lo
		}
	}

	// 逐条 antiAffinity 检查：**同一条约束内**的角色实例必须各占一个节点。
	//
	// 不同约束之间的角色可以共处一台机器——例如 `[namenode, secondarynamenode]`
	// 与 `[datanode]` 是两条独立约束，DataNode 完全可以和 NameNode 同机。
	// 把它们的下限相加是错的。
	for pi, pl := range pr.Placement {
		if len(pl.AntiAffinity) == 0 || pl.EffectiveScope() != "node" ||
			pl.EffectiveEnforcement() != EnforceRequired {
			continue
		}
		need := 0
		var parts []string
		for _, rn := range pl.AntiAffinity {
			if !enabled[rn] {
				continue
			}
			need += lowerBound[rn]
			parts = append(parts, fmt.Sprintf("%s×%d", rn, lowerBound[rn]))
		}
		if pr.MinNodes > 0 && need > pr.MinNodes {
			l.err("R19", fmt.Sprintf("%s.placement[%d]", where, pi), line,
				fmt.Sprintf("profile %q is unsatisfiable: this constraint requires %s to each occupy a separate node, needing %d in total, but minNodes = %d",
					pr.Name, strings.Join(parts, " + "), need, pr.MinNodes),
				"raise minNodes, or relax cardinality / downgrade this constraint to preferred")
		}
	}

	// 上限与下限自相矛盾
	for _, rn := range sortedKeys(pr.Roles) {
		o := pr.Roles[rn]
		if o.Cardinality == "" {
			continue
		}
		if lo, hi, ok := cardinalityBounds(o.Cardinality); ok && hi >= 0 && lo > hi {
			l.err("R19", fmt.Sprintf("%s.roles.%s.cardinality", where, rn), line,
				fmt.Sprintf("cardinality %q has a lower bound greater than its upper bound", o.Cardinality), "")
		}
	}
}

func findProfileCycle(p *Pack) []string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	state := map[string]int{}
	var stack []string
	var dfs func(string) []string

	dfs = func(n string) []string {
		state[n] = gray
		stack = append(stack, n)
		if pr := p.ProfileByName(n); pr != nil {
			for _, d := range pr.UpgradeFrom {
				if p.ProfileByName(d) == nil {
					continue
				}
				switch state[d] {
				case gray:
					for i, s := range stack {
						if s == d {
							return append(append([]string{}, stack[i:]...), d)
						}
					}
				case white:
					if c := dfs(d); c != nil {
						return c
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[n] = black
		return nil
	}

	for _, pr := range p.Profiles {
		if state[pr.Name] == white {
			if c := dfs(pr.Name); c != nil {
				return c
			}
		}
	}
	return nil
}

// ── R22–R26 参数 ────────────────────────────────────────────────────────

func (l *linter) checkParams() {
	l.checkParamMap("params", l.p.Params, l.p.LineOf("params"))
	for i, r := range l.p.Roles {
		l.checkParamMap(fmt.Sprintf("roles[%s].params", r.EffectiveName()),
			r.Params, l.p.LineOf("roles", strconv.Itoa(i), "params"))
	}
	for i, pr := range l.p.Profiles {
		l.checkParamMap(fmt.Sprintf("profiles[%s].params", pr.Name),
			pr.Params, l.p.LineOf("profiles", strconv.Itoa(i), "params"))
	}
}

func (l *linter) checkParamMap(where string, m map[string]Param, line int) {
	for _, name := range sortedKeys(m) {
		p := m[name]
		w := where + "." + name

		// profile 中的参数可能只是覆盖 default，此时 type 允许省略
		isOverride := strings.HasPrefix(where, "profiles[") && p.Type == ""
		if !isOverride {
			// R22
			if p.Type == "" {
				l.err("R22", w, line, "parameter is missing type", "")
				continue
			}
			if !p.Type.Valid() {
				l.err("R22", w, line,
					fmt.Sprintf("type %q is not one of the 12 valid types", p.Type),
					"valid types: string int float bool enum path port duration size cidr secret list<T>; "+
						"if the types aren't enough, add a new one to pack/v1 -- there is no custom-schema escape hatch")
				continue
			}
			// R23
			if p.Type.ElemType() == TypeEnum {
				if len(p.Values) == 0 {
					l.err("R23", w+".values", line, "enum type must declare values", "")
				} else if p.Default != nil {
					if err := p.ValidateValue(p.Default); err != nil {
						l.err("R23", w+".default", line,
							fmt.Sprintf("default is invalid: %v", err), "")
					}
				}
			} else if p.Default != nil {
				if err := p.ValidateValue(p.Default); err != nil {
					l.err("R22", w+".default", line,
						fmt.Sprintf("default does not match type %s: %v", p.Type, err), "")
				}
			}
		}

		// R24
		if p.RestartRequired && p.ReloadRequired {
			l.err("R24", w, line, "restartRequired and reloadRequired are mutually exclusive", "")
		}

		// R26b / R26c
		if p.From != "" && p.DefaultFrom != "" {
			l.err("R26b", w, line, "from and defaultFrom are mutually exclusive",
				"from means fully derived and not user-settable; defaultFrom means a computed default that can be overridden")
		}
		if p.From != "" && p.Default != nil {
			l.err("R26b", w, line, "from and default are mutually exclusive",
				"a parameter that declares from is derived by the engine and should not also have a default")
		}
		if p.DefaultFrom != "" && p.Default == nil {
			l.err("R26c", w, line, "declaring defaultFrom requires also declaring default",
				"a fallback value is needed for when defaultFrom evaluation fails (missing facts, division by zero, etc.)")
		}

		// R26d / R26e：generate
		if g := p.Generate; g != nil {
			if p.Type != TypeSecret {
				l.err("R26d", w+".generate", line,
					fmt.Sprintf("generate can only be used with type: secret, got %q", p.Type),
					"values of other types are either objective facts (use from) or should be decided by a human (use default / defaultFrom)")
			}
			for field, set := range map[string]bool{
				"default":     p.Default != nil,
				"defaultFrom": p.DefaultFrom != "",
				"from":        p.From != "",
			} {
				if set {
					l.err("R26d", w+".generate", line,
						"generate and "+field+" are mutually exclusive",
						"a generated value is produced and persisted by the engine; supplying an initial value too only makes it ambiguous which one wins")
				}
			}
			if g.Length != 0 && g.Length < MinGenerateLength {
				l.err("R26e", w+".generate.length", line,
					fmt.Sprintf("generate length %d is below the minimum %d", g.Length, MinGenerateLength),
					"a password shorter than this offers no protection against offline brute-forcing; this number is almost always a typo")
			}
			if g.Charset != "" && !containsString(Charsets, g.Charset) {
				l.err("R26e", w+".generate.charset", line,
					fmt.Sprintf("unknown charset %q", g.Charset),
					"available: "+quoteList(Charsets))
			}
		}

		// R41：facts 不得出现在 from 中
		if p.From != "" && ExprMentionsFacts(p.From) {
			l.err("R41", w+".from", line,
				"from references a node fact",
				"facts are mutable and are not an objective deployment fact. defaults computed from facts like memory should go in defaultFrom instead")
		}

		// R26：from / defaultFrom 的引用必须可解析
		l.checkExprRefs(w, line, p.From)
		l.checkExprRefs(w, line, p.DefaultFrom)

		// R25 在 checkParamReloadable 中统一处理
	}
}

func (l *linter) checkExprRefs(where string, line int, expr string) {
	if expr == "" {
		return
	}
	// 模板语法的表达式并入模板集合，保证可解析
	if strings.Contains(expr, "{{") {
		if err := l.ts.ParseExpr("expr:"+where, expr); err != nil {
			l.err("R26", where, line,
				fmt.Sprintf("expression parse failed: %v", err), "")
			return
		}
	}

	refs := CollectExprRefs(expr)
	for _, rn := range sortedKeys(refs.Roles) {
		if !l.roleSet[rn] {
			l.err("R26", where, line,
				fmt.Sprintf("expression references role %q, which does not exist", rn),
				"defined roles: "+quoteList(l.p.RoleNames()))
		}
	}
	for _, dn := range sortedKeys(refs.Requires) {
		dep, ok := l.depSet[dn]
		if !ok {
			l.err("R38", where, line,
				fmt.Sprintf("expression references dependency %q, which is not declared in requires.packs", dn),
				"declared dependencies: "+quoteList(sortedKeys(l.depSet)))
			continue
		}
		// R40
		if dep.EffectiveScope() == ScopeSite && ExprReferencesPaths(expr, dn) {
			l.err("R40", where, line,
				fmt.Sprintf("dependency %q is scope: site, its .Paths must not be referenced", dn),
				"that's a path on a different machine. a site-scoped dependency only provides .Topology / .Exports / .Version")
		}
	}

	// R49 —— 独立于依赖名是否解析得出：即便 requires.packs 里没声明它，
	// 「伸手取别人的参数」这个动作本身就该拦
	l.checkNoDepParams(where, line, expr)

	// R43 —— 需要跨 Pack 索引才能核对
	l.checkDepExports(where, line, expr)
}

// checkNoDepParams 实现 R49：消费方不得绕过导出契约读提供方的参数。
func (l *linter) checkNoDepParams(where string, line int, expr string) {
	for _, dn := range ExprReferencedDepParams(expr) {
		l.err("R49", where, line,
			fmt.Sprintf("must not read parameters of dependency %q", dn),
			"if the provider renames a parameter, every consumer breaks at once; and its superuser password shouldn't reach consumers in the first place. "+
				"the value should be explicitly exported by the provider via exports (spec §5.4)")
	}
}

// checkDepExports 实现 R43：引用的导出名必须存在于被依赖的 Pack。
//
// 消费方硬编码一个**不存在**的导出名，在 lint 阶段是查不出来的——直到
// 部署时渲染失败，而那时错误信息只会说某个键取不到值。
func (l *linter) checkDepExports(where string, line int, expr string) {
	if l.opts.Resolver == nil {
		return // 没给索引就无从核对；见 Options.Resolver 的说明
	}
	for _, ref := range ExprReferencedExports(expr) {
		dep, declared := l.depSet[ref.Dep]
		if !declared {
			continue // 依赖没声明，已由 R38 报出
		}
		exports, ok := l.opts.Resolver.Exports(ref.Dep, dep.Version)
		if !ok {
			l.warn("R43", where, line,
				fmt.Sprintf("%s is not available locally, cannot verify export name %q", ref.Dep, ref.Export),
				"the dependency may be published separately; re-validate after importing it")
			continue
		}
		if !containsString(exports, ref.Export) {
			l.err("R43", where, line,
				fmt.Sprintf("%s does not export %q", ref.Dep, ref.Export),
				"it exports: "+quoteList(exports))
		}
	}
}

// checkParamReloadable 实现 R25：只有声明了 execReload 的角色，
// 其可见参数才允许使用 reloadRequired。
func (l *linter) checkParamReloadable() {
	// 收集「哪些角色支持 reload」
	anyReload := false
	for _, r := range l.p.Roles {
		if r.Workload == nil {
			continue
		}
		switch r.Workload.Runtime {
		case RuntimeSystemd:
			if r.Workload.Systemd != nil && r.Workload.Systemd.ExecReload != "" {
				anyReload = true
			}
		case RuntimeDocker, RuntimeCompose:
			// 容器 runtime 的 reload 由引擎降级为重启，此处不作要求
			anyReload = true
		}
	}
	if anyReload {
		return
	}
	for _, name := range sortedKeys(l.p.AllParams()) {
		p := l.p.AllParams()[name]
		if p.ReloadRequired {
			l.err("R25", "params."+name, l.p.LineOf("params", name),
				fmt.Sprintf("parameter %q declares reloadRequired, but no role declares execReload", name),
				"declare a reload command in workload.systemd.execReload, or use restartRequired instead")
		}
	}
}
