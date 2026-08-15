//go:build linux

// Package e2e 是 M2 的验收测试：`mechlet apply -f <已解析规格>` 在
// systemd 容器里把 go-webapp 跑起来（docs/design/25-roadmap.md）。
//
// 它驱动的是**真正的 mechlet 二进制**，不是内部包——验收标准写的是那条
// 命令，测内部函数就等于换了个更容易通过的题目。
//
// 运行方式（在仓库根）：
//
//	make e2e
//
// 它交叉编译 mechlet、夹具应用与本测试，再在 test/node 容器里执行。
package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	// binDir 是挂载进容器的二进制目录，与真实安装布局一致
	// （docs/design/04-paths-and-storage.md）。
	//
	// **测试驱动 mechlet 时刻意用裸命令名**，走 /usr/bin 下的软链——
	// 那是「零 PATH 配置就能用」这条设计承诺的唯一验证方式。
	binDir = "/usr/local/lib/mecharion/current/bin"
	// dataDir 是本测试使用的数据目录，与真实安装隔离。
	dataDir = "/var/lib/mecharion-e2e"

	component = "webapp"
	role      = "default"
	unitName  = "mecharion-webapp-default.service"
	port      = 18080
)

func requireEnv(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("需要 root；在 test/node 容器里跑（make e2e）")
	}
	out, err := exec.Command("systemctl", "is-system-running").CombinedOutput()
	if err != nil && !strings.Contains(string(out), "degraded") {
		t.Skipf("本机没有作为 init 运行的 systemd: %s", bytes.TrimSpace(out))
	}
	if _, err := os.Stat(filepath.Join(binDir, "mechlet")); err != nil {
		t.Skipf("找不到 mechlet 二进制: %v", err)
	}
}

// TestApplyRunsWebapp 是 M2 的验收：一条命令把组件跑起来。
func TestApplyRunsWebapp(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	cleanup(t)
	t.Cleanup(func() { cleanup(t) })

	home := "/opt/mecharion-e2e/apps/" + component
	confDir := "/etc/mecharion-e2e/apps/" + component
	blobSum := installBlob(t, buildTarball(t))

	specPath := writeSpec(t, specOf(home, confDir, blobSum, "info", 1))

	out, err := runMechlet(ctx, "apply", "-f", specPath, "--data-dir", dataDir)
	t.Logf("mechlet apply 输出:\n%s", out)
	if err != nil {
		t.Fatalf("apply 失败: %v", err)
	}

	// ① 服务真的在跑
	waitUnitActive(t, 20*time.Second)

	// ② 端点真的能访问 —— 这才是「跑起来了」的定义
	body := waitHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", port), 20*time.Second)
	if strings.TrimSpace(body) != "ok" {
		t.Errorf("/healthz 返回 %q", body)
	}
	if root := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/", port)); !strings.Contains(root, "logLevel=info") {
		t.Errorf("根路径返回 %q", root)
	}

	// ③ 磁盘布局符合设计
	genDir := filepath.Join(home, "generations", "0001-1.2.0-1")
	if _, err := os.Stat(filepath.Join(genDir, "bin", "webapp")); err != nil {
		t.Errorf("载荷应当解在 generation 目录里: %v", err)
	}
	cur, err := os.Readlink(filepath.Join(home, "current"))
	if err != nil || cur != genDir {
		t.Errorf("current → %q（err=%v），期望 %q", cur, err, genDir)
	}
	// 配置在 generation 之外——这是唯一的不变式
	if _, err := os.Stat(filepath.Join(confDir, "app.yaml")); err != nil {
		t.Errorf("配置应当落在 generation 之外: %v", err)
	}

	// ④ 本地状态写下了台账
	in := loadInstance(t)
	if in.CurrentGeneration != 1 {
		t.Errorf("currentGeneration = %d", in.CurrentGeneration)
	}
	if len(in.Generations) != 1 || in.Generations[0].State != "active" {
		t.Errorf("台账 = %+v", in.Generations)
	}
}

