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

// 本文件是 **M5 第 7 步的验收**：把 20-continuous-reconcile §4 那张表整张跑
// 一遍，三个 Runtime 一起过。
//
// 表里最容易实现错的是 **report 与 ignore 的区别**：两者在盘上长得一模一样
// ——文件都没被改回。分开它们的唯一方式是「有没有被检出」，因此这些用例
// 除了看文件，还要看 agent 日志。
//
// 全部用**连不上 mechd 的 solo agent** + 手写规格：
//
//   - 恢复只可能来自周期调和，不可能是某次推送顺手做的
//   - driftPolicy 与 runState 可以逐用例精确设定，不必为每种组合造一个 Pack
//
// mechd 那一侧（把策略与运行态算出来并推下去）在 drift_linux_test.go 里
// 用真实的 mechd 栈验过，两边合起来才是完整链路。

// driftCase 是表里的一行。
type driftCase struct {
	policy string
	// wantReverted 是「文件会不会被改回去」。
	wantReverted bool
	// wantDetected 是「会不会被检出并上报」。
	//
	// 它与 wantReverted 一起才能把三种策略区分开：
	//
	//	report     检出、不改回
	//	reconcile  检出、改回
	//	ignore     **不检出**、不改回
	wantDetected bool
}

var driftCases = []driftCase{
	{policy: "report", wantReverted: false, wantDetected: true},
	{policy: "reconcile", wantReverted: true, wantDetected: true},
	{policy: "ignore", wantReverted: false, wantDetected: false},
}

// TestDriftMatrixSystemd 跑表里的第 1–3 行（systemd）。
func TestDriftMatrixSystemd(t *testing.T) {
	requireEnv(t)
	for _, tc := range driftCases {
		t.Run(tc.policy, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()

			cleanup(t)
			t.Cleanup(func() { cleanup(t) })

			home := "/opt/mecharion-e2e/apps/webapp"
			confDir := "/etc/mecharion-e2e/apps/webapp"
			sum := installBlob(t, buildTarball(t))

			s := specOf(home, confDir, sum, "info", 1)
			setResourceDriftPolicy(t, s, "template:app.yaml", tc.policy)
			specPath := writeSpec(t, s)

			if out, err := runMechlet(ctx, "apply", "-f", specPath,
				"--data-dir", dataDir); err != nil {
				t.Fatalf("首次 apply: %v\n%s", err, out)
			}
			waitUnitActive(t, 30*time.Second)

			readLog := startSoloAgent(ctx, t, dataDir, specPath, component+"__"+role)
			assertDriftCase(ctx, t, tc, filepath.Join(confDir, "app.yaml"), readLog)
		})
	}
}

// TestDriftMatrixDocker 跑表里的第 1–3 行（docker）。
func TestDriftMatrixDocker(t *testing.T) {
	requireDocker(t)
	for _, tc := range driftCases {
		t.Run(tc.policy, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			cleanupDocker(ctx, t)
			t.Cleanup(func() { cleanupDocker(context.Background(), t) })

			sum := stageImageBlob(ctx, t)
			confDir := filepath.Join("/etc/mecharion-e2e-docker/apps", dkComponent)

			s := dockerSpec(sum, confDir)
			setResourceDriftPolicy(t, s, "template:app.yaml", tc.policy)
			specPath := writeSpec(t, s)

			if out, err := runMechlet(ctx, "apply", "-f", specPath,
				"--data-dir", dkDataDir); err != nil {
				dumpDockerDiagnostics(ctx, t)
				t.Fatalf("首次 apply: %v\n%s", err, out)
			}

			readLog := startSoloAgent(ctx, t, dkDataDir, specPath,
				dkComponent+"__default")
			assertDriftCase(ctx, t, tc, filepath.Join(confDir, "app.yaml"), readLog)
		})
	}
}

