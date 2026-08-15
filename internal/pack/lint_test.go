package pack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePack 在临时目录里搭一个 Pack。extra 是 相对路径→内容。
func writePack(t *testing.T, manifest string, extra map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, PackFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, content := range extra {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// minimal 是一个能通过全部规则的最小 Pack，供各用例做增量修改。
const minimal = `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
blobs:
  main:
    linux/amd64:
      sha256: "0000000000000000000000000000000000000000000000000000000000000000"
      size: 1024
      filename: demo.tar.gz
params:
  port: { type: port, default: 8080 }
roles:
  - resources:
      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }
    workload:
      runtime: systemd
      systemd:
        exec: "{{ .Paths.Current }}/bin/demo"
`

func lintSrc(t *testing.T, manifest string, extra map[string]string, opts Options) *Result {
	t.Helper()
	p, err := Load(writePack(t, manifest, extra))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return Lint(p, opts)
}

// hasRule 报告结果中是否有指定规则的错误。
func hasRule(r *Result, rule string) bool {
	for _, f := range r.Findings {
		if f.Rule == rule && f.Severity == SevError {
			return true
		}
	}
	return false
}

func dump(r *Result) string {
	var b strings.Builder
	for _, f := range r.Findings {
		b.WriteString(f.String())
		b.WriteString("\n")
	}
	return b.String()
}

func TestMinimalPackIsClean(t *testing.T) {
	res := lintSrc(t, minimal, nil, Options{Hermetic: true})
	if !res.OK() {
		t.Fatalf("最小 Pack 应当无错误，实际:\n%s", dump(res))
	}
}

// TestRulesCatchViolations 是 lint 的主回归表：每个用例故意违反一条规则，
// 断言对应规则被触发。**「示例全过」不等于检查器有效，必须有反例。**
func TestRulesCatchViolations(t *testing.T) {
	cases := []struct {
		name     string
		rule     string
		manifest string
		extra    map[string]string
	}{
		{
			name:     "R01 schema 不匹配",
			rule:     "R01",
			manifest: strings.Replace(minimal, "schema: pack/v1", "schema: pack/v2", 1),
		},
		{
			name:     "R02 name 非法",
			rule:     "R02",
			manifest: strings.Replace(minimal, "name: demo", "name: Demo_App", 1),
		},
		{
			name:     "R04 blob 缺平台",
			rule:     "R04",
			manifest: strings.Replace(minimal, "platforms: [linux/amd64]", "platforms: [linux/amd64, linux/arm64]", 1),
		},
		{
			name:     "R05 blob 缺 filename",
			rule:     "R05",
			manifest: strings.Replace(minimal, "      filename: demo.tar.gz\n", "", 1),
		},
		{
			name:     "R06 引用不存在的 blob",
			rule:     "R06",
			manifest: strings.Replace(minimal, "blob: main", "blob: nope", 1),
		},
		{
			name: "R06b file 同时给了 content 与 blob",
			rule: "R06b",
			manifest: strings.Replace(minimal,
				`      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }`,
				`      - file: { blob: main, content: "x", path: /tmp/a }`, 1),
		},
		{
			name: "R06c raw blob 走了 archive",
			rule: "R06c",
			manifest: strings.Replace(minimal,
				"      filename: demo.tar.gz",
				"      filename: demo\n      mediaType: raw", 1),
		},
		{
			name: "R09 多角色却省略角色名",
			rule: "R09",
			manifest: minimal + `
  - name: other
    cardinality: "0-N"
`,
		},
		{
			name: "R10 角色依赖不存在",
			rule: "R10",
			manifest: strings.Replace(minimal,
				"  - resources:", "  - requires: [ghost]\n    resources:", 1),
		},
		{
			name: "R11 cardinality 语法非法",
			rule: "R11",
			manifest: strings.Replace(minimal,
				"  - resources:", `  - cardinality: "many"`+"\n    resources:", 1),
		},
		{
			name: "R12 scope cluster 未支持",
			rule: "R12",
			manifest: strings.Replace(minimal,
				"  - resources:", "  - scope: cluster\n    resources:", 1),
		},
		{
			name: "R13 placement 引用不存在的角色",
			rule: "R13",
			manifest: minimal + `
placement:
  - antiAffinity: [ghost, default]
    scope: node
    reason: x
`,
		},
		{
			name: "R14 affinity 只含一个角色",
			rule: "R14",
			manifest: minimal + `
placement:
  - affinity: [default]
    scope: node
    reason: x
`,
		},
		{
			name: "R15 affinity 与 antiAffinity 冲突",
			rule: "R15",
			manifest: strings.Replace(minimal, "  - resources:", "  - name: a\n    resources:", 1) + `
  - name: b
    cardinality: "0-N"
placement:
  - antiAffinity: [a, b]
    scope: node
    reason: x
  - affinity: [a, b]
    scope: node
    reason: y
`,
		},
		{
			name: "R16 多个 default profile",
			rule: "R16",
			manifest: minimal + `
profiles:
  - { name: p1, default: true }
  - { name: p2, default: true }
`,
		},
		{
			name: "R17 profile 引用不存在的角色",
			rule: "R17",
			manifest: minimal + `
profiles:
  - name: p1
    roles:
      ghost: { cardinality: "1" }
`,
		},
		{
			name: "R19 profile 不可满足",
			rule: "R19",
			manifest: strings.Replace(minimal, "  - resources:", "  - name: a\n    resources:", 1) + `
  - name: b
    cardinality: "0-N"
profiles:
  - name: tight
    minNodes: 1
    roles:
      a: { cardinality: "2" }
      b: { cardinality: "2" }
    placement:
      - antiAffinity: [a, b]
        scope: node
        reason: x
`,
		},
		{
			name: "R20 upgradeFrom 引用不存在的 profile",
			rule: "R20",
			manifest: minimal + `
profiles:
  - { name: p1, upgradeFrom: [ghost] }
`,
		},
		{
			name: "R21 模板引用未声明的参数",
			rule: "R21",
			manifest: strings.Replace(minimal,
				`      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }`,
				`      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }
      - template: { src: a.tmpl, dest: /tmp/a }`, 1),
			extra: map[string]string{"templates/a.tmpl": "x={{ .Params.ghost }}\n"},
		},
		{
			name:     "R22 未知参数类型",
			rule:     "R22",
			manifest: strings.Replace(minimal, "type: port", "type: ipv6", 1),
		},
		{
			name:     "R23 enum 缺 values",
			rule:     "R23",
			manifest: strings.Replace(minimal, "{ type: port, default: 8080 }", "{ type: enum, default: a }", 1),
		},
		{
			name: "R24 restart 与 reload 互斥",
			rule: "R24",
			manifest: strings.Replace(minimal, "{ type: port, default: 8080 }",
				"{ type: port, default: 8080, restartRequired: true, reloadRequired: true }", 1),
		},
		{
			name: "R25 reloadRequired 但无 execReload",
			rule: "R25",
			manifest: strings.Replace(minimal, "{ type: port, default: 8080 }",
				"{ type: port, default: 8080, reloadRequired: true }", 1),
		},
		{
			name: "R26b from 与 default 互斥",
			rule: "R26b",
			manifest: strings.Replace(minimal, "{ type: port, default: 8080 }",
				`{ type: string, default: x, from: "topology.role('default').nodes[0].address" }`, 1),
		},
		{
			name: "R26c defaultFrom 缺 default",
			rule: "R26c",
			manifest: strings.Replace(minimal, "{ type: port, default: 8080 }",
				`{ type: size, defaultFrom: "{{ .Node.Facts.Memory.Total }}" }`, 1),
		},
		{
			name: "R27 kind multi 的 default 不是列表",
			rule: "R27",
			manifest: minimal + `
paths:
  dataDirs:
    kind: multi
    default: "/var/lib/x"
`,
		},
		{
			name: "R28 linkInto 不在 generation 内",
			rule: "R28",
			manifest: minimal + `
paths:
  config:
    default: /etc/x
    linkInto: /opt/elsewhere/conf
`,
		},
		{
			name: "R29 inline 与 linkInto 互斥",
			rule: "R29",
			manifest: minimal + `
paths:
  config:
    layout: inline
    default: "{{ .Paths.Generation }}/conf"
    linkInto: "{{ .Paths.Generation }}/conf"
`,
		},
		{
			name: "R30 inline 用于 data",
			rule: "R30",
			manifest: minimal + `
paths:
  data:
    layout: inline
    default: "{{ .Paths.Generation }}/data"
`,
		},
		{
			name: "R31 command 缺守卫",
			rule: "R31",
			manifest: strings.Replace(minimal,
				`      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }`,
				`      - command: { run: "/bin/true" }`, 1),
		},
		{
			name: "R34 同一 Pack 被依赖两次",
			rule: "R34",
			manifest: minimal + `
requires:
  packs:
    - { name: jdk11, version: ">=11" }
    - { name: jdk11, version: ">=17" }
`,
		},
		{
			name: "R35 scope 非法",
			rule: "R35",
			manifest: minimal + `
requires:
  packs:
    - { name: jdk11, version: ">=11", scope: cluster }
`,
		},
		{
			name: "R38 引用未声明的依赖",
			rule: "R38",
			manifest: strings.Replace(minimal,
				`      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }`,
				`      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }
      - template: { src: a.tmpl, dest: /tmp/a }`, 1),
			extra: map[string]string{"templates/a.tmpl": "{{ .Requires.ghost.Paths.Current }}\n"},
		},
		{
			name: "R41 from 中引用 facts",
			rule: "R41",
			manifest: strings.Replace(minimal, "{ type: port, default: 8080 }",
				`{ type: size, from: "{{ .Node.Facts.Memory.Total }}" }`, 1),
		},
		{
			name: "R42 export 引用不存在的角色",
			rule: "R42",
			manifest: minimal + `
exports:
  client: { role: ghost, port: "9092" }
`,
		},
		{
			name: "R26d generate 用在非 secret 上",
			rule: "R26d",
			manifest: strings.Replace(minimal, "{ type: port, default: 8080 }",
				`{ type: string, generate: { length: 32 } }`, 1),
		},
		{
			name: "R26d generate 与 default 并存",
			rule: "R26d",
			manifest: strings.Replace(minimal, "{ type: port, default: 8080 }",
				`{ type: secret, generate: { length: 32 }, default: "hunter2" }`, 1),
		},
		{
			name: "R26e 生成长度过短",
			rule: "R26e",
			manifest: strings.Replace(minimal, "{ type: port, default: 8080 }",
				`{ type: secret, generate: { length: 4 } }`, 1),
		},
		{
			name: "R26e 未知字符集",
			rule: "R26e",
			manifest: strings.Replace(minimal, "{ type: port, default: 8080 }",
				`{ type: secret, generate: { charset: base64 } }`, 1),
		},
		{
			name: "R47 export 字段引用未声明的依赖",
			rule: "R38",
			manifest: minimal + `
exports:
  app:
    role: default
    port: "8080"
    fields:
      user: "{{ .Requires.ghost.Exports.x.user }}"
`,
		},
		{
			name: "R48 fields 为空",
			rule: "R48",
			manifest: minimal + `
exports:
  app:
    role: default
    port: "8080"
    fields: {}
`,
		},
		{
			name: "R49 伸手取依赖的参数",
			rule: "R49",
			manifest: strings.Replace(minimal, "{ type: port, default: 8080 }",
				`{ type: secret, from: "{{ .Requires.pg.Params.admin_password }}" }`, 1) + `
requires:
  packs:
    - { name: pg, version: ">=14", scope: site }
`,
		},
		{
			name: "R46 含口令的模板对其他人可读",
			rule: "R46",
			manifest: strings.Replace(
				strings.Replace(minimal, "  port: { type: port, default: 8080 }",
					"  port: { type: port, default: 8080 }\n  pw: { type: secret, required: true }", 1),
				`      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }`,
				`      - template: { src: a.tmpl, dest: /etc/app/env, mode: "0644" }`, 1),
			extra: map[string]string{"templates/a.tmpl": "PW={{ .Params.pw }}\n"},
		},
		{
			name: "R46 含口令的模板未声明 mode",
			rule: "R46",
			manifest: strings.Replace(
				strings.Replace(minimal, "  port: { type: port, default: 8080 }",
					"  port: { type: port, default: 8080 }\n  pw: { type: secret, required: true }", 1),
				`      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }`,
				`      - template: { src: a.tmpl, dest: /etc/app/env }`, 1),
			extra: map[string]string{"templates/a.tmpl": "PW={{ .Params.pw }}\n"},
		},
		{
			// 口令藏在 include 进来的片段里——不跟进片段就整个漏掉
			name: "R46 口令在片段中",
			rule: "R46",
			manifest: strings.Replace(
				strings.Replace(minimal, "  port: { type: port, default: 8080 }",
					"  port: { type: port, default: 8080 }\n  pw: { type: secret, required: true }", 1),
				`      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }`,
				`      - template: { src: a.tmpl, dest: /etc/app/env, mode: "0644" }`, 1),
			extra: map[string]string{
				"templates/a.tmpl":      `{{ template "_creds" . }}` + "\n",
				"templates/_creds.tmpl": `{{ define "_creds" }}PW={{ .Params.pw }}{{ end }}` + "\n",
			},
		},
		{
			// 内联 Environment= 会把口令写进 0644 的 unit 文件
			name: "R50 口令进了 systemd.env",
			rule: "R50",
			manifest: strings.Replace(
				strings.Replace(minimal, "  port: { type: port, default: 8080 }",
					"  port: { type: port, default: 8080 }\n  pw: { type: secret, required: true }", 1),
				`        exec: "{{ .Paths.Current }}/bin/demo"`,
				`        exec: "{{ .Paths.Current }}/bin/demo"`+"\n"+
					`        env: { DB_PASSWORD: "{{ .Params.pw }}" }`, 1),
		},
		{
			name: "R45 systemd_unit 名后缀非法",
			rule: "R45",
			manifest: strings.Replace(minimal,
				`      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }`,
				`      - systemd_unit: { name: bad-unit, content: "[Service]\nExecStart=/bin/true\n" }`, 1),
		},
		{
			name: "R07 模板文件不存在",
			rule: "R07",
			manifest: strings.Replace(minimal,
				`      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }`,
				`      - template: { src: missing.tmpl, dest: /tmp/a }`, 1),
		},
		{
			name: "R15 引用未定义的模板片段",
			rule: "R15",
			manifest: strings.Replace(minimal,
				`      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }`,
				`      - template: { src: a.tmpl, dest: /tmp/a }`, 1),
			extra: map[string]string{"templates/a.tmpl": `{{ template "nope" . }}` + "\n"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := lintSrc(t, tc.manifest, tc.extra, Options{})
			if !hasRule(res, tc.rule) {
				t.Errorf("期望触发 %s，实际结果:\n%s", tc.rule, dump(res))
			}
		})
	}
}

func TestHermeticCatchesExternalCalls(t *testing.T) {
	cases := []struct {
		name   string
		script string
		want   bool
	}{
		{"curl 下载", "#!/bin/sh\ncurl -o /tmp/x https://example.com/x\n", true},
		{"apt-get 安装", "#!/bin/sh\napt-get install -y libfoo\n", true},
		{"管道后的 wget", "#!/bin/sh\ntrue | wget http://x\n", true},
		{"&& 后的 pip", "#!/bin/sh\ncd /tmp && pip install foo\n", true},
		{"git clone", "#!/bin/sh\ngit clone https://x/y\n", true},
		{"go build", "#!/bin/sh\ngo build ./...\n", true},
		{"docker pull", "#!/bin/sh\ndocker pull nginx\n", true},
		{"绝对路径的 curl", "#!/bin/sh\n/usr/bin/curl http://x\n", true},
		{"注释中的 curl 不算", "#!/bin/sh\n# curl http://x\ntrue\n", false},
		{"git status 不算", "#!/bin/sh\ngit status\n", false},
		{"docker inspect 不算", "#!/bin/sh\ndocker inspect x\n", false},
		{"本地命令不算", "#!/bin/sh\nsystemctl restart x\nsetenforce 0\n", false},
	}

	manifest := strings.Replace(minimal,
		"roles:",
		"hooks:\n  postInstall: hooks/h.sh\nroles:", 1)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := lintSrc(t, manifest, map[string]string{"hooks/h.sh": tc.script},
				Options{Hermetic: true})
			got := hasRule(res, "R33")
			if got != tc.want {
				t.Errorf("hermetic 检出 = %v, 期望 %v\n%s", got, tc.want, dump(res))
			}
		})
	}
}

