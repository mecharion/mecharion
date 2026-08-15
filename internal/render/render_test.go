package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/spec"
)

// ── 夹具 ────────────────────────────────────────────────────────────────

// writePack 把一份 pack.yaml 与若干模板写进临时目录并解析。
func writePack(t *testing.T, yaml string, tmpls map[string]string) *pack.Pack {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(tmpls) > 0 {
		td := filepath.Join(dir, "templates")
		if err := os.MkdirAll(td, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range tmpls {
			if err := os.WriteFile(filepath.Join(td, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	p, err := pack.Load(dir)
	if err != nil {
		t.Fatalf("解析 Pack: %v", err)
	}
	return p
}

func node(name, addr string) Node {
	return Node{
		Name: name, Address: addr,
		Labels: map[string]string{},
		Roots:  map[string]string{},
		Facts:  map[string]any{},
	}
}

func inst(role, nodeName, addr string, ordinal int) Instance {
	return Instance{Role: role, Ordinal: ordinal, ConfigGroup: "default", Node: node(nodeName, addr)}
}

// minimalPack 是一个够用的最小 Pack：一个角色、一个模板资源。
const minimalPack = `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
params:
  port:
    type: port
    default: 8080
roles:
  - name: server
    cardinality: "1-N"
    resources:
      - template:
          src: app.conf.tmpl
          dest: "{{ .Paths.Config }}/app.conf"
          mode: "0644"
          notify: reload
    workload:
      runtime: systemd
      systemd:
        exec: "{{ .Paths.Current }}/bin/app --port {{ .Params.port }}"
    health:
      http: { path: /healthz, port: "{{ .Params.port }}" }
`

func mustRender(t *testing.T, req Request) *Result {
	t.Helper()
	res, err := Render(req)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return res
}

func argsOf(t *testing.T, s *spec.ResolvedSpec, id string) map[string]any {
	t.Helper()
	for _, r := range s.Resources {
		if r.ID == id {
			var m map[string]any
			if err := json.Unmarshal(r.Args, &m); err != nil {
				t.Fatal(err)
			}
			return m
		}
	}
	t.Fatalf("规格里没有资源 %q，实际有: %v", id, resourceIDs(s))
	return nil
}

func resourceIDs(s *spec.ResolvedSpec) []string {
	out := make([]string, 0, len(s.Resources))
	for _, r := range s.Resources {
		out = append(out, r.ID)
	}
	return out
}

// ── 基本链路 ────────────────────────────────────────────────────────────

func TestRenderProducesUsableSpec(t *testing.T) {
	p := writePack(t, minimalPack, map[string]string{
		"app.conf.tmpl": "port = {{ .Params.port }}\nhome = {{ .Paths.Home }}\n",
	})
	res := mustRender(t, Request{
		Component: "demo", Pack: p,
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
	})

	s := res.Spec("server", "n1")
	if s == nil {
		t.Fatalf("没有产出 server@n1 的规格，实际有: %v", res.Order)
	}
	// 产出必须能过 mechlet 侧的校验——那是这条管线唯一的下游
	if err := spec.Validate(s); err != nil {
		t.Fatalf("产出的规格通不过 spec.Validate: %v", err)
	}
	if err := spec.VerifyDigest(s); err != nil {
		t.Fatalf("digest 不自洽: %v", err)
	}

	args := argsOf(t, s, "template:/etc/mecharion/apps/demo/app.conf")
	content, _ := args["content"].(string)
	if !strings.Contains(content, "port = 8080") {
		t.Errorf("模板未按参数渲染，content=%q", content)
	}
	if !strings.Contains(content, "home = /opt/mecharion/apps/demo") {
		t.Errorf("模板未按路径渲染，content=%q", content)
	}
	if _, leaked := args["src"]; leaked {
		t.Error("template 资源的 src 不应出现在已解析规格里——mechlet 会报错")
	}
	if s.Workload.Systemd.Exec != "/opt/mecharion/apps/demo/current/bin/app --port 8080" {
		t.Errorf("workload.exec 渲染错误: %q", s.Workload.Systemd.Exec)
	}
	if s.Health.HTTP.Port != 8080 {
		t.Errorf("health.http.port 应为 8080，实际 %d", s.Health.HTTP.Port)
	}
}

// TestGenerationStaysPlaceholder 钉住唯一的 late-bound 值。
//
// generation 目录名含 mechlet 本地分配的序号，mechd 无从得知。
// 提前解析成任何具体值都是错的。
func TestGenerationStaysPlaceholder(t *testing.T) {
	p := writePack(t, `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
paths:
  config:
    default: "{{ .Node.Roots.etc }}/apps/{{ .Component }}"
    linkInto: "{{ .Paths.Generation }}/conf"
roles:
  - name: server
    cardinality: "1-N"
`, nil)
	res := mustRender(t, Request{
		Component: "demo", Pack: p,
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
	})
	s := res.Spec("server", "n1")
	if got := s.Paths["config"].LinkInto; got != spec.GenerationPlaceholder+"/conf" {
		t.Errorf("linkInto 应保留占位符，实际 %q", got)
	}
	if !spec.HasUnresolvedPlaceholder(s) {
		t.Error("规格里应当仍有 generation 占位符")
	}
}

// ── 参数链 ──────────────────────────────────────────────────────────────

func TestParamChainLayering(t *testing.T) {
	p := writePack(t, `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
params:
  a: { type: string, default: "pack" }
  b: { type: string, default: "pack" }
  c: { type: string, default: "pack" }
roles:
  - name: server
    cardinality: "1-N"
`, nil)

	res := mustRender(t, Request{
		Component: "demo", Pack: p,
		Instances: []Instance{{
			Role: "server", ConfigGroup: "grp", Node: node("n1", "10.0.0.1"),
		}},
		Overrides: Overrides{
			Component: map[string]any{"a": "component", "b": "component", "c": "component"},
			Role:      map[string]map[string]any{"server": {"b": "role", "c": "role"}},
			Group:     map[string]map[string]any{"grp": {"c": "group"}},
		},
	})
	s := res.Spec("server", "n1")
	for name, want := range map[string]string{"a": "component", "b": "role", "c": "group"} {
		if got := s.Params[name].Value; got != want {
			t.Errorf("参数 %s 应为 %q（后者覆盖前者），实际 %v", name, want, got)
		}
	}
}

func TestUnknownOverrideRejected(t *testing.T) {
	p := writePack(t, minimalPack, map[string]string{"app.conf.tmpl": "x"})
	_, err := Render(Request{
		Component: "demo", Pack: p,
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
		Overrides: Overrides{Component: map[string]any{"prot": 9090}},
	})
	if err == nil {
		t.Fatal("覆盖一个不存在的参数应当被拒绝")
	}
	// 打错字是最常见的情形，错误信息必须帮用户找到正确的名字
	for _, want := range []string{"prot", "does not declare", "port"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应含 %q，实际:\n%v", want, err)
		}
	}
}

func TestInvalidOverrideRejectedAtItsOwnLayer(t *testing.T) {
	p := writePack(t, minimalPack, map[string]string{"app.conf.tmpl": "x"})
	_, err := Render(Request{
		Component: "demo", Pack: p,
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
		Overrides: Overrides{Role: map[string]map[string]any{"server": {"port": 70000}}},
	})
	if err == nil {
		t.Fatal("非法端口应当在给出它的那一层就被拒绝")
	}
	if !strings.Contains(err.Error(), "role server") {
		t.Errorf("错误信息应指出是哪一层给的值，实际:\n%v", err)
	}
}

// TestFromRejectsUserOverride 钉住 from 与 defaultFrom 的分界。
func TestFromRejectsUserOverride(t *testing.T) {
	p := writePack(t, `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
params:
  primary_host:
    type: string
    from: "{{ (index (.Topology.Role \"server\") 0).Address }}"
roles:
  - name: server
    cardinality: "1-N"
`, nil)
	_, err := Render(Request{
		Component: "demo", Pack: p,
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
		Overrides: Overrides{Component: map[string]any{"primary_host": "1.2.3.4"}},
	})
	if err == nil {
		t.Fatal("from 参数不接受用户设值")
	}
	if !strings.Contains(err.Error(), "objective fact") {
		t.Errorf("错误信息应解释 from 的语义，实际:\n%v", err)
	}
}

// ── defaultFrom ─────────────────────────────────────────────────────────

// TestDefaultFromIsPerInstance 钉住「同一 Component 的不同实例可以得到不同的值」。
func TestDefaultFromIsPerInstance(t *testing.T) {
	p := writePack(t, heapPack, nil)

	big := inst("server", "big", "10.0.0.1", 0)
	big.Node.Facts = map[string]any{"memory": map[string]any{"total": "64Gi"}}
	small := inst("server", "small", "10.0.0.2", 1)
	small.Node.Facts = map[string]any{"memory": map[string]any{"total": "8Gi"}}

	res := mustRender(t, Request{
		Component: "demo", Pack: p,
		Instances: []Instance{big, small},
	})
	if got := res.Spec("server", "big").Params["heap"].Value; got != "31GB" {
		t.Errorf("大内存节点应被 31GB 上限截断，实际 %v", got)
	}
	if got := res.Spec("server", "small").Params["heap"].Value; got != "4Gi" {
		t.Errorf("小内存节点应取内存一半，实际 %v", got)
	}
}

const heapPack = `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
params:
  heap:
    type: size
    defaultFrom: '{{ min (div .Node.Facts.Memory.Total 2) "31GB" }}'
    default: 2GB
roles:
  - name: server
    cardinality: "1-N"
`

// TestDefaultFromFailureFallsBack 钉住「不中止部署」。
//
// 一个采集不到内存的节点不该阻断整个 Rollout。
func TestDefaultFromFailureFallsBack(t *testing.T) {
	p := writePack(t, heapPack, nil)
	bad := inst("server", "n1", "10.0.0.1", 0) // Facts 为空

	res := mustRender(t, Request{
		Component: "demo", Pack: p,
		Instances: []Instance{bad},
	})
	if got := res.Spec("server", "n1").Params["heap"].Value; got != "2GB" {
		t.Errorf("求值失败应回落到 default，实际 %v", got)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("回落必须留下告警——静默回落会让人以为算出来的就是这个值")
	}
	if !strings.Contains(res.Warnings[0], "heap") {
		t.Errorf("告警应指名到参数，实际: %s", res.Warnings[0])
	}
}

// TestDefaultFromYieldsToUser 钉住「用户覆盖后不再求值」。
func TestDefaultFromYieldsToUser(t *testing.T) {
	p := writePack(t, heapPack, nil)
	n := inst("server", "n1", "10.0.0.1", 0)
	n.Node.Facts = map[string]any{"memory": map[string]any{"total": "64Gi"}}

	res := mustRender(t, Request{
		Component: "demo", Pack: p,
		Instances: []Instance{n},
		Overrides: Overrides{Component: map[string]any{"heap": "12GB"}},
	})
	if got := res.Spec("server", "n1").Params["heap"].Value; got != "12GB" {
		t.Errorf("用户给了值就不该再求值 defaultFrom，实际 %v", got)
	}
}

// TestSizeShapeIsStableAcrossBranches 钉住一个容易漏的一致性问题。
//
// 若 defaultFrom 算出的是裸字节数而 default 是 "2GB"，同一个参数在模板里
// 就有两种形状，引用它的每个 Pack 都得自己处理这个差异。
func TestSizeShapeIsStableAcrossBranches(t *testing.T) {
	p := writePack(t, heapPack, nil)
	n := inst("server", "n1", "10.0.0.1", 0)
	n.Node.Facts = map[string]any{"memory": map[string]any{"total": "16Gi"}}

	res := mustRender(t, Request{
		Component: "demo", Pack: p, Instances: []Instance{n},
	})
	got, _ := res.Spec("server", "n1").Params["heap"].Value.(string)
	if _, err := pack.ParseSize(got); err != nil {
		t.Fatalf("defaultFrom 的结果 %q 不是合法 size：与走 default 时的形状不一致", got)
	}
	if got != "8Gi" {
		t.Errorf("16Gi 的一半应归一为 8Gi，实际 %q", got)
	}
}

// ── 路径 ────────────────────────────────────────────────────────────────

func TestPathsResolveInDependencyOrder(t *testing.T) {
	// data 依赖 home，home 依赖 Node.Roots —— 声明顺序与依赖顺序相反，
	// 逐轮推进到不动点才能解开
	p := writePack(t, `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
paths:
  data:
    default: "{{ .Paths.Home }}/data"
  home:
    default: "{{ .Node.Roots.opt }}/apps/{{ .Component }}"
roles:
  - name: server
    cardinality: "1-N"
`, nil)
	res := mustRender(t, Request{
		Component: "demo", Pack: p,
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
	})
	if got := res.Spec("server", "n1").Paths["data"].First(); got != "/opt/mecharion/apps/demo/data" {
		t.Errorf("data 应基于 home 解析，实际 %q", got)
	}
}

func TestPathCycleReportsNames(t *testing.T) {
	p := writePack(t, `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
paths:
  a: { default: "{{ .Paths.B }}/a" }
  b: { default: "{{ .Paths.A }}/b" }
roles:
  - name: server
    cardinality: "1-N"
`, nil)
	_, err := Render(Request{
		Component: "demo", Pack: p,
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
	})
	if err == nil {
		t.Fatal("互相引用的路径应当被拒绝而不是死循环")
	}
	for _, want := range []string{"a", "b", "reference each other"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应含 %q，实际:\n%v", want, err)
		}
	}
}

func TestNodeRootsOverrideDefaults(t *testing.T) {
	p := writePack(t, minimalPack, map[string]string{"app.conf.tmpl": "x"})
	n := inst("server", "n1", "10.0.0.1", 0)
	n.Node.Roots = map[string]string{"etc": "/srv/conf"}

	res := mustRender(t, Request{
		Component: "demo", Pack: p, Instances: []Instance{n},
	})
	s := res.Spec("server", "n1")
	if got := s.Paths["config"].First(); got != "/srv/conf/apps/demo" {
		t.Errorf("config 应跟随节点的 etc 根，实际 %q", got)
	}
	// 未覆盖的根仍走默认值
	if got := s.Paths["home"].First(); got != "/opt/mecharion/apps/demo" {
		t.Errorf("未覆盖的 opt 根应保持默认，实际 %q", got)
	}
}

// TestMultiDiskBinding 钉住 ConfigGroup 按卷名绑定多盘。
func TestMultiDiskBinding(t *testing.T) {
	p := writePack(t, `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
paths:
  dataDirs:
    kind: multi
    default: ["{{ .Node.Roots.data }}/apps/{{ .Component }}/dfs"]
    subpath: "dfs/dn"
roles:
  - name: server
    cardinality: "1-N"
`, nil)
	n := inst("server", "n1", "10.0.0.1", 0)
	n.Node.Volumes = map[string]Volume{
		"data1": {Path: "/data1"}, "data2": {Path: "/data2"},
	}
	n.PathBindings = map[string][]string{"dataDirs": {"data1", "data2"}}

	res := mustRender(t, Request{
		Component: "demo", Pack: p, Instances: []Instance{n},
	})
	got := res.Spec("server", "n1").Paths["dataDirs"].Values
	want := []string{"/data1/dfs/dn", "/data2/dfs/dn"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("多盘绑定结果 %v，期望 %v", got, want)
	}
}

func TestUnknownVolumeRejected(t *testing.T) {
	p := writePack(t, `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
paths:
  dataDirs: { kind: multi, default: ["/x"], subpath: "d" }
roles:
  - name: server
    cardinality: "1-N"
`, nil)
	n := inst("server", "n1", "10.0.0.1", 0)
	n.Node.Volumes = map[string]Volume{"data1": {Path: "/data1"}}
	n.PathBindings = map[string][]string{"dataDirs": {"data9"}}

	_, err := Render(Request{Component: "demo", Pack: p, Instances: []Instance{n}})
	if err == nil {
		t.Fatal("绑定到不存在的卷应当被拒绝")
	}
	// 报出该节点有哪些卷，用户才知道该改成什么
	if !strings.Contains(err.Error(), "data1") {
		t.Errorf("错误信息应列出该节点已声明的卷，实际:\n%v", err)
	}
}

// ── 拓扑 ────────────────────────────────────────────────────────────────

// TestTopologyCarriesPeerOwnPaths 钉住 spec §9.3 的关键性质。
//
// 节点间挂载点可以不同，因此**不能用本机的 .Paths 去推断对端**。
func TestTopologyCarriesPeerOwnPaths(t *testing.T) {
	p := writePack(t, `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
roles:
  - name: server
    cardinality: "1-N"
    resources:
      - template: { src: peers.tmpl, dest: "{{ .Paths.Config }}/peers" }
`, map[string]string{
		"peers.tmpl": `{{ range .Topology.Role "server" }}{{ .Address }}={{ .Paths.Data }}
{{ end }}`,
	})

	a := inst("server", "n1", "10.0.0.1", 0)
	a.Node.Roots = map[string]string{"data": "/data-a"}
	b := inst("server", "n2", "10.0.0.2", 1)
	b.Node.Roots = map[string]string{"data": "/data-b"}

	res := mustRender(t, Request{
		Component: "demo", Pack: p, Instances: []Instance{a, b},
	})
	content, _ := argsOf(t, res.Spec("server", "n1"),
		"template:/etc/mecharion/apps/demo/peers")["content"].(string)

	for _, want := range []string{"10.0.0.1=/data-a/apps/demo", "10.0.0.2=/data-b/apps/demo"} {
		if !strings.Contains(content, want) {
			t.Errorf("拓扑条目应带对端自己的路径，缺 %q。实际:\n%s", want, content)
		}
	}
}

// TestTopologyOrderedByOrdinal 钉住枚举顺序稳定。
//
// 不稳定会让 quorum 串每次渲染都不同，从而每次都产生新 generation。
func TestTopologyOrderedByOrdinal(t *testing.T) {
	p := writePack(t, `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
params:
  quorum:
    type: string
    from: '{{ range $i, $p := .Topology.Role "server" }}{{ if $i }},{{ end }}{{ $p.Address }}:2181{{ end }}'
roles:
  - name: server
    cardinality: "1-N"
`, nil)

	// 故意把节点名的字典序与 ordinal 反着给
	res := mustRender(t, Request{
		Component: "demo", Pack: p,
		Instances: []Instance{
			inst("server", "zzz", "10.0.0.1", 0),
			inst("server", "aaa", "10.0.0.2", 1),
		},
	})
	want := "10.0.0.1:2181,10.0.0.2:2181"
	if got := res.Spec("server", "zzz").Params["quorum"].Value; got != want {
		t.Errorf("拓扑应按 ordinal 排序（不是节点名），期望 %q，实际 %v", want, got)
	}
}

// TestOrdinalVisibleToTemplates 钉住 ZooKeeper 的 myid 这类用法。
func TestOrdinalVisibleToTemplates(t *testing.T) {
	p := writePack(t, `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
roles:
  - name: server
    cardinality: "1-N"
    resources:
      - template: { src: myid.tmpl, dest: "{{ .Paths.Data }}/myid" }
`, map[string]string{"myid.tmpl": "{{ .Topology.Ordinal }}"})

	res := mustRender(t, Request{
		Component: "demo", Pack: p,
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 7)},
	})
	got := argsOf(t, res.Spec("server", "n1"), "template:/var/lib/mecharion/apps/demo/myid")
	if got["content"] != "7" {
		t.Errorf("myid 应为固化的 ordinal 7，实际 %v", got["content"])
	}
}

// ── when / profile ──────────────────────────────────────────────────────

// TestWhenFalseResourceIsAbsent 钉住「求值为 false 的资源根本不在列表里」。
func TestWhenFalseResourceIsAbsent(t *testing.T) {
	p := writePack(t, `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
profiles:
  - { name: simple, default: true }
  - { name: ha }
roles:
  - name: server
    cardinality: "1-N"
    resources:
      - directory:
          id: ha-only
          path: "{{ .Paths.Data }}/ha"
          when: '{{ eq .Profile "ha" }}'
      - directory:
          id: always
          path: "{{ .Paths.Data }}/x"
`, nil)

	for _, tc := range []struct {
		profile string
		want    []string
	}{
		{"simple", []string{"always"}},
		{"ha", []string{"ha-only", "always"}},
	} {
		res := mustRender(t, Request{
			Component: "demo", Pack: p, Profile: tc.profile,
			Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
		})
		got := resourceIDs(res.Spec("server", "n1"))
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("profile=%s 的资源应为 %v，实际 %v", tc.profile, tc.want, got)
		}
	}
}

