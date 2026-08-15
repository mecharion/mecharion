package hook

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mecharion/mecharion/internal/command"
	"github.com/mecharion/mecharion/internal/spec"
)

// ── 夹具 ────────────────────────────────────────────────────────────────

// execTempDir 返回一个**可以执行文件**的临时目录。
//
// 不用 t.TempDir()：它落在 /tmp，而 systemd 的 tmp.mount 把 /tmp 挂成
// `nosuid,nodev,noexec` 的 tmpfs——从那儿 fork 一个脚本会得到
// 「permission denied」，而错误信息完全不提 noexec，极难联想。
//
// Mecharion 自己不受影响：Pack 一律解在 <data-dir>/packs 下（/var/lib），
// 不碰 /tmp。这纯粹是测试夹具的问题（spec §15.1 记着同一条环境事实）。
func execTempDir(t *testing.T) string {
	t.Helper()
	base := os.TempDir()
	if runtime.GOOS == "linux" {
		base = "/var/tmp"
	}
	dir, err := os.MkdirTemp(base, "m7nhook-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// packWith 造一个含若干 hook 脚本的 Pack 目录。
func packWith(t *testing.T, scripts map[string]string) string {
	t.Helper()
	dir := execTempDir(t)
	hooksDir := filepath.Join(dir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range scripts {
		p := filepath.Join(hooksDir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// specWith 造一份带 hook 与参数的规格。
func specWith(hooks []spec.Hook, params map[string]spec.ParamValue) *spec.ResolvedSpec {
	return &spec.ResolvedSpec{
		SchemaVersion: spec.SchemaVersion,
		Component:     "pg-main", Role: "primary", ConfigGroup: "default",
		Profile: "ha",
		Node:    spec.NodeRef{Name: "n1"},
		Ordinal: 2,
		Pack:    spec.PackRef{Name: "postgresql", Version: "16.4"},
		Site:    spec.SiteRef{Name: "site1"},
		Params:  params,
		Paths: map[string]spec.PathValue{
			"current":  {Name: "current", Values: []string{"/opt/x/current"}},
			"config":   {Name: "config", Values: []string{"/etc/x"}},
			"dataDirs": {Name: "dataDirs", Values: []string{"/data1/x", "/data2/x"}, Kind: "multi"},
		},
		Hooks: hooks,
	}
}

func envMap(t *testing.T, f *command.Fake) map[string]string {
	t.Helper()
	o, ok := f.LastOpts()
	if !ok {
		t.Fatal("没有记录到任何一次带设置的执行")
	}
	out := map[string]string{}
	for _, kv := range o.Env {
		if i := strings.Index(kv, "="); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}

// ── 环境注入 ────────────────────────────────────────────────────────────

func TestEnvCarriesIdentityAndPaths(t *testing.T) {
	dir := packWith(t, map[string]string{"h.sh": "#!/bin/sh\n"})
	genDir := t.TempDir()
	f := command.NewFake()
	e := &Executor{
		Runner: f, PackRoot: dir, RunDir: t.TempDir(),
		// 必须是真存在的目录：hook 以它为 cwd，执行器会先确认它可用
		GenerationDir: genDir,
	}
	s := specWith([]spec.Hook{{Point: PostInstall, Script: "hooks/h.sh"}}, nil)

	if _, err := e.Run(context.Background(), s, PostInstall); err != nil {
		t.Fatal(err)
	}
	env := envMap(t, f)

	for k, want := range map[string]string{
		"MECHARION_COMPONENT":     "pg-main",
		"MECHARION_ROLE":          "primary",
		"MECHARION_PROFILE":       "ha",
		"MECHARION_NODE":          "n1",
		"MECHARION_ORDINAL":       "2",
		"MECHARION_CONFIG_GROUP":  "default",
		"MECHARION_PACK":          "postgresql",
		"MECHARION_GENERATION":    genDir,
		"MECHARION_PATHS_CURRENT": "/opt/x/current",
		// 多值路径用 ':' 连接，shell 里 IFS=: 就能拆
		"MECHARION_PATHS_DATA_DIRS": "/data1/x:/data2/x",
	} {
		if env[k] != want {
			t.Errorf("%s 应为 %q，实际 %q", k, want, env[k])
		}
	}

	// 工作目录是 generation 目录
	o, _ := f.LastOpts()
	if o.Dir != genDir {
		t.Errorf("cwd 应为 generation 目录，实际 %q", o.Dir)
	}
}

// TestEnvIsReplacedNotInherited 钉住「完整替换」。
//
// 继承一份开发机上恰好存在的变量，会让「在我这儿能跑」变成常态。
func TestEnvIsReplacedNotInherited(t *testing.T) {
	t.Setenv("SOME_LOCAL_THING", "leaked")

	dir := packWith(t, map[string]string{"h.sh": "#!/bin/sh\n"})
	f := command.NewFake()
	e := &Executor{Runner: f, PackRoot: dir, RunDir: t.TempDir()}
	s := specWith([]spec.Hook{{Point: PreStart, Script: "hooks/h.sh"}}, nil)

	if _, err := e.Run(context.Background(), s, PreStart); err != nil {
		t.Fatal(err)
	}
	env := envMap(t, f)
	if _, leaked := env["SOME_LOCAL_THING"]; leaked {
		t.Error("hook 的环境不该继承调用进程的变量")
	}
	if env["PATH"] == "" {
		t.Error("必须显式给一个 PATH，否则脚本连 /bin/sh 都找不到")
	}
}

func TestEnvNameConversion(t *testing.T) {
	for in, want := range map[string]string{
		"admin_password": "ADMIN_PASSWORD",
		"dataDirs":       "DATA_DIRS",
		"current":        "CURRENT",
		"maxBodySize":    "MAX_BODY_SIZE",
		"port":           "PORT",
		"log-level":      "LOG_LEVEL",
		"jdbc_url":       "JDBC_URL",
	} {
		if got := envName(in); got != want {
			t.Errorf("envName(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// ── 密钥注入 ────────────────────────────────────────────────────────────

// TestSecretsGoToFilesNotEnv 钉住 16-secrets §6 的核心纪律。
//
// 环境变量会出现在 /proc/<pid>/environ 与崩溃转储里，且被子进程继承。
// 把口令交给一个任意脚本时，这是最容易漏掉的泄漏面。
func TestSecretsGoToFilesNotEnv(t *testing.T) {
	dir := packWith(t, map[string]string{"h.sh": "#!/bin/sh\n"})
	f := command.NewFake()
	runDir := t.TempDir()
	e := &Executor{Runner: f, PackRoot: dir, RunDir: runDir}

	s := specWith([]spec.Hook{{Point: PostStart, Script: "hooks/h.sh"}},
		map[string]spec.ParamValue{
			"port":           {Value: 5432, Type: "port"},
			"admin_password": {Sensitive: true, Type: "secret"},
		}).WithSecrets(map[string]string{"admin_password": "s3cr3t-value"})

	if _, err := e.Run(context.Background(), s, PostStart); err != nil {
		t.Fatal(err)
	}
	env := envMap(t, f)

	if _, bad := env["MECHARION_PARAM_ADMIN_PASSWORD"]; bad {
		t.Fatal("敏感参数不能进环境变量")
	}
	for k, v := range env {
		if strings.Contains(v, "s3cr3t-value") {
			t.Fatalf("口令明文出现在环境变量 %s 里", k)
		}
	}
	// 非敏感参数照常进环境变量
	if env["MECHARION_PARAM_PORT"] != "5432" {
		t.Errorf("普通参数应进环境变量，实际 %q", env["MECHARION_PARAM_PORT"])
	}
	// 敏感参数只给一个路径
	pw := env["MECHARION_PARAM_FILE_ADMIN_PASSWORD"]
	if pw == "" {
		t.Fatal("敏感参数应通过 MECHARION_PARAM_FILE_<NAME> 给出文件路径")
	}
}

// TestSecretFilesArePrivateAndRemoved 钉住权限与清理。
func TestSecretFilesArePrivateAndRemoved(t *testing.T) {
	dir := packWith(t, map[string]string{"h.sh": "#!/bin/sh\n"})
	runDir := t.TempDir()

	var capturedPath string
	f := command.NewFake()
	e := &Executor{Runner: f, PackRoot: dir, RunDir: runDir}
	s := specWith([]spec.Hook{{Point: PostStart, Script: "hooks/h.sh"}},
		map[string]spec.ParamValue{"admin_password": {Sensitive: true, Type: "secret"}}).
		WithSecrets(map[string]string{"admin_password": "s3cr3t-value"})

	if _, err := e.Run(context.Background(), s, PostStart); err != nil {
		t.Fatal(err)
	}
	env := envMap(t, f)
	capturedPath = env["MECHARION_PARAM_FILE_ADMIN_PASSWORD"]

	// hook 结束后整个目录必须没了
	if _, err := os.Stat(capturedPath); !os.IsNotExist(err) {
		t.Errorf("hook 结束后密钥文件应当已删除，实际 stat 得到 %v", err)
	}
	entries, _ := os.ReadDir(runDir)
	if len(entries) != 0 {
		t.Errorf("密钥目录应当整个删掉，实际还剩 %d 项", len(entries))
	}
}

// TestSecretFilesRemovedOnFailure 钉住「无论成败」。
func TestSecretFilesRemovedOnFailure(t *testing.T) {
	dir := packWith(t, map[string]string{"h.sh": "#!/bin/sh\n"})
	runDir := t.TempDir()

	f := command.NewFake()
	f.Default = command.Result{ExitCode: 1, Stderr: "boom"}
	e := &Executor{Runner: f, PackRoot: dir, RunDir: runDir}
	s := specWith([]spec.Hook{{Point: PostStart, Script: "hooks/h.sh"}},
		map[string]spec.ParamValue{"pw": {Sensitive: true, Type: "secret"}}).
		WithSecrets(map[string]string{"pw": "v"})

	if _, err := e.Run(context.Background(), s, PostStart); err == nil {
		t.Fatal("非零退出码应当算失败")
	}
	entries, _ := os.ReadDir(runDir)
	if len(entries) != 0 {
		t.Errorf("失败路径同样要清理密钥目录，实际还剩 %d 项", len(entries))
	}
}

// TestSecretFileModeIsPrivate 在真文件系统上验证权限位。
func TestSecretFileModeIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不表达 Unix 权限位")
	}
	runDir := t.TempDir()
	e := &Executor{RunDir: runDir}
	s := specWith(nil, nil).WithSecrets(map[string]string{"pw": "v"})

	dir, files, err := e.writeSecretFiles(s)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("密钥目录权限应为 0700，实际 %o", di.Mode().Perm())
	}
	fi, err := os.Stat(files["pw"])
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("密钥文件权限应为 0600，实际 %o", fi.Mode().Perm())
	}
	body, _ := os.ReadFile(files["pw"])
	if string(body) != "v" {
		t.Errorf("文件内容应是明文口令，实际 %q", body)
	}
}

// ── 脱敏 ────────────────────────────────────────────────────────────────

// TestOutputIsRedacted 钉住 hook 输出不把口令带进事件流。
//
// 一个 `set -x` 的脚本会把口令原样打出来，而那是比配置文件本身常见得多的
// 泄漏途径。
func TestOutputIsRedacted(t *testing.T) {
	dir := packWith(t, map[string]string{"h.sh": "#!/bin/sh\n"})
	f := command.NewFake()
	f.Default = command.Result{Stdout: "connecting with password=s3cr3t-value ok\n"}
	e := &Executor{Runner: f, PackRoot: dir, RunDir: t.TempDir()}
	s := specWith([]spec.Hook{{Point: PostStart, Script: "hooks/h.sh"}}, nil).
		WithSecrets(map[string]string{"pw": "s3cr3t-value"})

	res, err := e.Run(context.Background(), s, PostStart)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res[0].Output, "s3cr3t-value") {
		t.Fatalf("hook 输出里的口令必须脱敏，实际: %s", res[0].Output)
	}
	if !strings.Contains(res[0].Output, Redacted) {
		t.Errorf("脱敏后应留下 %s 标记，实际: %s", Redacted, res[0].Output)
	}
}

// TestFailureOutputIsRedactedToo 钉住失败路径也脱敏。
//
// 失败路径恰恰是最可能把口令打进日志的地方。
func TestFailureOutputIsRedactedToo(t *testing.T) {
	dir := packWith(t, map[string]string{"h.sh": "#!/bin/sh\n"})
	f := command.NewFake()
	f.Default = command.Result{ExitCode: 2, Stderr: "auth failed for password=s3cr3t-value"}
	e := &Executor{Runner: f, PackRoot: dir, RunDir: t.TempDir()}
	s := specWith([]spec.Hook{{Point: PostStart, Script: "hooks/h.sh"}}, nil).
		WithSecrets(map[string]string{"pw": "s3cr3t-value"})

	res, err := e.Run(context.Background(), s, PostStart)
	if err == nil {
		t.Fatal("应当失败")
	}
	if strings.Contains(err.Error(), "s3cr3t-value") {
		t.Fatalf("错误信息里的口令必须脱敏，实际: %v", err)
	}
	if len(res) != 1 || strings.Contains(res[0].Output, "s3cr3t-value") {
		t.Errorf("失败时的输出记录同样要脱敏: %+v", res)
	}
}

// TestLongerSecretsRedactedFirst 钉住替换顺序。
//
// 短值可能是长值的子串，先换短的会在长值里留下残片。
func TestLongerSecretsRedactedFirst(t *testing.T) {
	s := specWith(nil, nil).WithSecrets(map[string]string{
		"short": "abc",
		"long":  "abcdef123",
	})
	got := redactor(s)("value=abcdef123 other=abc")
	if strings.Contains(got, "def") {
		t.Errorf("长值应当被整体替换，不该留下残片: %s", got)
	}
}

// ── 执行语义 ────────────────────────────────────────────────────────────

func TestHooksRunInDeclaredOrder(t *testing.T) {
	dir := packWith(t, map[string]string{
		"a.sh": "#!/bin/sh\n", "b.sh": "#!/bin/sh\n", "c.sh": "#!/bin/sh\n",
	})
	f := command.NewFake()
	e := &Executor{Runner: f, PackRoot: dir, RunDir: t.TempDir()}
	s := specWith([]spec.Hook{
		{Point: PostStart, Script: "hooks/a.sh"},
		{Point: PreStart, Script: "hooks/x.sh"}, // 别的点，不该被跑
		{Point: PostStart, Script: "hooks/b.sh"},
		{Point: PostStart, Script: "hooks/c.sh"},
	}, nil)

	res, err := e.Run(context.Background(), s, PostStart)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range res {
		got = append(got, r.Script)
	}
	want := "hooks/a.sh,hooks/b.sh,hooks/c.sh"
	if strings.Join(got, ",") != want {
		t.Errorf("执行顺序应为 %s，实际 %s", want, strings.Join(got, ","))
	}
}

// TestFailureStopsSubsequentHooks 钉住失败即止。
//
// 后面的 hook 多半依赖前一个的结果。
func TestFailureStopsSubsequentHooks(t *testing.T) {
	dir := packWith(t, map[string]string{"a.sh": "#!/bin/sh\n", "b.sh": "#!/bin/sh\n"})
	f := command.NewFake()
	f.Default = command.Result{ExitCode: 1}
	e := &Executor{Runner: f, PackRoot: dir, RunDir: t.TempDir()}
	s := specWith([]spec.Hook{
		{Point: PostStart, Script: "hooks/a.sh"},
		{Point: PostStart, Script: "hooks/b.sh"},
	}, nil)

	res, err := e.Run(context.Background(), s, PostStart)
	if err == nil {
		t.Fatal("应当失败")
	}
	if len(res) != 1 {
		t.Errorf("第一个失败后不该再跑第二个，实际跑了 %d 个", len(res))
	}
}

// TestNoRetry 钉住「hook 不做重试」。
//
// 一个不幂等的 hook 重试一次就可能把事情做坏两遍，而引擎无从判断它是否幂等。
func TestNoRetry(t *testing.T) {
	dir := packWith(t, map[string]string{"a.sh": "#!/bin/sh\n"})
	f := command.NewFake()
	f.Default = command.Result{ExitCode: 1}
	e := &Executor{Runner: f, PackRoot: dir, RunDir: t.TempDir()}
	s := specWith([]spec.Hook{{Point: PostStart, Script: "hooks/a.sh"}}, nil)

	if _, err := e.Run(context.Background(), s, PostStart); err == nil {
		t.Fatal("应当失败")
	}
	if n := len(f.AllOpts()); n != 1 {
		t.Errorf("失败的 hook 只该跑一次，实际跑了 %d 次", n)
	}
}

func TestMissingScriptIsRefused(t *testing.T) {
	dir := packWith(t, nil)
	e := &Executor{Runner: command.NewFake(), PackRoot: dir, RunDir: t.TempDir()}
	s := specWith([]spec.Hook{{Point: PostStart, Script: "hooks/nope.sh"}}, nil)

	_, err := e.Run(context.Background(), s, PostStart)
	if err == nil {
		t.Fatal("脚本不存在应当报错而不是静默跳过")
	}
	if !strings.Contains(err.Error(), "nope.sh") {
		t.Errorf("错误信息应指名到脚本，实际: %v", err)
	}
}

func TestNoHooksAtPointIsNoop(t *testing.T) {
	dir := packWith(t, map[string]string{"a.sh": "#!/bin/sh\n"})
	f := command.NewFake()
	e := &Executor{Runner: f, PackRoot: dir, RunDir: t.TempDir()}
	s := specWith([]spec.Hook{{Point: PostStart, Script: "hooks/a.sh"}}, nil)

	res, err := e.Run(context.Background(), s, PreInstall)
	if err != nil || len(res) != 0 {
		t.Errorf("该点没有 hook 时应当什么都不做，实际 res=%v err=%v", res, err)
	}
	if len(f.AllOpts()) != 0 {
		t.Error("不该执行任何命令")
	}
}

func TestTimeoutParsing(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", DefaultTimeout, false},
		{"600s", 600 * time.Second, false},
		{"10m", 10 * time.Minute, false},
		{"abc", 0, true},
		{"-5s", 0, true},
		{"0s", 0, true},
	} {
		got, err := parseTimeout(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("timeout %q 应当被拒绝", tc.in)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("timeout %q → %v, %v，期望 %v", tc.in, got, err, tc.want)
		}
	}
}

// ── 真实执行 ────────────────────────────────────────────────────────────

// TestRealExecutionReadsSecretFile 用真的 /bin/sh 跑一遍。
//
// 替身能验证「传了什么」，验证不了「脚本真的读得到」——后者才是
// 密钥注入这条链路的意义所在。
func TestRealExecutionReadsSecretFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("需要 /bin/sh")
	}
	dir := packWith(t, map[string]string{
		"h.sh": "#!/bin/sh\nset -eu\n" +
			"echo \"component=${MECHARION_COMPONENT}\"\n" +
			"echo \"pw=$(cat \"${MECHARION_PARAM_FILE_ADMIN_PASSWORD}\")\"\n",
	})
	runDir := t.TempDir()
	e := &Executor{
		Runner: command.Exec{}, PackRoot: dir, RunDir: runDir,
		GenerationDir: dir,
	}
	s := specWith([]spec.Hook{{Point: PostStart, Script: "hooks/h.sh"}},
		map[string]spec.ParamValue{"admin_password": {Sensitive: true, Type: "secret"}}).
		WithSecrets(map[string]string{"admin_password": "real-secret-42"})

	res, err := e.Run(context.Background(), s, PostStart)
	if err != nil {
		t.Fatalf("执行 hook: %v", err)
	}
	if !strings.Contains(res[0].Output, "component=pg-main") {
		t.Errorf("脚本应读到身份变量，实际输出: %s", res[0].Output)
	}
	// 脚本确实读到了口令——但输出里已经脱敏
	if strings.Contains(res[0].Output, "real-secret-42") {
		t.Errorf("输出未脱敏: %s", res[0].Output)
	}
	if !strings.Contains(res[0].Output, "pw="+Redacted) {
		t.Errorf("脚本没能读到口令文件，实际输出: %s", res[0].Output)
	}
	if entries, _ := os.ReadDir(runDir); len(entries) != 0 {
		t.Errorf("密钥目录未清理，还剩 %d 项", len(entries))
	}
}

// TestRealTimeoutKills 用真进程验证超时。
func TestRealTimeoutKills(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("需要 /bin/sh")
	}
	dir := packWith(t, map[string]string{"slow.sh": "#!/bin/sh\nsleep 30\n"})
	e := &Executor{Runner: command.Exec{}, PackRoot: dir, RunDir: t.TempDir(), GenerationDir: dir}
	s := specWith([]spec.Hook{
		{Point: PreStart, Script: "hooks/slow.sh", Timeout: "300ms"},
	}, nil)

	started := time.Now()
	_, err := e.Run(context.Background(), s, PreStart)
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("超时应当算失败")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Errorf("错误信息应说明是超时，实际: %v", err)
	}
	// 提示要能指导用户，而不只是报告失败
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("错误信息应提示可以调大 timeout，实际: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("超时后应立即返回，实际耗时 %v", elapsed)
	}
}

// TestRealNonZeroExitCarriesOutput 钉住失败时输出进错误信息。
//
// 只说「退出码 1」而不带脚本说了什么，等于让人去机器上重跑一遍。
func TestRealNonZeroExitCarriesOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("需要 /bin/sh")
	}
	dir := packWith(t, map[string]string{
		"bad.sh": "#!/bin/sh\necho '数据目录已存在且非空' >&2\nexit 3\n",
	})
	e := &Executor{Runner: command.Exec{}, PackRoot: dir, RunDir: t.TempDir(), GenerationDir: dir}
	s := specWith([]spec.Hook{{Point: PreInstall, Script: "hooks/bad.sh"}}, nil)

	res, err := e.Run(context.Background(), s, PreInstall)
	if err == nil {
		t.Fatal("非零退出应当失败")
	}
	if !strings.Contains(err.Error(), "数据目录已存在且非空") {
		t.Errorf("错误信息应带上脚本的输出，实际: %v", err)
	}
	if len(res) != 1 || res[0].ExitCode != 3 {
		t.Errorf("应记录退出码 3，实际 %+v", res)
	}
}