func TestHermeticOffByDefault(t *testing.T) {
	manifest := strings.Replace(minimal, "roles:",
		"hooks:\n  postInstall: hooks/h.sh\nroles:", 1)
	res := lintSrc(t, manifest, map[string]string{"hooks/h.sh": "#!/bin/sh\ncurl http://x\n"},
		Options{Hermetic: false})
	if hasRule(res, "R33") {
		t.Error("未开启 --hermetic 时不应报 R33")
	}
}

// TestProfileGuardAvoidsFalsePositive 覆盖 R21 最容易误报的两种守卫形式。
func TestProfileGuardAvoidsFalsePositive(t *testing.T) {
	base := strings.Replace(minimal,
		`      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }`,
		`      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }
      - template: { src: inline-guard.tmpl, dest: /tmp/a }
      - template: { src: when-guard.tmpl, dest: /tmp/b, when: '{{ eq .Profile "ha" }}' }`, 1) + `
profiles:
  - name: simple
    default: true
  - name: ha
    params:
      ha_only: { type: string, default: x }
`
	extra := map[string]string{
		// 模板内部守卫
		"templates/inline-guard.tmpl": `{{ if eq .Profile "ha" }}{{ .Params.ha_only }}{{ end }}` + "\n",
		// 守卫在渲染它的资源的 when 上
		"templates/when-guard.tmpl": `{{ .Params.ha_only }}` + "\n",
	}

	res := lintSrc(t, base, extra, Options{})
	for _, f := range res.Findings {
		if f.Rule == "R21" && f.Severity == SevError {
			t.Errorf("守卫内的参数引用不应报错:\n%s", dump(res))
			break
		}
	}
}

