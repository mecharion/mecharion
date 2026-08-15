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

// 镜像回收的验收用一套独立的路径，与既有的 docker 验收完全隔开。
const (
	gcComponent = "gcweb"
	gcContainer = "mecharion-gcweb-default"
	gcDataDir   = "/var/lib/mecharion-e2e-imagegc"
	gcConfRoot  = "/etc/mecharion-e2e-imagegc"
	gcPort      = 18094
	gcImageBase = "m7n-e2e-gc"

	// 共用同一个镜像的第二个组件。
	shComponent = "gcshare"
	shContainer = "mecharion-gcshare-default"
	shPort      = 18095
)

// TestRetainedGenerationImagesSurvive 是 **M6 第 7 步的验收**，
// 覆盖验收表第 7、8 行。
//
// 一次升级把旧 generation 留在盘上，是为了**能退回去**。而对容器化的
// 工作负载来说，「能退回去」等于「那一代的镜像还在」——目录留着而镜像
// 被删掉，回滚会在最坏的时刻失败：服务已经停了，新版起不来，旧版也
// 装不回来（22-upgrade §2.5）。
//
// 因此这条测试连着核对两件相反的事：
//
//	保留中的三代，镜像**都还在**       ← 这一步的意义
//	被回收掉的那一代，镜像**没了**     ← 否则「回收」是句空话
//
// 四个镜像的内容各不相同，因此是四个不同的 blob、四个不同的 digest、
// 四代 generation——`retainGenerations: 3` 于是必然淘汰第一代。
func TestRetainedGenerationImagesSurvive(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	cleanupImageGC(ctx, t)
	t.Cleanup(func() { cleanupImageGC(context.Background(), t) })

	confDir := filepath.Join(gcConfRoot, "apps", gcComponent)
	tags := make([]string, 4)
	for i := range tags {
		tags[i] = fmt.Sprintf("%s:%d.0", gcImageBase, i+1)
		sum := stageGCImage(ctx, t, tags[i], i+1)
		out, err := runMechlet(ctx, "apply",
			"-f", writeSpec(t, gcSpec(sum, confDir, i+1)),
			"--data-dir", gcDataDir)
		if err != nil {
			dumpImageGCDiagnostics(ctx, t)
			t.Fatalf("第 %d 次 apply 失败: %v\n%s", i+1, err, out)
		}
	}

	// ── 台账：三代保留，各自记着自己的镜像 ──
	gens := gcGenerations(t)
	if len(gens) != 3 {
		t.Fatalf("应当保留 3 代，实际 %d 代: %+v", len(gens), gens)
	}
	for _, g := range gens {
		if len(g.Images) == 0 {
			t.Errorf("generation %04d 没记下它的镜像——回收就无从判断该留谁", g.Seq)
		}
	}

	// ── 保留中的三代，镜像都还在 ──
	for _, tag := range tags[1:] {
		if !imageExists(ctx, tag) {
			dumpImageGCDiagnostics(ctx, t)
			t.Errorf("保留中的 generation 引用的镜像 %s 不该被删——"+
				"没有它那一代就回滚不了", tag)
		}
	}

	// ── 被回收的那一代，镜像也该没了 ──
	//
	// 回收发生在 apply 末尾；再等一轮是因为「删不掉就留待下次」是允许的，
	// 但不该一直删不掉。
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return !imageExists(ctx, tags[0])
	}) {
		dumpImageGCDiagnostics(ctx, t)
		t.Errorf("被回收的 generation 的镜像 %s 应当一并回收", tags[0])
	}

	// ── 服务仍然在跑：回收不该碰到活着的东西 ──
	if got := gcContainerState(ctx); got != "running" {
		dumpImageGCDiagnostics(ctx, t)
		t.Errorf("回收之后容器状态应为 running，实际 %q", got)
	}
}

