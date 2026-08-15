//go:build linux

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hookComponent 与 webapp 用的组件名分开，避免两组测试互相踩状态。
const (
	hookComponent = "hookapp"
	hookUnit      = "mecharion-hookapp-default.service"
	hookPort      = 18081
	hookDataDir   = "/var/lib/mecharion-e2e-hooks"
)

// TestHooksRunAcrossLifecycle 是第 7 步的验收。
//
// 它复刻 postgresql `bootstrap-roles.sh` 的**完整形状**——`scope: once`、
// 以非 root 身份运行、口令经 0600 文件传入、在服务起来之后连上去干活——
// 但不需要一个真的 PostgreSQL。验收标准要的是这条链路通，而不是那个数据库。
//
// 覆盖四件在替身上验不出来的事：
//
//	① 真 systemd 下 preStart / postStart 与启动的相对顺序
//	② hook 以非 root 身份运行时**读得到**那个 0600 的口令文件
//	③ hook 结束后 /run 下的密钥目录整个消失
//	④ 口令不出现在 unit 文件、环境变量与调和报告里
func TestHooksRunAcrossLifecycle(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cleanupHookApp(ctx, t)
	t.Cleanup(func() { cleanupHookApp(context.Background(), t) })

	home := filepath.Join(hookDataDir, "apps", hookComponent)
	confDir := filepath.Join("/etc/mecharion-e2e-hooks/apps", hookComponent)
	sum := installBlobIn(t, hookDataDir, buildTarball(t))
	stageHookScripts(t)

	// 证据文件落在一个 hook 与测试都看得见的地方
	evidence := evidenceDir(t)

	s := hookSpec(home, confDir, sum, evidence)
	out, err := runMechlet(ctx, "apply", "-f", writeSpec(t, s), "--data-dir", hookDataDir)
	if err != nil {
		t.Logf("hook 侧看到的身份:\n%s\n路径权限:\n%s",
			tryRead(evidence, "id"), tryRead(evidence, "diag"))
		t.Fatalf("apply 失败: %v\n%s", err, out)
	}

	// ① 顺序：preStart 在服务起来之前，postStart 在之后
	order := readEvidence(t, evidence, "order")
	if !strings.Contains(order, "preStart") || !strings.Contains(order, "postStart") {
		t.Fatalf("两个 hook 都应当执行过，实际记录:\n%s", order)
	}
	if idx(order, "preStart") > idx(order, "postStart") {
		t.Errorf("preStart 必须排在 postStart 之前，实际:\n%s", order)
	}
	// 必须精确比较：`inactive` 里也含 "active"，用 Contains 会让这条
	// 断言在两个方向上都恒真
	if got := strings.TrimSpace(readEvidence(t, evidence, "postStart-service-state")); got != "active" {
		t.Errorf("postStart 执行时服务应当已经在运行，实际状态: %q", got)
	}
	// preStart 时则还没起来
	if got := strings.TrimSpace(readEvidence(t, evidence, "preStart-service-state")); got == "active" {
		t.Errorf("preStart 应当排在启动之前，实际那时服务已是 %q", got)
	}

	// ② 非 root 身份下读得到 0600 的口令文件
	if got := readEvidence(t, evidence, "whoami"); strings.TrimSpace(got) != "hookuser" {
		t.Errorf("hook 应以 hookuser 身份运行，实际 %q", strings.TrimSpace(got))
	}
	if got := strings.TrimSpace(readEvidence(t, evidence, "secret")); got != "top-secret-42" {
		t.Errorf("hook 应当读到口令明文，实际 %q", got)
	}
	// 权限位必须是 0600，不是靠放宽权限换来的可读
	if got := strings.TrimSpace(readEvidence(t, evidence, "secret-mode")); got != "600" {
		t.Errorf("口令文件权限应为 600，实际 %q", got)
	}

	// ③ hook 结束后密钥目录整个消失
	leftovers, _ := filepath.Glob("/run/mecharion/hooks/*")
	if len(leftovers) != 0 {
		t.Errorf("hook 结束后 /run/mecharion/hooks 下不该有残留，实际: %v", leftovers)
	}

	// ④ 口令不出现在任何持久化的地方
	assertNoSecretLeak(ctx, t, "top-secret-42", out)
}