// TestDriftMatrixCompose 跑表里的第 1–3 行（compose）。
func TestDriftMatrixCompose(t *testing.T) {
	requireDocker(t)
	requireCompose(t)
	for _, tc := range driftCases {
		t.Run(tc.policy, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			cleanupCompose(ctx, t)
			t.Cleanup(func() { cleanupCompose(context.Background(), t) })

			sum := stageImageBlobRoot(ctx, t, cpDataDir)
			confDir := filepath.Join(cpConfRoot, "apps", cpComponent)

			s := composeSpec(sum, confDir)
			setResourceDriftPolicy(t, s, "template:"+confDir+"/web.yaml", tc.policy)
			specPath := writeSpec(t, s)

			if out, err := runMechlet(ctx, "apply", "-f", specPath,
				"--data-dir", cpDataDir); err != nil {
				dumpComposeDiagnostics(ctx, t)
				t.Fatalf("首次 apply: %v\n%s", err, out)
			}

			readLog := startSoloAgent(ctx, t, cpDataDir, specPath,
				cpComponent+"__default")
			assertDriftCase(ctx, t, tc, filepath.Join(confDir, "web.yaml"), readLog)
		})
	}
}

// TestRunStateMatrixDocker 跑表里的第 5 行（docker）：
// **期望停止时，手工启动会被停回去。**
//
// 只做「停了就别拉起来」的话，期望运行态就是一句没人执行的声明——
// 有人手工把它启动起来，系统会一直默认那是对的，而维护窗口里那台机器
// 其实已经在对外提供服务了。
func TestRunStateMatrixDocker(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cleanupDocker(ctx, t)
	t.Cleanup(func() { cleanupDocker(context.Background(), t) })

	sum := stageImageBlob(ctx, t)
	confDir := filepath.Join("/etc/mecharion-e2e-docker/apps", dkComponent)

	s := dockerSpec(sum, confDir)
	if out, err := runMechlet(ctx, "apply", "-f", writeSpec(t, s),
		"--data-dir", dkDataDir); err != nil {
		dumpDockerDiagnostics(ctx, t)
		t.Fatalf("首次 apply: %v\n%s", err, out)
	}

	// 期望改为停止。**直接写进规格**而不是走 mechd：mechd 那一侧
	// （落库 + 推送）在 drift_linux_test.go 里用真实栈验过，这里要验的是
	// docker runtime 上「维持停止」这半段。
	s["runState"] = "stopped"
	_ = startSoloAgent(ctx, t, dkDataDir, writeSpec(t, s), dkComponent+"__default")

	if !waitUntil(ctx, 60*time.Second, func() bool {
		return containerState(ctx, t) != "running"
	}) {
		dumpDockerDiagnostics(ctx, t)
		t.Fatal("期望停止后容器应当被停掉")
	}

	// 有人手工把它起来了
	if out, err := exec.CommandContext(ctx, "docker", "start", dkContainer).
		CombinedOutput(); err != nil {
		t.Fatalf("手工启动容器: %v\n%s", err, out)
	}
	if containerState(ctx, t) != "running" {
		t.Fatal("手工启动没成功，这条测试证明不了任何东西")
	}

	if !waitUntil(ctx, 60*time.Second, func() bool {
		return containerState(ctx, t) != "running"
	}) {
		dumpDockerDiagnostics(ctx, t)
		t.Fatal("期望停止时被手工启动，调和应当把它停回去")
	}
}

