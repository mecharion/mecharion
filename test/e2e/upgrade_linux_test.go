//go:build linux

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件是 **M6 第 5 步的验收**：`component upgrade` / `rollback` 端到端。
//
// 用真实的 mechd 栈（不是 solo agent）：这两条命令的价值一半在 mechd 侧
// ——换版本、查兼容范围、重新解析、下发——那部分只有走完整链路才验得到。

// TestUpgradeAndRollback 走一遍升级再回滚。
//
// 回滚的判据不只是「版本变回去了」，还有**digest 回到当年那个**：
// 解析是纯函数，回到旧版本必然产出一模一样的 digest，节点因此命中已保留的
// generation，切一次软链就完事。digest 对不上就说明纯函数这条性质破了，
// 而那会让每次回滚都退化成一次完整的重新物化。
func TestUpgradeAndRollback(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	token, _ := deployForDrift(ctx, t)
	// 等它把状态报上去：调和跑完与 mechd 收到上报是两件事，
	// 中间隔着一个上报周期
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return instanceDigest(ctx, t, token) != ""
	}) {
		t.Fatal("部署之后应当上报 digest")
	}
	v1Digest := instanceDigest(ctx, t, token)

	stagePackVersion(t, "1.3.0", "")

	// ── 升级 ──
	out, err := runCtl(ctx, token, "component", "upgrade", "web", "--version", "1.3.0")
	if err != nil {
		t.Fatalf("upgrade 失败: %v\n%s", err, out)
	}
	if !waitUntil(ctx, 90*time.Second, func() bool {
		return componentVersion(ctx, t, token) == "1.3.0" && converged(ctx, t, token)
	}) {
		dumpDiagnostics(ctx, t)
		t.Fatalf("升级之后应当收敛到 1.3.0，status:\n%s", componentStatus(ctx, t, token))
	}
	v2Digest := instanceDigest(ctx, t, token)
	if v2Digest == v1Digest {
		t.Fatal("换了版本，digest 不该一样")
	}

	// **单机就是一阶段一批次**（22-multi-node §5 第 14 行）。
	//
	// 多节点那套代码不能让单机形态变复杂——而「变复杂」最先会表现成
	// 这里的分母不是 1。这条同时说明多节点的分批**退化得干净**：
	// 同一条代码路径，只是批数为 1。
	if out, err := runCtl(ctx, token, "rollout", "status", "web"); err != nil {
		t.Errorf("rollout status 失败: %v\n%s", err, out)
	} else if !strings.Contains(out, "1/1") {
		t.Errorf("单机应当是一阶段一批次，实际:\n%s", out)
	}
	if !isActive(ctx, saUnit) {
		t.Error("升级之后服务应当在跑")
	}

	// ── 回滚（不带参数，回到上一版）──
	out, err = runCtl(ctx, token, "component", "rollback", "web")
	if err != nil {
		t.Fatalf("rollback 失败: %v\n%s", err, out)
	}
	if !waitUntil(ctx, 90*time.Second, func() bool {
		return componentVersion(ctx, t, token) == "1.2.0" && converged(ctx, t, token)
	}) {
		dumpDiagnostics(ctx, t)
		t.Fatalf("回滚之后应当收敛回 1.2.0，status:\n%s", componentStatus(ctx, t, token))
	}

	// **digest 必须与当年一模一样**——那是「秒级回滚」成立的全部依据
	if got := instanceDigest(ctx, t, token); got != v1Digest {
		t.Errorf("回到同一版本应当产出同一个 digest（解析是纯函数）\n"+
			"  当年 %s\n  现在 %s", v1Digest, got)
	}
	if !isActive(ctx, saUnit) {
		t.Error("回滚之后服务应当在跑")
	}
}

// TestUpgradeRefusesIncompatible 钉住 upgradePolicy 检查（spec §4.2）。
//
// Mecharion 的升级模型是「物化新 generation → 原子切换 → **数据目录不动**」。
// 这个模型对某些大版本升级是错的：PostgreSQL 16 → 17 需要 pg_upgrade 与新的
// 数据目录，直接换二进制会让 PG 17 去启动 PG 16 的数据。
//
// 判据的**方向**要紧：目标版本的 compatible 是否包含当前版本——
// 新版本才知道自己能从哪些旧版本接管数据。
func TestUpgradeRefusesIncompatible(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	token, _ := deployForDrift(ctx, t)

	// 2.0.0 声明只接受从 2.x 升级而来，当前是 1.2.0
	stagePackVersion(t, "2.0.0", "~2")

	out, err := runCtl(ctx, token, "component", "upgrade", "web", "--version", "2.0.0")
	if err == nil {
		t.Fatalf("跨不兼容版本的升级应当被拒绝\n%s", out)
	}
	for _, want := range []string{"1.2.0", "2.0.0", "compatible"} {
		if !strings.Contains(out, want) {
			t.Errorf("拒绝信息里应当出现 %q，实际:\n%s", want, out)
		}
	}
	// **必须给出出路**，否则用户只知道被拒了
	if !strings.Contains(out, "--force") {
		t.Errorf("拒绝信息应给出确认安全时的做法，实际:\n%s", out)
	}

	// 版本不该被改掉
	if got := componentVersion(ctx, t, token); got != "1.2.0" {
		t.Errorf("被拒绝的升级不该改动版本，实际 %s", got)
	}

	// --force 放行
	if out, err := runCtl(ctx, token, "component", "upgrade", "web",
		"--version", "2.0.0", "--force", "--dry-run"); err != nil {
		t.Errorf("--force 应当放行: %v\n%s", err, out)
	}
	// dry-run 之后版本仍然不该变
	if got := componentVersion(ctx, t, token); got != "1.2.0" {
		t.Errorf("--dry-run 不该改动任何东西，版本变成了 %s", got)
	}
}