// TestReclaimNeverTouchesForeignImages 钉住「只删我们放进去的」。
//
// 镜像没有标签可依（容器有 managed-by，镜像没有），因此判据只能是
// 「它进过某一代台账」。一个用户自己 build 的同前缀镜像必须毫发无损——
// 误删一个镜像要靠重新分发几百 MB 来补（22-upgrade §2.5 ④）。
func TestReclaimNeverTouchesForeignImages(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	cleanupImageGC(ctx, t)
	t.Cleanup(func() { cleanupImageGC(context.Background(), t) })

	// 用户自己的镜像，名字**故意长得像我们的**
	foreign := gcImageBase + ":users-own"
	buildImage(ctx, t, foreign, 99)
	t.Cleanup(func() {
		_ = exec.Command("docker", "rmi", "-f", foreign).Run()
	})

	confDir := filepath.Join(gcConfRoot, "apps", gcComponent)
	for i := 0; i < 4; i++ {
		sum := stageGCImage(ctx, t, fmt.Sprintf("%s:%d.0", gcImageBase, i+1), i+1)
		if out, err := runMechlet(ctx, "apply",
			"-f", writeSpec(t, gcSpec(sum, confDir, i+1)),
			"--data-dir", gcDataDir); err != nil {
			t.Fatalf("第 %d 次 apply 失败: %v\n%s", i+1, err, out)
		}
	}

	if !imageExists(ctx, foreign) {
		t.Errorf("用户自己的镜像 %s 被删了——它从没进过任何一代台账", foreign)
	}
}

// TestSharedImageSurvivesAnotherComponentPrune 钉住「判据是全局的」。
//
// 一个镜像可以被**多个组件**共用——那是内容寻址分发的直接后果，也是
// 离线场景下最常见的形态（同一个 JDK、同一个 base image）。因此
// 「某一代被回收了」不等于「它的镜像可以删」：另一个组件可能正跑着它。
//
// 这条与上一条互补：上一条证明该删的删了，这条证明**不该删的没删**。
// 少了它，一个「清单里有什么就删什么」的实现也能通过全部验收，而它会在
// 真实机器上删掉正在运行的组件的镜像（22-upgrade §2.5 ③）。
func TestSharedImageSurvivesAnotherComponentPrune(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	cleanupImageGC(ctx, t)
	cleanupShared(ctx, t)
	t.Cleanup(func() {
		cleanupImageGC(context.Background(), t)
		cleanupShared(context.Background(), t)
	})

	confDir := filepath.Join(gcConfRoot, "apps", gcComponent)
	shConfDir := filepath.Join(gcConfRoot, "apps", shComponent)

	// 第一代的镜像**同时**被另一个组件用着
	shared := fmt.Sprintf("%s:1.0", gcImageBase)
	sharedSum := stageGCImage(ctx, t, shared, 1)

	if out, err := runMechlet(ctx, "apply",
		"-f", writeSpec(t, sharedSpec(sharedSum, shConfDir)),
		"--data-dir", gcDataDir); err != nil {
		dumpImageGCDiagnostics(ctx, t)
		t.Fatalf("部署共用镜像的组件失败: %v\n%s", err, out)
	}

	// gcweb 从这个镜像起步，然后连升三代把它挤出保留窗口
	if out, err := runMechlet(ctx, "apply",
		"-f", writeSpec(t, gcSpec(sharedSum, confDir, 1)),
		"--data-dir", gcDataDir); err != nil {
		t.Fatalf("首次 apply 失败: %v\n%s", err, out)
	}
	for i := 2; i <= 4; i++ {
		sum := stageGCImage(ctx, t, fmt.Sprintf("%s:%d.0", gcImageBase, i), i)
		if out, err := runMechlet(ctx, "apply",
			"-f", writeSpec(t, gcSpec(sum, confDir, i)),
			"--data-dir", gcDataDir); err != nil {
			t.Fatalf("第 %d 次 apply 失败: %v\n%s", i, err, out)
		}
	}

	// gcweb 的第一代已经被回收（保留 3 代）
	if gens := gcGenerations(t); len(gens) != 3 {
		t.Fatalf("gcweb 应当只剩 3 代，实际 %d", len(gens))
	}

	// **但镜像必须还在**：另一个组件正跑着它。
	//
	// 这一条上有两道独立的防线：我们的引用判据，以及 dockerd 自己拒绝删除
	// 正被容器使用的镜像（`RemoveImage` 刻意不加 --force，就是为了留住
	// 后一道）。因此把引用判据改坏时**这一行不会红**——真正钉住它的是
	// internal/reclaim 的单元验收。留着它是为了「两道防线一起塌」时有人喊。
	if !imageExists(ctx, shared) {
		dumpImageGCDiagnostics(ctx, t)
		t.Fatalf("镜像 %s 还被 %s 引用着，不该被回收", shared, shComponent)
	}
	if got := containerStateOf(ctx, shContainer); got != "running" {
		dumpImageGCDiagnostics(ctx, t)
		t.Errorf("共用镜像的组件应当仍在运行，实际 %q", got)
	}
	// 载荷同理
	blobPath := filepath.Join(gcDataDir, "blobs", "sha256",
		sharedSum[:2], sharedSum)
	if _, err := os.Stat(blobPath); err != nil {
		t.Errorf("还被引用的载荷不该被回收: %v", err)
	}
}

