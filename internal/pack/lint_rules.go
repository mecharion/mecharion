package pack

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// dnsLabel 是 name 字段的合法形式（规范 R02）。
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// cardinalityRe 匹配 "1" / "0-1" / "1-N" / "0-N" / "3"。
var cardinalityRe = regexp.MustCompile(`^(\d+)(?:-(\d+|N))?$`)

// platformRe 匹配 "linux/amd64"。
var platformRe = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+$`)

// sha256Re 匹配 64 位小写十六进制。
var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ── R01–R08 结构 ────────────────────────────────────────────────────────

func (l *linter) checkStructure() {
	p := l.p

	if p.Schema != SchemaV1 {
		l.err("R01", "schema", p.LineOf("schema"),
			fmt.Sprintf("schema = %q, this implementation only supports %q", p.Schema, SchemaV1), "")
	}

	if p.Name == "" {
		l.err("R02", "name", p.LineOf("name"), "name must not be empty", "")
	} else {
		if !dnsLabel.MatchString(p.Name) {
			l.err("R02", "name", p.LineOf("name"),
				fmt.Sprintf("name %q does not conform to DNS label rules", p.Name),
				"only lowercase letters, digits, and hyphens are allowed, and it must not start or end with a hyphen")
		}
		if len(p.Name) > 63 {
			l.err("R02", "name", p.LineOf("name"),
				fmt.Sprintf("name length %d exceeds 63", len(p.Name)), "")
		}
	}

	if strings.TrimSpace(p.Version) == "" {
		l.err("R03", "version", p.LineOf("version"), "version must not be empty", "")
	}
	if p.Revision < 1 {
		l.err("R03", "revision", p.LineOf("revision"),
			fmt.Sprintf("revision = %d, must be >= 1", p.Revision), "defaults to 1 when omitted")
	}

	// R04：platforms 非空，且每个平台在每个 blob 中都有条目
	if len(p.Platforms) == 0 {
		l.err("R04", "platforms", p.LineOf("platforms"), "platforms must not be empty",
			"must be declared explicitly in published artifacts; source files may omit it and let mechpack assemble fill it in")
	}
	for _, plat := range p.Platforms {
		if !platformRe.MatchString(plat) {
			l.err("R04", "platforms", p.LineOf("platforms"),
				fmt.Sprintf("platform identifier %q has invalid format", plat), "e.g. linux/amd64")
		}
	}
	for _, bn := range sortedKeys(p.Blobs) {
		blob := p.Blobs[bn]
		for _, plat := range p.Platforms {
			if _, ok := blob[plat]; !ok {
				l.err("R04", "blobs."+bn, p.LineOf("blobs", bn),
					fmt.Sprintf("blob %q is missing an entry for platform %q", bn, plat),
					"add the blob for that platform, or remove the platform from platforms")
			}
		}
		// R05：每个条目的必填字段
		for _, plat := range sortedKeys(blob) {
			e := blob[plat]
			where := "blobs." + bn + "." + plat
			line := p.LineOf("blobs", bn, plat)
			if e.SHA256 == "" {
				l.err("R05", where, line, "missing sha256", "")
			} else if !sha256Re.MatchString(e.SHA256) {
				l.err("R05", where, line,
					fmt.Sprintf("sha256 %q is not 64 lowercase hex characters", e.SHA256), "")
			}
			if e.Size <= 0 {
				l.err("R05", where, line, "missing size, or size <= 0", "")
			}
			if e.Filename == "" {
				l.err("R05", where, line, "missing filename",
					"blobs are stored under their digest; filename preserves the source filename for operator traceability")
			}
			if e.MediaType != "" && !validMediaType(e.MediaType) {
				l.err("R05", where, line,
					fmt.Sprintf("mediaType %q is not recognized", e.MediaType),
					"available: tar / tar.gz / tar.zst / zip / raw / docker-archive")
			}
		}
	}

	// R07：模板 / 文件 / hook 引用的文件必须存在
	l.checkFileRefs()
}

func validMediaType(s string) bool {
	switch s {
	case MediaTar, MediaTarGz, MediaTarZst, MediaZip, MediaRaw, MediaDockerArchive:
		return true
	}
	return false
}

func (l *linter) checkFileRefs() {
	p := l.p
	p.AllResources(func(owner string, idx int, r Resource) {
		where := fmt.Sprintf("%s.resources[%d].%s", owner, idx, r.Type)
		switch r.Type {
		case ResTemplate:
			if src := r.Arg("src"); src != "" && !p.HasFile(path.Join(DirTemplates, src)) {
				l.err("R07", where, r.Line,
					fmt.Sprintf("template %s/%s does not exist", DirTemplates, src), "")
			}
		case ResFile:
			if src := r.Arg("source"); src != "" && !p.HasFile(path.Join(DirFiles, src)) {
				l.err("R07", where, r.Line,
					fmt.Sprintf("file %s/%s does not exist", DirFiles, src), "")
			}
		case ResScript:
			if src := r.Arg("src"); src != "" && !p.HasFile(path.Join(DirHooks, src)) {
				l.err("R07", where, r.Line,
					fmt.Sprintf("script %s/%s does not exist", DirHooks, src), "")
			}
		}
	})
	p.AllHooks(func(owner, point string, hk Hook) {
		if hk.Script == "" {
			l.err("R07", owner+"."+point, hk.Line, "hook is missing script", "")
			return
		}
		// 允许写成 "hooks/x.sh" 或 "x.sh"——归一化只此一处，
		// 渲染侧用的是同一个函数
		rel := HookScriptPath(hk.Script)
		if !p.HasFile(rel) {
			l.err("R07", owner+"."+point, hk.Line,
				fmt.Sprintf("hook script %s does not exist", rel), "")
		}
	})
	// compose 的模板文件
	for _, role := range p.Roles {
		if role.Workload == nil || role.Workload.Compose == nil {
			continue
		}
		f := role.Workload.Compose.File
		if f != "" && !p.HasFile(path.Join(DirTemplates, f)) {
			l.err("R07", fmt.Sprintf("roles[%s].workload.compose.file", role.EffectiveName()), 0,
				fmt.Sprintf("compose template %s/%s does not exist", DirTemplates, f), "")
		}
	}
}

// ── R09–R12 角色 ────────────────────────────────────────────────────────

func (l *linter) checkRoles() {
	p := l.p

	if len(p.Roles) == 0 {
		l.err("R09", "roles", p.LineOf("roles"), "roles must not be empty", "declare at least one role")
		return
	}

	seen := map[string]int{}
	for i, r := range p.Roles {
		name := r.EffectiveName()
		where := fmt.Sprintf("roles[%d]", i)
		line := p.LineOf("roles", strconv.Itoa(i))

		if prev, dup := seen[name]; dup {
			l.err("R09", where, line,
				fmt.Sprintf("role name %q is duplicated (already appears in roles[%d])", name, prev), "")
		}
		seen[name] = i

		if r.Name != "" && !dnsLabel.MatchString(r.Name) {
			l.err("R09", where+".name", line,
				fmt.Sprintf("role name %q does not conform to DNS label rules", r.Name), "")
		}
		if r.Name == "" && len(p.Roles) > 1 {
			l.err("R09", where+".name", line,
				"role name cannot be omitted in a multi-role Pack", "only a single-role Pack may omit it, defaulting to \"default\"")
		}

		// R11
		if !validCardinality(r.EffectiveCardinality()) {
			l.err("R11", where+".cardinality", line,
				fmt.Sprintf("cardinality %q has invalid syntax", r.Cardinality),
				`available: "1" / "0-1" / "1-N" / "0-N" / "N"`)
		}

		// R12
		if r.Scope != "" && r.Scope != "node" {
			if r.Scope == "cluster" {
				l.err("R12", where+".scope", line,
					"scope: cluster is not yet supported", "v1 only supports node; see ADR-0017 for cluster-scoped roles")
			} else {
				l.err("R12", where+".scope", line,
					fmt.Sprintf("scope %q is invalid", r.Scope), "available: node (v1)")
			}
		}

		// R44：quorum 角色的 cardinality 下限须 ≥ 1
		if r.Quorum {
			if lo, _, ok := cardinalityBounds(r.EffectiveCardinality()); ok && lo < 1 {
				// cardinality "0" 是「默认关闭、由 profile 打开」的惯用写法，
				// 此时下限由 profile 覆盖，不在此报错
				if r.EffectiveCardinality() != "0" {
					l.warn("R44", where+".cardinality", line,
						fmt.Sprintf("role %q declares quorum but cardinality's lower bound is %d", name, lo),
						"a quorum role should usually have at least 1 instance")
				}
			}
		}

		// R10：角色依赖存在且不成环
		for _, dep := range r.Requires {
			if !l.roleSet[dep] {
				l.err("R10", where+".requires", line,
					fmt.Sprintf("role %q depends on role %q, which does not exist", name, dep),
					"defined roles: "+quoteList(p.RoleNames()))
			}
		}

		// 工作负载与健康检查
		l.checkWorkload(i, r)
	}

	if cyc := findRoleCycle(p); cyc != nil {
		l.err("R10", "roles", p.LineOf("roles"),
			"role dependency cycle: "+strings.Join(cyc, " → "), "requires only constrains ordering and must not form a cycle")
	}
}

func validCardinality(s string) bool {
	m := cardinalityRe.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	if m[2] == "" || m[2] == "N" {
		return true
	}
	lo, _ := strconv.Atoi(m[1])
	hi, _ := strconv.Atoi(m[2])
	return hi >= lo
}

// cardinalityBounds 返回下限与上限（上限 -1 表示 N）。
func cardinalityBounds(s string) (lo, hi int, ok bool) {
	m := cardinalityRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	lo, _ = strconv.Atoi(m[1])
	switch m[2] {
	case "":
		return lo, lo, true
	case "N":
		return lo, -1, true
	default:
		hi, _ = strconv.Atoi(m[2])
		return lo, hi, true
	}
}

func findRoleCycle(p *Pack) []string {
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
		r := p.RoleByName(n)
		if r != nil {
			for _, d := range r.Requires {
				if p.RoleByName(d) == nil {
					continue
				}
				switch state[d] {
				case gray:
					// 找到环，截取
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

	names := p.RoleNames()
	sort.Strings(names)
	for _, n := range names {
		if state[n] == white {
			if c := dfs(n); c != nil {
				return c
			}
		}
	}
	return nil
}

func (l *linter) checkWorkload(idx int, r Role) {
	name := r.EffectiveName()
	where := fmt.Sprintf("roles[%s].workload", name)

	if r.Workload == nil {
		if r.Health != nil {
			l.warn("R09", fmt.Sprintf("roles[%s].health", name), 0,
				fmt.Sprintf("role %q declares health but has no workload", name),
				"a role with no process does not need health checks")
		}
		return
	}

	w := r.Workload
	known := false
	for _, k := range KnownRuntimes {
		if w.Runtime == k {
			known = true
		}
	}
	if !known {
		hint := "v1 supports systemd / docker / compose"
		if w.Runtime == RuntimePodman {
			hint = "podman is not yet implemented, v1 supports systemd / docker / compose"
		}
		l.err("R09", where+".runtime", 0,
			fmt.Sprintf("runtime %q is not supported", w.Runtime), hint)
	}

	// runtime 与其参数段必须匹配
	switch w.Runtime {
	case RuntimeSystemd:
		if w.Systemd == nil {
			l.err("R09", where, 0, "runtime: systemd is missing the systemd block", "")
		} else if strings.TrimSpace(w.Systemd.Exec) == "" {
			l.err("R09", where+".systemd.exec", 0, "systemd.exec must not be empty", "")
		}
	case RuntimeDocker:
		if w.Docker == nil {
			l.err("R09", where, 0, "runtime: docker is missing the docker block", "")
		} else if w.Docker.ImageBlob == "" {
			l.err("R09", where+".docker.imageBlob", 0, "docker.imageBlob must not be empty", "")
		}
	case RuntimeCompose:
		if w.Compose == nil {
			l.err("R09", where, 0, "runtime: compose is missing the compose block", "")
		}
	}

	// 健康检查探针互斥
	if r.Health != nil {
		switch n := r.Health.ProbeCount(); {
		case n == 0:
			l.err("R09", fmt.Sprintf("roles[%s].health", name), 0,
				"health declares no probes", "choose exactly one: http / tcp / exec")
		case n > 1:
			l.err("R09", fmt.Sprintf("roles[%s].health", name), 0,
				fmt.Sprintf("health declares %d probe types, but they are mutually exclusive", n), "")
		}
	}
}

// ── R13–R15 放置约束 ────────────────────────────────────────────────────

func (l *linter) checkPlacement() {
	l.checkPlacementList("placement", l.p.Placement, l.roleSet)
	for i, pr := range l.p.Profiles {
		enabled := l.enabledRoles(pr)
		l.checkPlacementList(fmt.Sprintf("profiles[%s].placement", pr.Name), pr.Placement, enabled)
		_ = i
	}
}

func (l *linter) checkPlacementList(where string, list []Placement, valid map[string]bool) {
	type key struct{ a, b, scope string }
	seenAnti := map[key]bool{}
	seenAff := map[key]bool{}

	for i, pl := range list {
		w := fmt.Sprintf("%s[%d]", where, i)

		if len(pl.AntiAffinity) > 0 && len(pl.Affinity) > 0 {
			l.err("R13", w, 0, "antiAffinity and affinity are mutually exclusive and cannot both be declared", "")
			continue
		}
		roles := pl.Roles()
		if len(roles) == 0 {
			l.err("R13", w, 0, "must declare either antiAffinity or affinity", "")
			continue
		}

		// R14
		if len(pl.Affinity) == 1 {
			l.err("R14", w+".affinity", 0,
				fmt.Sprintf("affinity contains only one role %q, which has no effect", pl.Affinity[0]),
				"a single element is only meaningful in antiAffinity (meaning instances of that role exclude each other)")
		}

		// R13
		for _, rn := range roles {
			if !valid[rn] {
				hint := "roles enabled in this profile: " + quoteList(sortedKeys(valid))
				l.err("R13", w, 0,
					fmt.Sprintf("referenced role %q does not exist here or has been disabled", rn), hint)
			}
		}

		if pl.EffectiveEnforcement() != EnforceRequired && pl.EffectiveEnforcement() != EnforcePreferred {
			l.err("R13", w+".enforcement", 0,
				fmt.Sprintf("enforcement %q is invalid", pl.Enforcement), "available: required / preferred")
		}

		// R15：同一组角色不得在相同 scope 下既 affinity 又 antiAffinity
		scope := pl.EffectiveScope()
		for a := 0; a < len(roles); a++ {
			for b := a + 1; b < len(roles); b++ {
				x, y := roles[a], roles[b]
				if x > y {
					x, y = y, x
				}
				k := key{x, y, scope}
				if len(pl.AntiAffinity) > 0 {
					seenAnti[k] = true
					if seenAff[k] {
						l.err("R15", w, 0,
							fmt.Sprintf("roles %q and %q are declared as both affinity and antiAffinity under scope=%s", x, y, scope), "")
					}
				} else {
					seenAff[k] = true
					if seenAnti[k] {
						l.err("R15", w, 0,
							fmt.Sprintf("roles %q and %q are declared as both affinity and antiAffinity under scope=%s", x, y, scope), "")
					}
				}
			}
		}

		if pl.Reason == "" && pl.EffectiveEnforcement() == EnforceRequired {
			l.warn("R13", w+".reason", 0,
				"a required constraint should have a reason", "the reason appears in the validation-failure error message and directly affects whether operators can act on it")
		}
	}
}

func (l *linter) enabledRoles(pr Profile) map[string]bool {
	out := map[string]bool{}
	for _, n := range l.p.RoleNames() {
		out[n] = true
	}
	for n, o := range pr.Roles {
		if !o.IsEnabled() {
			delete(out, n)
		}
	}
	return out
}