func TestProfileOverridesParamDefault(t *testing.T) {
	p := writePack(t, `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
params:
  replicas: { type: int, default: 1, min: 1, max: 9 }
profiles:
  - { name: simple, default: true }
  - name: ha
    params:
      replicas: { default: 3 }
roles:
  - name: server
    cardinality: "1-N"
`, nil)

	res := mustRender(t, Request{
		Component: "demo", Pack: p, Profile: "ha",
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
	})
	if got := res.Spec("server", "n1").Params["replicas"].Value; !sameValue(got, 3) {
		t.Errorf("profile 应覆盖 default，实际 %v", got)
	}

	// 只改 default 不该抹掉 type/min/max —— 否则非法值会溜过校验
	_, err := Render(Request{
		Component: "demo", Pack: p, Profile: "ha",
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
		Overrides: Overrides{Component: map[string]any{"replicas": 99}},
	})
	if err == nil {
		t.Fatal("profile 覆盖 default 后，max 约束仍应生效")
	}
}

// ── 密钥 ────────────────────────────────────────────────────────────────

type fakeVault struct {
	values map[string]string
	calls  int
}

func (f *fakeVault) Ensure(component, param string, g pack.Generate) (StoredSecret, error) {
	f.calls++
	key := component + "/" + param
	if _, ok := f.values[key]; !ok {
		f.values[key] = "generated-" + param
	}
	return StoredSecret{ID: "sec-" + param, Version: 1, Value: f.values[key]}, nil
}