// TestRunStateMatrixCompose 跑表里的第 5 行（compose）。
func TestRunStateMatrixCompose(t *testing.T) {
	requireDocker(t)
	requireCompose(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cleanupCompose(ctx, t)
	t.Cleanup(func() { cleanupCompose(context.Background(), t) })

	sum := stageImageBlobRoot(ctx, t, cpDataDir)
	confDir := filepath.Join(cpConfRoot, "apps", cpComponent)

	s := composeSpec(sum, confDir)
	if out, err := runMechlet(ctx, "apply", "-f", writeSpec(t, s),
		"--data-dir", cpDataDir); err != nil {
		dumpComposeDiagnostics(ctx, t)
		t.Fatalf("首次 apply: %v\n%s", err, out)
	}

	s["runState"] = "stopped"
	_ = startSoloAgent(ctx, t, cpDataDir, writeSpec(t, s), cpComponent+"__default")

	allStopped := func() bool {
		for _, st := range composeStates(ctx, t) {
			if st == "running" {
				return false
			}
		}
		return true
	}
	if !waitUntil(ctx, 90*time.Second, allStopped) {
		dumpComposeDiagnostics(ctx, t)
		t.Fatal("期望停止后 project 应当被停掉")
	}

	if out, err := exec.CommandContext(ctx, "docker", "compose", "-p", cpProject,
		"start").CombinedOutput(); err != nil {
		t.Fatalf("手工启动 project: %v\n%s", err, out)
	}
	if allStopped() {
		t.Fatal("手工启动没成功，这条测试证明不了任何东西")
	}

	if !waitUntil(ctx, 90*time.Second, allStopped) {
		dumpComposeDiagnostics(ctx, t)
		t.Fatal("期望停止时被手工启动，调和应当把 project 停回去")
	}
}

// ── 夹具 ────────────────────────────────────────────────────────────────

// driftCanary 是手工改进配置文件的内容。
const driftCanary = "log_level: trace # 手工改的\n"

// assertDriftCase 手改配置，然后按策略核对两件事：文件改没改回、有没有被检出。
func assertDriftCase(
	ctx context.Context, t *testing.T, tc driftCase, confPath string, readLog func() string,
) {
	t.Helper()

	before, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("读配置 %s: %v", confPath, err)
	}
	if err := os.WriteFile(confPath, []byte(driftCanary), 0o644); err != nil {
		t.Fatal(err)
	}

	// ① 文件改没改回
	if tc.wantReverted {
		if !waitUntil(ctx, 60*time.Second, func() bool {
			b, rerr := os.ReadFile(confPath)
			return rerr == nil && string(b) == string(before)
		}) {
			cur, _ := os.ReadFile(confPath)
			t.Fatalf("driftPolicy=%s 应当把文件改回，实际:\n%s", tc.policy, cur)
		}
	} else {
		// 等几轮，确认它**一直**没被改回——只看一次可能只是还没轮到
		deadline := time.Now().Add(4 * wdInterval)
		for time.Now().Before(deadline) {
			b, rerr := os.ReadFile(confPath)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(b) != driftCanary {
				t.Fatalf("driftPolicy=%s 不该把文件改回，实际:\n%s", tc.policy, b)
			}
			time.Sleep(wdInterval / 2)
		}
	}

	// ② 有没有被检出 —— **这是 report 与 ignore 唯一的区别**
	//
	// reconcile 不查日志：**文件被改回本身就是被检出的证据**，
	// 而它走的是 Apply 那条路径，不打「仅上报」那句话。
	if tc.wantReverted {
		return
	}
	detected := func() bool { return strings.Contains(readLog(), "drift detected") }
	if tc.wantDetected {
		if !waitUntil(ctx, 30*time.Second, detected) {
			t.Errorf("driftPolicy=%s 应当检出漂移，agent 日志里没有", tc.policy)
		}
	} else if detected() {
		t.Errorf("driftPolicy=ignore 根本不该比对，却检出了漂移")
	}
}

// setResourceDriftPolicy 改一条资源的 driftPolicy。
//
// 找不到那条资源时**报错而不是静默跳过**：id 写错的话整个用例会退化成
// 「用默认策略跑一遍」，三行表格看起来全过，实际只测了一行。
func setResourceDriftPolicy(t *testing.T, s map[string]any, id, policy string) {
	t.Helper()
	res, ok := s["resources"].([]map[string]any)
	if !ok {
		t.Fatalf("规格里的 resources 不是期望的形状: %T", s["resources"])
	}
	for _, r := range res {
		if r["id"] == id {
			r["driftPolicy"] = policy
			return
		}
	}
	var ids []string
	for _, r := range res {
		ids = append(ids, fmt.Sprint(r["id"]))
	}
	t.Fatalf("规格里没有资源 %q，实际有: %s", id, strings.Join(ids, ", "))
}
