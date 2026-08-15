package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/spec"
)

// examplesDir 定位仓库里的示例 Pack。容器里没有源码树，跳过。
func examplesDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", "examples", "packs")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("没有源码树，跳过（容器内运行）")
	}
	return dir
}

// realisticFacts 是一台 32GB / 8 核机器，够 defaultFrom 算出正经的值。
func realisticFacts() map[string]any {
	return map[string]any{
		"hostname": "node",
		"arch":     "amd64",
		"os":       map[string]any{"family": "debian", "version": "12"},
		"cpu":      map[string]any{"cores": 8, "threads": 16},
		"memory":   map[string]any{"total": "32Gi", "available": "28Gi"},
	}
}

func exampleNode(name, addr string) Node {
	return Node{
		Name: name, Address: addr,
		Labels: map[string]string{"rack": "r1"},
		Roots:  map[string]string{},
		Facts:  realisticFacts(),
	}
}

// TestRenderRealExamplePacks 让管线跑一遍真实的示例 Pack。
//
// 自造的最小 Pack 只能验证「我想到的那些情况」。真实 Pack 里的多角色、
// profile 守卫、跨 Pack 引用、多盘枚举，才是这条管线要面对的东西——
// 这是继 lint 之后第二次用示例集当验收夹具。
func TestRenderRealExamplePacks(t *testing.T) {
	root := examplesDir(t)

	jdk := Binding{
		Pack: "jdk11", Component: "jdk11", Version: "11.0.22", Scope: pack.ScopeNode,
		Paths: map[string][]string{
			"home":    {"/opt/mecharion/apps/jdk11"},
			"current": {"/opt/mecharion/apps/jdk11/current"},
		},
	}
	zk := Binding{
		Pack: "zookeeper", Component: "zk-main", Version: "3.9.1", Scope: pack.ScopeSite,
		Exports: map[string]Export{
			"client": {Value: "10.0.0.1:2181,10.0.0.2:2181,10.0.0.3:2181"},
		},
		Topology: []Peer{
			{Node: "n1", Address: "10.0.0.1", Ordinal: 0, Role: "server"},
			{Node: "n2", Address: "10.0.0.2", Ordinal: 1, Role: "server"},
			{Node: "n3", Address: "10.0.0.3", Ordinal: 2, Role: "server"},
		},
	}
	pg := Binding{
		Pack: "postgresql", Component: "pg-main", Version: "16.4", Scope: pack.ScopeSite,
		Exports: map[string]Export{
			"app": {
				Fields: map[string]string{
					"host": "10.0.0.9", "port": "5432",
					"database": "appdb", "username": "app", "password": "pg-s3cret",
				},
				SensitiveFields: map[string]bool{"password": true},
			},
		},
		Topology: []Peer{{Node: "n9", Address: "10.0.0.9", Ordinal: 0, Role: "primary"}},
	}

	cases := []struct {
		pack     string
		profile  string
		roles    map[string]int // 角色 → 实例数
		requires map[string]Binding
		// set 是运维必须给的值：required 且没有 generate 的参数。
		// 这类参数**故意**不给默认值——一个有默认口令的对象存储比没有更糟。
		set map[string]any
	}{
		{pack: "go-webapp", roles: map[string]int{"default": 1}},
		{pack: "jdk11", roles: map[string]int{"default": 1}},
		{pack: "nginx", roles: map[string]int{"default": 1}},
		{pack: "zookeeper", roles: map[string]int{"server": 3},
			requires: map[string]Binding{"jdk11": jdk}},
		{pack: "minio", roles: map[string]int{"server": 4},
			set: map[string]any{"root_password": "minio-admin-pw"}},
		{pack: "java-webapp", roles: map[string]int{"default": 1},
			requires: map[string]Binding{"jdk11": jdk, "postgresql": pg}},
		{pack: "elasticsearch", roles: map[string]int{"master": 3, "data": 2}},
		{pack: "host-tuning", roles: map[string]int{"default": 1}},
		{pack: "docker", roles: map[string]int{"engine": 1}},

		// kafka 的三个形态是**架构**而非规模之别，各自的角色集完全不同——
		// 逐个跑一遍才谈得上覆盖 profile 这条轴
		{pack: "kafka", profile: "kraft-combined",
			roles:    map[string]int{"combined": 3},
			requires: map[string]Binding{"jdk17": jdk}},
		{pack: "kafka", profile: "kraft-separated",
			roles:    map[string]int{"controller": 3, "broker": 2},
			requires: map[string]Binding{"jdk17": jdk}},
		{pack: "kafka", profile: "zookeeper",
			roles:    map[string]int{"broker": 3},
			requires: map[string]Binding{"jdk17": jdk, "zookeeper": zk}},

		// 这两个是路线图里的试金石：postgresql 有多角色 + hooks +
		// 配置位于数据目录内，hdfs 有 profile 守卫 + 模板片段 + 多盘
		{pack: "postgresql", roles: map[string]int{"primary": 1, "replica": 2},
			set: map[string]any{"admin_password": "pg-admin-password"}},
		{pack: "hdfs", profile: "simple",
			roles:    map[string]int{"namenode": 1, "datanode": 3},
			requires: map[string]Binding{"jdk11": jdk}},
		{pack: "hdfs", profile: "ha",
			roles:    map[string]int{"namenode": 2, "journalnode": 3, "datanode": 3, "zkfc": 2},
			requires: map[string]Binding{"jdk11": jdk, "zookeeper": zk}},
	}

	for _, tc := range cases {
		t.Run(tc.pack+"/"+tc.profile, func(t *testing.T) {
			p, err := pack.Load(filepath.Join(root, tc.pack))
			if err != nil {
				t.Skipf("加载 Pack: %v", err)
			}

			var insts []Instance
			i := 0
			for _, role := range sortedRoleNames(tc.roles) {
				for n := 0; n < tc.roles[role]; n++ {
					i++
					in := Instance{
						Role: role, Ordinal: n, ConfigGroup: "default",
						Node: exampleNode(nodeName(i), addrOf(i)),
					}
					insts = append(insts, in)
				}
			}

			req := Request{
				Site:      spec.SiteRef{Name: "site1", Kind: "cluster"},
				Component: tc.pack, Pack: p,
				PackRef:   spec.PackRef{Name: p.Name, Version: p.Version},
				Profile:   tc.profile,
				Instances: insts,
				Requires:  tc.requires,
				Overrides: Overrides{Component: tc.set},
				Secrets:   &fakeVault{values: map[string]string{}},
			}
			res, err := Render(req)
			if err != nil {
				t.Fatalf("渲染示例 Pack %s 失败: %v", tc.pack, err)
			}

			for _, key := range res.Order {
				s := res.Specs[key]
				if err := spec.Validate(s); err != nil {
					t.Errorf("%s: 产出的规格通不过 mechlet 侧校验: %v", key, err)
				}
				if err := spec.VerifyDigest(s); err != nil {
					t.Errorf("%s: %v", key, err)
				}
				assertNoLeftoverTemplate(t, key, s)
			}
		})
	}
}