// TestProfileGuardStillCatchesUnguarded 确认加了守卫识别之后，
// 真正无守卫的引用仍然会被抓到。
func TestProfileGuardStillCatchesUnguarded(t *testing.T) {
	base := strings.Replace(minimal,
		`      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }`,
		`      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }
      - template: { src: bare.tmpl, dest: /tmp/a }`, 1) + `
profiles:
  - name: simple
    default: true
  - name: ha
    params:
      ha_only: { type: string, default: x }
`
	extra := map[string]string{"templates/bare.tmpl": `{{ .Params.ha_only }}` + "\n"}

	res := lintSrc(t, base, extra, Options{})
	if !hasRule(res, "R21") {
		t.Errorf("无守卫地引用 profile 专有参数应当报错:\n%s", dump(res))
	}
}

// TestExprReferencedParams 钉住「本 Pack 的参数」与「依赖的参数」区分得开。
//
// 两种写法只差一个 `requires.<dep>.` 前缀，而 `.Params.x` 的前置字符
// 恰好也是点号——靠「前面不是点」区分不开。这里曾经就是这么错的：
// 导出字段里的 password 没被识别成敏感，`inspect` 不打标记，消费方作者
// 也就不知道自己该声明 secret。
func TestExprReferencedParams(t *testing.T) {
	cases := []struct {
		expr string
		want []string
	}{
		{`{{ .Params.app_password }}`, []string{"app_password"}},
		{`params.app_password`, []string{"app_password"}},
		{`{{ .Params.a }}:{{ .Params.b }}`, []string{"a", "b"}},
		// 依赖的参数不算自己的
		{`{{ .Requires.pg.Params.admin_password }}`, nil},
		{`requires.pg.params.admin_password`, nil},
		// 混在一起时各归各的
		{`{{ .Params.port }}/{{ .Requires.pg.Params.x }}`, []string{"port"}},
		{`{{ .Paths.Config }}`, nil},
		{``, nil},
	}
	for _, tc := range cases {
		got := ExprReferencedParams(tc.expr)
		if len(got) != len(tc.want) {
			t.Errorf("%q → %v，期望 %v", tc.expr, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%q → %v，期望 %v", tc.expr, got, tc.want)
				break
			}
		}
	}
}

