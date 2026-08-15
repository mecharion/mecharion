package pack

import (
	"fmt"
	"strconv"
	"strings"
)

// ── R27–R30 路径 ────────────────────────────────────────────────────────

// generationRefs 是判定「位于 generation 内」的模板引用形式。
var generationRefs = []string{".Paths.Generation", ".paths.generation"}

// dataLogPaths 是不允许使用 layout: inline 的路径名（必须跨 generation 存活）。
var dataLogPaths = map[string]bool{"data": true, "logs": true}

func (l *linter) checkPaths() {
	l.checkPathMap("paths", l.p.Paths, l.p.LineOf("paths"))
	for i, r := range l.p.Roles {
		l.checkPathMap(fmt.Sprintf("roles[%s].paths", r.EffectiveName()),
			r.Paths, l.p.LineOf("roles", strconv.Itoa(i), "paths"))
	}
}

func (l *linter) checkPathMap(where string, m map[string]Path, line int) {
	for _, name := range sortedKeys(m) {
		p := m[name]
		w := where + "." + name

		defs, err := p.DefaultStrings()
		if err != nil {
			l.err("R27", w+".default", line, err.Error(), "")
			continue
		}
		if len(defs) == 0 {
			l.err("R27", w+".default", line, "missing default", "")
			continue
		}

		// R27
		if p.EffectiveKind() == KindMulti && !p.DefaultIsList() {
			l.err("R27", w+".default", line,
				"kind: multi's default must be a list",
				fmt.Sprintf("write it as default: [%q]", defs[0]))
		}
		if p.EffectiveKind() == KindSingle && p.DefaultIsList() {
			l.err("R27", w+".default", line,
				"default is a list but kind: multi is not declared", "")
		}
		if p.Kind != "" && p.EffectiveKind() != KindSingle && p.EffectiveKind() != KindMulti {
			l.err("R27", w+".kind", line,
				fmt.Sprintf("kind %q is invalid", p.Kind), "available: single / multi")
		}

		inGen := func(s string) bool {
			for _, ref := range generationRefs {
				if strings.Contains(s, ref) {
					return true
				}
			}
			return false
		}

		// R28
		if p.LinkInto != "" && !inGen(p.LinkInto) {
			l.err("R28", w+".linkInto", line,
				"linkInto's target must be inside {{ .Paths.Generation }}",
				"linkInto's purpose is to symlink external storage into the generation directory")
		}

		// R29 / R30
		if p.EffectiveLayout() == LayoutInline {
			if p.LinkInto != "" {
				l.err("R29", w, line, "layout: inline and linkInto are mutually exclusive",
					"inline means the config already lives inside generation and needs no symlink")
			}
			allIn := true
			for _, d := range defs {
				if !inGen(d) {
					allIn = false
				}
			}
			if !allIn {
				l.err("R29", w+".default", line,
					"with layout: inline, default must be inside {{ .Paths.Generation }}", "")
			}
			if dataLogPaths[name] {
				l.err("R30", w, line,
					fmt.Sprintf("layout: inline must not be used for %q", name),
					"data / logs must survive across generations")
			}
		} else if p.Layout != "" && p.EffectiveLayout() != LayoutSeparate {
			l.err("R29", w+".layout", line,
				fmt.Sprintf("layout %q is invalid", p.Layout), "available: separate / inline")
		}
	}
}

// ── R06b–R06c、R31 资源 ─────────────────────────────────────────────────

