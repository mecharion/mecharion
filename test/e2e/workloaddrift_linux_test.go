//go:build linux

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// M5 第 3 步的验收：**手停 / 手删工作负载，一个周期内恢复**，三个 Runtime
// 行为一致。
//
// systemd 那一侧由 drift_linux_test.go 覆盖（`TestDriftSurvivesAgentRestart`
// 的恢复判据就是它）。这里补上 docker 与 compose——**它们才是这条接缝的
// 真正考验**：同一句「工作负载没在跑，拉起来」，在三个 Runtime 下要分别
// 变成 systemctl start、docker create+start、compose up+start，
// 而调和器一行都不该知道这件事。
//
// 测试**不用 mechd**：期望状态直接放进 mechlet 的 desired 目录，agent 的
// upstream 指向一个不存在的 socket。这样恢复只可能来自周期调和，
// 而不是某次推送顺手做的。

// wdInterval 是这些验收用的调和周期。
const wdInterval = 3 * time.Second

// TestDockerWorkloadDriftRecovers 钉住 `docker rm -f` 之后容器会回来。
//
// 这是容器场景下最常见的一类现场：有人为了排障删了容器，或者一次
// `docker system prune` 顺手带走了它。裸机上对应的是 `systemctl stop`，
// 但**后果不同**——systemd 的 unit 还在，容器却是真的没了，
// 恢复要重新 create 而不只是 start。
func TestDockerWorkloadDriftRecovers(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	cleanupDocker(ctx, t)
	t.Cleanup(func() { cleanupDocker(context.Background(), t) })

	sum := stageImageBlob(ctx, t)
	confDir := filepath.Join("/etc/mecharion-e2e-docker/apps", dkComponent)
	specPath := writeSpec(t, dockerSpec(sum, confDir))

	// 先正常装一遍
	if out, err := runMechlet(ctx, "apply", "-f", specPath, "--data-dir", dkDataDir); err != nil {
		dumpDockerDiagnostics(ctx, t)
		t.Fatalf("首次 apply: %v\n%s", err, out)
	}
	_ = startSoloAgent(ctx, t, dkDataDir, specPath, dkComponent+"__default")

	before := containerID(ctx, t)
	if before == "" {
		t.Fatal("容器没建起来")
	}

	// **把容器整个删掉** —— 不是停，是删
	if out, err := exec.CommandContext(ctx, "docker", "rm", "-f", dkContainer).
		CombinedOutput(); err != nil {
		t.Fatalf("删容器: %v\n%s", err, out)
	}
	if containerID(ctx, t) != "" {
		t.Fatal("容器没被删掉，这条测试证明不了任何东西")
	}

	if !waitUntil(ctx, 60*time.Second, func() bool {
		return containerState(ctx, t) == "running"
	}) {
		dumpDockerDiagnostics(ctx, t)
		t.Fatal("容器被删之后，周期调和没有把它重建并拉起")
	}

	// 是**重建**出来的，不是原来那个
	if after := containerID(ctx, t); after == before {
		t.Errorf("容器 id 没变（%s）——那说明它压根没被删掉", short12(after))
	}
	if body := waitContainerHTTP(t, dkPort, 60*time.Second); body == "" {
		dumpDockerDiagnostics(ctx, t)
		t.Fatal("重建出来的容器没有在服务")
	}
}

// TestDockerWorkloadStopRecovers 钉住 `docker stop` 之后容器会被拉起。
//
// 与上一条的区别在恢复路径：容器还在，只是停了——**不该重建**。
// 重建会丢掉容器里的可写层，而那正是排障时有人想看的东西。
func TestDockerWorkloadStopRecovers(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	cleanupDocker(ctx, t)
	t.Cleanup(func() { cleanupDocker(context.Background(), t) })

	sum := stageImageBlob(ctx, t)
	confDir := filepath.Join("/etc/mecharion-e2e-docker/apps", dkComponent)
	specPath := writeSpec(t, dockerSpec(sum, confDir))

	if out, err := runMechlet(ctx, "apply", "-f", specPath, "--data-dir", dkDataDir); err != nil {
		dumpDockerDiagnostics(ctx, t)
		t.Fatalf("首次 apply: %v\n%s", err, out)
	}
	_ = startSoloAgent(ctx, t, dkDataDir, specPath, dkComponent+"__default")

	before := containerID(ctx, t)
	if out, err := exec.CommandContext(ctx, "docker", "stop", "--timeout", "3", dkContainer).
		CombinedOutput(); err != nil {
		t.Fatalf("停容器: %v\n%s", err, out)
	}

	if !waitUntil(ctx, 60*time.Second, func() bool {
		return containerState(ctx, t) == "running"
	}) {
		dumpDockerDiagnostics(ctx, t)
		t.Fatal("容器被停之后，周期调和没有把它拉起")
	}
	// digest 没变，**不该重建**
	if after := containerID(ctx, t); after != before {
		t.Errorf("容器被重建了（%s → %s）——只是停了而已，重建会丢掉可写层",
			short12(before), short12(after))
	}
}