func (f *fakeVault) Store(component, param, value string) (StoredSecret, error) {
	f.values[component+"/"+param] = value
	return StoredSecret{ID: "sec-" + param, Version: 1, Value: value}, nil
}

const secretPack = `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
params:
  app_password:
    type: secret
    generate: { length: 32 }
roles:
  - name: server
    cardinality: "1-N"
    resources:
      - template:
          src: app.conf.tmpl
          dest: "{{ .Paths.Config }}/app.conf"
          mode: "0600"
`

// TestSecretNeverAppearsInSpec 钉住 16-secrets 的核心不变式。
func TestSecretNeverAppearsInSpec(t *testing.T) {
	p := writePack(t, secretPack, map[string]string{
		"app.conf.tmpl": "password={{ .Params.app_password }}\n",
	})
	v := &fakeVault{values: map[string]string{}}
	res := mustRender(t, Request{
		Component: "demo", Pack: p, Secrets: v,
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
	})
	s := res.Spec("server", "n1")

	blob, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "generated-app_password") {
		t.Fatal("密钥明文出现在了规格里——归档、审计、diff 都会带上它")
	}
	if !strings.Contains(string(blob), spec.SecretToken("sec-app_password")) {
		t.Error("规格里应当是哨兵串")
	}
	if len(s.SecretRefs) != 1 || s.SecretRefs[0].ID != "sec-app_password" {
		t.Errorf("secretRefs 应有一条，实际 %+v", s.SecretRefs)
	}
	if s.Params["app_password"].Value != nil {
		t.Error("敏感参数的 Value 必须为空")
	}
	if !s.Params["app_password"].Sensitive {
		t.Error("secret 类型的参数必须标 Sensitive")
	}

	// 明文走的是另一条路：随 gRPC 消息下发，不落盘
	if res.Secrets["sec-app_password"] != "generated-app_password" {
		t.Errorf("明文应随 Result.Secrets 单独下发，实际 %v", res.Secrets)
	}

	// mechlet 侧还原后必须拿到明文
	back, err := spec.ResolveSecrets(s, res.Secrets)
	if err != nil {
		t.Fatalf("还原密钥: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(back.Resources[0].Args, &m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m["content"].(string), "password=generated-app_password") {
		t.Errorf("还原后应是明文，实际 %v", m["content"])
	}
}