func (l *linter) checkResources() {
	known := map[string]bool{}
	for _, t := range KnownResourceTypes {
		known[t] = true
	}

	l.p.AllResources(func(owner string, idx int, r Resource) {
		where := fmt.Sprintf("%s.resources[%d]", owner, idx)

		if !known[r.Type] {
			l.err("R06", where, r.Line,
				fmt.Sprintf("unknown resource type %q", r.Type),
				"valid types: "+strings.Join(KnownResourceTypes, " "))
			return
		}
		w := where + "." + r.Type

		// R06：blob 引用必须存在
		if b := r.Arg("blob"); b != "" {
			if _, ok := l.p.Blobs[b]; !ok {
				l.err("R06", w+".blob", r.Line,
					fmt.Sprintf("referenced blob %q is not declared in blobs", b),
					"declared blobs: "+quoteList(sortedKeys(l.p.Blobs)))
			}
		}

		switch r.Type {
		case ResFile:
			// R06b：content / source / blob 三者恰有其一
			n := 0
			for _, k := range []string{"content", "source", "blob"} {
				if r.HasArg(k) {
					n++
				}
			}
			switch {
			case n == 0:
				l.err("R06b", w, r.Line, "file must declare one of content / source / blob", "")
			case n > 1:
				l.err("R06b", w, r.Line, "file's content / source / blob are mutually exclusive, only one is allowed", "")
			}

		case ResArchive:
			// R06c：raw 载荷不应走 archive
			if b := r.Arg("blob"); b != "" {
				if blob, ok := l.p.Blobs[b]; ok {
					for _, plat := range sortedKeys(blob) {
						if blob[plat].MediaType == MediaRaw {
							l.err("R06c", w+".blob", r.Line,
								fmt.Sprintf("blob %q has mediaType raw, should not be used with the archive resource", b),
								"for a raw binary, use file: { blob: ..., path: ..., mode: \"0755\" }")
							break
						}
					}
				}
			}

		case ResCommand, ResScript:
			// R31：守卫至少三选一
			if !r.HasArg("unless") && !r.HasArg("onlyif") && !r.HasArg("creates") {
				l.err("R31", w, r.Line,
					fmt.Sprintf("%s resource is missing a guard", r.Type),
					"must declare one of creates / unless / onlyif -- a command without a guard isn't idempotent and can't participate in reconciliation")
			}

		case ResSystemdUnit:
			// R45
			name := r.Arg("name")
			if name == "" {
				l.err("R45", w+".name", r.Line, "systemd_unit is missing name", "")
			} else if !validUnitName(name) {
				l.err("R45", w+".name", r.Line,
					fmt.Sprintf("unit name %q has an invalid suffix", name),
					"valid suffixes: .service .socket .timer .target .path .mount .oneshot")
			}
			if r.Arg("content") == "" {
				l.err("R45", w+".content", r.Line, "systemd_unit is missing content", "")
			}
			if st := r.Arg("state"); st != "" && st != "started" && st != "stopped" && st != "absent" {
				l.err("R45", w+".state", r.Line,
					fmt.Sprintf("state %q is invalid", st), "available: started / stopped / absent")
			}
		}

		// driftPolicy
		if dp := r.DriftPolicy; dp != "" && dp != "report" && dp != "reconcile" && dp != "ignore" {
			l.err("R06", w+".driftPolicy", r.Line,
				fmt.Sprintf("driftPolicy %q is invalid", dp), "available: report / reconcile / ignore")
		}
	})
}

