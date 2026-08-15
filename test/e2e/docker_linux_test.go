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

// docker runtime 验收用的隔离目录，与 systemd 的测试分开。
const (
	dkComponent = "dweb"
	dkContainer = "mecharion-dweb-default"
	dkDataDir   = "/var/lib/mecharion-e2e-docker"
	dkPort      = 18090
	dkImage     = "m7n-e2e-webapp:1.0"
)

// requireDocker 在没有可用 dockerd 时跳过。
//
// 同一个测试二进制在两个节点镜像上都会被跑到：贫瘠的 test/node 上没有
// docker，跳过是对的；test/node-docker 上才真正执行。
func requireDocker(t *testing.T) {
	t.Helper()
	requireEnv(t)
	out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").
		CombinedOutput()
	if err != nil || len(out) == 0 {
		t.Skipf("本节点没有可用的 dockerd（用 ./hack/testenv.sh --docker up）: %s", out)
	}
}

// TestDockerRuntimeRunsWebapp 是 M4 第 3 步的验收：**单容器 webapp 跑起来**。
//
// 它走完整条路径：把镜像做成 blob → mechlet apply → docker load → create →
// start → 真的能收到 HTTP 响应。中间任何一环打桩，这条验收就换了个更容易
// 通过的题目。
//
// 用的是**同一个 reconciler、同一份规格结构**——与 systemd 的差别只在
// workload 那一段。这正是 Runtime 接缝要证明的事。
func TestDockerRuntimeRunsWebapp(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	cleanupDocker(ctx, t)
	t.Cleanup(func() { cleanupDocker(context.Background(), t) })

	sum := stageImageBlob(ctx, t)
	confDir := filepath.Join("/etc/mecharion-e2e-docker/apps", dkComponent)

	out, err := runMechlet(ctx, "apply",
		"-f", writeSpec(t, dockerSpec(sum, confDir)),
		"--data-dir", dkDataDir)
	if err != nil {
		dumpDockerDiagnostics(ctx, t)
		t.Fatalf("apply 失败: %v\n%s", err, out)
	}

	// ① 容器真的在跑
	if got := containerState(ctx, t); got != "running" {
		dumpDockerDiagnostics(ctx, t)
		t.Fatalf("容器状态应为 running，实际 %q", got)
	}

	// ② 标签齐全——这是「只操作自己的容器」那条纪律的依据
	labels := containerLabels(ctx, t)
	for k, want := range map[string]string{
		"dev.mecharion.managed-by": "mecharion",
		"dev.mecharion.component":  dkComponent,
		"dev.mecharion.role":       "default",
	} {
		if labels[k] != want {
			t.Errorf("标签 %s 应为 %q，实际 %q", k, want, labels[k])
		}
	}
	if labels["dev.mecharion.spec-digest"] == "" {
		t.Error("缺少 spec-digest 标签——没有它就判断不了要不要重建容器")
	}

	// ③ 服务真的能应答
	if body := waitContainerHTTP(t, dkPort, 60*time.Second); body == "" {
		dumpDockerDiagnostics(ctx, t)
		t.Fatal("容器里的服务没有应答")
	}
}

// TestDockerApplyIsIdempotent 钉住 digest 未变时不重建容器。
//
// 重建等于一次无谓的服务中断。这条在 docker 下比 systemd 下更要紧——
// 那边最多是多一次 daemon-reload，这边是容器真的被删了重建。
func TestDockerApplyIsIdempotent(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	cleanupDocker(ctx, t)
	t.Cleanup(func() { cleanupDocker(context.Background(), t) })

	sum := stageImageBlob(ctx, t)
	confDir := filepath.Join("/etc/mecharion-e2e-docker/apps", dkComponent)
	specPath := writeSpec(t, dockerSpec(sum, confDir))

	if out, err := runMechlet(ctx, "apply", "-f", specPath, "--data-dir", dkDataDir); err != nil {
		t.Fatalf("首次 apply: %v\n%s", err, out)
	}
	first := containerID(ctx, t)
	if first == "" {
		t.Fatal("容器没建起来")
	}

	if out, err := runMechlet(ctx, "apply", "-f", specPath, "--data-dir", dkDataDir); err != nil {
		t.Fatalf("再次 apply: %v\n%s", err, out)
	}
	if second := containerID(ctx, t); second != first {
		t.Errorf("规格没变却重建了容器：%s → %s\n"+
			"  那是一次无谓的服务中断", short12(first), short12(second))
	}
}