// TestMissingSecretIsRefusedNotBlanked 钉住「绝不把哨兵串当空值」。
func TestMissingSecretIsRefusedNotBlanked(t *testing.T) {
	p := writePack(t, secretPack, map[string]string{
		"app.conf.tmpl": "password={{ .Params.app_password }}\n",
	})
	v := &fakeVault{values: map[string]string{}}
	res := mustRender(t, Request{
		Component: "demo", Pack: p, Secrets: v,
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
	})
	_, err := spec.ResolveSecrets(res.Spec("server", "n1"), nil)
	if err == nil {
		t.Fatal("缺少密钥值时必须报错，而不是写一个空口令进配置")
	}
	if !strings.Contains(err.Error(), "app_password") {
		t.Errorf("错误信息应指名到参数，实际:\n%v", err)
	}
}

// TestUserSuppliedSecretIsSealedToo 钉住一个曾经漏掉的洞。
//
// 早先只遮蔽 generate 出来的密钥。用户用 --set-file 给的口令虽然在 Params
// 里被抹成空值，**明文却仍留在渲染出的配置内容里**，随规格进归档、审计
// 与 diff——正是 16-secrets §1 那条不变式要挡住的事。
//
// 这个洞是 minio 的示例暴露的：它的 root_password 是 required 而非 generate。
func TestUserSuppliedSecretIsSealedToo(t *testing.T) {
	p := writePack(t, `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
params:
  root_password:
    type: secret
    required: true
roles:
  - name: server
    cardinality: "1-N"
    resources:
      - template:
          src: app.conf.tmpl
          dest: "{{ .Paths.Config }}/app.conf"
          mode: "0600"
`, map[string]string{"app.conf.tmpl": "password={{ .Params.root_password }}\n"})

	v := &fakeVault{values: map[string]string{}}
	res := mustRender(t, Request{
		Component: "demo", Pack: p, Secrets: v,
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
		Overrides: Overrides{Component: map[string]any{"root_password": "operator-typed-this"}},
	})
	s := res.Spec("server", "n1")

	blob, _ := json.Marshal(s)
	if strings.Contains(string(blob), "operator-typed-this") {
		t.Fatal("用户给的口令明文留在了规格里——归档、审计、diff 都会带上它")
	}
	if len(s.SecretRefs) != 1 {
		t.Fatalf("用户给的敏感值也应产生 secretRef，实际 %+v", s.SecretRefs)
	}
	// 而且它必须进了 Vault：不然重启 mechd 后就还原不出来了
	if v.values["demo/root_password"] != "operator-typed-this" {
		t.Errorf("用户给的敏感值应被固化进 Vault，实际 %v", v.values)
	}
	back, err := spec.ResolveSecrets(s, res.Secrets)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(back.Resources[0].Args, &m)
	if !strings.Contains(m["content"].(string), "password=operator-typed-this") {
		t.Errorf("消费点应还原为明文，实际 %v", m["content"])
	}
}