// TestApplyIsIdempotent 钉住重复 apply 不惊动进程。
//
// 调和每 60 秒跑一次，`apply` 与它走同一条代码路径。这条不成立的话，
// 服务会被无休止地重启。
func TestApplyIsIdempotent(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	cleanup(t)
	t.Cleanup(func() { cleanup(t) })

	home := "/opt/mecharion-e2e/apps/" + component
	confDir := "/etc/mecharion-e2e/apps/" + component
	blobSum := installBlob(t, buildTarball(t))
	specPath := writeSpec(t, specOf(home, confDir, blobSum, "info", 1))

	if out, err := runMechlet(ctx, "apply", "-f", specPath, "--data-dir", dataDir); err != nil {
		t.Fatalf("首次 apply 失败: %v\n%s", err, out)
	}
	waitUnitActive(t, 20*time.Second)
	pid1 := mainPID(t)

	for i := 0; i < 3; i++ {
		out, err := runMechlet(ctx, "apply", "-f", specPath, "--data-dir", dataDir)
		if err != nil {
			t.Fatalf("第 %d 次 apply 失败: %v\n%s", i+2, err, out)
		}
		if !strings.Contains(out, "ok") {
			t.Errorf("第 %d 次应当报 ok，实际:\n%s", i+2, out)
		}
	}

	if pid2 := mainPID(t); pid2 != pid1 {
		t.Errorf("重复 apply 不该重启进程：MainPID %s → %s", pid1, pid2)
	}
}

// TestConfigChangeSwitchesGeneration 钉住配置变更走完整的 generation 切换。
func TestConfigChangeSwitchesGeneration(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	cleanup(t)
	t.Cleanup(func() { cleanup(t) })

	home := "/opt/mecharion-e2e/apps/" + component
	confDir := "/etc/mecharion-e2e/apps/" + component
	blobSum := installBlob(t, buildTarball(t))

	p1 := writeSpec(t, specOf(home, confDir, blobSum, "info", 1))
	if out, err := runMechlet(ctx, "apply", "-f", p1, "--data-dir", dataDir); err != nil {
		t.Fatalf("首次 apply: %v\n%s", err, out)
	}
	waitUnitActive(t, 20*time.Second)
	waitHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", port), 20*time.Second)

	// 改配置 → 新 generation
	p2 := writeSpec(t, specOf(home, confDir, blobSum, "debug", 1))
	out, err := runMechlet(ctx, "apply", "-f", p2, "--data-dir", dataDir)
	t.Logf("第二次 apply:\n%s", out)
	if err != nil {
		t.Fatalf("第二次 apply: %v", err)
	}

	genDir2 := filepath.Join(home, "generations", "0002-1.2.0-1")
	cur, err := os.Readlink(filepath.Join(home, "current"))
	if err != nil || cur != genDir2 {
		t.Errorf("current → %q，期望 %q", cur, genDir2)
	}
	// 旧 generation 完整保留——回滚靠它还在
	if _, err := os.Stat(filepath.Join(home, "generations", "0001-1.2.0-1", "bin", "webapp")); err != nil {
		t.Errorf("旧 generation 必须保留: %v", err)
	}

	// 新配置生效
	waitUnitActive(t, 20*time.Second)
	body := waitHTTPContains(t, fmt.Sprintf("http://127.0.0.1:%d/", port), "logLevel=debug", 20*time.Second)
	if !strings.Contains(body, "logLevel=debug") {
		t.Errorf("新配置未生效: %q", body)
	}

	// 回滚 = 重新下发上一份规格
	out, err = runMechlet(ctx, "apply", "-f", p1, "--data-dir", dataDir)
	t.Logf("回滚:\n%s", out)
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if !strings.Contains(out, "rollback") {
		t.Errorf("报告应当标明这是一次回滚:\n%s", out)
	}
	cur, _ = os.Readlink(filepath.Join(home, "current"))
	if !strings.HasSuffix(cur, "0001-1.2.0-1") {
		t.Errorf("回滚后 current → %q", cur)
	}
	waitHTTPContains(t, fmt.Sprintf("http://127.0.0.1:%d/", port), "logLevel=info", 20*time.Second)
}