// TestOnceHookNotResentIsNotMechletsJob 钉住职责切分。
//
// mechlet **完全不理解 once 语义**：规格里出现的 hook 它就执行。
// 「整个 Component 只跑一次」是个跨节点概念，仲裁在 mechd——放在唯一
// 有全局视角的地方，mechlet 才永远不需要相互查询。
//
// 因此重复 apply 同一份规格时，hook 会**再跑一次**。这不是缺陷，
// 是这一层该有的行为；真正的去重发生在下发侧。
func TestOnceHookNotResentIsNotMechletsJob(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cleanupHookApp(ctx, t)
	t.Cleanup(func() { cleanupHookApp(context.Background(), t) })

	home := filepath.Join(hookDataDir, "apps", hookComponent)
	confDir := filepath.Join("/etc/mecharion-e2e-hooks/apps", hookComponent)
	sum := installBlobIn(t, hookDataDir, buildTarball(t))
	stageHookScripts(t)

	evidence := evidenceDir(t)
	specPath := writeSpec(t, hookSpec(home, confDir, sum, evidence))

	for i := 1; i <= 2; i++ {
		if out, err := runMechlet(ctx, "apply", "-f", specPath, "--data-dir", hookDataDir); err != nil {
			t.Fatalf("第 %d 次 apply 失败: %v\n%s", i, err, out)
		}
	}

	// 第二次 apply 时 digest 没变、服务还在跑，因此不会再走一次 start，
	// preStart / postStart 也就不会重跑——**这是「没有启动动作」的结果，
	// 不是 mechlet 认得 once**
	n := strings.Count(readEvidence(t, evidence, "order"), "postStart")
	if n != 1 {
		t.Errorf("服务已在运行且 digest 未变时不该重启，因此 postStart 只该跑 1 次，实际 %d 次", n)
	}
}

// TestPreStartFailureAbortsAndLeavesServiceDown 钉住失败处理。
func TestPreStartFailureAbortsAndLeavesServiceDown(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cleanupHookApp(ctx, t)
	t.Cleanup(func() { cleanupHookApp(context.Background(), t) })

	home := filepath.Join(hookDataDir, "apps", hookComponent)
	confDir := filepath.Join("/etc/mecharion-e2e-hooks/apps", hookComponent)
	sum := installBlobIn(t, hookDataDir, buildTarball(t))
	stageHookScripts(t)

	evidence := evidenceDir(t)
	s := hookSpec(home, confDir, sum, evidence)
	// 让 preStart 失败
	s["hooks"] = []map[string]any{{
		"point": "preStart", "script": "hooks/fail.sh", "timeout": "30s",
	}}

	out, err := runMechlet(ctx, "apply", "-f", writeSpec(t, s), "--data-dir", hookDataDir)
	if err == nil {
		t.Fatalf("preStart 失败时整轮调和应当失败\n%s", out)
	}
	// 错误信息必须带上脚本说了什么，否则只能上机器重跑一遍
	if !strings.Contains(out, "前置检查未通过") {
		t.Errorf("失败输出应带上 hook 的 stderr，实际:\n%s", out)
	}
	if isActive(ctx, hookUnit) {
		t.Error("preStart 失败后不该把服务拉起来")
	}
}

// ── 夹具 ────────────────────────────────────────────────────────────────