// TestExportSecretInference 钉住导出字段的敏感标记由引用推导得出。
//
// **推导而非声明**：Pack 无法把它写错，也就不会与实际不一致。
func TestExportSecretInference(t *testing.T) {
	src := minimal + `
params2: {}
exports:
  app:
    role: default
    port: "8080"
    fields:
      username: "{{ .Params.user }}"
      password: "{{ .Params.pw }}"
`
	src = strings.Replace(src,
		"  port: { type: port, default: 8080 }",
		"  port: { type: port, default: 8080 }\n"+
			"  user: { type: string, default: app }\n"+
			"  pw:   { type: secret, generate: { length: 32 } }", 1)
	src = strings.Replace(src, "params2: {}\n", "", 1)

	p, err := Load(writePack(t, src, nil))
	if err != nil {
		t.Fatal(err)
	}
	e := p.Exports["app"]
	if p.ExprCarriesSecret(e.Fields["username"]) {
		t.Error("username 引用的是普通参数，不该被判为敏感")
	}
	if !p.ExprCarriesSecret(e.Fields["pw"]) && !p.ExprCarriesSecret(e.Fields["password"]) {
		t.Error("password 引用了 secret 参数，必须被判为敏感")
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"1024":   1024,
		"1B":     1,
		"4GB":    4 * 1000 * 1000 * 1000,
		"512MB":  512 * 1000 * 1000,
		"1Gi":    1 << 30,
		"31GB":   31 * 1000 * 1000 * 1000,
		"1.5GB":  1_500_000_000,
		" 2 MB ": 2 * 1000 * 1000,
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil {
			t.Errorf("ParseSize(%q) 报错: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSize(%q) = %d, 期望 %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "abc", "4XB", "GB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) 应当报错", bad)
		}
	}
}