// TestDriftReloadKeepsProcessAlive 走通 notify → reload 的完整链路。
//
// 它同时验证 examples/packs/go-webapp 推荐的那条 execReload 写法
// （`/bin/sh -c 'kill -HUP $MAINPID'`）在真机上确实送达了信号：
// 惯用的 `/bin/kill` 由 procps 提供，最小化镜像里根本不存在。
//
// 期望是**热加载而非重启**——MainPID 不变，配置却生效了。
func TestDriftReloadKeepsProcessAlive(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	cleanup(t)
	t.Cleanup(func() { cleanup(t) })

	home := "/opt/mecharion-e2e/apps/" + component
	confDir := "/etc/mecharion-e2e/apps/" + component
	blobSum := installBlob(t, buildTarball(t))

	// driftPolicy: reconcile —— 期望没变而机器变了时自动改回，
	// 改回之后触发声明的 notify
	s := specOf(home, confDir, blobSum, "info", 1)
	resources := s["resources"].([]map[string]any)
	resources[1]["driftPolicy"] = "reconcile"
	specPath := writeSpec(t, s)

	if out, err := runMechlet(ctx, "apply", "-f", specPath, "--data-dir", dataDir); err != nil {
		t.Fatalf("首次 apply: %v\n%s", err, out)
	}
	waitUnitActive(t, 20*time.Second)
	waitHTTPContains(t, fmt.Sprintf("http://127.0.0.1:%d/", port), "logLevel=info", 20*time.Second)
	pidBefore := mainPID(t)

	// 有人手工改坏了配置
	confPath := filepath.Join(confDir, "app.yaml")
	if err := os.WriteFile(confPath,
		[]byte(fmt.Sprintf("port: %d\nlog_level: trace\n", port)), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runMechlet(ctx, "apply", "-f", specPath, "--data-dir", dataDir)
	t.Logf("漂移调和:\n%s", out)
	if err != nil {
		t.Fatalf("调和失败: %v", err)
	}
	if !strings.Contains(out, "reload") {
		t.Errorf("报告里应当出现 reload:\n%s", out)
	}

	// ① 配置被改回了期望值
	body, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "log_level: info") {
		t.Errorf("配置应当被改回，实际 %q", body)
	}

	// ② 进程收到了 SIGHUP —— 没重启，但值生效了
	if pidAfter := mainPID(t); pidAfter != pidBefore {
		t.Errorf("reload 不该重启进程：MainPID %s → %s\n"+
			"（若 execReload 里的命令不存在，systemd 会 reload 失败）",
			pidBefore, pidAfter)
	}
	got := waitHTTPContains(t, fmt.Sprintf("http://127.0.0.1:%d/", port),
		"logLevel=info", 10*time.Second)
	if !strings.Contains(got, "logLevel=info") {
		t.Errorf("热加载后的取值 = %q", got)
	}
}

// TestApplyDryRun 钉住 --dry-run 不改动机器。
func TestApplyDryRun(t *testing.T) {
	requireEnv(t)
	cleanup(t)
	t.Cleanup(func() { cleanup(t) })

	home := "/opt/mecharion-e2e/apps/" + component
	confDir := "/etc/mecharion-e2e/apps/" + component
	blobSum := installBlob(t, buildTarball(t))
	specPath := writeSpec(t, specOf(home, confDir, blobSum, "info", 1))

	out, err := runMechlet(context.Background(), "apply", "-f", specPath,
		"--data-dir", dataDir, "--dry-run")
	if err != nil {
		t.Fatalf("dry-run 失败: %v\n%s", err, out)
	}
	if !strings.Contains(out, "was not changed") {
		t.Errorf("输出应当说明没动手:\n%s", out)
	}
	if _, err := os.Stat(home); err == nil {
		t.Error("--dry-run 不该创建任何目录")
	}
	if _, err := os.Stat("/etc/systemd/system/" + unitName); err == nil {
		t.Error("--dry-run 不该写 unit")
	}
}

// TestApplyRejectsBadSpec 钉住规格有问题时的退出码。
func TestApplyRejectsBadSpec(t *testing.T) {
	requireEnv(t)

	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"schemaVersion":1,"component":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runMechlet(context.Background(), "apply", "-f", bad, "--data-dir", dataDir)
	if err == nil {
		t.Fatal("残缺的规格应当被拒绝")
	}
	var ee *exec.ExitError
	if !asExitError(err, &ee) || ee.ExitCode() != 3 {
		t.Errorf("规格问题的退出码应当是 3（10-cli.md §6），实际 %v", err)
	}
}

