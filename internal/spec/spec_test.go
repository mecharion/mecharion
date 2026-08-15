package spec

import (
	"encoding/json"
	"strings"
	"testing"
)

// minimal 构造一份能通过校验的最小规格。
func minimal() *ResolvedSpec {
	return &ResolvedSpec{
		SchemaVersion: SchemaVersion,
		Site:          SiteRef{Name: "s1", Kind: "standalone"},
		Component:     "webapp",
		Role:          "default",
		ConfigGroup:   "default",
		Node:          NodeRef{Name: "node-1", Address: "10.0.0.1"},
		Pack:          PackRef{Name: "go-webapp", Version: "1.2.0", Revision: 1},
		Paths: map[string]PathValue{
			"home":   {Name: "home", Values: []string{"/opt/mecharion/apps/webapp"}, Kind: "single", Mode: "0755"},
			"config": {Name: "config", Values: []string{"/etc/mecharion/apps/webapp"}, Kind: "single", Mode: "0750"},
		},
		Resources: []Resource{
			{ID: "archive:main", Type: "archive", Args: json.RawMessage(`{"blob":"main"}`), DriftPolicy: "report"},
		},
		Workload: &Workload{
			Runtime: "systemd",
			Systemd: &SystemdWorkload{Exec: "/opt/mecharion/apps/webapp/current/bin/webapp"},
		},
		Topology: Topology{Roles: map[string][]Instance{
			"default": {{Node: "node-1", Address: "10.0.0.1", Ordinal: 0}},
		}},
	}
}