func TestParamTypeValidity(t *testing.T) {
	valid := []ParamType{"string", "int", "port", "size", "duration", "cidr",
		"secret", "list<string>", "list<port>"}
	for _, tp := range valid {
		if !tp.Valid() {
			t.Errorf("%q 应当是合法类型", tp)
		}
	}
	invalid := []ParamType{"", "ipv6", "list<list<int>>", "map", "object"}
	for _, tp := range invalid {
		if tp.Valid() {
			t.Errorf("%q 应当是非法类型", tp)
		}
	}
}

// TestExamplesPassLint 是 M1 的验收判据：仓库中的全部示例必须通过
// 含 hermetic 的 lint。
func TestExamplesPassLint(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "packs")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Skipf("示例目录不可用: %v", err)
	}
	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, PackFile)); err != nil {
			continue
		}
		checked++
		t.Run(e.Name(), func(t *testing.T) {
			p, err := Load(dir)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			res := Lint(p, Options{Hermetic: true})
			if !res.OK() {
				t.Errorf("示例未通过 lint:\n%s", dump(res))
			}
			for _, f := range res.Warnings() {
				t.Logf("警告: %s", f.String())
			}
		})
	}
	if checked == 0 {
		t.Fatal("没有找到任何示例 Pack")
	}
}

