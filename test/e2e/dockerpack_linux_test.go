//go:build linux

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// docker 官方 Pack 的验收用一套独立的路径，与既有的 dockerd 完全隔开。
const (
	dpComponent = "dockerd2"
	dpDataDir   = "/var/lib/mecharion-e2e-dockerpack"
	dpConfRoot  = "/etc/mecharion-e2e-dockerpack"
	dpSocket    = "/run/m7n-docker2.sock"
	dpEngineDir = "/var/lib/m7n-docker2"
	dpUnit      = "mecharion-dockerd2-engine.service"
)

// TestDockerPackInstallsDaemonOffline 是 M4 第 7 步的验收：**离线装出 dockerd**。
//
// 这条测试走的是完整的真实链路，中间没有手写的规格：
//
//	examples/packs/docker  →  mechctl component render  →  mechlet apply
//	                                                   →  systemd 起 dockerd
//	                                                   →  docker version 能应答
//
// **载荷从哪来是这里唯一被替换的东西**：官方静态包有几百 MB，不进仓库、
// 也不在测试里下载（那会让验收依赖网络，正是本项目要消灭的东西）。
// 改用本机既有的 docker 二进制现场打一个同样布局的 tar——
// 「字节从哪来」恰好是用户在真实 Pack 阶段自己要做的那部分。
//
// 被验证的因此是全部其余环节：archive 解包、daemon.json 渲染、
// unit 生成（PATH / Delegate / KillMode）、hook、健康探针、以及
// **它真的是一台独立的 daemon**。
func TestDockerPackInstallsDaemonOffline(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	cleanupDockerPack(ctx, t)
	t.Cleanup(func() { cleanupDockerPack(context.Background(), t) })

	packDir := stageDockerPack(t)
	sum := stageEngineBlob(ctx, t)
	rewriteBlobSum(t, filepath.Join(packDir, "pack.yaml"), sum)

	specPath := renderDockerPack(ctx, t, packDir)

	out, err := runMechlet(ctx, "apply", "-f", specPath, "--data-dir", dpDataDir)
	if err != nil {
		dumpDockerPackDiagnostics(ctx, t)
		t.Fatalf("离线装 dockerd 失败: %v\n%s", err, out)
	}

	// ① unit 真的起来了
	if st := unitProperty(t, dpUnit, "ActiveState"); st != "active" {
		dumpDockerPackDiagnostics(ctx, t)
		t.Fatalf("%s 应当是 active，实际 %q", dpUnit, st)
	}

	// ② daemon 真的能应答 —— 这才是「装出 dockerd」的判据
	ver := waitDockerVersion(ctx, t, 90*time.Second)
	if ver == "" {
		dumpDockerPackDiagnostics(ctx, t)
		t.Fatal("新装的 dockerd 没有应答 docker version")
	}
	t.Logf("新装的 dockerd 版本: %s", ver)

	// ③ 它是**另一台** daemon，不是我们连到了宿主那台。
	//
	// 少了这一条，一个根本没起来的 dockerd 也能让上一步通过——
	// docker CLI 会去连默认 socket。
	if _, err := os.Stat(filepath.Join(dpEngineDir, "engine-id")); err != nil {
		t.Errorf("data-root %s 里应当有 engine-id（说明是它自己的库）: %v", dpEngineDir, err)
	}
	outer := dockerEngineID(ctx, "")
	inner := dockerEngineID(ctx, dpSocket)
	if inner == "" {
		t.Error("新 daemon 报不出 engine id")
	} else if inner == outer {
		t.Errorf("新旧 daemon 的 engine id 相同（%s）—— 说明连的是同一台", short12(inner))
	}

	// ④ unit 里那几行**非默认**的设置真的在。
	//
	// 它们不是装饰：少了 PATH，dockerd 起得来但一 run 容器就找不到 runc；
	// 少了 Delegate，容器的资源限制会被 systemd 收编而失效。
	unit := readUnit(t, dpUnit)
	for _, want := range []string{
		"Delegate=yes",
		"KillMode=process",
		"OOMScoreAdjust=-500",
		"TimeoutStartSec=0",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit 里缺少 %q，实际:\n%s", want, unit)
		}
	}
	if !strings.Contains(unit, "Environment=PATH=") {
		t.Errorf("unit 里缺少 PATH —— dockerd 运行期要 exec runc/containerd，实际:\n%s", unit)
	}

	// ⑤ daemon.json 是渲染出来的，且指向我们要的位置
	conf := readFile(t, filepath.Join(dpConfRoot, "apps", dpComponent, "daemon.json"))
	for _, want := range []string{
		`"data-root": "` + dpEngineDir + `"`,
		`"unix://` + dpSocket + `"`,
		`"storage-driver": "vfs"`,
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("daemon.json 里缺少 %s，实际:\n%s", want, conf)
		}
	}
}

