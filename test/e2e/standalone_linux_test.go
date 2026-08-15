//go:build linux

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 单机验收用的隔离目录，与 M2 的 webapp 测试分开。
const (
	saPrefix   = "/usr/local/lib/mecharion-sa"
	saDataDir  = "/var/lib/mecharion-sa"
	saConfDir  = "/etc/mecharion-sa"
	saSocket   = "/run/mecharion-sa/mechd.sock"
	saHTTPAddr = "127.0.0.1:18443"
	saNode     = "sa-node"
	saUnit     = "mecharion-web-default.service"
	saPort     = 18082
)

// TestStandaloneEndToEnd 是 **M3 的验收**：
//
//	mechctl component deploy go-webapp 端到端跑通
//
// 它不走任何内部包：起真的 mechd、跑真的 mechlet agent、用真的 mechctl
// 发命令，最后确认 systemd 里真的有一个在跑的服务、并且 mechd 认为它
// 已经收敛。
//
// 「端到端」在这里是字面意思——中间任何一环打桩，这条验收就换了个
// 更容易通过的题目。
func TestStandaloneEndToEnd(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cleanupStandalone(ctx, t)
	t.Cleanup(func() { cleanupStandalone(context.Background(), t) })

	// ── ① 装 Pack 与载荷 ──
	stagePack(t)
	sum := installBlobIn(t, saDataDir, buildTarball(t))
	rewritePackBlob(t, sum)

	// ── ② 起 mechd ──
	mechd := startMechd(ctx, t)
	defer stopProc(mechd)
	waitAPI(ctx, t)

	token := readToken(t)

	// ── ③ 注册节点（单机安装会做这件事；这里直接建库记录） ──
	seedSite(ctx, t)

	// ── ④ 起 mechlet agent ──
	agent := startAgent(ctx, t)
	defer stopProc(agent)

	// ── ⑤ deploy ──
	out, err := runCtl(ctx, token, "component", "deploy", "go-webapp",
		"-c", "web", "--nodes", saNode)
	if err != nil {
		t.Fatalf("deploy 失败: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Deployed web") {
		t.Fatalf("deploy 输出不对:\n%s", out)
	}

	// ── ⑥ 等它真的跑起来 ──
	//
	// 判据是 **systemd 里那个 unit 真的 active**，不是「命令返回了 0」。
	if !waitUntil(ctx, 90*time.Second, func() bool { return isActive(ctx, saUnit) }) {
		dumpDiagnostics(ctx, t)
		t.Fatalf("%s 没有起来", saUnit)
	}

	// ── ⑦ 等 mechd 认为它收敛 ──
	//
	// 收敛 = **上报的 digest == 期望的 digest 且健康**。
	// 靠状态判定而不是靠 mechlet 说「我成功了」。
	var st struct {
		Converged bool `json:"converged"`
		Instances []struct {
			Node      string `json:"node"`
			Want      string `json:"want"`
			Got       string `json:"got"`
			Converged bool   `json:"converged"`
			Workload  string `json:"workload"`
			Health    string `json:"health"`
		} `json:"instances"`
	}
	ok := waitUntil(ctx, 90*time.Second, func() bool {
		out, err := runCtl(ctx, token, "component", "status", "web", "-o", "json")
		if err != nil {
			return false
		}
		st = struct {
			Converged bool `json:"converged"`
			Instances []struct {
				Node      string `json:"node"`
				Want      string `json:"want"`
				Got       string `json:"got"`
				Converged bool   `json:"converged"`
				Workload  string `json:"workload"`
				Health    string `json:"health"`
			} `json:"instances"`
		}{}
		if json.Unmarshal([]byte(out), &st) != nil {
			return false
		}
		return st.Converged
	})
	if !ok {
		dumpDiagnostics(ctx, t)
		t.Fatalf("mechd 一直没认为它收敛，最后一次状态: %+v", st)
	}

	if len(st.Instances) != 1 {
		t.Fatalf("应有 1 个实例，实际 %d", len(st.Instances))
	}
	in := st.Instances[0]
	if in.Got != in.Want {
		t.Errorf("digest 应当一致：期望 %s，上报 %s", in.Want, in.Got)
	}
	if in.Workload != "running" || in.Health != "healthy" {
		t.Errorf("工作负载与健康状态不对: workload=%s health=%s", in.Workload, in.Health)
	}

	// ── ⑧ diff 应当没有待下发的变化 ──
	out, err = runCtl(ctx, token, "component", "diff", "web")
	if err != nil {
		t.Fatalf("diff 失败: %v\n%s", err, out)
	}
	if !strings.Contains(out, "has no pending changes") {
		t.Errorf("已收敛时 diff 不该有变化:\n%s", out)
	}
}

// TestStandaloneConfigChangePropagates 钉住「改一个参数会一路传到机器上」。
//
// 这是 deploy 之外另一半的价值：期望状态变了，机器跟着变。
func TestStandaloneConfigChangePropagates(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cleanupStandalone(ctx, t)
	t.Cleanup(func() { cleanupStandalone(context.Background(), t) })

	stagePack(t)
	sum := installBlobIn(t, saDataDir, buildTarball(t))
	rewritePackBlob(t, sum)

	mechd := startMechd(ctx, t)
	defer stopProc(mechd)
	waitAPI(ctx, t)
	token := readToken(t)
	seedSite(ctx, t)
	agent := startAgent(ctx, t)
	defer stopProc(agent)

	if out, err := runCtl(ctx, token, "component", "deploy", "go-webapp",
		"-c", "web", "--nodes", saNode); err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	if !waitUntil(ctx, 90*time.Second, func() bool { return isActive(ctx, saUnit) }) {
		dumpDiagnostics(ctx, t)
		t.Fatal("服务没起来")
	}

	confPath := "/etc/mecharion/apps/web/app.yaml"
	if !waitUntil(ctx, 30*time.Second, func() bool {
		b, err := os.ReadFile(confPath)
		return err == nil && strings.Contains(string(b), "level: info")
	}) {
		dumpDiagnostics(ctx, t)
		t.Fatalf("没有渲染出 %s", confPath)
	}

	// 改参数
	if out, err := runCtl(ctx, token, "component", "deploy", "go-webapp",
		"-c", "web", "--nodes", saNode, "--update",
		"--set", "log_level=debug"); err != nil {
		t.Fatalf("改参数: %v\n%s", out, err)
	}

	// 新配置一路传到磁盘上
	if !waitUntil(ctx, 60*time.Second, func() bool {
		b, err := os.ReadFile(confPath)
		return err == nil && strings.Contains(string(b), "level: debug")
	}) {
		b, _ := os.ReadFile(confPath)
		dumpDiagnostics(ctx, t)
		t.Fatalf("参数改动没有传到机器上，%s 现在是:\n%s", confPath, b)
	}
}

// ── 夹具 ────────────────────────────────────────────────────────────────

// stagePack 把 go-webapp 的 Pack 放进 mechd 的 Pack 集合。
func stagePack(t *testing.T) {
	t.Helper()
	dst := filepath.Join(saDataDir, "packs", "go-webapp")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	copyTree(t, packSource(t), dst)
}

// packSource 定位示例 Pack。
//
// 容器里 examples/ 挂在 /examples（只读）；开发机上走相对路径。
func packSource(t *testing.T) string {
	t.Helper()
	for _, cand := range []string{
		"/examples/packs/go-webapp",
		filepath.Join("..", "..", "examples", "packs", "go-webapp"),
	} {
		if _, err := os.Stat(filepath.Join(cand, "pack.yaml")); err == nil {
			abs, _ := filepath.Abs(cand)
			return abs
		}
	}
	t.Skip("找不到 go-webapp 示例 Pack")
	return ""
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// rewritePackBlob 把 pack.yaml 里的占位 sha256 换成夹具真实的摘要。
//
// 示例 Pack 的 blobs 是占位值（真实产物由 `mechpack assemble` 填）。
// 这条验收要的是**链路**，因此把摘要换成测试夹具的即可。
func rewritePackBlob(t *testing.T, sum string) {
	t.Helper()
	p := filepath.Join(saDataDir, "packs", "go-webapp", "pack.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	body = strings.ReplaceAll(body, strings.Repeat("0", 64), sum)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func startMechd(ctx context.Context, t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(ctx, filepath.Join(binDir, "mechd"), "serve",
		"--data-dir", filepath.Join(saDataDir, "mechd"),
		"--conf-dir", saConfDir,
		"--pack-dir", filepath.Join(saDataDir, "packs"),
		"--socket", saSocket,
		"--http", saHTTPAddr,
		"--insecure-http", // 测试里不折腾证书信任；TLS 另有单测
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func startAgent(ctx context.Context, t *testing.T, extra ...string) *exec.Cmd {
	t.Helper()
	args := append([]string{"agent",
		"--data-dir", saDataDir,
		"--upstream", "unix://" + saSocket,
		"--node", saNode,
	}, extra...)
	cmd := exec.CommandContext(ctx, filepath.Join(binDir, "mechlet"), args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func stopProc(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
}

// waitAPI 等 mechd 的 HTTP 接口就绪。
func waitAPI(ctx context.Context, t *testing.T) {
	t.Helper()
	ok := waitUntil(ctx, 30*time.Second, func() bool {
		out, err := exec.CommandContext(ctx, filepath.Join(binDir, "mechctl"),
			"component", "list",
			"--server", "http://"+saHTTPAddr,
			"--token", "probe").CombinedOutput()
		// 401 说明服务已经在听了——这一步只关心「起来没有」
		return err != nil && strings.Contains(string(out), "token")
	})
	if !ok {
		t.Fatal("mechd 的 HTTP 接口没有就绪")
	}
}

func readToken(t *testing.T) string {
	t.Helper()
	p := filepath.Join(saConfDir, "admin.token")
	var b []byte
	if !waitUntil(context.Background(), 20*time.Second, func() bool {
		var err error
		b, err = os.ReadFile(p)
		return err == nil && len(b) > 0
	}) {
		t.Fatalf("mechd 没有生成 %s", p)
	}
	return strings.TrimSpace(string(b))
}

// seedSite 建站点并登记本机节点。
//
// 真实安装里这是 `mechlet install --standalone` 的第 ④ 步。这条测试
// 直接调那段逻辑，避免为了跑验收先把整台机器改掉（软链进 /usr/bin、
// 写 systemd unit 之类会污染容器里的其它测试）。
func seedSite(ctx context.Context, t *testing.T) {
	t.Helper()
	out, err := exec.CommandContext(ctx, filepath.Join(binDir, "mechlet"),
		"install", "--standalone",
		"--prefix", saPrefix,
		// **不碰 /usr/bin**：容器里那四条软链是测试夹具自己的，
		// 让安装改掉它们会连累同一次运行里的其它测试
		"--link-dir", saPrefix+"/bin",
		"--data-dir", saDataDir,
		"--conf-dir", saConfDir,
		"--node", saNode,
		"--http", saHTTPAddr,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("install --standalone 失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Install complete") {
		t.Fatalf("install 输出不对:\n%s", out)
	}
}

func runCtl(ctx context.Context, token string, args ...string) (string, error) {
	full := append(args,
		"--server", "http://"+saHTTPAddr,
		"--token", token)
	out, err := exec.CommandContext(ctx,
		filepath.Join(binDir, "mechctl"), full...).CombinedOutput()
	return string(out), err
}

func waitUntil(ctx context.Context, d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		if cond() {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// dumpDiagnostics 在失败时把现场打出来。
//
// 一条跨三个进程的验收失败时，「哪一环断了」是唯一重要的问题；
// 让人去容器里手工复现是最贵的排查方式。
func dumpDiagnostics(ctx context.Context, t *testing.T) {
	t.Helper()
	for _, c := range [][]string{
		{"systemctl", "status", saUnit, "--no-pager", "-l"},
		{"journalctl", "-u", saUnit, "--no-pager", "-n", "50"},
		{"ls", "-la", filepath.Join(saDataDir, "mechlet")},
	} {
		out, _ := exec.CommandContext(ctx, c[0], c[1:]...).CombinedOutput()
		t.Logf("$ %s\n%s", strings.Join(c, " "), out)
	}
}

func cleanupStandalone(ctx context.Context, t *testing.T) {
	t.Helper()
	_ = exec.CommandContext(ctx, "systemctl", "stop", saUnit).Run()
	_ = exec.CommandContext(ctx, "systemctl", "disable", saUnit).Run()
	_ = os.Remove("/etc/systemd/system/" + saUnit)
	for _, u := range []string{"mecharion-mechd.service", "mecharion-mechlet.service"} {
		_ = exec.CommandContext(ctx, "systemctl", "disable", "--now", u).Run()
		_ = os.Remove("/etc/systemd/system/" + u)
	}
	_ = exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()

	for _, d := range []string{
		saDataDir, saConfDir, saPrefix,
		"/run/mecharion-sa", "/opt/mecharion/apps/web",
		"/etc/mecharion/apps/web", "/var/lib/mecharion/apps/web",
	} {
		_ = os.RemoveAll(d)
	}
}