// stageHookScripts 把 hook 脚本放进 mechlet 找得到的 Pack 目录。
func stageHookScripts(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(hookDataDir, "packs", "go-webapp", "hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// probe.sh 复刻 bootstrap-roles.sh 的形状：非 root 身份、口令走文件、
	// 需要服务已经在跑。它把观察到的一切写进证据目录供断言。
	probe := `#!/bin/sh
set -eu
EV="${MECHARION_PARAM_EVIDENCE_DIR}"
PHASE="$1"

echo "${PHASE}" >> "${EV}/order"
whoami > "${EV}/whoami"

# 服务此刻在不在跑？postgresql 的 bootstrap 正是靠这个前提才能连库
systemctl is-active ` + hookUnit + ` > "${EV}/${PHASE}-service-state" 2>&1 || true

# 口令经 0600 文件传入，不经环境变量也不经命令行
PWFILE="${MECHARION_PARAM_FILE_ADMIN_PASSWORD}"
# 出问题时最想知道的两件事：以谁的身份跑的、路径上哪一层拦住了
id > "${EV}/id" 2>&1 || true
ls -ldn /run /run/mecharion /run/mecharion/hooks "${PWFILE}" > "${EV}/diag" 2>&1 || true
cat "${PWFILE}" > "${EV}/secret"
stat -c '%a' "${PWFILE}" > "${EV}/secret-mode"

# 敏感值绝不该同时出现在环境里
if env | grep -q "top-secret-42"; then
    echo "口令泄漏进了环境变量" >&2
    exit 1
fi
`
	writeExec(t, filepath.Join(dir, "probe.sh"), probe)
	writeExec(t, filepath.Join(dir, "fail.sh"),
		"#!/bin/sh\necho '前置检查未通过：数据目录已存在且非空' >&2\nexit 3\n")
	return dir
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// hookSpec 造一份带 preStart / postStart hook 的规格。
func hookSpec(home, confDir, blobSum, evidence string) map[string]any {
	s := specOf(home, confDir, blobSum, "info", 1)
	s["component"] = hookComponent
	s["params"] = map[string]any{
		"port": map[string]any{"value": hookPort, "type": "port"},
		// evidence_dir 是测试专用的普通参数，走环境变量
		"evidence_dir": map[string]any{"value": evidence, "type": "path"},
		// admin_password 是敏感的：走 0600 文件，Value 里带明文是
		// `mechlet apply -f` 这条调试路径的约定（16-secrets §4）
		"admin_password": map[string]any{
			"value": "top-secret-42", "type": "secret", "sensitive": true,
		},
	}
	// 换掉端口与 unit 名对应的 exec / health
	s["workload"] = map[string]any{
		"runtime": "systemd",
		"systemd": map[string]any{
			"exec":       home + "/current/bin/webapp --config " + confDir + "/app.yaml",
			"execReload": "/bin/sh -c 'kill -HUP $MAINPID'",
			"restart":    "on-failure",
			"restartSec": "1s",
		},
	}
	s["health"] = map[string]any{
		"http":         map[string]any{"path": "/healthz", "port": hookPort},
		"startupGrace": "20s",
	}
	s["resources"] = []map[string]any{
		{
			"id": "archive:main", "type": "archive", "origin": "role",
			"args": map[string]any{
				"blob": "main", "dest": "{{ .Paths.Generation }}", "strip": 1,
			},
			"driftPolicy": "report",
		},
		{
			"id": "template:app.yaml", "type": "template", "origin": "role",
			"args": map[string]any{
				"dest": confDir + "/app.yaml",
				"content": fmt.Sprintf("# 由 Mecharion 渲染\nport: %d\nlog_level: info\n",
					hookPort),
				"mode": "0644",
			},
			"driftPolicy": "report",
		},
		// hook 以 hookuser 身份运行，因此这个用户必须先存在
		{
			"id": "user:hookuser", "type": "user", "origin": "role",
			"args": map[string]any{"name": "hookuser", "system": true},
		},
	}
	s["hooks"] = []map[string]any{
		{
			"point": "preStart", "script": "hooks/probe.sh",
			"args": []string{"preStart"}, "user": "hookuser", "timeout": "60s",
		},
		{
			"point": "postStart", "script": "hooks/probe.sh",
			"args": []string{"postStart"}, "user": "hookuser", "timeout": "60s",
		},
	}
	return s
}

func readEvidence(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("读取证据 %s: %v", name, err)
	}
	return string(b)
}

func idx(haystack, needle string) int { return strings.Index(haystack, needle) }

func isActive(ctx context.Context, unit string) bool {
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", unit).CombinedOutput()
	return strings.TrimSpace(string(out)) == "active"
}

// assertNoSecretLeak 确认口令没有落在任何持久化的地方。
func assertNoSecretLeak(ctx context.Context, t *testing.T, secret, applyOutput string) {
	t.Helper()

	// unit 文件是 0644 全体可读
	unitPath := "/etc/systemd/system/" + hookUnit
	if b, err := os.ReadFile(unitPath); err == nil && strings.Contains(string(b), secret) {
		t.Errorf("口令出现在 unit 文件 %s 里——它是 0644 全体可读的", unitPath)
	}
	// systemctl show 会原样打印 Environment=
	out, _ := exec.CommandContext(ctx, "systemctl", "show", hookUnit, "-p", "Environment").
		CombinedOutput()
	if strings.Contains(string(out), secret) {
		t.Errorf("口令出现在 systemctl show 的输出里: %s", out)
	}
	// 调和报告会进事件流
	if strings.Contains(applyOutput, secret) {
		t.Errorf("口令出现在调和输出里:\n%s", applyOutput)
	}
	// 本地状态文件
	statePath := filepath.Join(hookDataDir, "mechlet")
	_ = filepath.Walk(statePath, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if b, rerr := os.ReadFile(p); rerr == nil && strings.Contains(string(b), secret) {
			t.Errorf("口令出现在本地状态文件 %s 里", p)
		}
		return nil
	})
}

func cleanupHookApp(ctx context.Context, t *testing.T) {
	t.Helper()
	_ = exec.CommandContext(ctx, "systemctl", "stop", hookUnit).Run()
	_ = exec.CommandContext(ctx, "systemctl", "disable", hookUnit).Run()
	_ = os.Remove("/etc/systemd/system/" + hookUnit)
	_ = exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()
	_ = os.RemoveAll(hookDataDir)
	_ = os.RemoveAll("/etc/mecharion-e2e-hooks")
	_ = os.RemoveAll("/run/mecharion/hooks")
}

// installBlobIn 把载荷放进指定数据目录的内容寻址位置。
func installBlobIn(t *testing.T, root string, body []byte) string {
	t.Helper()
	sum := sha256.Sum256(body)
	hexSum := hex.EncodeToString(sum[:])

	dir := filepath.Join(root, "blobs", "sha256", hexSum[:2])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, hexSum), body, 0o644); err != nil {
		t.Fatal(err)
	}
	return hexSum
}

// evidenceDir 返回一个 hook（以非 root 身份运行）也写得进去的目录。
//
// 不用 t.TempDir()：它落在 /tmp 下、且整条父路径是 0700 root——
// 以 hookuser 身份跑的脚本连穿都穿不过去。这与容器里 /tmp 被挂成
// noexec 是同一类环境事实，只是症状不同。
func evidenceDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/var/tmp", "m7nev-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func tryRead(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "(" + err.Error() + ")"
	}
	return string(b)
}