// TestSecretWithoutVaultIsRefused 钉住「没接 Vault 时不能假装成功」。
func TestSecretWithoutVaultIsRefused(t *testing.T) {
	p := writePack(t, `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
params:
  pw: { type: secret, required: true }
roles:
  - name: server
    cardinality: "1-N"
`, nil)
	_, err := Render(Request{
		Component: "demo", Pack: p, // 故意不给 Secrets
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
		Overrides: Overrides{Component: map[string]any{"pw": "hunter2hunter2"}},
	})
	if err == nil {
		t.Fatal("没有 Vault 时处理敏感值必须报错，否则明文会原样留在规格里")
	}
	if !strings.Contains(err.Error(), "SecretVault") {
		t.Errorf("错误信息应说清缺的是什么，实际:\n%v", err)
	}
}

// TestGenerateOnlyOnce 钉住「同一 Component 的所有实例共用一份凭据」。
func TestGenerateOnlyOnce(t *testing.T) {
	p := writePack(t, secretPack, map[string]string{
		"app.conf.tmpl": "password={{ .Params.app_password }}\n",
	})
	v := &fakeVault{values: map[string]string{}}
	mustRender(t, Request{
		Component: "demo", Pack: p, Secrets: v,
		Instances: []Instance{
			inst("server", "n1", "10.0.0.1", 0),
			inst("server", "n2", "10.0.0.2", 1),
			inst("server", "n3", "10.0.0.3", 2),
		},
	})
	// Ensure 每个实例都会问一次，但值必须只生成一次
	if len(v.values) != 1 {
		t.Errorf("同一 Component 的实例应共用一份凭据，实际生成了 %d 份", len(v.values))
	}
}

// TestSecretRefsParticipateInDigest 钉住 16-secrets §5 推翻的那条早期设计。
//
// 若轮换不改 digest，就不产生新 generation，配置差异会被当成漂移，
// 默认策略只上报不改——**轮换永远发不出去**。
func TestSecretRefsParticipateInDigest(t *testing.T) {
	base := &spec.ResolvedSpec{
		SchemaVersion: spec.SchemaVersion,
		Component:     "demo", Role: "server",
		Node:      spec.NodeRef{Name: "n1"},
		Resources: []spec.Resource{},
	}
	v1 := *base
	v1.SecretRefs = []spec.SecretRef{{ID: "s1", Version: 1}}
	v2 := *base
	v2.SecretRefs = []spec.SecretRef{{ID: "s1", Version: 2}}

	d1, err := spec.ComputeDigest(&v1)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := spec.ComputeDigest(&v2)
	if err != nil {
		t.Fatal(err)
	}
	if d1 == d2 {
		t.Fatal("轮换密钥必须改变 digest，否则新口令永远发不到节点上")
	}
}

// TestSentinelCollisionRefused 钉住「不依赖概率」。
func TestSentinelCollisionRefused(t *testing.T) {
	p := writePack(t, secretPack, map[string]string{
		"app.conf.tmpl": "x=" + spec.SecretPrefix + "evil@@\npassword={{ .Params.app_password }}\n",
	})
	_, err := Render(Request{
		Component: "demo", Pack: p, Secrets: &fakeVault{values: map[string]string{}},
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
	})
	if err == nil {
		t.Fatal("渲染结果里出现哨兵串前缀时必须报错")
	}
	if !strings.Contains(err.Error(), spec.SecretPrefix) {
		t.Errorf("错误信息应点出哨兵串，实际:\n%v", err)
	}
}

// ── 敏感传播 ────────────────────────────────────────────────────────────

const consumerPack = `
schema: pack/v1
name: webapp
version: "1.0.0"
platforms: [linux/amd64]
requires:
  packs:
    - { name: postgresql, version: ">=16", scope: site }
params:
  db_password:
    type: string
    from: "{{ .Requires.postgresql.Exports.app.password }}"
roles:
  - name: server
    cardinality: "1-N"
`