// ── 夹具 ────────────────────────────────────────────────────────────────

// stageGCImage 造一个内容独一无二的镜像并做成 blob。
//
// 每一代的内容必须不同，否则四次 apply 会命中同一个 digest，
// 一代都不会被淘汰——那样这条测试会「通过」，却什么都没验证。
func stageGCImage(ctx context.Context, t *testing.T, tag string, n int) string {
	t.Helper()
	buildImage(ctx, t, tag, n)

	tar := filepath.Join(t.TempDir(), "image.tar")
	if out, err := exec.CommandContext(ctx, "docker", "save",
		"-o", tar, tag).CombinedOutput(); err != nil {
		t.Fatalf("docker save %s: %v\n%s", tag, err, out)
	}
	body, err := os.ReadFile(tar)
	if err != nil {
		t.Fatal(err)
	}
	// 镜像已经在本地了，仍然把它删掉——load 是 Materialize 的真实路径，
	// 而「镜像是不是真的被 load 回来了」正是后面要断言的事。
	if err := exec.CommandContext(ctx, "docker", "rmi", "-f", tag).Run(); err != nil {
		t.Logf("清理镜像 %s（忽略）: %v", tag, err)
	}
	return installBlobIn(t, gcDataDir, body)
}

// buildImage 用 webapp 夹具建一个带标记文件的镜像。
func buildImage(ctx context.Context, t *testing.T, tag string, n int) {
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
	// 这一行让每个镜像的层各不相同
	if err := os.WriteFile(filepath.Join(build, "gen"),
		[]byte(fmt.Sprintf("generation %d\n", n)), 0o644); err != nil {
		t.Fatal(err)
	}
	// FROM scratch：静态二进制不需要基础镜像，也就不需要联网
	dockerfile := "FROM scratch\nCOPY webapp /webapp\nCOPY gen /gen\n" +
		"ENTRYPOINT [\"/webapp\"]\n"
	if err := os.WriteFile(filepath.Join(build, "Dockerfile"),
		[]byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, "docker", "build",
		"-t", tag, build).CombinedOutput(); err != nil {
		t.Fatalf("构建镜像 %s: %v\n%s", tag, err, out)
	}
}