// ── R52 容器挂载不得引用 .Paths.Current ─────────────────────────────────

// dockerPack 是一个用 docker runtime 的最小 Pack，mounts 由用例给。
func dockerPack(mounts string) string {
	return `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
blobs:
  main:
    linux/amd64:
      sha256: "0000000000000000000000000000000000000000000000000000000000000000"
      size: 1024
      filename: demo.tar
      mediaType: docker-archive
roles:
  - workload:
      runtime: docker
      docker:
        imageBlob: main
        mounts:
` + mounts
}

// TestR52RejectsCurrentPathInDockerMount 是 M4 第 8 步的验收。
//
// Docker 在**创建容器时**解析 bind mount 的路径。`current` 是软链，
// 解析之后容器绑死在当时那个 generation 上；之后切换 generation，
// 容器里看到的仍是旧的，而 `ls -l` 看软链一切正常。
//
// 这类「文件明明改了、进程就是读不到」的现场极难查——因此宁可在 lint
// 阶段拒绝。
func TestR52RejectsCurrentPathInDockerMount(t *testing.T) {
	res := lintSrc(t, dockerPack(
		`          - { from: "{{ .Paths.Current }}/conf", to: /etc/app }`+"\n"),
		nil, Options{})
	if !hasRule(res, "R52") {
		t.Errorf("挂载源引用 .Paths.Current 时应当报 R52，实际:\n%s", dump(res))
	}
}