// TestSensitivityPropagatesFromExports 钉住 15-render-pipeline §4。
//
// 这条判断**不可能在 lint 里做**——lint 只看得见一个 Pack，而提供方可能
// 来自别处、单独发布。mechd 是唯一有全局视角的地方。
func TestSensitivityPropagatesFromExports(t *testing.T) {
	p := writePack(t, consumerPack, nil)
	res := mustRender(t, Request{
		Component: "webapp", Pack: p,
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
		Requires: map[string]Binding{
			"postgresql": {
				Pack: "postgresql", Component: "pg-main", Scope: pack.ScopeSite,
				Exports: map[string]Export{
					"app": {
						Fields:          map[string]string{"host": "10.0.0.9", "password": "s3cret"},
						SensitiveFields: map[string]bool{"password": true},
					},
				},
			},
		},
	})
	s := res.Spec("server", "n1")

	// 消费方声明的是 type: string，但取值来自敏感字段 —— mechd 直接接管
	if !s.Params["db_password"].Sensitive {
		t.Fatal("取值来自敏感导出字段的参数必须被标为敏感，不管消费方声明的是什么")
	}
	if s.Params["db_password"].Value != nil {
		t.Error("敏感参数的 Value 必须为空")
	}

	blob, _ := json.Marshal(s)
	if strings.Contains(string(blob), "s3cret") {
		t.Fatal("provider 的口令明文进了消费方的规格")
	}
	// 非敏感字段照常可见——否则排障时连「连的是哪个库」都看不到
	if !strings.Contains(string(blob), "10.0.0.9") {
		t.Error("非敏感的导出字段不应被遮蔽")
	}

	var found bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "type: secret") {
			found = true
		}
	}
	if !found {
		t.Errorf("应提示消费方补上 type: secret（但不阻断），实际告警: %v", res.Warnings)
	}
}

// TestOverTaggingWarns 钉住反向的提示。
//
// 过度标注同样有害：它会让排障时连「连的哪个库」都看不到，标记退化成噪音。
func TestOverTaggingWarns(t *testing.T) {
	p := writePack(t, `
schema: pack/v1
name: webapp
version: "1.0.0"
platforms: [linux/amd64]
requires:
  packs:
    - { name: postgresql, version: ">=16", scope: site }
params:
  db_host:
    type: secret
    from: "{{ .Requires.postgresql.Exports.app.host }}"
roles:
  - name: server
    cardinality: "1-N"
`, nil)
	res := mustRender(t, Request{
		Component: "webapp", Pack: p,
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
		Requires: map[string]Binding{
			"postgresql": {
				Pack: "postgresql", Component: "pg-main", Scope: pack.ScopeSite,
				Exports: map[string]Export{
					"app": {
						Fields:          map[string]string{"host": "10.0.0.9", "password": "s3cret"},
						SensitiveFields: map[string]bool{"password": true},
					},
				},
			},
		},
	})
	var found bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "turning the marker into noise") {
			found = true
		}
	}
	if !found {
		t.Errorf("过度标注应当被提示，实际告警: %v", res.Warnings)
	}
}

// TestNodeScopedDepPaths 钉住 scope:node 的依赖暴露 .Paths。
func TestNodeScopedDepPaths(t *testing.T) {
	p := writePack(t, `
schema: pack/v1
name: webapp
version: "1.0.0"
platforms: [linux/amd64]
requires:
  packs:
    - { name: jdk11, version: ">=11", scope: node }
roles:
  - name: server
    cardinality: "1-N"
    workload:
      runtime: systemd
      systemd:
        exec: "{{ .Requires.jdk11.Paths.Current }}/bin/java -jar app.jar"
`, nil)
	res := mustRender(t, Request{
		Component: "webapp", Pack: p,
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
		Requires: map[string]Binding{
			"jdk11": {
				Pack: "jdk11", Component: "jdk11", Scope: pack.ScopeNode,
				Paths: map[string][]string{"current": {"/opt/mecharion/apps/jdk11/current"}},
			},
		},
	})
	want := "/opt/mecharion/apps/jdk11/current/bin/java -jar app.jar"
	if got := res.Spec("server", "n1").Workload.Systemd.Exec; got != want {
		t.Errorf("exec 应引用依赖的本机路径，期望 %q，实际 %q", want, got)
	}
}

// ── notify ──────────────────────────────────────────────────────────────

// TestRestartRequiredUpgradesReload 钉住 15-render-pipeline §7。
//
// 这个判断只有 mechd 做得了——它持有上一版规格，知道哪些参数变了。
func TestRestartRequiredUpgradesReload(t *testing.T) {
	src := `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
params:
  port: { type: port, default: 8080, restartRequired: true }
  msg:  { type: string, default: "hi", reloadRequired: true }
roles:
  - name: server
    cardinality: "1-N"
    resources:
      - template:
          id: conf
          src: app.conf.tmpl
          dest: "{{ .Paths.Config }}/app.conf"
          notify: reload
`
	p := writePack(t, src, map[string]string{
		"app.conf.tmpl": "port={{ .Params.port }}\nmsg={{ .Params.msg }}\n",
	})
	base := Request{
		Component: "demo", Pack: p,
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
	}
	first := mustRender(t, base)
	prev := map[string]*spec.ResolvedSpec{"server@n1": first.Spec("server", "n1")}

	// 只改 reloadRequired 的参数 → 保持 reload
	onlyMsg := base
	onlyMsg.Previous = prev
	onlyMsg.Overrides = Overrides{Component: map[string]any{"msg": "yo"}}
	if got := notifyOf(t, mustRender(t, onlyMsg), "conf"); got != "reload" {
		t.Errorf("只改 reloadRequired 参数时应保持 reload，实际 %q", got)
	}

	// 改了 restartRequired 的参数 → 提升为 restart
	withPort := base
	withPort.Previous = prev
	withPort.Overrides = Overrides{Component: map[string]any{"port": 9090}}
	res := mustRender(t, withPort)
	if got := notifyOf(t, res, "conf"); got != "restart" {
		t.Errorf("restartRequired 参数变更时应提升为 restart，实际 %q", got)
	}
	var explained bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "promoted to restart") {
			explained = true
		}
	}
	if !explained {
		t.Errorf("提升动作应当留下可追溯的说明，实际告警: %v", res.Warnings)
	}
}

