//go:build linux

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// compose 验收用的隔离目录，与 docker、systemd 的测试都分开。
const (
	cpComponent = "cshop"
	cpProject   = "mecharion-cshop"
	cpDataDir   = "/var/lib/mecharion-e2e-compose"
	cpConfRoot  = "/etc/mecharion-e2e-compose"
	cpWebPort   = 18110
	cpSidePort  = 18111
)

// TestComposeRuntimeRunsTwoServices 是 M4 第 6 步的验收：
// **一个双服务 project 跑起来**。
//
// 双服务而非单服务是刻意的。单服务的 compose 与 docker 几乎没有区别，
// 证明不了这个 Runtime 真正要证明的东西：
//
//	① 标签能进到**每一个** service 的容器（compose 没有 --label，
//	   只能靠生成的 override 文件）
//	② exec 标签只落在 execService 那一个上——多打一个，探针就可能
//	   进错容器
//	③ Observe 是**聚合**的：两个容器归一成一个 project 状态
func TestComposeRuntimeRunsTwoServices(t *testing.T) {
	requireDocker(t)
	requireCompose(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cleanupCompose(ctx, t)
	t.Cleanup(func() { cleanupCompose(context.Background(), t) })

	sum := stageImageBlobRoot(ctx, t, cpDataDir)
	confDir := filepath.Join(cpConfRoot, "apps", cpComponent)

	out, err := runMechlet(ctx, "apply",
		"-f", writeSpec(t, composeSpec(sum, confDir)),
		"--data-dir", cpDataDir)
	if err != nil {
		dumpComposeDiagnostics(ctx, t)
		t.Fatalf("apply 失败: %v\n%s", err, out)
	}

	// ① 两个 service 的容器都在跑
	states := composeStates(ctx, t)
	if len(states) != 2 {
		dumpComposeDiagnostics(ctx, t)
		t.Fatalf("project 应有 2 个容器，实际 %d 个: %v", len(states), states)
	}
	for name, st := range states {
		if st != "running" {
			dumpComposeDiagnostics(ctx, t)
			t.Fatalf("容器 %s 状态应为 running，实际 %q", name, st)
		}
	}

	// ② 标签进到了每一个容器
	for name := range states {
		labels := composeLabels(ctx, t, name)
		for k, want := range map[string]string{
			"dev.mecharion.managed-by": "mecharion",
			"dev.mecharion.component":  cpComponent,
			"dev.mecharion.role":       "default",
		} {
			if labels[k] != want {
				t.Errorf("容器 %s 的标签 %s 应为 %q，实际 %q", name, k, want, labels[k])
			}
		}
		if labels["dev.mecharion.spec-digest"] == "" {
			t.Errorf("容器 %s 缺少 spec-digest 标签", name)
		}
	}

	// ③ exec 标签**只在 execService 上**
	var tagged []string
	for name := range states {
		if composeLabels(ctx, t, name)["dev.mecharion.exec"] == "true" {
			tagged = append(tagged, name)
		}
	}
	if len(tagged) != 1 {
		t.Errorf("exec 标签应当只落在 execService 一个容器上，实际 %v", tagged)
	} else if !strings.Contains(tagged[0], "web") {
		t.Errorf("exec 标签应落在 web 上（execService=web），实际 %s", tagged[0])
	}

	// ④ 两个 service 都真的在服务
	for _, port := range []int{cpWebPort, cpSidePort} {
		if body := waitContainerHTTP(t, port, 60*time.Second); body == "" {
			dumpComposeDiagnostics(ctx, t)
			t.Fatalf("端口 %d 上的服务没有应答", port)
		}
	}
}

// TestComposeApplyIsIdempotent 钉住 digest 未变时不重建 project。
//
// compose 的 `up` 自己也会做差异化，但那不够：它比的是配置，而我们比的是
// **期望状态的摘要**。少了这一层，每轮调和都会去 fork 一次 compose，
// 而 compose 的 up 在某些版本下会重建健康检查未就绪的容器。
func TestComposeApplyIsIdempotent(t *testing.T) {
	requireDocker(t)
	requireCompose(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cleanupCompose(ctx, t)
	t.Cleanup(func() { cleanupCompose(context.Background(), t) })

	sum := stageImageBlobRoot(ctx, t, cpDataDir)
	confDir := filepath.Join(cpConfRoot, "apps", cpComponent)
	specPath := writeSpec(t, composeSpec(sum, confDir))

	if out, err := runMechlet(ctx, "apply", "-f", specPath, "--data-dir", cpDataDir); err != nil {
		dumpComposeDiagnostics(ctx, t)
		t.Fatalf("首次 apply: %v\n%s", err, out)
	}
	first := composeIDs(ctx, t)
	if len(first) != 2 {
		t.Fatalf("project 应有 2 个容器，实际 %v", first)
	}

	if out, err := runMechlet(ctx, "apply", "-f", specPath, "--data-dir", cpDataDir); err != nil {
		dumpComposeDiagnostics(ctx, t)
		t.Fatalf("再次 apply: %v\n%s", err, out)
	}
	second := composeIDs(ctx, t)

	for name, id := range first {
		if second[name] != id {
			t.Errorf("容器 %s 被重建了（%s → %s）——digest 没变就不该动它",
				name, short12(id), short12(second[name]))
		}
	}
}

// TestComposeRefusesForeignProject 是 compose 侧的**标签纪律验收**。
//
// `docker compose -p X down` 会拆掉整个 project，不挑着删。因此这条比
// docker 那条更要紧：那边最多误删一个容器，这边是一整组。
func TestComposeRefusesForeignProject(t *testing.T) {
	requireDocker(t)
	requireCompose(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cleanupCompose(ctx, t)
	t.Cleanup(func() { cleanupCompose(context.Background(), t) })

	sum := stageImageBlobRoot(ctx, t, cpDataDir)
	confDir := filepath.Join(cpConfRoot, "apps", cpComponent)

	// 镜像已被 stageImageBlobRoot 删掉了，先装回来
	loadStagedImage(ctx, t, cpDataDir, sum)
	foreign := startForeignProject(ctx, t)

	out, err := runMechlet(ctx, "apply",
		"-f", writeSpec(t, composeSpec(sum, confDir)),
		"--data-dir", cpDataDir)
	if err == nil {
		t.Fatalf("同名 project 不是我们的时，apply 应当失败\n%s", out)
	}
	if !strings.Contains(out, "标签") {
		t.Errorf("失败原因应说清是标签问题，实际:\n%s", out)
	}

	// **那个容器必须还在**——这才是这条测试真正要保护的东西
	if id := containerIDOf(ctx, foreign); id == "" {
		t.Fatal("外来容器被删掉了 —— 那正是这条纪律要防止的事")
	}
}

// TestComposeExecProbeEntersExecService 钉住 exec 探针进的是 execService。
//
// 探针命令**只存在于镜像里**，宿主机上没有。同 docker 那条，这既证明
// ExecIn 真的进了容器，也证明「健康检查跨 Runtime 行为一致」在 compose
// 下同样成立。
func TestComposeExecProbeEntersExecService(t *testing.T) {
	requireDocker(t)
	requireCompose(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cleanupCompose(ctx, t)
	t.Cleanup(func() { cleanupCompose(context.Background(), t) })

	if _, err := os.Stat("/webapp"); err == nil {
		t.Fatal("宿主机上不该有 /webapp，否则这条测试证明不了任何东西")
	}

	sum := stageImageBlobRoot(ctx, t, cpDataDir)
	confDir := filepath.Join(cpConfRoot, "apps", cpComponent)

	s := composeSpec(sum, confDir)
	s["health"] = map[string]any{
		"exec":         map[string]any{"command": []string{"/webapp", "--help"}},
		"startupGrace": "30s",
	}

	out, err := runMechlet(ctx, "apply", "-f", writeSpec(t, s), "--data-dir", cpDataDir)
	if err != nil {
		dumpComposeDiagnostics(ctx, t)
		t.Fatalf("exec 探针应当在 execService 的容器里跑通: %v\n%s", err, out)
	}
	if !strings.Contains(out, "health check passed") {
		t.Errorf("报告里应显示健康检查通过，实际:\n%s", out)
	}
}

// ── 夹具 ────────────────────────────────────────────────────────────────

// startForeignProject 用 compose **自己**造一个同名 project，返回它的容器名。
//
// 手工 `docker create` 打上 project 标签是不够的：试过了，compose 不认那种
// 容器——`down` 与 `--remove-orphans` 都不会碰它，于是「守卫拿掉后测试照样
// 通过」，测的其实是 compose 的自我保护而不是我们的纪律。
//
// 只有 compose 自己建出来的容器才带全那一套标签（config-hash、oneoff、
// version……），也只有那种容器会被 `down` 拆掉。**这才是真正的破坏路径。**
func startForeignProject(ctx context.Context, t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	file := filepath.Join(dir, "foreign.yaml")
	body := fmt.Sprintf(`services:
  legacy:
    image: %s
    entrypoint: ["/webapp"]
    command: ["--config", "/nonexistent-is-fine"]
`, dkImage)
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.CommandContext(ctx, "docker", "compose",
		"-p", cpProject, "-f", file, "up", "--no-start").CombinedOutput()
	if err != nil {
		t.Fatalf("造外来 project: %v\n%s", err, out)
	}

	name := cpProject + "-legacy-1"
	if containerIDOf(ctx, name) == "" {
		t.Fatalf("外来 project 没建起来，现有容器: %v", composeContainers(ctx, t))
	}
	return name
}

// requireCompose 在没有 compose 插件时跳过。
func requireCompose(t *testing.T) {
	t.Helper()
	out, err := exec.Command("docker", "compose", "version", "--short").CombinedOutput()
	if err != nil || len(out) == 0 {
		t.Skipf("本节点没有 compose 插件: %s", out)
	}
}

// composeSpec 造一份 runtime: compose 的已解析规格。
//
// compose 文件本身作为一条 **template 资源**下发——这正是渲染流水线为
// `runtime: compose` 自动产出的那条（19-container-runtime §6.6.1）。
// 这里手写它，是因为 e2e 直接喂已解析规格；形状必须与流水线产出的一致，
// 否则这条验收测的就不是真实路径。
func composeSpec(blobSum, confDir string) map[string]any {
	home := filepath.Join(cpDataDir, "apps", cpComponent)
	dataDir := filepath.Join(cpDataDir, "data", cpComponent)

	return map[string]any{
		"schemaVersion": 1,
		"site":          map[string]any{"name": "e2e", "kind": "standalone"},
		"component":     cpComponent,
		"role":          "default",
		"configGroup":   "default",
		"node":          map[string]any{"name": "node-1", "address": "127.0.0.1"},
		"ordinal":       0,
		"pack": map[string]any{
			"name": "shop", "version": "1.0.0", "revision": 1,
		},
		"params": map[string]any{
			"port": map[string]any{"value": cpWebPort, "type": "port"},
		},
		"paths": map[string]any{
			"home": map[string]any{
				"name": "home", "values": []string{home}, "kind": "single", "mode": "0755",
			},
			"config": map[string]any{
				"name": "config", "values": []string{confDir}, "kind": "single", "mode": "0755",
			},
			"data": map[string]any{
				"name": "data", "values": []string{dataDir}, "kind": "single", "mode": "0755",
			},
		},
		"blobs": []map[string]any{{
			"name": "image", "sha256": blobSum, "size": 0,
			"filename": "webapp-image.tar", "mediaType": "docker-archive",
		}},
		"resources": []map[string]any{
			{
				"id": "template:" + confDir + "/web.yaml", "type": "template", "origin": "role",
				"args": map[string]any{
					"dest":    confDir + "/web.yaml",
					"content": fmt.Sprintf("port: %d\nlog_level: info\n", cpWebPort),
					"mode":    "0644",
				},
				"driftPolicy": "reconcile",
			},
			{
				"id": "template:" + confDir + "/sidecar.yaml", "type": "template", "origin": "role",
				"args": map[string]any{
					"dest":    confDir + "/sidecar.yaml",
					"content": fmt.Sprintf("port: %d\nlog_level: warn\n", cpSidePort),
					"mode":    "0644",
				},
				"driftPolicy": "reconcile",
			},
			{
				// 渲染流水线自动产出的那条
				"id": "template:" + confDir + "/compose.yaml", "type": "template", "origin": "role",
				"args": map[string]any{
					"dest":    confDir + "/compose.yaml",
					"content": composeFileBody(confDir),
					"mode":    "0644",
				},
				"driftPolicy": "reconcile",
			},
		},
		"workload": map[string]any{
			"runtime": "compose",
			"compose": map[string]any{
				// 已解析规格里是**绝对路径**，不是模板名
				"file":        confDir + "/compose.yaml",
				"imageBlobs":  []string{"image"},
				"projectName": cpProject,
				// 两个 service，必须指定进哪一个
				"execService": "web",
			},
		},
		"health": map[string]any{
			"http":         map[string]any{"path": "/healthz", "port": cpWebPort},
			"startupGrace": "30s",
		},
		"topology": map[string]any{
			"roles": map[string]any{
				"default": []map[string]any{
					{"node": "node-1", "address": "127.0.0.1", "ordinal": 0},
				},
			},
		},
		"reconcile": map[string]any{"retainGenerations": 3},
	}
}

// composeFileBody 是那份双服务 compose 文件。
//
// 两个 service 用**同一个镜像**、不同配置——这样只需要一个离线 blob，
// 而「两个容器」这件要验证的事一点不打折。
func composeFileBody(confDir string) string {
	return fmt.Sprintf(`# 由 Mecharion 渲染
services:
  web:
    image: %[1]s
    command: ["--config", "/etc/app/web.yaml"]
    volumes:
      - %[2]s:/etc/app:ro
    ports:
      - "%[3]d:%[3]d"
  sidecar:
    image: %[1]s
    command: ["--config", "/etc/app/sidecar.yaml"]
    volumes:
      - %[2]s:/etc/app:ro
    ports:
      - "%[4]d:%[4]d"
`, dkImage, confDir, cpWebPort, cpSidePort)
}

// loadStagedImage 把已 stage 的镜像装回本地镜像库。
//
// stageImageBlobRoot 做完会 `docker rmi` —— 那是为了让 Materialize 真的走一遍
// load。要手工造外来容器时得先把镜像装回来。
func loadStagedImage(ctx context.Context, t *testing.T, root, sum string) {
	t.Helper()
	tar := filepath.Join(root, "blobs", "sha256", sum[:2], sum)
	if out, err := exec.CommandContext(ctx, "docker", "load", "-i", tar).CombinedOutput(); err != nil {
		t.Fatalf("装回测试镜像: %v\n%s", err, out)
	}
}

// ── 观察 ────────────────────────────────────────────────────────────────

// composeContainers 列出 project 下的容器名。
func composeContainers(ctx context.Context, t *testing.T) []string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", "ps", "--all", "--format", "{{.Names}}",
		"--filter", "label=com.docker.compose.project="+cpProject).Output()
	if err != nil {
		return nil
	}
	var names []string
	for _, l := range strings.Split(string(out), "\n") {
		if s := strings.TrimSpace(l); s != "" {
			names = append(names, s)
		}
	}
	return names
}

func composeStates(ctx context.Context, t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, n := range composeContainers(ctx, t) {
		out[n] = inspectOf(ctx, n, "{{.State.Status}}")
	}
	return out
}

func composeIDs(ctx context.Context, t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, n := range composeContainers(ctx, t) {
		out[n] = inspectOf(ctx, n, "{{.Id}}")
	}
	return out
}

func composeLabels(ctx context.Context, t *testing.T, name string) map[string]string {
	t.Helper()
	raw := inspectOf(ctx, name, "{{json .Config.Labels}}")
	out := map[string]string{}
	if raw == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Logf("解析标签: %v", err)
	}
	return out
}

func containerIDOf(ctx context.Context, name string) string {
	return inspectOf(ctx, name, "{{.Id}}")
}

func inspectOf(ctx context.Context, name, format string) string {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", format, name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func dumpComposeDiagnostics(ctx context.Context, t *testing.T) {
	t.Helper()
	for _, c := range [][]string{
		{"docker", "ps", "-a", "--filter", "label=com.docker.compose.project=" + cpProject},
		{"docker", "compose", "-p", cpProject, "logs", "--tail", "30", "--no-color"},
	} {
		out, _ := exec.CommandContext(ctx, c[0], c[1:]...).CombinedOutput()
		t.Logf("$ %s\n%s", strings.Join(c, " "), out)
	}
}

func cleanupCompose(ctx context.Context, t *testing.T) {
	t.Helper()
	_ = exec.CommandContext(ctx, "docker", "compose", "-p", cpProject,
		"down", "--remove-orphans", "--timeout", "3").Run()
	for _, n := range composeContainers(ctx, t) {
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", n).Run()
	}
	_ = exec.CommandContext(ctx, "docker", "rmi", "-f", dkImage).Run()
	_ = os.RemoveAll(cpDataDir)
	_ = os.RemoveAll(cpConfRoot)
}