// TestDockerConfigChangeRecreates 钉住**容器不可变**。
//
// env / port / command 在创建时固化，`docker update` 改不了它们——
// 配置变了只能删掉重建。
func TestDockerConfigChangeRecreates(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	cleanupDocker(ctx, t)
	t.Cleanup(func() { cleanupDocker(context.Background(), t) })

	sum := stageImageBlob(ctx, t)
	confDir := filepath.Join("/etc/mecharion-e2e-docker/apps", dkComponent)

	if out, err := runMechlet(ctx, "apply",
		"-f", writeSpec(t, dockerSpec(sum, confDir)),
		"--data-dir", dkDataDir); err != nil {
		t.Fatalf("首次 apply: %v\n%s", err, out)
	}
	first := containerID(ctx, t)

	// 改一个环境变量：期望状态变了
	s := dockerSpec(sum, confDir)
	w := s["workload"].(map[string]any)["docker"].(map[string]any)
	w["env"] = map[string]string{"LOG_LEVEL": "debug"}

	if out, err := runMechlet(ctx, "apply",
		"-f", writeSpec(t, s), "--data-dir", dkDataDir); err != nil {
		dumpDockerDiagnostics(ctx, t)
		t.Fatalf("改配置后 apply: %v\n%s", err, out)
	}

	second := containerID(ctx, t)
	if second == first {
		t.Error("配置变了却没重建容器——env 在创建时就固化了，改不了")
	}
	if !strings.Contains(containerEnv(ctx, t), "LOG_LEVEL=debug") {
		t.Error("新容器应当带上新的环境变量")
	}
}

// TestDockerRefusesForeignContainer 是**负向测试**：同名但无标签的容器一律不碰。
//
// ADR-0011 把标签纪律列为高风险点：这台 dockerd 默认是用户自己的，
// 漏一处就可能删掉与 Mecharion 无关的生产容器。
func TestDockerRefusesForeignContainer(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	cleanupDocker(ctx, t)
	t.Cleanup(func() { cleanupDocker(context.Background(), t) })

	sum := stageImageBlob(ctx, t)
	confDir := filepath.Join("/etc/mecharion-e2e-docker/apps", dkComponent)

	// 用户自己建了一个同名容器（没有 Mecharion 标签）。
	//
	// stageImageBlob 刻意把镜像从本地删掉了（好让 Materialize 真的走一遍
	// docker load），因此这里要先把它装回来才能建容器。
	blobPath := filepath.Join(dkDataDir, "blobs", "sha256", sum[:2], sum)
	if out, err := exec.CommandContext(ctx, "docker", "load",
		"-i", blobPath).CombinedOutput(); err != nil {
		t.Fatalf("装回测试镜像: %v\n%s", err, out)
	}
	if out, err := exec.CommandContext(ctx, "docker", "create",
		"--name", dkContainer, "--label", "com.example.owner=someone-else",
		dkImage, "--help").CombinedOutput(); err != nil {
		t.Fatalf("造一个「用户自己的」容器: %v\n%s", err, out)
	}
	before := containerID(ctx, t)

	out, err := runMechlet(ctx, "apply",
		"-f", writeSpec(t, dockerSpec(sum, confDir)),
		"--data-dir", dkDataDir)
	if err == nil {
		t.Fatal("同名的外来容器应当让 apply 失败，而不是被悄悄删掉")
	}
	if !strings.Contains(out, "标签") {
		t.Errorf("错误信息应说清是标签问题，实际:\n%s", out)
	}

	// **它必须还在**
	if after := containerID(ctx, t); after != before {
		t.Fatalf("用户自己的容器被动了！%s → %s", short12(before), short12(after))
	}
}

// ── 夹具 ────────────────────────────────────────────────────────────────

// stageImageBlob 把 webapp 夹具做成一个容器镜像 blob。
//
// 这正是 Pack 的 `blobs` 在容器场景下的形态：`docker save` 的 tar 就是
// 一个普通 blob，走与裸机完全相同的内容寻址分发链路（ADR-0011）——
// **不需要 registry**。
func stageImageBlob(ctx context.Context, t *testing.T) string {
	return stageImageBlobRoot(ctx, t, dkDataDir)
}