func notifyOf(t *testing.T, res *Result, id string) string {
	t.Helper()
	for _, r := range res.Spec("server", "n1").Resources {
		if r.ID == id {
			return r.Notify
		}
	}
	t.Fatalf("没有资源 %q", id)
	return ""
}

// ── 纯函数性质 ──────────────────────────────────────────────────────────

// TestRenderIsDeterministic 钉住 15-render-pipeline §9。
//
// 「为什么这台机器上是这份配置」必须能离线回答；同一条管线少走
// 「落库 + 下发」两步就是 --dry-run 与 diff，不另写一套预演逻辑。
func TestRenderIsDeterministic(t *testing.T) {
	p := writePack(t, minimalPack, map[string]string{
		"app.conf.tmpl": "port = {{ .Params.port }}\n",
	})
	req := Request{
		Component: "demo", Pack: p,
		Instances: []Instance{
			inst("server", "n1", "10.0.0.1", 0),
			inst("server", "n2", "10.0.0.2", 1),
		},
	}
	a := mustRender(t, req)
	b := mustRender(t, req)
	for _, k := range a.Order {
		if a.Specs[k].Digest != b.Specs[k].Digest {
			t.Fatalf("%s 两次渲染的 digest 不同 —— 管线不是纯函数，"+
				"每轮调和都会产生新 generation", k)
		}
	}
}

// TestMissingKeyIsError 钉住 missingkey=error。
//
// 引用不存在的键必须失败，不能渲染出半份配置——一个少了整段的配置文件
// 往往能启动，然后以难以理解的方式行为异常。
func TestMissingKeyIsError(t *testing.T) {
	p := writePack(t, `
schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
roles:
  - name: server
    cardinality: "1-N"
    resources:
      - template: { src: bad.tmpl, dest: "{{ .Paths.Config }}/x" }
`, map[string]string{"bad.tmpl": "v={{ .Params.nonexistent }}\n"})

	_, err := Render(Request{
		Component: "demo", Pack: p,
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
	})
	if err == nil {
		t.Fatal("引用不存在的参数应当渲染失败")
	}
}

// TestNoEscapeHatchFuncs 钉住受限函数集。
//
// 一个 `{{ env "..." }}` 就能让整个 Pack 依赖部署机的环境，
// hermetic 承诺当场作废。
func TestNoEscapeHatchFuncs(t *testing.T) {
	fm := FuncMap()
	for _, banned := range []string{"env", "exec", "readFile", "include", "getHostByName", "expandenv"} {
		if _, exists := fm[banned]; exists {
			t.Errorf("函数集里不该有 %q —— 它能绕过 hermetic 约束", banned)
		}
	}
}