// TestComposeWorkloadDriftRecovers 钉住 `compose down` 之后 project 会回来。
//
// compose 的粒度是整个 project：down 会把全部容器与网络一起拆掉，
// 恢复要重新 up。这与 docker 的单容器恢复是两条不同的代码路径，
// 而**上层那句「没在跑就拉起来」是同一句**——这正是接缝要证明的事。
func TestComposeWorkloadDriftRecovers(t *testing.T) {
	requireDocker(t)
	requireCompose(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
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
	_ = startSoloAgent(ctx, t, cpDataDir, specPath, cpComponent+"__default")

	if len(composeContainers(ctx, t)) != 2 {
		t.Fatalf("project 应有 2 个容器，实际 %v", composeContainers(ctx, t))
	}

	// **把整个 project 拆掉**
	if out, err := exec.CommandContext(ctx, "docker", "compose", "-p", cpProject,
		"down", "--remove-orphans", "--timeout", "3").CombinedOutput(); err != nil {
		t.Fatalf("拆 project: %v\n%s", err, out)
	}
	if n := len(composeContainers(ctx, t)); n != 0 {
		t.Fatalf("project 没被拆干净，还剩 %d 个容器", n)
	}

	if !waitUntil(ctx, 90*time.Second, func() bool {
		states := composeStates(ctx, t)
		if len(states) != 2 {
			return false
		}
		for _, st := range states {
			if st != "running" {
				return false
			}
		}
		return true
	}) {
		dumpComposeDiagnostics(ctx, t)
		t.Fatal("project 被拆之后，周期调和没有把它重建并拉起")
	}
	for _, port := range []int{cpWebPort, cpSidePort} {
		if body := waitContainerHTTP(t, port, 60*time.Second); body == "" {
			dumpComposeDiagnostics(ctx, t)
			t.Fatalf("恢复之后端口 %d 上的服务没有应答", port)
		}
	}
}

// ── 夹具 ────────────────────────────────────────────────────────────────

// startSoloAgent 起一个**连不上任何 mechd** 的 agent，并把期望状态放好。
//
// 这样恢复只可能来自周期调和，不可能是某次推送顺手做的——
// 后者会让这条验收变成一道容易得多的题。
func startSoloAgent(
	ctx context.Context, t *testing.T, dataDir, specPath, key string,
) func() string {
	t.Helper()

	seedDesired(ctx, t, dataDir, key, specPath)

	// 日志落到文件，测试才读得到。
	//
	// 「report（检出了但按策略不改）」与「ignore（根本没比对）」在盘上
	// 长得一模一样——文件都没被改回。**只有日志能分开它们**，而这个
	// 区别正是三种策略里最容易实现错的一处。
	logPath := filepath.Join(t.TempDir(), "agent.log")
	lf, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lf.Close() })

	cmd := exec.CommandContext(ctx, filepath.Join(binDir, "mechlet"), "agent",
		"--data-dir", dataDir,
		"--upstream", "unix:///run/mecharion-nonexistent.sock",
		"--node", "node-1",
		"--reconcile-interval", wdInterval.String())
	cmd.Stdout, cmd.Stderr = lf, lf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopProc(cmd)
		if t.Failed() {
			if b, rerr := os.ReadFile(logPath); rerr == nil {
				t.Logf("agent 日志:\n%s", b)
			}
		}
	})

	// 等它跑过至少一轮，免得测试抢在第一轮之前动手
	time.Sleep(2 * wdInterval)

	return func() string {
		b, rerr := os.ReadFile(logPath)
		if rerr != nil {
			return ""
		}
		return string(b)
	}
}

// seedDesired 把一份规格放进 mechlet 的期望状态目录。
//
// digest 由 **mechlet 自己**算——`apply --dry-run -o json` 输出的就是封好的
// 规格。测试里自己拼一个假 digest 也能跑，但那会让「digest 是规格内容的
// 函数」这条不变式在验收里失效，而它正是 generation 判定的全部依据。
func seedDesired(ctx context.Context, t *testing.T, dataDir, key, specPath string) {
	t.Helper()

	out, err := exec.CommandContext(ctx, filepath.Join(binDir, "mechlet"), "apply",
		"-f", specPath, "--data-dir", dataDir, "--dry-run", "-o", "json").Output()
	if err != nil {
		t.Fatalf("封装规格: %v", err)
	}
	if !strings.Contains(string(out), `"digest"`) {
		t.Fatalf("dry-run 应当输出封好的规格，实际:\n%s", out)
	}

	dir := filepath.Join(dataDir, "desired")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, key+".json"), out, 0o600); err != nil {
		t.Fatal(err)
	}
}