func validUnitName(s string) bool {
	for _, suf := range []string{".service", ".socket", ".timer", ".target", ".path", ".mount", ".oneshot"} {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}

// ── R32 hooks ───────────────────────────────────────────────────────────

func (l *linter) checkHooks() {
	validPoint := map[string]bool{}
	for _, p := range LifecyclePoints {
		validPoint[p] = true
	}

	checkPoints := func(owner string, h Hooks, roleName string) {
		for _, point := range sortedKeys(h.Points) {
			if !validPoint[point] {
				l.err("R32", owner+"."+point, 0,
					fmt.Sprintf("unknown lifecycle point %q", point),
					"valid values: "+strings.Join(LifecyclePoints, " "))
			}
			for _, hk := range h.Points[point] {
				w := fmt.Sprintf("%s.%s", owner, point)
				sc := hk.EffectiveScope()
				if sc != ScopePerInstance && sc != ScopeOnce {
					l.err("R32", w+".scope", hk.Line,
						fmt.Sprintf("scope %q is invalid", hk.Scope), "available: perInstance / once")
					continue
				}
				// R32：scope: once 所属角色的 cardinality 下限须 ≥ 1
				if sc == ScopeOnce && roleName != "" {
					if r := l.p.RoleByName(roleName); r != nil {
						lo, _, ok := cardinalityBounds(r.EffectiveCardinality())
						// "0" 是「由 profile 打开」的惯用写法：检查各 profile
						if ok && lo < 1 && !l.someProfileGivesInstances(roleName) {
							l.err("R32", w, hk.Line,
								fmt.Sprintf("scope: once hook's owning role %q has cardinality lower bound %d, which may have no instance to run it",
									roleName, lo),
								"raise the role's cardinality lower bound, or set it to >= 1 in some profile")
						}
					}
				}
			}
		}
	}

	checkPoints("hooks", l.p.Hooks, "")
	for _, r := range l.p.Roles {
		checkPoints(fmt.Sprintf("roles[%s].hooks", r.EffectiveName()), r.Hooks, r.EffectiveName())
	}
}

func (l *linter) someProfileGivesInstances(role string) bool {
	for _, pr := range l.p.Profiles {
		o, ok := pr.Roles[role]
		if !ok || !o.IsEnabled() {
			continue
		}
		card := o.Cardinality
		if card == "" {
			if r := l.p.RoleByName(role); r != nil {
				card = r.EffectiveCardinality()
			}
		}
		if lo, _, ok := cardinalityBounds(card); ok && lo >= 1 {
			return true
		}
	}
	return false
}

// ── R34–R43 依赖与升级 ──────────────────────────────────────────────────

func (l *linter) checkDependencies() {
	p := l.p

	checkList := func(where string, r *Requires, line int) {
		if r == nil {
			return
		}
		seen := map[string]bool{}
		for i, d := range r.Packs {
			w := fmt.Sprintf("%s.packs[%d]", where, i)
			if d.Name == "" {
				l.err("R34", w, line, "dependency is missing name", "")
				continue
			}
			// R34
			if seen[d.Name] {
				l.err("R34", w, line,
					fmt.Sprintf("the same Pack %q is declared as a dependency twice", d.Name),
					"when two major versions are both needed they are naturally two different Packs (e.g. jdk11 / jdk17) with different names -- so there is no alias mechanism")
			}
			seen[d.Name] = true

			// R34b：版本约束必须可解析。写错的约束会在放置阶段才炸，
			// 而那时错误信息只说「找不到满足条件的版本」，看不出是表达式有问题。
			if _, err := ParseConstraint(d.Version); err != nil {
				l.err("R34b", w+".version", line,
					fmt.Sprintf("version constraint is invalid: %v", err),
					"supports * / >= / > / <= / < / = / ~ / ^, comma-separated to require all (spec §5.5)")
			}

			// R35
			if sc := d.EffectiveScope(); sc != ScopeNode && sc != ScopeSite {
				l.err("R35", w+".scope", line,
					fmt.Sprintf("scope %q is invalid", d.Scope),
					"available: node (local files, requires same node) / site (network-reachable is enough)")
			}
			if d.Name == p.Name {
				l.err("R37", w, line, "a Pack cannot depend on itself", "")
			}
		}
	}

	checkList("requires", p.Requires, p.LineOf("requires"))
	for i, pr := range p.Profiles {
		checkList(fmt.Sprintf("profiles[%s].requires", pr.Name), pr.Requires,
			p.LineOf("profiles", strconv.Itoa(i), "requires"))
	}
	for _, role := range p.Roles {
		if role.Workload != nil {
			checkList(fmt.Sprintf("roles[%s].workload.requires", role.EffectiveName()),
				role.Workload.Requires, 0)
		}
	}

	// R34b：compatible 与 requires 用同一套约束语法
	if p.UpgradePolicy != nil && strings.TrimSpace(p.UpgradePolicy.Compatible) != "" {
		if _, err := ParseConstraint(p.UpgradePolicy.Compatible); err != nil {
			l.err("R34b", "upgradePolicy.compatible", p.LineOf("upgradePolicy"),
				fmt.Sprintf("version constraint is invalid: %v", err),
				"uses the same syntax as requires.packs[].version (spec §5.5)")
		}
	}

	// R36
	if p.UpgradePolicy != nil && strings.TrimSpace(p.UpgradePolicy.Compatible) == "" {
		l.err("R36", "upgradePolicy.compatible", p.LineOf("upgradePolicy"),
			"upgradePolicy is declared but compatible is empty",
			`defaults to "*" (upgradable from any version); restrict to the same major version with e.g. "~16"`)
	}

	// R42：exports 引用的角色必须存在
	for _, name := range sortedKeys(p.Exports) {
		e := p.Exports[name]
		w := "exports." + name
		line := p.LineOf("exports", name)
		if e.Role == "" {
			l.err("R42", w+".role", line, "export is missing role", "")
		} else if !l.roleSet[e.Role] {
			l.err("R42", w+".role", line,
				fmt.Sprintf("export %q references role %q, which does not exist", name, e.Role),
				"defined roles: "+quoteList(p.RoleNames()))
		}
		if e.Port == "" {
			l.err("R42", w+".port", line, "export is missing port", "")
		}

		// R47：fields 的值必须是可解析的表达式，且引用的参数存在于本 Pack
		all := p.AllParams()
		for _, f := range e.FieldNames() {
			fw := w + ".fields." + f
			l.checkExprRefs(fw, line, e.Fields[f])
			for _, pn := range ExprReferencedParams(e.Fields[f]) {
				if _, ok := all[pn]; !ok {
					l.err("R47", fw, line,
						fmt.Sprintf("exported field references parameter %q, which does not exist", pn),
						"an export exposes a value from this Pack; the field name seen by consumers can differ "+
							"from the parameter name, but the parameter itself must exist")
				}
			}
		}

		// R48：只有 role + port 时按默认 format 处理，不算错；
		// 但显式给了空的 fields 多半是写了一半忘了填
		if e.Fields != nil && len(e.Fields) == 0 {
			l.err("R48", w+".fields", line, "fields is empty",
				"either remove it (to export the address via the default format) or fill in fields")
		}
	}
}

// ── R15 / R21 模板 ──────────────────────────────────────────────────────

func (l *linter) checkTemplates() {
	p := l.p
	ts := l.ts

	// 把 pack.yaml 中的字段表达式并入模板集合
	l.parseFieldExprs()

	all := ts.CollectAllRefs()

	// R15：{{ template "x" }} 引用的模板必须已定义
	for _, name := range sortedKeys(all.Templates) {
		if !ts.Defined[name] {
			l.err("R15", DirTemplates, 0,
				fmt.Sprintf("template reference {{ template %q }} is undefined", name),
				"define a fragment with {{ define \"name\" }}; a filename starting with _ marks it as a fragment")
		}
	}

	// R21：对每个 profile 独立校验模板中的参数引用。
	//
	// 守卫来自两处，必须合取：
	//   ① 渲染该模板的 template 资源上的 `when`
	//   ② 模板内部的 `{{ if eq .Profile "…" }}`
	// 缺了任何一处都会产生大量误报。
	renderGuards := l.templateRenderGuards()
	reported := map[string]bool{}

	for _, name := range ts.Files {
		refs := ts.CollectRefs(name)
		outer, rendered := renderGuards[name]
		if !rendered {
			// 片段（_ 前缀）不被直接渲染，其引用随引用它的主模板一并校验
			if strings.HasPrefix(pathBase(name), PartialPrefix) {
				continue
			}
			outer = Guard{}
		}

		for _, profile := range p.ProfileNames() {
			if !outer.Admits(profile) {
				continue
			}
			visible := p.ParamsForProfile(profile)
			label := profile
			if label == "" {
				label = "(no profile)"
			}
			for _, ref := range refs.Params {
				if !ref.Guard.Admits(profile) {
					continue
				}
				if _, ok := visible[ref.Name]; ok {
					continue
				}
				key := name + "|" + ref.Name + "|" + profile
				if reported[key] {
					continue
				}
				reported[key] = true
				l.err("R21", DirTemplates+"/"+name, 0,
					fmt.Sprintf("profile %s references undeclared parameter .Params.%s", label, ref.Name),
					"the parameter may only be declared in another profile; promote it to top-level params, "+
						"wrap it in {{ if eq .Profile \"...\" }}, or add a when to the resource that renders it")
			}
		}

		// 角色与依赖引用与形态无关，只校验一遍
		for _, rn := range sortedKeys(refs.Roles) {
			if !l.roleSet[rn] {
				l.err("R21", DirTemplates+"/"+name, 0,
					fmt.Sprintf("references role .Topology.Role %q, which does not exist", rn),
					"defined roles: "+quoteList(p.RoleNames()))
			}
		}
		for _, dn := range sortedKeys(refs.Requires) {
			if _, ok := l.depSet[dn]; !ok {
				l.err("R38", DirTemplates+"/"+name, 0,
					fmt.Sprintf("references undeclared dependency .Requires.%s", dn),
					"declared dependencies: "+quoteList(sortedKeys(l.depSet)))
			}
		}
	}

	// R21：模板中与 .Profile 比较的字面量必须是已定义的形态
	if len(p.Profiles) > 0 {
		known := map[string]bool{}
		for _, n := range p.ProfileNames() {
			known[n] = true
		}
		for _, name := range sortedKeys(all.Profiles) {
			if !known[name] {
				l.err("R21", DirTemplates, 0,
					fmt.Sprintf("template compares .Profile against %q, which is not a defined profile", name),
					"defined profiles: "+quoteList(p.ProfileNames()))
			}
		}
	} else if all.UsesProfile {
		l.warn("R21", DirTemplates, 0,
			"template references .Profile, but the Pack declares no profiles",
			".Profile is always an empty string when profiles are not declared")
	}

	// R40：scope: site 的依赖不得被引用 .Paths
	for _, dn := range sortedKeys(l.depSet) {
		if l.depSet[dn].EffectiveScope() != ScopeSite {
			continue
		}
		for _, name := range ts.Files {
			body := l.templateBody(name)
			if ExprReferencesPaths(body, dn) {
				l.err("R40", DirTemplates+"/"+name, 0,
					fmt.Sprintf("dependency %q is scope: site, its .Paths must not be referenced", dn),
					"that's a path on a different machine. a site-scoped dependency only provides .Topology / .Exports / .Version")
			}
		}
	}

	// R49 / R43：模板里同样不得读取依赖的参数，导出名同样要核对
	for _, name := range ts.Files {
		body := l.templateBody(name)
		l.checkNoDepParams(DirTemplates+"/"+name, 0, body)
		l.checkDepExports(DirTemplates+"/"+name, 0, body)
	}

	l.checkSensitiveFileMode()

	// R25 需要 workload 信息，放在模板之后统一执行
	l.checkParamReloadable()
}

// parseFieldExprs 把 pack.yaml 中含模板语法的字段并入模板集合，
// 使 pack.yaml 字段能调用 templates/ 中的片段（规范 §9.1）。
func (l *linter) parseFieldExprs() {
	add := func(name, expr string) {
		if expr == "" || !strings.Contains(expr, "{{") {
			return
		}
		if err := l.ts.ParseExpr("field:"+name, expr); err != nil {
			l.err("R15", name, 0, fmt.Sprintf("field expression parse failed: %v", err), "")
		}
	}

	for _, r := range l.p.Roles {
		rn := r.EffectiveName()
		if r.Workload != nil && r.Workload.Systemd != nil {
			add(fmt.Sprintf("roles[%s].workload.systemd.exec", rn), r.Workload.Systemd.Exec)
			add(fmt.Sprintf("roles[%s].workload.systemd.execReload", rn), r.Workload.Systemd.ExecReload)
		}
		for pn, pv := range r.Params {
			add(fmt.Sprintf("roles[%s].params.%s.from", rn, pn), pv.From)
			add(fmt.Sprintf("roles[%s].params.%s.defaultFrom", rn, pn), pv.DefaultFrom)
		}
	}
	for pn, pv := range l.p.Params {
		add("params."+pn+".from", pv.From)
		add("params."+pn+".defaultFrom", pv.DefaultFrom)
	}
	for _, pr := range l.p.Profiles {
		for pn, pv := range pr.Params {
			add(fmt.Sprintf("profiles[%s].params.%s.from", pr.Name, pn), pv.From)
			add(fmt.Sprintf("profiles[%s].params.%s.defaultFrom", pr.Name, pn), pv.DefaultFrom)
		}
	}
	for _, en := range sortedKeys(l.p.Exports) {
		e := l.p.Exports[en]
		add(fmt.Sprintf("exports.%s.port", en), e.Port)
		add(fmt.Sprintf("exports.%s.format", en), e.Format)
		for _, f := range e.FieldNames() {
			add(fmt.Sprintf("exports.%s.fields.%s", en, f), e.Fields[f])
		}
	}
}

// templateRenderGuards 计算每个模板文件「在哪些形态下会被渲染」。
//
// 同一个模板可能被多条资源引用；此时守卫取**并集**（任一条件成立即会渲染）。
// checkSensitiveFileMode 实现 R46：渲染了敏感参数的文件，其他人不得可读。
//
// 只管 others 位（`mode & 0007 == 0`），不强制 0600：多个同组服务共读一份
// 凭据文件是正当模式，0640 完全合理。
//
// **未声明 mode 同样算违规**——引擎缺省写 0644，那是全体可读。让它静默
// 通过等于把最容易犯的错放过去。
func (l *linter) checkSensitiveFileMode() {
	all := l.p.AllParams()

	// 该模板（含它 include 的片段）是否引用了敏感参数
	carriesSecret := func(src string) bool {
		for _, pr := range l.ts.CollectRefsDeep(src).Params {
			if pv, ok := all[pr.Name]; ok && pv.IsSensitive() {
				return true
			}
		}
		return false
	}

	// R50：内联 Environment= 会把值写进 unit 文件。
	//
	// unit 文件按 systemd 惯例是 0644 全体可读，而且 `systemctl show -p
	// Environment` 会原样打印它——支持包与故障报告里常有这条命令的输出。
	// EnvironmentFile 则只暴露路径，值不进 systemctl（已实测确认）。
	for _, role := range l.p.Roles {
		if role.Workload == nil || role.Workload.Systemd == nil {
			continue
		}
		for _, k := range sortedKeys(role.Workload.Systemd.Env) {
			if !l.p.ExprCarriesSecret(role.Workload.Systemd.Env[k]) {
				continue
			}
			l.err("R50",
				fmt.Sprintf("roles[%s].workload.systemd.env.%s", role.EffectiveName(), k),
				l.p.LineOf("roles"),
				fmt.Sprintf("environment variable %s's value contains a sensitive parameter", k),
				"inline Environment= gets written into the 0644 unit file and shows up in systemctl show output. "+
					"use envFile instead, pointing to a file with mode 0640")
		}
	}

	// R52：容器的挂载不得引用 .Paths.Current
	//
	// Docker 在**创建容器时**解析 bind mount 的路径。`current` 是一条软链，
	// 解析之后容器绑的是当时那个 generation 目录。之后 generation 切换、
	// 软链改指向，容器里看到的仍然是旧的那份——而 `ls -l` 看软链一切正常。
	//
	// 这类「文件明明改了、进程就是读不到」的现场极难查，因此宁可在 lint
	// 阶段拒绝。**同一句话在 systemd 的 exec 里是正确写法**，所以这条规则
	// 只能按 runtime 分别判断（19-container-runtime §3）。
	l.checkContainerMountPaths()

	// R51：自动改回 + 重启 = 服务在运维手底下重启
	l.p.AllResources(func(owner string, idx int, r Resource) {
		if r.DriftPolicy == "reconcile" && r.Notify == "restart" {
			l.warn("R51", fmt.Sprintf("%s.resources[%d]", owner, idx), r.Line,
				"driftPolicy: reconcile is used together with notify: restart",
				"an operator just wants to try a parameter, and the service gets restarted -- this is the last thing a tool should decide on its own. "+
					"the engine downgrades it to report-only by default; the deployer must opt in with allowDriftRestart to actually execute it")
		}
	})

	l.p.AllResources(func(owner string, idx int, r Resource) {
		var src, dest string
		switch r.Type {
		case ResTemplate:
			src, dest = r.Arg("src"), r.Arg("dest")
		case ResFile:
			// file 的内容若来自 content 且含敏感参数，同样要管
			dest = r.Arg("path")
		default:
			return
		}

		sensitive := false
		if src != "" {
			sensitive = carriesSecret(src)
		} else if c := r.Arg("content"); c != "" {
			sensitive = l.p.ExprCarriesSecret(c)
		}
		if !sensitive {
			return
		}

		where := fmt.Sprintf("%s.resources[%d]", owner, idx)
		mode := r.Arg("mode")
		if mode == "" {
			l.err("R46", where, r.Line,
				fmt.Sprintf("%s renders a sensitive parameter, but mode is not declared", dest),
				"the default is 0644 -- world-readable. declare it explicitly for files containing secrets, e.g. mode: \"0640\"")
			return
		}
		n, err := strconv.ParseUint(mode, 8, 32)
		if err != nil {
			return // mode 写法非法由 R11 报
		}
		if n&0o007 != 0 {
			l.err("R46", where, r.Line,
				fmt.Sprintf("%s renders a sensitive parameter, but mode %s is readable by other users", dest, mode),
				"the others bits must be 0. 0600 is not required -- multiple same-group services sharing one credential file is legitimate, "+
					"0640 is perfectly fine")
		}
	})
}

func (l *linter) templateRenderGuards() map[string]Guard {
	out := map[string]Guard{}
	seen := map[string]bool{}

	record := func(src, when string) {
		if src == "" {
			return
		}
		g := ProfileGuardOf(when)
		if !seen[src] {
			seen[src] = true
			out[src] = g
			return
		}
		// 已有守卫，取并集：任一路径无条件渲染则整体无条件
		prev := out[src]
		if len(prev.Only) == 0 && len(prev.Not) == 0 {
			return
		}
		if len(g.Only) == 0 && len(g.Not) == 0 {
			out[src] = Guard{}
			return
		}
		merged := Guard{Only: map[string]bool{}, Not: map[string]bool{}}
		mergeSet(merged.Only, prev.Only)
		mergeSet(merged.Only, g.Only)
		// Not 只有在两侧都排除时才继续排除
		for k := range prev.Not {
			if g.Not[k] {
				merged.Not[k] = true
			}
		}
		out[src] = merged
	}

	l.p.AllResources(func(owner string, idx int, r Resource) {
		if r.Type == ResTemplate {
			record(r.Arg("src"), r.When)
		}
	})
	for _, role := range l.p.Roles {
		if role.Workload != nil && role.Workload.Compose != nil {
			record(role.Workload.Compose.File, "")
		}
	}
	return out
}

func pathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func (l *linter) templateBody(name string) string {
	if l.ts.tmpl == nil {
		return ""
	}
	t := l.ts.tmpl.Lookup(name)
	if t == nil || t.Tree == nil || t.Tree.Root == nil {
		return ""
	}
	return t.Tree.Root.String()
}

// checkContainerMountPaths 实现规则 52。
//
// 两种 runtime 的判定精度不同，因此严重级别也不同：
//
//	docker   知道确切是哪个字段  → 报错
//	compose  挂载写在模板里，认不出是哪一行 → 告警
//
// compose 一律降级为告警是刻意的：模板里出现 `.Paths.Current` 几乎总是
// 错的（compose 文件里的一切都在创建容器时固化），但**指不出具体位置的
// 错误会让人无从下手**。宁可说「这个文件里有一处，去看看」，也不要报一条
// 用户对不上号的错。
func (l *linter) checkContainerMountPaths() {
	const advice = "mount config at {{ .Paths.Config }} and data at {{ .Paths.Data }}; " +
		"for something that genuinely needs to live inside generation, write {{ .Paths.Generation }} explicitly -- " +
		"it points at the current one and won't be fooled by the symlink"

	for _, role := range l.p.Roles {
		w := role.Workload
		if w == nil {
			continue
		}
		owner := fmt.Sprintf("roles[%s].workload", role.EffectiveName())
		line := l.p.LineOf("roles")

		if w.Runtime == RuntimeDocker && w.Docker != nil {
			for i, m := range w.Docker.Mounts {
				if !ExprReferencesCurrentPath(m.From) {
					continue
				}
				l.err("R52", fmt.Sprintf("%s.docker.mounts[%d].from", owner, i), line,
					fmt.Sprintf("mount source %q references .Paths.Current", m.From),
					advice)
			}
		}

		if w.Runtime == RuntimeCompose && w.Compose != nil && w.Compose.File != "" {
			name := strings.TrimPrefix(w.Compose.File, DirTemplates+"/")
			body := l.templateBody(name)
			if body != "" && ExprReferencesCurrentPath(body) {
				l.warn("R52", DirTemplates+"/"+name, 0,
					"the compose file references .Paths.Current",
					"everything in a compose file is fixed at container-creation time; the symlink gets bound to "+
						"whatever generation was current then. "+advice)
			}
		}
	}
}