// TestDockerPackDaemonCanRunContainer 钉住装出来的是一台**能干活**的 daemon。
//
// 「version 能应答」只证明 API 起来了。dockerd 在运行期还要 exec
// containerd / containerd-shim / runc / docker-init——这些只在 generation
// 目录里，全靠 unit 里那行 PATH。真跑一个容器才验得到这条。
func TestDockerPackDaemonCanRunContainer(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	cleanupDockerPack(ctx, t)
	t.Cleanup(func() { cleanupDockerPack(context.Background(), t) })

	packDir := stageDockerPack(t)
	sum := stageEngineBlob(ctx, t)
	rewriteBlobSum(t, filepath.Join(packDir, "pack.yaml"), sum)

	if out, err := runMechlet(ctx, "apply",
		"-f", renderDockerPack(ctx, t, packDir), "--data-dir", dpDataDir); err != nil {
		dumpDockerPackDiagnostics(ctx, t)
		t.Fatalf("离线装 dockerd 失败: %v\n%s", err, out)
	}
	if waitDockerVersion(ctx, t, 90*time.Second) == "" {
		dumpDockerPackDiagnostics(ctx, t)
		t.Fatal("新装的 dockerd 没有应答")
	}

	// 把 webapp 镜像喂给新 daemon —— 走 load，仍然不联网
	img := buildWebappImage(ctx, t)
	if out, err := exec.CommandContext(ctx, "docker", "-H", "unix://"+dpSocket,
		"load", "-i", img).CombinedOutput(); err != nil {
		dumpDockerPackDiagnostics(ctx, t)
		t.Fatalf("新 daemon 装不进镜像: %v\n%s", err, out)
	}

	// 真的跑一个容器。--network none 是因为本 Pack 的测试参数关掉了
	// iptables 与网桥（嵌套环境里那两样会与外层 dockerd 打架）
	run := exec.CommandContext(ctx, "docker", "-H", "unix://"+dpSocket,
		"run", "--rm", "--network", "none", dkImage, "--help")
	out, err := run.CombinedOutput()
	if err != nil {
		dumpDockerPackDiagnostics(ctx, t)
		t.Fatalf("新 daemon 跑不起容器（多半是 PATH 缺了 runc/containerd）: %v\n%s", err, out)
	}
}

// ── 夹具 ────────────────────────────────────────────────────────────────

// stageDockerPack 把示例 Pack 放进 mechlet 的 Pack 集合。
//
// 两个理由必须落在 <data-dir>/packs/<name>，而不是随便一个临时目录：
// /examples 是只读挂载而这条验收要改 pack.yaml 里的占位 sha256；
// **且 hook 是按这个位置找脚本的**——放别处的表现是
// 「渲染一切正常，执行 hook 时报文件不存在」。
func stageDockerPack(t *testing.T) string {
	t.Helper()
	var src string
	for _, cand := range []string{
		"/examples/packs/docker",
		filepath.Join("..", "..", "examples", "packs", "docker"),
	} {
		if _, err := os.Stat(filepath.Join(cand, "pack.yaml")); err == nil {
			src, _ = filepath.Abs(cand)
			break
		}
	}
	if src == "" {
		t.Skip("找不到 docker 示例 Pack")
	}
	dst := filepath.Join(dpDataDir, "packs", "docker")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	copyTree(t, src, dst)
	return dst
}

// engineBinaries 是官方静态包里的全套二进制。
//
// **必须一个不少**：dockerd 在运行期 exec 它们，缺一个的表现是
// 「daemon 起来了，但一 run 容器就报找不到 xxx」——而那时人已经不会
// 再怀疑安装环节了。
var engineBinaries = []string{
	"dockerd", "docker", "containerd", "containerd-shim-runc-v2",
	"ctr", "runc", "docker-init", "docker-proxy",
}

// stageEngineBlob 把本机既有的 docker 二进制打成官方静态包同布局的 tar。
//
// 官方包解开是 `docker/<binaries>`，Pack 用 `strip: 1` 剥掉那一层。
// 这里照做，于是 Pack 里的 archive 声明**一个字都不用改**。
func stageEngineBlob(ctx context.Context, t *testing.T) string {
	t.Helper()

	stage := filepath.Join(t.TempDir(), "docker")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range engineBinaries {
		src := findEngineBinary(name)
		if src == "" {
			// 找不到就跳过整条验收，而不是装一个残缺的 daemon 再让它
			// 以别的方式失败
			t.Skipf("本机没有 %s，无法造出离线载荷", name)
		}
		body, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("读 %s: %v", src, err)
		}
		if err := os.WriteFile(filepath.Join(stage, name), body, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	tarPath := filepath.Join(t.TempDir(), "docker-static.tgz")
	cmd := exec.CommandContext(ctx, "tar", "-czf", tarPath,
		"-C", filepath.Dir(stage), "docker")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("打包离线载荷: %v\n%s", err, out)
	}
	body, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	return installBlobIn(t, dpDataDir, body)
}

