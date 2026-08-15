//go:build linux

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件是 **M6 第 3 步的验收**：升级失败时自动切回，**服务不丢**。
//
// 判据不是「报告里说回滚了」，而是**服务还在应答**。一次升级失败最坏的
// 结局是「旧版被停掉、新版起不来、没人把它切回去」——那正是 M6 之前的行为。
//
// 全部用连不上 mechd 的 solo agent + 手写规格：升级由**改本机期望状态**
// 触发，回滚只可能来自节点自己的判断。

// TestRollbackOnStartFailureSystemd 钉住「新版起不来」。
//
// v2 的 exec 指向一个不存在的二进制：systemd 起不来，调和在切完软链之后
// 失败——这正是必须回滚的那一侧。
func TestRollbackOnStartFailureSystemd(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cleanup(t)
	t.Cleanup(func() { cleanup(t) })

	home := "/opt/mecharion-e2e/apps/webapp"
	confDir := "/etc/mecharion-e2e/apps/webapp"
	sum := installBlob(t, buildTarball(t))

	v1 := specOf(home, confDir, sum, "info", 1)
	readLog := deployAndWatch(ctx, t, dataDir, v1, component+"__"+role)

	v2 := specOf(home, confDir, sum, "info", 2)
	v2["workload"].(map[string]any)["systemd"].(map[string]any)["exec"] =
		home + "/current/bin/does-not-exist --config " + confDir + "/app.yaml"
	seedDesired(ctx, t, dataDir, component+"__"+role, writeSpec(t, v2))

	assertRolledBack(ctx, t, readLog, port)
}

// TestRollbackOnHealthFailureSystemd 钉住「起来了但健康检查不过」。
//
// 这一条比「起不来」更险：进程在跑，看起来一切正常，只有探针知道它没就绪。
func TestRollbackOnHealthFailureSystemd(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cleanup(t)
	t.Cleanup(func() { cleanup(t) })

	home := "/opt/mecharion-e2e/apps/webapp"
	confDir := "/etc/mecharion-e2e/apps/webapp"
	sum := installBlob(t, buildTarball(t))

	v1 := specOf(home, confDir, sum, "info", 1)
	readLog := deployAndWatch(ctx, t, dataDir, v1, component+"__"+role)

	// v2 起得来，但探针指向一个没人监听的端口
	v2 := specOf(home, confDir, sum, "info", 2)
	v2["health"] = map[string]any{
		"tcp":          map[string]any{"port": 9},
		"startupGrace": "5s",
		"interval":     "500ms",
		"timeout":      "500ms",
	}
	seedDesired(ctx, t, dataDir, component+"__"+role, writeSpec(t, v2))

	assertRolledBack(ctx, t, readLog, port)
}

// TestRollbackOnHealthFailureDocker 钉住容器场景下的回滚。
//
// 回滚在这里不是「切软链」那么简单：容器不可变，回到旧版意味着**按旧规格
// 重建容器**。上层那句「回落到最后一次成功的规格」却是同一句。
func TestRollbackOnHealthFailureDocker(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	cleanupDocker(ctx, t)
	t.Cleanup(func() { cleanupDocker(context.Background(), t) })

	sum := stageImageBlob(ctx, t)
	confDir := filepath.Join("/etc/mecharion-e2e-docker/apps", dkComponent)

	v1 := dockerSpec(sum, confDir)
	readLog := deployAndWatch(ctx, t, dkDataDir, v1, dkComponent+"__default")

	v2 := dockerSpec(sum, confDir)
	v2["health"] = map[string]any{
		"tcp":          map[string]any{"port": 9},
		"startupGrace": "5s",
		"interval":     "500ms",
		"timeout":      "500ms",
	}
	seedDesired(ctx, t, dkDataDir, dkComponent+"__default", writeSpec(t, v2))

	assertRolledBack(ctx, t, readLog, dkPort)
	if got := containerState(ctx, t); got != "running" {
		dumpDockerDiagnostics(ctx, t)
		t.Errorf("回滚之后容器应当在跑，实际 %q", got)
	}
}

// TestRollbackOnHealthFailureCompose 钉住 compose project 的回滚。
func TestRollbackOnHealthFailureCompose(t *testing.T) {
	requireDocker(t)
	requireCompose(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	cleanupCompose(ctx, t)
	t.Cleanup(func() { cleanupCompose(context.Background(), t) })

	sum := stageImageBlobRoot(ctx, t, cpDataDir)
	confDir := filepath.Join(cpConfRoot, "apps", cpComponent)

	v1 := composeSpec(sum, confDir)
	readLog := deployAndWatch(ctx, t, cpDataDir, v1, cpComponent+"__default")

	v2 := composeSpec(sum, confDir)
	v2["health"] = map[string]any{
		"tcp":          map[string]any{"port": 9},
		"startupGrace": "5s",
		"interval":     "500ms",
		"timeout":      "500ms",
	}
	seedDesired(ctx, t, cpDataDir, cpComponent+"__default", writeSpec(t, v2))

	assertRolledBack(ctx, t, readLog, cpWebPort)
}

// ── 夹具 ────────────────────────────────────────────────────────────────

// deployAndWatch 装好 v1、起一个 solo agent，并等回滚点落下。
//
// 等回滚点是必须的：没有它，后面那次「升级失败」就没有落脚点，
// 测的就变成了「首装失败会怎样」——另一个题目。
func deployAndWatch(
	ctx context.Context, t *testing.T, dataDir string, v1 map[string]any, key string,
) func() string {
	t.Helper()

	specPath := writeSpec(t, v1)
	if out, err := runMechlet(ctx, "apply", "-f", specPath, "--data-dir", dataDir); err != nil {
		t.Fatalf("首装 v1 失败: %v\n%s", err, out)
	}
	readLog := startSoloAgent(ctx, t, dataDir, specPath, key)

	applied := filepath.Join(dataDir, "desired", key+".applied.json")
	if !waitUntil(ctx, 60*time.Second, func() bool {
		_, err := os.Stat(applied)
		return err == nil
	}) {
		t.Fatalf("v1 成功之后应当留下回滚点 %s", applied)
	}
	return readLog
}

// assertRolledBack 等回滚发生，并核对**服务真的还在应答**。
//
// 两条判据缺一不可：日志说回滚了，且端口上真的有人应答。只看日志的话，
// 一个「报告说回滚了、其实什么也没起来」的实现照样通过——而那正是这个
// 里程碑要消灭的结局。
func assertRolledBack(ctx context.Context, t *testing.T, readLog func() string, port int) {
	t.Helper()

	if !waitUntil(ctx, 120*time.Second, func() bool {
		return strings.Contains(readLog(), "rolled back")
	}) {
		t.Fatalf("升级失败之后应当自动回滚，agent 日志里没有:\n%s", tailOf(readLog(), 40))
	}

	if body := waitHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", port),
		60*time.Second); body == "" {
		t.Fatalf("**服务丢了**——回滚之后端口 %d 上没有应答\n%s",
			port, tailOf(readLog(), 40))
	}
}

// tailOf 取日志最后 n 行，供失败时展示。
func tailOf(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