// ── 夹具 ────────────────────────────────────────────────────────────────

// stagePackVersion 在 Pack 集合里再放一份不同版本的 go-webapp。
//
// 每个子目录是一个 Pack，同名不同版本各占一个目录——这正是 packindex
// 的布局，测试不该绕开它自己造索引。
func stagePackVersion(t *testing.T, version, compatible string) {
	t.Helper()
	dst := filepath.Join(saDataDir, "packs", "go-webapp-"+version)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	copyTree(t, filepath.Join(saDataDir, "packs", "go-webapp"), dst)

	p := filepath.Join(dst, "pack.yaml")
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Replace(string(body), `version: "1.2.0"`, `version: "`+version+`"`, 1)
	if out == string(body) {
		t.Fatalf("没能改掉版本号，pack.yaml 的形状变了？")
	}
	if compatible != "" {
		out = strings.Replace(out, "\nparams:",
			"\nupgradePolicy:\n  compatible: \""+compatible+"\"\n\nparams:", 1)
	}
	if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}

func componentStatusJSON(ctx context.Context, t *testing.T, token string) struct {
	Version   string `json:"version"`
	Converged bool   `json:"converged"`
	Instances []struct {
		Want string `json:"want"`
		Got  string `json:"got"`
	} `json:"instances"`
} {
	t.Helper()
	var st struct {
		Version   string `json:"version"`
		Converged bool   `json:"converged"`
		Instances []struct {
			Want string `json:"want"`
			Got  string `json:"got"`
		} `json:"instances"`
	}
	out, err := runCtl(ctx, token, "component", "status", "web", "-o", "json")
	if err != nil {
		return st
	}
	_ = json.Unmarshal([]byte(out), &st)
	return st
}

func componentVersion(ctx context.Context, t *testing.T, token string) string {
	return componentStatusJSON(ctx, t, token).Version
}

func converged(ctx context.Context, t *testing.T, token string) bool {
	return componentStatusJSON(ctx, t, token).Converged
}

// instanceDigest 取第一个实例**实际上报**的 digest。
//
// 用 got 而不是 want：want 是 mechd 期望的，got 才是机器上真在跑的。
func instanceDigest(ctx context.Context, t *testing.T, token string) string {
	st := componentStatusJSON(ctx, t, token)
	if len(st.Instances) == 0 {
		return ""
	}
	return st.Instances[0].Got
}

// TestRolloutLifecycle 是 **M6 第 6 步的验收**：升级过程可见、可暂停、可中止。
//
// Rollout 存在的理由是给「升级到一半」一个名字。没有它，运维只能看到
// 「收敛没收敛」，而他想问的是**「这次升级怎么样了、能不能停下、
// 要不要退回去」**。
func TestRolloutLifecycle(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	token, _ := deployForDrift(ctx, t)
	stagePackVersion(t, "1.3.0", "")

	// 没升级过时：没有可看的
	if out, err := runCtl(ctx, token, "rollout", "status", "web"); err == nil {
		t.Errorf("还没发生过版本变更时应当明确说没有，实际:\n%s", out)
	}

	if out, err := runCtl(ctx, token, "component", "upgrade", "web",
		"--version", "1.3.0"); err != nil {
		t.Fatalf("upgrade 失败: %v\n%s", err, out)
	}

	// ── 可见 ──
	out, err := runCtl(ctx, token, "rollout", "status", "web")
	if err != nil {
		t.Fatalf("rollout status 失败: %v\n%s", err, out)
	}
	for _, want := range []string{"upgrade", "1.2.0", "1.3.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("rollout status 应当显示 %q，实际:\n%s", want, out)
		}
	}

	// ── 收敛之后自动变成「已成功」 ──
	//
	// 判据来自**观测到的状态**（实例上报的 digest），不是「mechd 发过什么」。
	if !waitUntil(ctx, 120*time.Second, func() bool {
		return rolloutState(ctx, t, token) == "succeeded"
	}) {
		dumpDiagnostics(ctx, t)
		t.Fatalf("收敛之后 Rollout 应当变成 succeeded，实际 %q\nstatus:\n%s",
			rolloutState(ctx, t, token), componentStatus(ctx, t, token))
	}

	// ── history 记得住 ──
	out, err = runCtl(ctx, token, "rollout", "history", "web")
	if err != nil {
		t.Fatalf("rollout history 失败: %v\n%s", err, out)
	}
	if !strings.Contains(out, "1.3.0") {
		t.Errorf("history 里应当有这次升级，实际:\n%s", out)
	}

	// 已经结束的 Rollout 不能被暂停
	if out, err := runCtl(ctx, token, "rollout", "pause", "web"); err == nil {
		t.Errorf("已结束的版本变更不该能被冻结，实际:\n%s", out)
	}
}