// TestFuncMapMatchesLintNames 钉住 lint 与渲染用的是同一份函数集。
//
// 两边不一致的后果是**过了 lint 却渲染不出来**，而那要到部署时才暴露。
func TestFuncMapMatchesLintNames(t *testing.T) {
	fm := FuncMap()
	builtin := map[string]bool{
		"and": true, "or": true, "not": true, "len": true, "index": true,
		"slice": true, "printf": true, "print": true, "println": true,
		"eq": true, "ne": true, "lt": true, "le": true, "gt": true, "ge": true,
		"call": true, "html": true, "js": true, "urlquery": true,
	}
	var missing []string
	for _, name := range pack.FuncNames {
		if builtin[name] {
			continue // text/template 自带，不需要我们提供
		}
		if _, ok := fm[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("lint 认这些函数但渲染没实现: %v\n"+
			"  后果是 Pack 过了 lint 却在部署时渲染失败", missing)
	}

	declared := map[string]bool{}
	for _, n := range pack.FuncNames {
		declared[n] = true
	}
	for name := range fm {
		if !declared[name] {
			t.Errorf("渲染提供了 %q 但 pack.FuncNames 里没有 —— "+
				"lint 会把用到它的模板判为解析失败", name)
		}
	}
}

// ── 函数行为 ────────────────────────────────────────────────────────────

func TestFormatSizeRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{8 << 30, "8Gi"},
		{31_000_000_000, "31GB"},
		{1536, "1536"}, // 除不尽任何单位 → 原样输出字节数
		{1 << 10, "1Ki"},
	} {
		if got := FormatSize(tc.in); got != tc.want {
			t.Errorf("FormatSize(%d) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
	// 除不尽时输出裸字节数，仍必须是合法 size
	if _, err := pack.ParseSize(FormatSize(1536)); err != nil {
		t.Errorf("FormatSize 的输出必须能被 ParseSize 认回来: %v", err)
	}
}

func TestArithAcceptsSizeLiterals(t *testing.T) {
	fm := FuncMap()
	div := fm["div"].(func(a, b any) (int64, error))
	got, err := div("16Gi", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got != 8<<30 {
		t.Errorf("div(16Gi, 2) = %d，期望 %d", got, int64(8)<<30)
	}
	if _, err := div(1, 0); err == nil {
		t.Error("除零应当报错而不是产生一个荒谬的值")
	}
}

func TestQuoteEscapes(t *testing.T) {
	fm := FuncMap()
	q := fm["quote"].(func(v any) string)
	if got := q(`a"b`); got != `"a\"b"` {
		t.Errorf("quote 未转义内部引号: %s", got)
	}
}

// ── runtime: compose ────────────────────────────────────────────────────

// composePack 是一个 compose 角色。
const composePack = `
schema: pack/v1
name: shop
version: "1.0.0"
platforms: [linux/amd64]
params:
  port:
    type: port
    default: 8080
roles:
  - name: server
    cardinality: "1-N"
    workload:
      runtime: compose
      compose:
        file: compose.yaml.tmpl
        imageBlobs: [web]
        execService: web
`

// TestComposeFileIsEmittedByPipeline 钉住 compose 文件由**流水线**产出。
//
// Pack 作者只写模板名。让他再声明一条 template 资源的话，两处必须一致而
// lint 又查不了（两边都是模板表达式），写错的表现是 `compose up` 读到一份
// 过期文件——那种现场极难查（19-container-runtime §6.6.1）。
func TestComposeFileIsEmittedByPipeline(t *testing.T) {
	p := writePack(t, composePack, map[string]string{
		"compose.yaml.tmpl": "services:\n  web:\n    ports: [\"{{ .Params.port }}:8080\"]\n",
	})
	res := mustRender(t, Request{
		Component: "shop", Pack: p,
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
	})

	s := res.Spec("server", "n1")
	if s == nil {
		t.Fatalf("没有产出规格，实际有: %v", res.Order)
	}
	confDir := s.Paths["config"].First()
	dest := confDir + "/compose.yaml"

	// ① 那条资源在，且内容是**渲染过的**
	args := argsOf(t, s, "template:"+dest)
	body, _ := args["content"].(string)
	if !strings.Contains(body, `"8080:8080"`) {
		t.Errorf("compose 文件应当已求值参数，实际:\n%s", body)
	}
	// src 不该出现在已解析规格里——mechd 渲染，mechlet 只落盘
	if _, ok := args["src"]; ok {
		t.Error("已解析规格里不该有 src")
	}

	// ② workload.compose.file 被改写成了**绝对路径**
	var cw struct {
		File        string `json:"file"`
		ExecService string `json:"execService"`
	}
	if err := json.Unmarshal(s.Workload.Compose, &cw); err != nil {
		t.Fatal(err)
	}
	if cw.File != dest {
		t.Errorf("规格里的 file 应当是绝对路径 %q，实际 %q", dest, cw.File)
	}
	if cw.ExecService != "web" {
		t.Errorf("execService 应当透传，实际 %q", cw.ExecService)
	}
}

// TestComposeFileCollidesWithDeclaredResource 钉住撞车要报错。
//
// 谁后写谁赢是最难查的一类 bug：盘上的文件与 Pack 作者看到的声明对不上，
// 而两处都「看起来是对的」。
func TestComposeFileCollidesWithDeclaredResource(t *testing.T) {
	pk := strings.Replace(composePack, `  - name: server
    cardinality: "1-N"`, `  - name: server
    cardinality: "1-N"
    resources:
      - template:
          src: compose.yaml.tmpl
          dest: "{{ .Paths.Config }}/compose.yaml"
          mode: "0644"`, 1)

	p := writePack(t, pk, map[string]string{
		"compose.yaml.tmpl": "services:\n  web: {}\n",
	})
	_, err := Render(Request{
		Component: "shop", Pack: p,
		Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
	})
	if err == nil {
		t.Fatal("Pack 自己也往 compose.yaml 写时应当报错")
	}
	if !strings.Contains(err.Error(), "compose") {
		t.Errorf("错误应说清是与自动产出的 compose 文件撞了，实际: %v", err)
	}
}

// TestHookScriptPathAcceptsBothForms 钉住 hook 路径的两种写法都能落地。
//
// 规范允许 `hooks/x.sh` 与 `x.sh`（spec §16.3），lint 也确实两种都放行。
// 但渲染侧曾经无条件再拼一次 `hooks/`，于是写全路径的 Pack **能过 lint、
// 部署时才炸**：
//
//	执行 hook: hooks/hooks/pre-install.sh: no such file or directory
//
// 这类 bug 的根源是同一条规则被写了两遍。现在两处共用 pack.HookScriptPath，
// 这条测试守住它们不再分叉。
func TestHookScriptPathAcceptsBothForms(t *testing.T) {
	for _, form := range []string{"hooks/setup.sh", "setup.sh"} {
		t.Run(form, func(t *testing.T) {
			pk := strings.Replace(minimalPack, `    workload:`, `    hooks:
      preInstall: `+form+`
    workload:`, 1)

			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(pk), 0o644); err != nil {
				t.Fatal(err)
			}
			for sub, files := range map[string]map[string]string{
				"templates": {"app.conf.tmpl": "port = {{ .Params.port }}\n"},
				"hooks":     {"setup.sh": "#!/bin/sh\nexit 0\n"},
			} {
				if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
					t.Fatal(err)
				}
				for name, body := range files {
					if err := os.WriteFile(filepath.Join(dir, sub, name), []byte(body), 0o755); err != nil {
						t.Fatal(err)
					}
				}
			}
			p, err := pack.Load(dir)
			if err != nil {
				t.Fatalf("解析 Pack: %v", err)
			}

			res := mustRender(t, Request{
				Component: "demo", Pack: p,
				Instances: []Instance{inst("server", "n1", "10.0.0.1", 0)},
			})
			s := res.Spec("server", "n1")
			if len(s.Hooks) != 1 {
				t.Fatalf("应当有 1 个 hook，实际 %v", s.Hooks)
			}
			// 两种写法都要归一到同一条 Pack 内相对路径
			if got := s.Hooks[0].Script; got != "hooks/setup.sh" {
				t.Errorf("hook 路径应归一为 hooks/setup.sh，实际 %q", got)
			}
		})
	}
}

// TestDriftPolicyOverrideRelaxes 钉住站点覆盖在渲染时就合进最终值。
//
// 合在这里而不是留给 mechlet，是 ADR-0006 的直接结论：mechlet 不做任何
// 判断，规格里的 driftPolicy 就是**已经算好的最终值**。放到 mechlet 侧
// 意味着每个 Runtime、每条调和路径都要记得再合一次，而漏掉一处的表现是
// 「策略在某些资源上不生效」——那种现场几乎查不出来。
func TestDriftPolicyOverrideRelaxes(t *testing.T) {
	pk := strings.Replace(minimalPack, `          notify: reload`,
		`          notify: reload
          driftPolicy: reconcile`, 1)
	p := writePack(t, pk, map[string]string{
		"app.conf.tmpl": "port = {{ .Params.port }}\n",
	})

	for _, tc := range []struct {
		override, want string
	}{
		{"", "reconcile"},
		{"report", "report"},
		{"ignore", "ignore"},
	} {
		t.Run("override="+tc.override, func(t *testing.T) {
			res := mustRender(t, Request{
				Component: "demo", Pack: p,
				Instances:   []Instance{inst("server", "n1", "10.0.0.1", 0)},
				DriftPolicy: tc.override,
			})
			s := res.Spec("server", "n1")
			var got string
			for _, r := range s.Resources {
				if strings.HasPrefix(r.ID, "template:") {
					got = r.DriftPolicy
				}
			}
			if got != tc.want {
				t.Errorf("Pack 声明 reconcile、覆盖 %q 时应得 %q，实际 %q",
					tc.override, tc.want, got)
			}
		})
	}
}