// assertNoLeftoverTemplate 确认规格里没有未求值的模板语法。
//
// 漏渲染的后果不是报错而是**把 `{{ .Params.x }}` 字面写进配置文件**——
// 应用多半能启动，然后以难以理解的方式行为异常。唯一允许残留的是
// generation 占位符。
func assertNoLeftoverTemplate(t *testing.T, key string, s *spec.ResolvedSpec) {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(b), spec.GenerationPlaceholder, "")
	if i := strings.Index(text, "{{"); i >= 0 {
		end := min(i+120, len(text))
		t.Errorf("%s: 规格里残留了未求值的模板语法:\n  …%s…", key, text[i:end])
	}
}

// TestExamplePackSecretsAreSealed 在真实 Pack 上验证密钥不进规格。
func TestExamplePackSecretsAreSealed(t *testing.T) {
	root := examplesDir(t)
	p, err := pack.Load(filepath.Join(root, "minio"))
	if err != nil {
		t.Skipf("加载 minio: %v", err)
	}

	v := &fakeVault{values: map[string]string{}}
	var insts []Instance
	for i := 0; i < 4; i++ {
		insts = append(insts, Instance{
			Role: "server", Ordinal: i, ConfigGroup: "default",
			Node: exampleNode(nodeName(i+1), addrOf(i+1)),
		})
	}
	res, err := Render(Request{
		Component: "minio", Pack: p,
		PackRef:   spec.PackRef{Name: p.Name, Version: p.Version},
		Instances: insts, Secrets: v,
		Overrides: Overrides{Component: map[string]any{"root_password": "minio-admin-pw"}},
	})
	if err != nil {
		t.Fatalf("渲染 minio: %v", err)
	}
	if len(res.Secrets) == 0 {
		t.Skip("该版本的 minio 示例没有 generate 参数")
	}

	for _, key := range res.Order {
		blob, _ := json.Marshal(res.Specs[key])
		for id, plain := range res.Secrets {
			if strings.Contains(string(blob), plain) {
				t.Errorf("%s: 密钥 %s 的明文出现在规格里", key, id)
			}
		}
	}
}