func TestDigestIsStable(t *testing.T) {
	a, b := minimal(), minimal()
	da, err := ComputeDigest(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := ComputeDigest(b)
	if err != nil {
		t.Fatal(err)
	}
	if da != db {
		t.Errorf("同样内容的两份规格摘要不同:\n  %s\n  %s", da, db)
	}
	if len(da) != 64 {
		t.Errorf("摘要应是 64 位十六进制，实际长度 %d", len(da))
	}
}

func TestDigestIgnoresItself(t *testing.T) {
	s := minimal()
	d1, _ := ComputeDigest(s)
	s.Digest = "随便填点什么"
	d2, _ := ComputeDigest(s)
	if d1 != d2 {
		t.Error("Digest 字段本身不应参与摘要计算")
	}
}

// TestDigestIgnoresSensitiveValues 钉住「密钥轮换不触发 generation 切换」。
func TestDigestIgnoresSensitiveValues(t *testing.T) {
	s := minimal()
	s.Params = map[string]ParamValue{
		"admin_password": {Type: "secret", Sensitive: true, Value: "旧密码"},
		"port":           {Type: "port", Value: 8080},
	}
	d1, _ := ComputeDigest(s)

	s.Params["admin_password"] = ParamValue{Type: "secret", Sensitive: true, Value: "新密码"}
	d2, _ := ComputeDigest(s)

	if d1 != d2 {
		t.Error("敏感参数变化不应改变摘要——否则密钥轮换会触发一次完整的 generation 切换")
	}

	// 非敏感参数变化必须改变摘要
	s.Params["port"] = ParamValue{Type: "port", Value: 9090}
	d3, _ := ComputeDigest(s)
	if d1 == d3 {
		t.Error("非敏感参数变化必须改变摘要")
	}
}

func TestDigestChangesWithTopology(t *testing.T) {
	s := minimal()
	d1, _ := ComputeDigest(s)

	// 加一个对等实例——渲染结果会变，因此摘要必须变
	s.Topology.Roles["default"] = append(s.Topology.Roles["default"],
		Instance{Node: "node-2", Address: "10.0.0.2", Ordinal: 1})
	d2, _ := ComputeDigest(s)

	if d1 == d2 {
		t.Error("拓扑变化必须改变摘要")
	}
}

func TestSealAndVerify(t *testing.T) {
	s := minimal()
	if err := Seal(s); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDigest(s); err != nil {
		t.Fatalf("刚封装的规格应当校验通过: %v", err)
	}
	s.Component = "被篡改了"
	if err := VerifyDigest(s); err == nil {
		t.Error("内容被改动后校验应当失败")
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	s := minimal()
	_ = Seal(s)
	b, _ := json.Marshal(s)

	var m map[string]any
	_ = json.Unmarshal(b, &m)
	m["未来才有的字段"] = 1
	b2, _ := json.Marshal(m)

	if _, err := Parse(b2); err == nil {
		t.Error("未知字段应当报错而非静默忽略——静默忽略会让行为与下发方预期不符")
	}
}

func TestParseAppliesDefaults(t *testing.T) {
	s := minimal()
	_ = Seal(s)
	b, _ := json.Marshal(s)

	got, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reconcile.Interval != DefaultInterval {
		t.Errorf("调和间隔默认值 = %q，期望 %q", got.Reconcile.Interval, DefaultInterval)
	}
	if got.Reconcile.HealthInterval != DefaultHealthInterval {
		t.Errorf("健康检查间隔默认值 = %q，期望 %q", got.Reconcile.HealthInterval, DefaultHealthInterval)
	}
	if got.Reconcile.RetainGenerations != DefaultRetainGenerations {
		t.Errorf("保留数默认值 = %d，期望 %d", got.Reconcile.RetainGenerations, DefaultRetainGenerations)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ResolvedSpec)
		want   string
	}{
		{"缺 component", func(s *ResolvedSpec) { s.Component = "" }, "component"},
		{"缺 role", func(s *ResolvedSpec) { s.Role = "" }, "role"},
		{"缺 node.name", func(s *ResolvedSpec) { s.Node.Name = "" }, "node.name"},
		{"schemaVersion 过高", func(s *ResolvedSpec) { s.SchemaVersion = SchemaVersion + 1 }, "upgrade mechlet"},
		{"资源缺 id", func(s *ResolvedSpec) { s.Resources[0].ID = "" }, "id"},
		{"资源 id 重复", func(s *ResolvedSpec) {
			s.Resources = append(s.Resources, s.Resources[0])
		}, "duplicate"},
		{"path 无值", func(s *ResolvedSpec) {
			s.Paths["home"] = PathValue{Name: "home"}
		}, "no values"},
		{"multi + inline", func(s *ResolvedSpec) {
			s.Paths["home"] = PathValue{Name: "home", Values: []string{"/a"}, Kind: "multi", Layout: "inline"}
		}, "cannot be combined with layout=inline"},
		{"systemd 缺 exec", func(s *ResolvedSpec) { s.Workload.Systemd.Exec = "" }, "exec"},
		{"未知 runtime", func(s *ResolvedSpec) { s.Workload.Runtime = "k8s" }, "unknown runtime"},
		{"两种探针", func(s *ResolvedSpec) {
			s.Health = &Health{TCP: &TCPProbe{Port: 1}, Exec: &ExecProbe{Command: []string{"x"}}}
		}, "exactly one probe"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := minimal()
			tc.mutate(s)
			err := Validate(s)
			if err == nil {
				t.Fatalf("应当校验失败")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息应包含 %q，实际: %v", tc.want, err)
			}
		})
	}
}

func TestValidateAcceptsMinimal(t *testing.T) {
	if err := Validate(minimal()); err != nil {
		t.Errorf("最小规格应当通过校验: %v", err)
	}
}

// TestResolveGeneration 钉住「唯一 late-bound 占位符」的替换。
func TestResolveGeneration(t *testing.T) {
	s := minimal()
	s.Paths["config"] = PathValue{
		Name:     "config",
		Values:   []string{"/etc/mecharion/apps/webapp"},
		Kind:     "single",
		LinkInto: GenerationPlaceholder + "/config",
	}
	s.Resources[0].Args = json.RawMessage(`{"blob":"main","dest":"` + GenerationPlaceholder + `"}`)
	s.Workload.Systemd.Exec = GenerationPlaceholder + "/bin/webapp"

	if !HasUnresolvedPlaceholder(s) {
		t.Fatal("替换前应当检测到占位符")
	}

	genDir := "/opt/mecharion/apps/webapp/generations/0007-1.2.0-1"
	out, err := ResolveGeneration(s, genDir)
	if err != nil {
		t.Fatal(err)
	}

	if HasUnresolvedPlaceholder(out) {
		t.Error("替换后不应残留占位符")
	}
	if got := out.Paths["config"].LinkInto; got != genDir+"/config" {
		t.Errorf("linkInto = %q", got)
	}
	if got := out.Workload.Systemd.Exec; got != genDir+"/bin/webapp" {
		t.Errorf("exec = %q", got)
	}
	if !strings.Contains(string(out.Resources[0].Args), genDir) {
		t.Errorf("资源 Args 未被替换: %s", out.Resources[0].Args)
	}

	// 原始规格不应被修改
	if !HasUnresolvedPlaceholder(s) {
		t.Error("ResolveGeneration 不应修改入参")
	}
}