// TestRolloutAbortReallyRollsBack 钉住 abort **真的回退**。
//
// 一条只把记录标成「已中止」的 abort，会让运维以为世界回到了升级前，
// 而机器上跑的还是新版——那比没有这个命令更糟。
func TestRolloutAbortReallyRollsBack(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	token, _ := deployForDrift(ctx, t)
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return instanceDigest(ctx, t, token) != ""
	}) {
		t.Fatal("部署之后应当上报 digest")
	}
	v1Digest := instanceDigest(ctx, t, token)
	stagePackVersion(t, "1.3.0", "")

	if out, err := runCtl(ctx, token, "component", "upgrade", "web",
		"--version", "1.3.0"); err != nil {
		t.Fatalf("upgrade 失败: %v\n%s", err, out)
	}

	// 立刻冻结。**M7 起 pause 停的不只是判定，还有批次队列**：已经放行的
	// 那批照常跑完，但下一批不会被放行（M6 时它只冻判定，因为单机只有
	// 一批、没什么可停）。这里 pause 发生在第一批放行之前，因此机器根本
	// 不会动。
	if out, err := runCtl(ctx, token, "rollout", "pause", "web"); err != nil {
		t.Fatalf("rollout pause 失败: %v\n%s", err, out)
	}
	if got := rolloutState(ctx, t, token); got != "paused" {
		t.Errorf("pause 之后状态应为 paused，实际 %q", got)
	}

	// **冻结要真的冻住**：等足够多个上报周期，机器仍在 1.2.0、状态仍是
	// paused。少了这一条，pause 就只是个显示字段——人还在查现场，队列
	// 却自己往前走了，abort 也就不再有对象可中止。
	if waitUntil(ctx, 30*time.Second, func() bool {
		return rolloutState(ctx, t, token) != "paused" ||
			instanceDigest(ctx, t, token) != v1Digest
	}) {
		dumpDiagnostics(ctx, t)
		t.Fatalf("冻结期间不该有任何变化，实际状态 %q digest %q",
			rolloutState(ctx, t, token), instanceDigest(ctx, t, token))
	}

	// ── 中止：必须真的回到 1.2.0 ──
	//
	// 不带 -y 时必须**拒绝**：非交互环境下沉默照做，等于把「危险操作要确认」
	// 这条约定在唯一会用到它的地方（脚本）取消掉。
	if out, err := runCtl(ctx, token, "rollout", "abort", "web"); err == nil {
		t.Errorf("非交互环境下不带 -y 的 abort 应当被拒绝，实际:\n%s", out)
	}
	if out, err := runCtl(ctx, token, "rollout", "abort", "web", "-y"); err != nil {
		t.Fatalf("rollout abort 失败: %v\n%s", err, out)
	}
	if !waitUntil(ctx, 120*time.Second, func() bool {
		return componentVersion(ctx, t, token) == "1.2.0" &&
			instanceDigest(ctx, t, token) == v1Digest
	}) {
		dumpDiagnostics(ctx, t)
		t.Fatalf("abort 应当真的把机器退回 1.2.0，实际版本 %q digest %q",
			componentVersion(ctx, t, token), instanceDigest(ctx, t, token))
	}
	if !isActive(ctx, saUnit) {
		t.Error("中止之后服务应当在跑")
	}

	// **组件声明的版本也要退回去**，不只是机器上的文件。
	//
	// 单机只有一批，而 pause 发生在放行之前，机器根本没动过——因此
	// 「把机器退回去」在这条路径上无从检验。真正跨版本的中止在多节点
	// 验收里（test/multinode：三批里中途 abort，已经升上去的要退回来）。
	if got := componentVersion(ctx, t, token); got != "1.2.0" {
		t.Errorf("abort 之后组件版本应回到 1.2.0，实际 %q", got)
	}
	// abort **不是**「把记录标成已中止」就完了：它开了一条 rollback，
	// 而那条要自己走完。冻结状态绝不能留下来——留着的话，下一次变更会
	// 撞上一条永远不动的 Rollout。
	if !waitUntil(ctx, 120*time.Second, func() bool {
		return rolloutState(ctx, t, token) == "succeeded"
	}) {
		t.Errorf("abort 开出的回退应当走完，实际停在 %q", rolloutState(ctx, t, token))
	}
}

func rolloutState(ctx context.Context, t *testing.T, token string) string {
	t.Helper()
	out, err := runCtl(ctx, token, "rollout", "status", "web", "-o", "json")
	if err != nil {
		return ""
	}
	var v struct {
		State string `json:"state"`
	}
	if json.Unmarshal([]byte(out), &v) != nil {
		return ""
	}
	return v.State
}
