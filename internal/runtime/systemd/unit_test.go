package systemd

import (
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/spec"
)

func webapp(mut ...func(*spec.SystemdWorkload)) runtime.WorkloadSpec {
	sd := &spec.SystemdWorkload{
		Exec: "/opt/mecharion/apps/webapp/current/bin/webapp " +
			"--config /etc/mecharion/apps/webapp/app.yaml",
		User: "webapp", Group: "webapp",
	}
	for _, m := range mut {
		m(sd)
	}
	return runtime.WorkloadSpec{
		Site: "s1", Component: "webapp", Role: "default", ConfigGroup: "default",
		Generation: 7, Home: "/opt/mecharion/apps/webapp",
		GenerationDir: "/opt/mecharion/apps/webapp/generations/0007-1.2.0-1",
		Workload:      &spec.Workload{Runtime: "systemd", Systemd: sd},
	}
}

func render(t *testing.T, w runtime.WorkloadSpec) string {
	t.Helper()
	s, err := renderUnit(w)
	if err != nil {
		t.Fatalf("渲染 unit 失败: %v", err)
	}
	return s
}

func TestUnitName(t *testing.T) {
	if got := UnitName("pg-main", "primary"); got != "mecharion-pg-main-primary.service" {
		t.Errorf("UnitName = %q", got)
	}
}

func TestRenderUnitBasics(t *testing.T) {
	got := render(t, webapp())

	for _, want := range []string{
		"[Unit]", "[Service]", "[Install]",
		"Description=Mecharion webapp/default",
		"ExecStart=/opt/mecharion/apps/webapp/current/bin/webapp",
		"User=webapp",
		"Group=webapp",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit 应包含 %q:\n%s", want, got)
		}
	}

	// 头部注释是排障入口：这个 unit 从哪来、属于哪个 generation
	if !strings.Contains(got, "component=webapp role=default") ||
		!strings.Contains(got, "generation=0007") {
		t.Errorf("头部注释应标明来源与 generation:\n%s", got)
	}
	if !strings.Contains(got, "请勿手工编辑") {
		t.Error("应当警告手工编辑会被调和覆盖")
	}
}

// TestRenderUnitWaitsForNetworkOnline 钉住用 network-online 而非 network。
//
// 绑定具体地址的服务在 network.target 时地址还没配好，会以
// 「Cannot assign requested address」启动失败——而且是间歇性的。
func TestRenderUnitWaitsForNetworkOnline(t *testing.T) {
	got := render(t, webapp())
	if !strings.Contains(got, "After=network-online.target") {
		t.Error("应当 After=network-online.target")
	}
	if !strings.Contains(got, "Wants=network-online.target") {
		t.Error("只 After 不 Wants 的话 network-online.target 根本不会被拉起")
	}
}

// TestRenderUnitRestartDefault 钉住未声明 restart 时的缺省。
func TestRenderUnitRestartDefault(t *testing.T) {
	got := render(t, webapp())
	if !strings.Contains(got, "Restart=on-failure") {
		t.Errorf("未声明时应缺省 on-failure（而非 always——"+
			"always 会让一个正常退出的一次性任务被反复拉起）:\n%s", got)
	}

	got = render(t, webapp(func(s *spec.SystemdWorkload) { s.Restart = "always" }))
	if !strings.Contains(got, "Restart=always") {
		t.Error("声明了就该用声明的值")
	}
}

func TestRenderUnitOptionalFields(t *testing.T) {
	got := render(t, webapp(func(s *spec.SystemdWorkload) {
		s.ExecReload = "/bin/kill -HUP $MAINPID"
		s.WorkingDir = "/opt/mecharion/apps/webapp/current"
		s.EnvFile = "/etc/mecharion/apps/webapp/env"
		s.RestartSec = "5s"
		s.LimitNofile = 65536
		s.KillMode = "mixed"
		s.TimeoutStop = "90s"
		s.ExtraUnit = "OOMScoreAdjust=-500"
	}))

	for _, want := range []string{
		"ExecReload=/bin/kill -HUP $MAINPID",
		"WorkingDirectory=/opt/mecharion/apps/webapp/current",
		"RestartSec=5s",
		"LimitNOFILE=65536",
		"KillMode=mixed",
		"TimeoutStopSec=90s",
		"OOMScoreAdjust=-500",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit 应包含 %q:\n%s", want, got)
		}
	}

	// EnvironmentFile 必须带 `-` 前缀
	if !strings.Contains(got, "EnvironmentFile=-/etc/mecharion/apps/webapp/env") {
		t.Errorf("EnvironmentFile 应带 `-` 前缀——它常由 template 资源生成，"+
			"unit 可能先被读到:\n%s", got)
	}
}

func TestRenderUnitOmitsUnsetFields(t *testing.T) {
	got := render(t, webapp(func(s *spec.SystemdWorkload) {
		s.User, s.Group = "", ""
	}))
	for _, absent := range []string{
		"User", "Group", "WorkingDirectory", "EnvironmentFile",
		"LimitNOFILE", "KillMode", "TimeoutStopSec", "ExecReload",
	} {
		if hasDirective(got, absent) {
			t.Errorf("未声明的字段不该出现: %s=\n%s", absent, got)
		}
	}
}

// hasDirective 报告 unit 中是否有一条 `key=…` 指令。
//
// 用整行前缀而非子串：`configGroup=default` 出现在头部注释里，
// 子串匹配 "Group=" 会命中它。
func hasDirective(unit, key string) bool {
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), key+"=") {
			return true
		}
	}
	return false
}