func TestResolveGenerationEscapesSpecialChars(t *testing.T) {
	s := minimal()
	s.Workload.Systemd.Exec = GenerationPlaceholder + "/bin/x"

	// 路径里含需要 JSON 转义的字符
	genDir := `/opt/m7n/apps/a"b\c/generations/0001`
	out, err := ResolveGeneration(s, genDir)
	if err != nil {
		t.Fatalf("含特殊字符的路径应当被正确转义: %v", err)
	}
	if got := out.Workload.Systemd.Exec; got != genDir+"/bin/x" {
		t.Errorf("exec = %q, 期望 %q", got, genDir+"/bin/x")
	}
}

func TestResolveGenerationNoPlaceholder(t *testing.T) {
	s := minimal()
	out, err := ResolveGeneration(s, "/whatever")
	if err != nil {
		t.Fatal(err)
	}
	if out.Component != s.Component {
		t.Error("无占位符时应原样返回副本")
	}
}

func TestInstanceKey(t *testing.T) {
	s := minimal()
	if got := s.InstanceKey(); got != "webapp__default" {
		t.Errorf("InstanceKey = %q", got)
	}
}

// TestRunStateIsNotInDigest 钉住期望运行态不参与 digest。
//
// 若它参与，一次 `component stop` 会分配一个全新的 generation 目录、
// 解压同样的载荷、切一次软链，然后什么也不启动；`component start` 再来
// 一遍。回滚历史里于是堆满只差运行态的 generation，而 retainGenerations
// 会把真正有用的旧版本挤出去——**一次停机操作把回滚能力吃掉了**。
func TestRunStateIsNotInDigest(t *testing.T) {
	base := &ResolvedSpec{
		SchemaVersion: SchemaVersion,
		Component:     "web", Role: "default",
		Pack: PackRef{Name: "go-webapp", Version: "1.0.0"},
	}
	running, err := ComputeDigest(base)
	if err != nil {
		t.Fatal(err)
	}

	stopped := *base
	stopped.RunState = RunStateStopped
	got, err := ComputeDigest(&stopped)
	if err != nil {
		t.Fatal(err)
	}
	if got != running {
		t.Errorf("停一个服务不改变盘上任何字节，digest 不该变\n  running=%s\n  stopped=%s",
			running, got)
	}
}

// TestEffectiveRunStateDefaultsToRunning 钉住空串按 running 处理。
//
// 三态会让每个使用点都要写一遍「空串算什么」，而漏掉一处的后果是
// 「某条路径上组件永远不被拉起来」。
func TestEffectiveRunStateDefaultsToRunning(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"", true},
		{RunStateRunning, true},
		{RunStateStopped, false},
		// 认不出的值按 running 处理：一个因为拼错而永远不启动的服务，
		// 比一个多跑起来的服务难查得多
		{"garbage", true},
	} {
		s := &ResolvedSpec{RunState: tc.in}
		if got := s.WantsRunning(); got != tc.want {
			t.Errorf("RunState=%q 时 WantsRunning 应为 %v，实际 %v", tc.in, tc.want, got)
		}
	}
}