// findEngineBinary 找一个 docker 配套二进制。
//
// 光靠 PATH 不够：发行版包把 `docker-init` 装在 /usr/libexec/docker 下，
// 它**不在 PATH 上**（dockerd 按绝对路径找它）。官方静态包里却是平铺的，
// 因此造载荷时得两处都看。
func findEngineBinary(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, dir := range []string{"/usr/libexec/docker", "/usr/lib/docker"} {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// rewriteBlobSum 把 pack.yaml 里 amd64 那条占位 sha256 换成真实摘要。
func rewriteBlobSum(t *testing.T, path, sum string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.ReplaceAll(string(b),
		"0000dddd00000000000000000000000000000000000000000000000000000000", sum)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// renderDockerPack 用 `mechctl component render` 把真 Pack 渲染成已解析规格。
//
// 走真渲染而不是手写规格，是这条验收与前几条的关键区别：手写规格测的是
// Runtime，走渲染测的才是 **Pack 本身写得对不对**。
func renderDockerPack(ctx context.Context, t *testing.T, packDir string) string {
	t.Helper()

	planPath := filepath.Join(t.TempDir(), "plan.yaml")
	plan := fmt.Sprintf(`site:
  name: e2e
  kind: standalone
component: %s
pack: %s
nodes:
  - name: node-1
    address: 127.0.0.1
    # 根的名字是 opt/etc/data/logs/run（不是 home/config/data）——
    # 写错了不会报错，只是**静默落回默认根**，把东西装到 /opt/mecharion
    roots:
      opt:  %s/opt
      etc:  %s
      data: %s/data
      logs: %s/logs
      run:  %s/run
instances:
  - role: engine
    node: node-1
    ordinal: 0
params:
  component:
    socket: %s
    data_root: %s
    # 嵌套环境里 overlay2 装不上（外层已经是 overlay），vfs 慢但总能跑。
    # 这正是把 vfs 放进 enum 的理由
    pidfile: /run/m7n-docker2.pid
    storage_driver: vfs
    # 关掉网络管理：外层 dockerd 已经在管 iptables，两个一起改会互相拆台
    iptables: false
    # none：不建默认网桥。**它会清掉外层的 docker0**（见 Pack 的 bridge
    # 参数说明），因此 cleanup 里要把外层 daemon 重启回来。
    #
    # 自定义网桥名不行：dockerd 要求那个设备已经存在，而容器里连 ip 命令
    # 都没有。两台 dockerd 共处一个网络命名空间本就是侵入式的。
    bridge: none
    # 不抢 /usr/local/bin/docker —— 那是外层 docker 的位置
    install_cli_links: false
`, dpComponent, packDir, dpDataDir, dpConfRoot, dpDataDir, dpDataDir, dpDataDir,
		dpSocket, dpEngineDir)

	if err := os.WriteFile(planPath, []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(t.TempDir(), "specs")
	cmd := exec.CommandContext(ctx, "mechctl", "component", "render",
		"-f", planPath, "--out", outDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("渲染 docker Pack: %v\n%s", err, out)
	}

	matches, _ := filepath.Glob(filepath.Join(outDir, "*.json"))
	if len(matches) != 1 {
		t.Fatalf("应当渲染出 1 份规格，实际 %v（渲染输出:\n%s）", matches, out)
	}
	return matches[0]
}

// buildWebappImage 造一个能喂给新 daemon 的镜像 tar。
func buildWebappImage(ctx context.Context, t *testing.T) string {
	t.Helper()

	build := filepath.Join(t.TempDir(), "img")
	if err := os.MkdirAll(build, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(binDir, "webapp"))
	if err != nil {
		t.Skipf("找不到 webapp 夹具（先 make e2ebin）: %v", err)
	}
	if err := os.WriteFile(filepath.Join(build, "webapp"), body, 0o755); err != nil {
		t.Fatal(err)
	}
	df := "FROM scratch\nCOPY webapp /webapp\nENTRYPOINT [\"/webapp\"]\n"
	if err := os.WriteFile(filepath.Join(build, "Dockerfile"), []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, "docker", "build",
		"-t", dkImage, build).CombinedOutput(); err != nil {
		t.Fatalf("构建测试镜像: %v\n%s", err, out)
	}

	tar := filepath.Join(t.TempDir(), "image.tar")
	if out, err := exec.CommandContext(ctx, "docker", "save",
		"-o", tar, dkImage).CombinedOutput(); err != nil {
		t.Fatalf("docker save: %v\n%s", err, out)
	}
	return tar
}

// ── 观察 ────────────────────────────────────────────────────────────────

// waitDockerVersion 等新 daemon 应答 `docker version`。
func waitDockerVersion(ctx context.Context, t *testing.T, d time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		out, err := exec.CommandContext(ctx, "docker", "-H", "unix://"+dpSocket,
			"version", "--format", "{{.Server.Version}}").Output()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			return strings.TrimSpace(string(out))
		}
		time.Sleep(2 * time.Second)
	}
	return ""
}

// dockerEngineID 取一台 daemon 的 engine id；host 为空则用默认 socket。
func dockerEngineID(ctx context.Context, sock string) string {
	args := []string{}
	if sock != "" {
		args = append(args, "-H", "unix://"+sock)
	}
	args = append(args, "info", "--format", "{{.ID}}")
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func unitProperty(t *testing.T, unit, prop string) string {
	t.Helper()
	out, _ := exec.Command("systemctl", "show", "-p", prop, "--value", unit).Output()
	return strings.TrimSpace(string(out))
}

func readUnit(t *testing.T, unit string) string {
	t.Helper()
	for _, dir := range []string{"/etc/systemd/system", "/run/systemd/system"} {
		if b, err := os.ReadFile(filepath.Join(dir, unit)); err == nil {
			return string(b)
		}
	}
	t.Fatalf("找不到 unit 文件 %s", unit)
	return ""
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(b)
}

func dumpDockerPackDiagnostics(ctx context.Context, t *testing.T) {
	t.Helper()
	for _, c := range [][]string{
		{"systemctl", "status", "--no-pager", "--full", dpUnit},
		{"journalctl", "-u", dpUnit, "--no-pager", "-n", "40"},
	} {
		out, _ := exec.CommandContext(ctx, c[0], c[1:]...).CombinedOutput()
		t.Logf("$ %s\n%s", strings.Join(c, " "), out)
	}
	if b, err := os.ReadFile(filepath.Join(dpConfRoot, "apps", dpComponent, "daemon.json")); err == nil {
		t.Logf("daemon.json:\n%s", b)
	}
}

func cleanupDockerPack(ctx context.Context, t *testing.T) {
	t.Helper()
	_ = exec.CommandContext(ctx, "systemctl", "stop", dpUnit).Run()
	_ = exec.CommandContext(ctx, "systemctl", "disable", dpUnit).Run()
	for _, dir := range []string{"/etc/systemd/system", "/run/systemd/system"} {
		_ = os.Remove(filepath.Join(dir, dpUnit))
	}
	_ = exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()
	_ = os.Remove(dpSocket)
	_ = os.RemoveAll(dpDataDir)
	_ = os.RemoveAll(dpConfRoot)
	_ = os.RemoveAll(dpEngineDir)
	restoreOuterDocker(ctx, t)
	// 根写错时东西会落到默认根下，一并清掉免得污染别的验收
	_ = os.RemoveAll("/opt/mecharion/apps/" + dpComponent)
	_ = os.RemoveAll("/etc/mecharion/apps/" + dpComponent)
}

// restoreOuterDocker 把外层 dockerd 的网络修回来。
//
// 本条验收用 `bridge: none` 起第二台 dockerd——那是嵌套环境里唯一可行的
// 设置（自定义网桥要求设备已存在，而容器里连 ip 命令都没有），代价是
// **它会清掉外层的 docker0**。
//
// 不修的后果不在这里，而是**之后所有 docker / compose 验收一起挂在**
//
//	adding interface vethX to bridge docker0 failed: Device does not exist
//
// 上——一个看起来与本条测试毫无关系的错误。测试扰动了什么就该修回什么。
func restoreOuterDocker(ctx context.Context, t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/sys/class/net/docker0"); err == nil {
		return // 没被动过
	}
	if out, err := exec.CommandContext(ctx, "systemctl", "restart", "docker").
		CombinedOutput(); err != nil {
		t.Logf("重启外层 docker（忽略）: %v\n%s", err, out)
	}

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat("/sys/class/net/docker0"); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
	// 报出来而不是静默放过：后面的验收会以一句看不懂的话失败
	t.Error("外层 docker0 没能恢复 —— 后续的 docker / compose 验收会跟着挂")
}