// TestRenderUnitEnvironmentQuoting 钉住带空格的环境变量被正确引用。
//
// systemd 按 shell 风格拆分 Environment= 这一行。漏掉引号会让一个带空格
// 的 JAVA_OPTS 被拆成多个环境变量——症状是「参数莫名其妙丢了一半」，
// 而且服务照常启动，问题要到很久以后才暴露。
func TestRenderUnitEnvironmentQuoting(t *testing.T) {
	got := render(t, webapp(func(s *spec.SystemdWorkload) {
		s.Env = map[string]string{
			"SIMPLE":    "value",
			"JAVA_OPTS": "-Xms2g -Xmx2g -XX:+UseG1GC",
			"QUOTED":    `say "hi"`,
			"BACKSLASH": `C:\path`,
			"EMPTY":     "",
		}
	}))

	for _, want := range []string{
		`Environment=SIMPLE=value`,
		`Environment=JAVA_OPTS="-Xms2g -Xmx2g -XX:+UseG1GC"`,
		`Environment=QUOTED="say \"hi\""`,
		`Environment=BACKSLASH="C:\\path"`,
		`Environment=EMPTY=""`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("应包含 %s\n实际:\n%s", want, got)
		}
	}
}

// TestRenderUnitEnvironmentIsSorted 钉住环境变量顺序稳定。
//
// map 遍历顺序随机，不排序会让每次调和都「检测到 unit 变了」，
// 进而无谓地 daemon-reload——每 60 秒一次。
func TestRenderUnitEnvironmentIsSorted(t *testing.T) {
	w := webapp(func(s *spec.SystemdWorkload) {
		s.Env = map[string]string{"Z": "1", "A": "2", "M": "3", "B": "4", "Y": "5"}
	})
	first := render(t, w)
	for i := 0; i < 20; i++ {
		if got := render(t, w); got != first {
			t.Fatalf("同一份规格渲染出的 unit 必须逐字节一致\n第一次:\n%s\n第 %d 次:\n%s",
				first, i, got)
		}
	}
	ia := strings.Index(first, "Environment=A=")
	iz := strings.Index(first, "Environment=Z=")
	if ia < 0 || iz < 0 || ia > iz {
		t.Errorf("环境变量应按键名排序:\n%s", first)
	}
}

// TestRenderUnitRejectsRelativeExec 钉住相对路径被提前拦下。
//
// systemd 不经过 shell。写成相对路径时它要到 start 那一刻才报
// 「Failed at step EXEC」，完全看不出是路径问题。
func TestRenderUnitRejectsRelativeExec(t *testing.T) {
	cases := []struct {
		name string
		exec string
		want string
	}{
		{"相对路径", "bin/webapp --port 8080", "绝对路径"},
		{"裸命令名", "webapp", "绝对路径"},
		{"PATH 里的命令", "java -jar app.jar", "绝对路径"},
		{"空", "   ", "为空"},
		{"引号没闭合", `"/opt/my app/bin/x --flag`, "闭合"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := renderUnit(webapp(func(s *spec.SystemdWorkload) { s.Exec = tc.exec }))
			if err == nil {
				t.Fatal("应当被拒绝")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息应包含 %q，实际: %v", tc.want, err)
			}
			if faults.ClassOf(err) != faults.Permanent {
				t.Errorf("写错路径是配置问题，应归为 permanent，实际 %s", faults.ClassOf(err))
			}
		})
	}
}

// TestRenderUnitAcceptsQuotedAndPrefixedExec 钉住合法但少见的写法不被误伤。
func TestRenderUnitAcceptsQuotedAndPrefixedExec(t *testing.T) {
	for _, exec := range []string{
		`"/opt/mecharion/apps/my app/bin/x" --flag`, // 路径含空格
		`-/opt/mecharion/bin/x`,                     // `-` = 忽略失败
		`+/opt/mecharion/bin/x`,                     // `+` = 全权限运行
		`@/opt/mecharion/bin/x argv0`,               // `@` = 自定义 argv[0]
	} {
		if _, err := renderUnit(webapp(func(s *spec.SystemdWorkload) {
			s.Exec = exec
		})); err != nil {
			t.Errorf("exec %q 是合法的 systemd 写法，不该被拒: %v", exec, err)
		}
	}
}

// TestRenderUnitRejectsUnresolvedPlaceholder 钉住未替换的 generation 占位符。
//
// 让它写进 unit 会得到一个字面量为 "{{ .Paths.Generation }}" 的路径，
// 服务起不来，而错误信息里只有一个看不懂的路径。
func TestRenderUnitRejectsUnresolvedPlaceholder(t *testing.T) {
	_, err := renderUnit(webapp(func(s *spec.SystemdWorkload) {
		s.Exec = spec.GenerationPlaceholder + "/bin/webapp"
	}))
	if err == nil {
		t.Fatal("残留占位符必须被拒绝")
	}
	if !strings.Contains(err.Error(), "ResolveGeneration") {
		t.Errorf("错误信息应指出该先调用 ResolveGeneration，实际: %v", err)
	}
}

func TestRenderUnitRequiresSystemdSection(t *testing.T) {
	w := webapp()
	w.Workload.Systemd = nil
	if _, err := renderUnit(w); err == nil {
		t.Fatal("缺 systemd 段应当报错")
	}
}