// TestEffectiveDriftPolicy 钉住「取更松的那个」，而不是「覆盖直接赢」。
//
// 「覆盖直接赢」看起来更直白，但它是错的：一个 Component 级的单值要同时
// 作用于几十条资源。Pack 作者把某个文件标成 ignore（应用自己会改写它），
// 一个 report 覆盖会把它拽回来报警——那不是运维想表达的意思，
// 而是单值粒度的副作用。
func TestEffectiveDriftPolicy(t *testing.T) {
	for _, tc := range []struct {
		declared, override, want string
		why                      string
	}{
		{DriftReconcile, DriftReport, DriftReport, "放松，生效"},
		{DriftReconcile, DriftIgnore, DriftIgnore, "放松到底"},
		{DriftIgnore, DriftReport, DriftIgnore, "已经更松，覆盖不该收紧它"},
		{DriftReport, DriftReport, DriftReport, "同档"},
		{DriftReconcile, "", DriftReconcile, "没有覆盖"},
		{"", "", DriftReport, "都没声明 → 规范默认 report"},
		{"", DriftIgnore, DriftIgnore, "默认 report 之上再放松"},
	} {
		if got := EffectiveDriftPolicy(tc.declared, tc.override); got != tc.want {
			t.Errorf("EffectiveDriftPolicy(%q, %q) = %q，期望 %q（%s）",
				tc.declared, tc.override, got, tc.want, tc.why)
		}
	}
}

// TestCheckDriftOverrideRejectsTightening 钉住收紧被拒。
//
// reconcile 最坏的后果是「运维只是想试个参数，服务却被改回去并重启了」，
// 而按下这个决定的人不在现场。
func TestCheckDriftOverrideRejectsTightening(t *testing.T) {
	for _, ok := range []string{"", DriftReport, DriftIgnore} {
		if err := CheckDriftOverride(ok); err != nil {
			t.Errorf("%q 应当被接受: %v", ok, err)
		}
	}
	err := CheckDriftOverride(DriftReconcile)
	if err == nil {
		t.Fatal("reconcile 作为覆盖只能是收紧，应当被拒绝")
	}
	if !strings.Contains(err.Error(), "relax") {
		t.Errorf("错误应说清「只能放松」，实际: %v", err)
	}
	if CheckDriftOverride("typo") == nil {
		t.Error("拼错的取值应当被拒绝")
	}
}

// TestDriftPolicyIsNotInDigest 钉住改策略不会切 generation。
//
// 若它参与 digest，站点侧放松策略（多半发生在**事故当中**，运维正想临时
// 改个值而不被改回去）会切一次 generation、把服务重启一遍。
// §4.3 花了整节说「自动改回不得顺带重启」，这会从另一条路径犯同一个错。
func TestDriftPolicyIsNotInDigest(t *testing.T) {
	mk := func(policy string) *ResolvedSpec {
		return &ResolvedSpec{
			SchemaVersion: SchemaVersion,
			Component:     "web", Role: "default",
			Resources: []Resource{
				{ID: "template:a", Type: "template", DriftPolicy: policy},
			},
		}
	}
	strict, err := ComputeDigest(mk(DriftReconcile))
	if err != nil {
		t.Fatal(err)
	}
	loose, err := ComputeDigest(mk(DriftReport))
	if err != nil {
		t.Fatal(err)
	}
	if strict != loose {
		t.Errorf("改漂移策略不改变盘上任何字节，digest 不该变\n  %s\n  %s", strict, loose)
	}
}

// TestComputeDigestDoesNotMutateInput 钉住算摘要没有副作用。
//
// ComputeDigest 为了排除若干字段会做浅拷贝，而 Resources 是切片——
// 浅拷贝之后清字段会**改到调用方那一份**。表现是「算了一次 digest 之后，
// 规格里的 driftPolicy 全没了」，而下发出去的正是那一份。
func TestComputeDigestDoesNotMutateInput(t *testing.T) {
	s := &ResolvedSpec{
		SchemaVersion: SchemaVersion,
		Component:     "web", Role: "default", RunState: RunStateStopped,
		Resources: []Resource{
			{ID: "template:a", Type: "template", DriftPolicy: DriftReconcile},
		},
	}
	if _, err := ComputeDigest(s); err != nil {
		t.Fatal(err)
	}
	if got := s.Resources[0].DriftPolicy; got != DriftReconcile {
		t.Errorf("算摘要不该改动输入，driftPolicy 变成了 %q", got)
	}
	if s.RunState != RunStateStopped {
		t.Errorf("算摘要不该改动输入，runState 变成了 %q", s.RunState)
	}
}