func sortedRoleNames(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// 顺序只影响节点编号的分配，稳定即可
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func nodeName(i int) string { return "n" + itoa(i) }
func addrOf(i int) string   { return "10.0.0." + itoa(i) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestExampleExportsEvaluate 钉住 exports 的两种形态在真实 Pack 上求值正确。
//
// 这是 Pack 之间唯一被推荐的耦合方式，因此它算错的后果是**下游全体**
// 拿到一个看起来正常的错连接串。
func TestExampleExportsEvaluate(t *testing.T) {
	root := examplesDir(t)

	t.Run("format 形态", func(t *testing.T) {
		p, err := pack.Load(filepath.Join(root, "zookeeper"))
		if err != nil {
			t.Skipf("加载 zookeeper: %v", err)
		}
		var insts []Instance
		for i := 0; i < 3; i++ {
			insts = append(insts, Instance{
				Role: "server", Ordinal: i, ConfigGroup: "default",
				Node: exampleNode(nodeName(i+1), addrOf(i+1)),
			})
		}
		res, err := Render(Request{
			Component: "zk-main", Pack: p,
			PackRef:   spec.PackRef{Name: p.Name, Version: p.Version},
			Instances: insts,
			Requires: map[string]Binding{"jdk11": {
				Pack: "jdk11", Component: "jdk11", Scope: pack.ScopeNode,
				Paths: map[string][]string{"current": {"/opt/mecharion/apps/jdk11/current"}},
			}},
			Secrets: &fakeVault{values: map[string]string{}},
		})
		if err != nil {
			t.Fatalf("渲染: %v", err)
		}

		// 多实例按 ordinal 顺序拼成一串，消费方只引用导出名
		want := "10.0.0.1:2181,10.0.0.2:2181,10.0.0.3:2181"
		if got := res.Exports["client"].Value; got != want {
			t.Errorf("client 导出应为 %q，实际 %q", want, got)
		}
		if res.Exports["client"].Fields != nil {
			t.Error("format 形态不该有 fields")
		}
	})

	t.Run("fields 形态与敏感推导", func(t *testing.T) {
		p, err := pack.Load(filepath.Join(root, "postgresql"))
		if err != nil {
			t.Skipf("加载 postgresql: %v", err)
		}
		res, err := Render(Request{
			Component: "pg-main", Pack: p,
			PackRef: spec.PackRef{Name: p.Name, Version: p.Version},
			Instances: []Instance{{
				Role: "primary", Ordinal: 0, ConfigGroup: "default",
				Node: exampleNode("n1", "10.0.0.1"),
			}},
			Overrides: Overrides{Component: map[string]any{
				"admin_password": "pg-admin-password",
			}},
			Secrets: &fakeVault{values: map[string]string{}},
		})
		if err != nil {
			t.Fatalf("渲染: %v", err)
		}

		app := res.Exports["app"]
		if app.Fields == nil {
			t.Fatal("fields 形态应当产出具名字段")
		}
		if app.Fields["host"] != "10.0.0.1" {
			t.Errorf("host 应为提供该连接点的机器地址，实际 %q", app.Fields["host"])
		}
		if app.Fields["port"] == "" || app.Fields["database"] == "" {
			t.Errorf("字段不完整: %+v", app.Fields)
		}

		// **敏感标记是推导出来的**：password 引用了 secret 参数。
		// 让 Pack 作者手工标注等于给了一个会被忘记的机会。
		if !app.SensitiveFields["password"] {
			t.Error("引用了 secret 参数的字段必须被推导为敏感")
		}
		for _, f := range []string{"host", "port", "database", "username"} {
			if app.SensitiveFields[f] {
				t.Errorf("字段 %s 不该被标为敏感——过度标注会让排障时看不到连的是哪个库", f)
			}
		}
		// 口令本身确实被求值出来了（供消费方使用），只是带着标记
		if app.Fields["password"] == "" {
			t.Error("敏感字段仍应有值，标记管的是它怎么被对待")
		}
	})
}