// stageImageBlobRoot 同上，但落在指定的数据目录下——compose 的验收
// 用自己的隔离目录，两套测试不能共用 blob 库。
func stageImageBlobRoot(ctx context.Context, t *testing.T, root string) string {
	t.Helper()

	build := filepath.Join(t.TempDir(), "img")
	if err := os.MkdirAll(build, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(binDir, "webapp")
	body, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("找不到 webapp 夹具（先 make e2ebin）: %v", err)
	}
	if err := os.WriteFile(filepath.Join(build, "webapp"), body, 0o755); err != nil {
		t.Fatal(err)
	}
	// FROM scratch：静态二进制不需要任何基础镜像，也就**不需要联网**
	dockerfile := "FROM scratch\nCOPY webapp /webapp\nENTRYPOINT [\"/webapp\"]\n"
	if err := os.WriteFile(filepath.Join(build, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
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
	blob, err := os.ReadFile(tar)
	if err != nil {
		t.Fatal(err)
	}
	// 镜像已经在本地了，但仍然走一遍 load —— 那是 Materialize 的真实路径
	if err := exec.CommandContext(ctx, "docker", "rmi", "-f", dkImage).Run(); err != nil {
		t.Logf("清理镜像（忽略）: %v", err)
	}
	return installBlobIn(t, root, blob)
}

// dockerSpec 造一份 runtime: docker 的已解析规格。
//
// 除 workload 那一段外，与 systemd 的规格**结构完全相同**——
// 同一个 reconciler、同一套 paths、同一套健康检查。
func dockerSpec(blobSum, confDir string) map[string]any {
	home := filepath.Join(dkDataDir, "apps", dkComponent)
	dataDir := filepath.Join(dkDataDir, "data", dkComponent)

	return map[string]any{
		"schemaVersion": 1,
		"site":          map[string]any{"name": "e2e", "kind": "standalone"},
		"component":     dkComponent,
		"role":          "default",
		"configGroup":   "default",
		"node":          map[string]any{"name": "node-1", "address": "127.0.0.1"},
		"ordinal":       0,
		"pack": map[string]any{
			"name": "go-webapp", "version": "1.2.0", "revision": 1,
		},
		"params": map[string]any{
			"port": map[string]any{"value": dkPort, "type": "port"},
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
				"id": "template:app.yaml", "type": "template", "origin": "role",
				"args": map[string]any{
					"dest": confDir + "/app.yaml",
					"content": fmt.Sprintf(
						"# 由 Mecharion 渲染\nport: %d\nlog_level: info\n", dkPort),
					"mode": "0644",
				},
				"driftPolicy": "report",
			},
		},
		"workload": map[string]any{
			"runtime": "docker",
			"docker": map[string]any{
				"imageBlob": "image",
				"args":      []string{"--config", "/etc/app/app.yaml"},
				// **挂稳定路径，不挂 .Paths.Current**：docker 在创建容器时
				// 解析路径，软链会被绑死在当时那个 generation 上（规则 R52）
				"mounts": []map[string]any{
					{"from": confDir, "to": "/etc/app", "readOnly": true},
					{"from": dataDir, "to": "/data"},
				},
				"ports": []map[string]any{
					{"host": fmt.Sprintf("%d", dkPort), "container": dkPort, "protocol": "tcp"},
				},
				"restart": "unless-stopped",
			},
		},
		"health": map[string]any{
			"http":         map[string]any{"path": "/healthz", "port": dkPort},
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

// ── 观察 ────────────────────────────────────────────────────────────────

func dockerInspect(ctx context.Context, t *testing.T, format string) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", format, dkContainer).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func containerState(ctx context.Context, t *testing.T) string {
	return dockerInspect(ctx, t, "{{.State.Status}}")
}

func containerID(ctx context.Context, t *testing.T) string {
	return dockerInspect(ctx, t, "{{.Id}}")
}

func containerEnv(ctx context.Context, t *testing.T) string {
	return dockerInspect(ctx, t, "{{json .Config.Env}}")
}

func containerLabels(ctx context.Context, t *testing.T) map[string]string {
	t.Helper()
	raw := dockerInspect(ctx, t, "{{json .Config.Labels}}")
	out := map[string]string{}
	if raw == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Logf("解析标签: %v", err)
	}
	return out
}

// waitContainerHTTP 等容器里的服务应答。
//
// 与 systemd 的 waitHTTP 是同一件事，只是这里探的是**发布出来的端口**
// ——从宿主机看，容器化与裸机的服务没有区别，这正是 http 探针不需要
// 进 Runtime 接口的原因（ADR-0032）。
func waitContainerHTTP(t *testing.T, port int, d time.Duration) string {
	return waitHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", port), d)
}

func short12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func dumpDockerDiagnostics(ctx context.Context, t *testing.T) {
	t.Helper()
	for _, c := range [][]string{
		{"docker", "ps", "-a", "--filter", "name=" + dkContainer},
		{"docker", "logs", "--tail", "30", dkContainer},
		{"docker", "inspect", "--format", "{{json .State}}", dkContainer},
	} {
		out, _ := exec.CommandContext(ctx, c[0], c[1:]...).CombinedOutput()
		t.Logf("$ %s\n%s", strings.Join(c, " "), out)
	}
}

func cleanupDocker(ctx context.Context, t *testing.T) {
	t.Helper()
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", dkContainer).Run()
	_ = exec.CommandContext(ctx, "docker", "rmi", "-f", dkImage).Run()
	_ = os.RemoveAll(dkDataDir)
	_ = os.RemoveAll("/etc/mecharion-e2e-docker")
}

// TestDockerExecProbeRunsInsideContainer 是 M4 第 5 步的验收，
// 也是 **ADR-0032 的实证**。
//
// 探针要执行的命令**只存在于镜像里**——宿主机上没有这个文件。
// 如果 exec 探针在宿主机上跑，它必然失败；只有真的 `docker exec` 进容器
// 才能通过。因此这条测试同时证明了两件事：
//
//	① ExecIn 的 docker 实现确实进了容器
//	② 「健康检查跨 Runtime 行为一致」这条承诺真正成立——
//	   同一份 health.exec 声明，在 systemd 与 docker 下都能用
func TestDockerExecProbeRunsInsideContainer(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	cleanupDocker(ctx, t)
	t.Cleanup(func() { cleanupDocker(context.Background(), t) })

	sum := stageImageBlob(ctx, t)
	confDir := filepath.Join("/etc/mecharion-e2e-docker/apps", dkComponent)

	// 宿主机上**没有** /webapp——它只在镜像里。
	// 这正是「exec 探针必须进容器」的最小证明。
	if _, err := os.Stat("/webapp"); err == nil {
		t.Fatal("宿主机上不该有 /webapp，否则这条测试证明不了任何东西")
	}

	s := dockerSpec(sum, confDir)
	s["health"] = map[string]any{
		"exec":         map[string]any{"command": []string{"/webapp", "--help"}},
		"startupGrace": "30s",
	}

	out, err := runMechlet(ctx, "apply",
		"-f", writeSpec(t, s), "--data-dir", dkDataDir)
	if err != nil {
		dumpDockerDiagnostics(ctx, t)
		t.Fatalf("exec 探针应当在容器里跑通: %v\n%s", err, out)
	}
	if !strings.Contains(out, "health check passed") {
		t.Errorf("报告里应显示健康检查通过，实际:\n%s", out)
	}
}

// TestDockerExecProbeFailsHonestly 钉住探针**真的在探**。
//
// 上一条只证明了「能跑通」；这条证明它不是永远返回成功——
// 一个永远通过的健康检查比没有健康检查更糟，它会让人以为服务是好的。
func TestDockerExecProbeFailsHonestly(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	cleanupDocker(ctx, t)
	t.Cleanup(func() { cleanupDocker(context.Background(), t) })

	sum := stageImageBlob(ctx, t)
	confDir := filepath.Join("/etc/mecharion-e2e-docker/apps", dkComponent)

	s := dockerSpec(sum, confDir)
	// 容器里没有这个命令
	s["health"] = map[string]any{
		"exec":         map[string]any{"command": []string{"/definitely-not-here"}},
		"startupGrace": "5s",
	}

	out, err := runMechlet(ctx, "apply",
		"-f", writeSpec(t, s), "--data-dir", dkDataDir)
	if err == nil {
		t.Fatalf("探针指向一个不存在的命令时，调和应当失败\n%s", out)
	}
	if !strings.Contains(out, "health check") {
		t.Errorf("失败原因应指明是健康检查，实际:\n%s", out)
	}
}