// ── 夹具 ────────────────────────────────────────────────────────────────

// buildTarball 把预先交叉编译好的 webapp 打成 tar.gz。
//
// 二进制由宿主机编译（容器里刻意没有工具链），通过 bin/ 挂载进来。
func buildTarball(t *testing.T) []byte {
	t.Helper()
	binPath := filepath.Join(binDir, "webapp")
	body, err := os.ReadFile(binPath)
	if err != nil {
		t.Skipf("找不到夹具二进制 %s: %v（先跑 make e2e）", binPath, err)
	}

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	tw := tar.NewWriter(zw)

	// 顶层套一个版本目录，好让 strip: 1 有东西可剥——真实的上游 tarball
	// 几乎都是这个形状
	for _, e := range []struct {
		name string
		mode int64
		body []byte
	}{
		{"webapp-1.2.0/bin/webapp", 0o755, body},
		{"webapp-1.2.0/README.md", 0o644, []byte("Mecharion 端到端夹具\n")},
	} {
		if err := tw.WriteHeader(&tar.Header{
			Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

// installBlob 把载荷放进内容寻址的 blob 存储，返回其 sha256。
func installBlob(t *testing.T, body []byte) string {
	t.Helper()
	sum := sha256.Sum256(body)
	hexSum := hex.EncodeToString(sum[:])

	dir := filepath.Join(dataDir, "blobs", "sha256", hexSum[:2])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, hexSum), body, 0o644); err != nil {
		t.Fatal(err)
	}
	return hexSum
}

// specOf 构造一份 go-webapp 的已解析规格。
//
// 内容对应 examples/packs/go-webapp/pack.yaml 被 mechd 解析之后的样子：
// 模板已渲染成 content、when 已求值、拓扑已快照。
func specOf(home, confDir, blobSum, logLevel string, revision int) map[string]any {
	genPlaceholder := "{{ .Paths.Generation }}"
	return map[string]any{
		"schemaVersion": 1,
		"site":          map[string]any{"name": "e2e", "kind": "standalone"},
		"component":     component,
		"role":          role,
		"configGroup":   "default",
		"node":          map[string]any{"name": "node-1", "address": "127.0.0.1"},
		"ordinal":       0,
		"pack": map[string]any{
			"name": "go-webapp", "version": "1.2.0", "revision": revision,
		},
		"params": map[string]any{
			"port":      map[string]any{"value": port, "type": "port"},
			"log_level": map[string]any{"value": logLevel, "type": "enum"},
		},
		"paths": map[string]any{
			"home": map[string]any{
				"name": "home", "values": []string{home}, "kind": "single", "mode": "0755",
			},
			"config": map[string]any{
				"name": "config", "values": []string{confDir}, "kind": "single", "mode": "0755",
			},
			"data": map[string]any{
				"name": "data", "values": []string{dataDir + "/apps/" + component},
				"kind": "single", "mode": "0755",
			},
		},
		"blobs": []map[string]any{{
			"name": "main", "sha256": blobSum, "size": 0,
			"filename": "webapp-1.2.0-linux-amd64.tar.gz",
		}},
		"resources": []map[string]any{
			{
				"id": "archive:main", "type": "archive", "origin": "role",
				"args": map[string]any{
					"blob": "main", "dest": genPlaceholder, "strip": 1,
				},
				"driftPolicy": "report",
			},
			{
				"id": "template:app.yaml", "type": "template", "origin": "role",
				"args": map[string]any{
					"dest": confDir + "/app.yaml",
					"content": fmt.Sprintf("# 由 Mecharion 渲染\nport: %d\nlog_level: %s\n",
						port, logLevel),
					"mode": "0644",
				},
				"driftPolicy": "report",
				"notify":      "reload",
			},
		},
		"workload": map[string]any{
			"runtime": "systemd",
			"systemd": map[string]any{
				"exec": home + "/current/bin/webapp --config " + confDir + "/app.yaml",
				// SIGHUP 用 systemd 自己的 $MAINPID 展开；不用 /bin/kill，
				// debian:12-slim 里没装 procps
				"execReload": "/bin/sh -c 'kill -HUP $MAINPID'",
				"restart":    "on-failure",
				"restartSec": "1s",
			},
		},
		"health": map[string]any{
			"http":         map[string]any{"path": "/healthz", "port": port},
			"startupGrace": "20s",
		},
		"topology": map[string]any{
			"roles": map[string]any{
				role: []map[string]any{
					{"node": "node-1", "address": "127.0.0.1", "ordinal": 0},
				},
			},
		},
		"reconcile": map[string]any{"retainGenerations": 3},
	}
}

func writeSpec(t *testing.T, s map[string]any) string {
	t.Helper()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// ── 驱动与断言 ──────────────────────────────────────────────────────────

// runMechlet 用**裸命令名**调用，不给绝对路径。
//
// 这样它必须靠 /usr/bin 下的软链被找到——「不做任何环境变量配置就能使用」
// 这条承诺若失效，这里第一个失败。
func runMechlet(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "mechlet", args...)
	cmd.Env = append(os.Environ(), "MECHARION_E2E=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestBinariesOnDefaultPath 钉住零 PATH 配置。
//
// 容器**刻意不设 ENV PATH**（test/node/Dockerfile），因此这里用的是发行版
// 默认 PATH。四个二进制都要能被直接找到。
func TestBinariesOnDefaultPath(t *testing.T) {
	requireEnv(t)
	for _, b := range []string{"mechctl", "mechpack", "mechd", "mechlet"} {
		p, err := exec.LookPath(b)
		if err != nil {
			t.Errorf("%s 不在默认 PATH 上: %v", b, err)
			continue
		}
		if !strings.HasPrefix(p, "/usr/bin/") {
			t.Errorf("%s 解析到 %s，期望 /usr/bin 下的软链", b, p)
		}
	}
}

func systemctl(t *testing.T, args ...string) string {
	t.Helper()
	out, _ := exec.Command("systemctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out))
}

func waitUnitActive(t *testing.T, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if systemctl(t, "is-active", unitName) == "active" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s 未能进入 active：\n%s\n%s",
		unitName,
		systemctl(t, "status", unitName, "--no-pager"),
		systemctl(t, "show", unitName, "-p", "ActiveState,SubState,Result,ExecMainStatus"))
}

func mainPID(t *testing.T) string {
	t.Helper()
	return systemctl(t, "show", unitName, "-p", "MainPID", "--value")
}

func httpGet(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // 测试内的本地请求
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func waitHTTP(t *testing.T, url string, within time.Duration) string {
	t.Helper()
	return waitHTTPContains(t, url, "", within)
}

func waitHTTPContains(t *testing.T, url, want string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		last = httpGet(t, url)
		if last != "" && (want == "" || strings.Contains(last, want)) {
			return last
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s 在 %s 内未返回期望内容（want=%q，最后一次 %q）\njournal:\n%s",
		url, within, want, last, journal(t))
	return last
}

func journal(t *testing.T) string {
	t.Helper()
	out, _ := exec.Command("journalctl", "-u", unitName, "--no-pager", "-n", "30").CombinedOutput()
	return string(out)
}

// instance 是本地状态文件中我们关心的部分。
type instance struct {
	CurrentGeneration int `json:"currentGeneration"`
	Generations       []struct {
		Seq   int    `json:"seq"`
		State string `json:"state"`
		Dir   string `json:"dir"`
	} `json:"generations"`
}

func loadInstance(t *testing.T) instance {
	t.Helper()
	p := filepath.Join(dataDir, "mechlet", "instances", component+"__"+role+".json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取本地状态 %s: %v", p, err)
	}
	var in instance
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatal(err)
	}
	return in
}

// cleanup 把上一次跑剩下的东西清干净。
func cleanup(t *testing.T) {
	t.Helper()
	_ = exec.Command("systemctl", "disable", "--now", unitName).Run()
	_ = os.Remove("/etc/systemd/system/" + unitName)
	_ = exec.Command("systemctl", "daemon-reload").Run()
	_ = exec.Command("systemctl", "reset-failed", unitName).Run()

	for _, p := range []string{
		"/opt/mecharion-e2e", "/etc/mecharion-e2e",
		filepath.Join(dataDir, "mechlet"), filepath.Join(dataDir, "apps"),
	} {
		_ = os.RemoveAll(p)
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	for err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