// TestR52AcceptsStablePaths 钉住规则**不误伤**。
//
// 一条只会误报的规则比没有规则更糟：它会把人训练成无视 lint 输出。
// `.Paths.Generation` 尤其要放行——它是**显式**指向本次那个 generation，
// 正是这条规则推荐的替代写法。
func TestR52AcceptsStablePaths(t *testing.T) {
	for _, from := range []string{
		"{{ .Paths.Config }}",
		"{{ .Paths.Data }}",
		"{{ .Paths.Generation }}/conf",
		"/var/lib/fixed",
	} {
		t.Run(from, func(t *testing.T) {
			res := lintSrc(t, dockerPack(
				`          - { from: "`+from+`", to: /etc/app }`+"\n"),
				nil, Options{})
			if hasRule(res, "R52") {
				t.Errorf("%s 是正确写法，不该报 R52，实际:\n%s", from, dump(res))
			}
		})
	}
}

// TestR52LeavesSystemdAlone 钉住这条规则**只管容器**。
//
// 同一句 `{{ .Paths.Current }}/bin/demo` 在 systemd 的 exec 里是**正确**
// 写法——那是进程启动时才解析的路径，切 generation 后重启就指向新的。
// 一刀切会让每一个正常的 systemd Pack 都报错。
func TestR52LeavesSystemdAlone(t *testing.T) {
	res := lintSrc(t, minimal, nil, Options{})
	if hasRule(res, "R52") {
		t.Errorf("systemd 的 exec 用 .Paths.Current 是对的，不该报 R52，实际:\n%s", dump(res))
	}
}

// TestR52WarnsOnComposeTemplate 钉住 compose 侧降级为告警。
//
// compose 的挂载写在模板里，lint 认不出是哪一行。**指不出位置的错误会让
// 人无从下手**，因此说「这个文件里有一处，去看看」而不是拒绝构建。
func TestR52WarnsOnComposeTemplate(t *testing.T) {
	manifest := `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
blobs:
  main:
    linux/amd64:
      sha256: "0000000000000000000000000000000000000000000000000000000000000000"
      size: 1024
      filename: demo.tar
      mediaType: docker-archive
roles:
  - workload:
      runtime: compose
      compose:
        file: compose.yaml.tmpl
        imageBlobs: [main]
`
	res := lintSrc(t, manifest, map[string]string{
		"templates/compose.yaml.tmpl": "services:\n  web:\n    volumes:\n" +
			"      - {{ .Paths.Current }}/conf:/etc/app\n",
	}, Options{})

	var warned bool
	for _, f := range res.Findings {
		if f.Rule == "R52" && f.Severity == SevWarn {
			warned = true
		}
	}
	if !warned {
		t.Errorf("compose 模板引用 .Paths.Current 时应当告警，实际:\n%s", dump(res))
	}
	if hasRule(res, "R52") {
		t.Error("compose 侧应当是告警而非错误——lint 指不出具体是哪一行挂载")
	}
}