// gcSpec 与 dockerSpec 同形，只是每一代的模板内容不同——
// 那让 digest 每次都变，从而每次都产生一个新的 generation。
func gcSpec(blobSum, confDir string, n int) map[string]any {
	home := filepath.Join(gcDataDir, "apps", gcComponent)
	dataDir := filepath.Join(gcDataDir, "data", gcComponent)

	return map[string]any{
		"schemaVersion": 1,
		"site":          map[string]any{"name": "e2e", "kind": "standalone"},
		"component":     gcComponent,
		"role":          "default",
		"configGroup":   "default",
		"node":          map[string]any{"name": "node-1", "address": "127.0.0.1"},
		"ordinal":       0,
		"pack": map[string]any{
			"name": "go-webapp", "version": fmt.Sprintf("1.%d.0", n), "revision": 1,
		},
		"params": map[string]any{
			"port": map[string]any{"value": gcPort, "type": "port"},
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
						"# 由 Mecharion 渲染\nport: %d\nlog_level: info\ngen: %d\n",
						gcPort, n),
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
				"mounts": []map[string]any{
					{"from": confDir, "to": "/etc/app", "readOnly": true},
					{"from": dataDir, "to": "/data"},
				},
				"ports": []map[string]any{
					{"host": fmt.Sprintf("%d", gcPort), "container": gcPort, "protocol": "tcp"},
				},
				"restart": "unless-stopped",
			},
		},
		"health": map[string]any{
			"http":         map[string]any{"path": "/healthz", "port": gcPort},
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

// sharedSpec 是第二个组件的规格：与 gcSpec 同形，只换了名字与端口，
// **镜像 blob 刻意相同**——共用镜像正是这条验收要造出来的局面。
func sharedSpec(blobSum, confDir string) map[string]any {
	s := gcSpec(blobSum, confDir, 1)
	s["component"] = shComponent
	s["params"] = map[string]any{
		"port": map[string]any{"value": shPort, "type": "port"},
	}
	s["paths"] = map[string]any{
		"home": map[string]any{
			"name": "home", "kind": "single", "mode": "0755",
			"values": []string{filepath.Join(gcDataDir, "apps", shComponent)},
		},
		"config": map[string]any{
			"name": "config", "kind": "single", "mode": "0755",
			"values": []string{confDir},
		},
		"data": map[string]any{
			"name": "data", "kind": "single", "mode": "0755",
			"values": []string{filepath.Join(gcDataDir, "data", shComponent)},
		},
	}
	s["resources"] = []map[string]any{{
		"id": "template:app.yaml", "type": "template", "origin": "role",
		"args": map[string]any{
			"dest": confDir + "/app.yaml",
			"content": fmt.Sprintf(
				"# 由 Mecharion 渲染\nport: %d\nlog_level: info\n", shPort),
			"mode": "0644",
		},
		"driftPolicy": "report",
	}}
	s["workload"] = map[string]any{
		"runtime": "docker",
		"docker": map[string]any{
			"imageBlob": "image",
			"args":      []string{"--config", "/etc/app/app.yaml"},
			"mounts": []map[string]any{
				{"from": confDir, "to": "/etc/app", "readOnly": true},
				{"from": filepath.Join(gcDataDir, "data", shComponent), "to": "/data"},
			},
			"ports": []map[string]any{
				{"host": fmt.Sprintf("%d", shPort), "container": shPort, "protocol": "tcp"},
			},
			"restart": "unless-stopped",
		},
	}
	s["health"] = map[string]any{
		"http":         map[string]any{"path": "/healthz", "port": shPort},
		"startupGrace": "30s",
	}
	return s
}

func cleanupShared(ctx context.Context, t *testing.T) {
	t.Helper()
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", shContainer).Run()
}

func containerStateOf(ctx context.Context, name string) string {
	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{.State.Status}}", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gcGenerations 读台账里的 generation 列表。
func gcGenerations(t *testing.T) []struct {
	Seq    int      `json:"seq"`
	Images []string `json:"images"`
} {
	t.Helper()
	path := filepath.Join(gcDataDir, "mechlet", "instances",
		gcComponent+"__default.json")
	var in struct {
		Generations []struct {
			Seq    int      `json:"seq"`
			Images []string `json:"images"`
		} `json:"generations"`
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读台账 %s: %v", path, err)
	}
	if err := json.Unmarshal(body, &in); err != nil {
		t.Fatalf("解析台账: %v\n%s", err, body)
	}
	return in.Generations
}

// imageExists 问 dockerd 某个镜像还在不在。
func imageExists(ctx context.Context, tag string) bool {
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect",
		"--format", "{{.Id}}", tag).Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

func gcContainerState(ctx context.Context) string {
	return containerStateOf(ctx, gcContainer)
}

func dumpImageGCDiagnostics(ctx context.Context, t *testing.T) {
	t.Helper()
	for _, c := range [][]string{
		{"docker", "images", "--format", "{{.Repository}}:{{.Tag}} {{.ID}}"},
		{"docker", "ps", "-a", "--filter", "name=" + gcContainer,
			"--format", "{{.Names}} {{.Status}} {{.Image}}"},
	} {
		out, _ := exec.CommandContext(ctx, c[0], c[1:]...).CombinedOutput()
		t.Logf("$ %s\n%s", strings.Join(c, " "), out)
	}
	if body, err := os.ReadFile(filepath.Join(gcDataDir, "mechlet",
		"instances", gcComponent+"__default.json")); err == nil {
		t.Logf("台账:\n%s", body)
	}
	if body, err := os.ReadFile(filepath.Join(gcDataDir, "mechlet",
		"garbage.json")); err == nil {
		t.Logf("回收清单:\n%s", body)
	}
}

func cleanupImageGC(ctx context.Context, t *testing.T) {
	t.Helper()
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", gcContainer).Run()
	for i := 1; i <= 4; i++ {
		_ = exec.CommandContext(ctx, "docker", "rmi", "-f",
			fmt.Sprintf("%s:%d.0", gcImageBase, i)).Run()
	}
	for _, d := range []string{gcDataDir, gcConfRoot} {
		if err := os.RemoveAll(d); err != nil {
			t.Logf("清理 %s（忽略）: %v", d, err)
		}
	}
}
