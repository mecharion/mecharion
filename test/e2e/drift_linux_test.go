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

// driftInterval 是这些验收用的调和周期。
//
// 生产默认 60 秒。测试压到 3 秒不是为了"快"，是为了让"一个周期内"这个
// 判据在几十秒的测试预算里可观察——判据本身没有变。
const driftInterval = 3 * time.Second

// TestDriftIsDetectedWithoutAnyPush 是 **M5 第 1 步的验收**，
// 也是[常驻 Agent 存在理由](../../docs/adr/0001-agent-based.md)的第一次实证。
//
// 它做的事只有一件：部署完之后**再也不碰 mechd**，手工改掉机器上的配置
// 文件，然后等。到 M4 为止这条测试必然失败——mechlet 只在收到推送时才
// 调和，没人推就什么都不做。
//
// 判据分两层，缺一不可：
//
//	① 漂移被**检出**（status 里看得到）
//	② driftPolicy: report 下文件**没有被改回去**
//
// 少了②，一个「每轮无条件重新 Apply 全部资源」的实现也能让①通过——
// 那不是漂移检测，那是定时重装。
func TestDriftIsDetectedWithoutAnyPush(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	token, confPath := deployForDrift(ctx, t)

	// 手工改掉配置 —— 模拟凌晨救火的运维
	const canary = "log_level: debug # 手工改的\n"
	if err := os.WriteFile(confPath, []byte(canary), 0o644); err != nil {
		t.Fatal(err)
	}

	// **从这里开始不再碰 mechd。** 只有周期调和能发现这件事。
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return len(driftedResources(ctx, t, token)) > 0
	}) {
		dumpDiagnostics(ctx, t)
		t.Fatal("手改配置后一直没有被检出——周期调和没在跑")
	}

	// ② driftPolicy: report（go-webapp 的默认）下不该被改回去
	body, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != canary {
		t.Errorf("默认策略是 report，文件不该被改回。\n实际:\n%s", body)
	}
}

// TestDriftSurvivesAgentRestart 钉住 ADR-0033 的核心收益。
//
// mechlet 重启之后、mechd 还没来得及推送之前，它必须**已经知道该维持
// 什么**。这正是把期望状态与密钥落到本机的全部理由——断连 + 重启是最坏
// 的组合，也是最需要 Agent 自己顶上的时候。
//
// 做法：部署好之后杀掉 agent，改配置，**换一个连不上 mechd 的 upstream**
// 重新起 agent。这样漂移只可能来自本机保存的期望状态。
func TestDriftSurvivesAgentRestart(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	_, confPath := deployForDrift(ctx, t)

	// 停掉 agent 与 mechd —— 从现在起没有任何推送可言
	stopDriftProcs(t)

	const canary = "log_level: debug # 断连期间手工改的\n"
	if err := os.WriteFile(confPath, []byte(canary), 0o644); err != nil {
		t.Fatal(err)
	}

	// 只起 agent，upstream 指向一个不存在的 socket
	solo := exec.CommandContext(ctx, filepath.Join(binDir, "mechlet"), "agent",
		"--data-dir", saDataDir,
		"--upstream", "unix:///run/mecharion-sa/nonexistent.sock",
		"--node", saNode,
		"--reconcile-interval", driftInterval.String())
	solo.Stdout, solo.Stderr = os.Stdout, os.Stderr
	if err := solo.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProc(solo) })

	// 判据：它把**服务**恢复了。
	//
	// 配置文件是 report 策略不会被改回，因此这里改用 workload——
	// 先手工停掉服务，看断连状态下的周期调和会不会把它拉起来。
	if out, err := exec.CommandContext(ctx, "systemctl", "stop", saUnit).
		CombinedOutput(); err != nil {
		t.Fatalf("停服务: %v\n%s", err, out)
	}
	if !waitUntil(ctx, 60*time.Second, func() bool { return isActive(ctx, saUnit) }) {
		dumpDiagnostics(ctx, t)
		t.Fatal("断连 + 重启之后，周期调和没有把服务恢复——" +
			"本机的期望状态副本没起作用（ADR-0033）")
	}
}

// TestDesiredStateIsOnDiskWithoutPlaintext 钉住落盘的东西长什么样。
//
// 两件事同时成立才对：期望状态**在盘上**（否则重启就没了），
// 且盘上**没有明文口令**（规格会进日志、进诊断包、被人 cat）。
func TestDesiredStateIsOnDiskWithoutPlaintext(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	deployForDrift(ctx, t)

	desiredDir := filepath.Join(saDataDir, "desired")
	entries, err := os.ReadDir(desiredDir)
	if err != nil {
		t.Fatalf("期望状态目录应当存在: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		t.Fatal("期望状态没有落盘，重启后就什么都不知道了")
	}
	// 文件名要能被人一眼看懂（原则七 现场可诊断）
	if !strings.Contains(strings.Join(names, " "), "web__default") {
		t.Errorf("文件名应当一眼看出是哪个实例，实际 %v", names)
	}

	// 保管库存在，且里面不是明文
	vaultDir := filepath.Join(saDataDir, "vault")
	if _, err := os.Stat(filepath.Join(vaultDir, "kek")); err != nil {
		t.Errorf("保管库主密钥应当存在: %v", err)
	}
}

// ── 夹具 ────────────────────────────────────────────────────────────────

var driftProcs []*exec.Cmd

// deployForDrift 把 go-webapp 部署好，返回 token 与配置文件路径。
//
// 与 M3 的单机验收走同一条路，只是把调和周期压短。**刻意不抽成共用夹具**：
// 那条验收测的是「deploy 端到端跑通」，这里测的是「部署之后还在维持」，
// 两者失败时要能分辨是哪一件事坏了。
func deployForDrift(ctx context.Context, t *testing.T) (token, confPath string) {
	t.Helper()

	cleanupStandalone(ctx, t)
	t.Cleanup(func() {
		stopDriftProcs(t)
		cleanupStandalone(context.Background(), t)
	})

	stagePack(t)
	sum := installBlobIn(t, saDataDir, buildTarball(t))
	rewritePackBlob(t, sum)

	mechd := startMechd(ctx, t)
	driftProcs = append(driftProcs, mechd)
	waitAPI(ctx, t)
	token = readToken(t)
	seedSite(ctx, t)

	agent := startAgent(ctx, t, "--reconcile-interval", driftInterval.String())
	driftProcs = append(driftProcs, agent)

	if out, err := runCtl(ctx, token, "component", "deploy", "go-webapp",
		"-c", "web", "--nodes", saNode); err != nil {
		t.Fatalf("deploy 失败: %v\n%s", err, out)
	}
	if !waitUntil(ctx, 90*time.Second, func() bool { return isActive(ctx, saUnit) }) {
		dumpDiagnostics(ctx, t)
		t.Fatal("服务没有起来")
	}

	// 渲染产物落在**节点根**下（/etc/mecharion），不是 mechd 自己的 conf-dir。
	// 这条测试的单机安装没有改节点根，因此就是默认位置。
	confPath = "/etc/mecharion/apps/web/app.yaml"

	// **等一轮调和真正跑完**，而不只是「unit 起来了」。
	//
	// 台账写在健康检查之后（M6 第 1 步：健康没过的 generation 不该被记成
	// active）。只等 unit active 会让测试在调和还在健康检查阶段时就往下走，
	// 读不到台账；随后的清理又会把服务拆掉，让那次在途的检查以
	// connection refused 失败——现场看起来像「部署失败」，其实是测试抢跑。
	ledger := filepath.Join(saDataDir, "mechlet", "instances", "web__default.json")
	if !waitUntil(ctx, 90*time.Second, func() bool {
		if _, err := os.Stat(confPath); err != nil {
			return false
		}
		_, err := os.Stat(ledger)
		return err == nil
	}) {
		dumpDiagnostics(ctx, t)
		t.Fatalf("一轮调和没有跑完（缺 %s 或 %s）", confPath, ledger)
	}
	return token, confPath
}

func stopDriftProcs(t *testing.T) {
	t.Helper()
	for i := len(driftProcs) - 1; i >= 0; i-- {
		stopProc(driftProcs[i])
	}
	driftProcs = nil
}

// driftedResources 问 mechd 有哪些资源被报告为漂移。
//
// **问 mechd 而不是看本机日志**：漂移要能被运维在中心看到才算数，
// 只在 mechlet 的日志里出现一行等于没有。
func driftedResources(ctx context.Context, t *testing.T, token string) []string {
	t.Helper()
	out, err := runCtl(ctx, token, "component", "status", "web", "-o", "json")
	if err != nil {
		return nil
	}
	var st struct {
		Instances []struct {
			Drift []struct {
				Resource string `json:"resource"`
				Policy   string `json:"policy"`
			} `json:"drift"`
		} `json:"instances"`
	}
	if json.Unmarshal([]byte(out), &st) != nil {
		return nil
	}
	var all []string
	for _, in := range st.Instances {
		for _, d := range in.Drift {
			all = append(all, d.Resource)
		}
	}
	return all
}

// TestComponentStopIsHonored 是 **M5 第 2 步的验收**：停了就不会被拉起来。
//
// 这条测试的价值全在**对照**上：同一个动作（`systemctl stop`），
// 在没人说过话时会被当成漂移拉起来，在 `component stop` 之后会被维持。
// 只测后者的话，一个「干脆再也不拉起任何东西」的实现也能通过。
func TestComponentStopIsHonored(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	token, _ := deployForDrift(ctx, t)

	// ── 对照组：没人说过话，手停 → 被拉起来 ──
	runSystemctl(ctx, t, "stop", saUnit)
	if !waitUntil(ctx, 60*time.Second, func() bool { return isActive(ctx, saUnit) }) {
		dumpDiagnostics(ctx, t)
		t.Fatal("没人表达过意图时，手工停止应当被当成漂移并恢复")
	}

	// ── 实验组：component stop 之后，手停 → 保持停着 ──
	if out, err := runCtl(ctx, token, "component", "stop", "web"); err != nil {
		t.Fatalf("component stop 失败: %v\n%s", err, out)
	}

	// mechd 会立刻推一次，调和器把它停掉
	if !waitUntil(ctx, 60*time.Second, func() bool { return !isActive(ctx, saUnit) }) {
		dumpDiagnostics(ctx, t)
		t.Fatal("component stop 之后服务应当被停掉")
	}

	// **关键**：等好几个调和周期，确认它一直停着
	if stayedStopped := !waitUntil(ctx, 4*driftInterval, func() bool {
		return isActive(ctx, saUnit)
	}); !stayedStopped {
		dumpDiagnostics(ctx, t)
		t.Fatal("期望停止的服务被周期调和拉起来了——" +
			"这正是与运维打架的那种行为（20-continuous-reconcile §2.2）")
	}

	// status 里要看得出「是故意停的」，否则与「它挂了」长得一样
	out, err := runCtl(ctx, token, "component", "status", "web")
	if err != nil {
		t.Fatalf("status 失败: %v\n%s", err, out)
	}
	if !strings.Contains(out, "stopped") {
		t.Errorf("status 应显示期望运行态，实际:\n%s", out)
	}
}

// TestComponentStopUndoesManualStart 钉住第三条规则：
// **期望停止却在跑，也是漂移，要停回去。**
//
// 只做「停了就别拉起来」的话，`component stop` 就成了一句没人执行的声明——
// 有人手工把它启动起来，系统会一直默认那是对的，而维护窗口里那台机器
// 其实已经在对外提供服务了。
func TestComponentStopUndoesManualStart(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	token, _ := deployForDrift(ctx, t)

	if out, err := runCtl(ctx, token, "component", "stop", "web"); err != nil {
		t.Fatalf("component stop 失败: %v\n%s", err, out)
	}
	if !waitUntil(ctx, 60*time.Second, func() bool { return !isActive(ctx, saUnit) }) {
		dumpDiagnostics(ctx, t)
		t.Fatal("component stop 之后服务应当被停掉")
	}

	// 有人手工把它起来了
	runSystemctl(ctx, t, "start", saUnit)
	if !isActive(ctx, saUnit) {
		t.Fatal("手工启动没成功，这条测试证明不了任何东西")
	}

	if !waitUntil(ctx, 60*time.Second, func() bool { return !isActive(ctx, saUnit) }) {
		dumpDiagnostics(ctx, t)
		t.Fatal("期望停止时被手工启动，调和应当把它停回去")
	}
}

// TestComponentStartResumes 钉住 start 能恢复。
//
// 一个停得下来却起不回来的组件，等于被从管理中摘掉了。
func TestComponentStartResumes(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	token, _ := deployForDrift(ctx, t)

	if out, err := runCtl(ctx, token, "component", "stop", "web"); err != nil {
		t.Fatalf("component stop 失败: %v\n%s", err, out)
	}
	if !waitUntil(ctx, 60*time.Second, func() bool { return !isActive(ctx, saUnit) }) {
		t.Fatal("component stop 之后服务应当被停掉")
	}

	if out, err := runCtl(ctx, token, "component", "start", "web"); err != nil {
		t.Fatalf("component start 失败: %v\n%s", err, out)
	}
	if !waitUntil(ctx, 60*time.Second, func() bool { return isActive(ctx, saUnit) }) {
		dumpDiagnostics(ctx, t)
		t.Fatal("component start 之后服务应当起来")
	}
}

func runSystemctl(ctx context.Context, t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput(); err != nil {
		t.Fatalf("systemctl %v: %v\n%s", args, err, out)
	}
}

// TestDriftPolicyOverrideRejectsTightening 是 **M5 第 4 步验收的一半**：收紧被拒。
//
// `driftPolicy` 写在 Pack 里，等于 Pack 作者决定了运维现场的临时修改能不能
// 活下来——这个权责关系是反的。站点可以放松它，但**不能反向收紧**：
// reconcile 最坏的后果是「运维只是想试个参数，服务却被改回去并重启了」，
// 而按下这个决定的人不在现场。
func TestDriftPolicyOverrideRejectsTightening(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	token, _ := deployForDrift(ctx, t)

	out, err := runCtl(ctx, token, "component", "set-drift-policy", "web", "reconcile")
	if err == nil {
		t.Fatalf("收紧应当被拒绝\n%s", out)
	}
	if !strings.Contains(out, "relax") {
		t.Errorf("错误应说清「只能放松」，实际:\n%s", out)
	}

	// 拼错的取值同样要拒
	if out, err := runCtl(ctx, token, "component", "set-drift-policy", "web", "typo"); err == nil {
		t.Errorf("拼错的策略应当被拒绝\n%s", out)
	}
}

// TestDriftPolicyOverrideRelaxes 是另一半：放松生效，**且不重启服务**。
//
// 两条判据缺一不可。放松策略多半发生在**事故当中**——运维正想临时改个值
// 而不被工具改回去。若这条命令顺带重启了服务，它就从「帮忙」变成了
// 「二次伤害」，而 06-state-and-drift §4.3 花了整节说这件事不能发生。
func TestDriftPolicyOverrideRelaxes(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	token, confPath := deployForDrift(ctx, t)
	pidBefore := unitProperty(t, saUnit, "MainPID")
	if pidBefore == "" || pidBefore == "0" {
		t.Fatalf("服务应当在跑，MainPID=%q", pidBefore)
	}

	if out, err := runCtl(ctx, token, "component", "set-drift-policy", "web", "ignore"); err != nil {
		t.Fatalf("放松策略失败: %v\n%s", err, out)
	}

	// **不重启**：digest 不含 driftPolicy，因此不该切 generation
	time.Sleep(3 * driftInterval)
	if pidAfter := unitProperty(t, saUnit, "MainPID"); pidAfter != pidBefore {
		dumpDiagnostics(ctx, t)
		t.Errorf("放松漂移策略把服务重启了（pid %s → %s）——"+
			"事故当中的一条止血命令不该顺带停机（06-state-and-drift §4.3）",
			pidBefore, pidAfter)
	}

	// ignore = 不比对：手改配置**不该**被报成漂移
	if err := os.WriteFile(confPath, []byte("log_level: debug # 手工改的\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * driftInterval)
	for time.Now().Before(deadline) {
		if d := driftedResources(ctx, t, token); len(d) > 0 {
			t.Fatalf("策略已放松到 ignore，不该再报漂移，实际: %v", d)
		}
		time.Sleep(driftInterval / 2)
	}

	// 清除覆盖 → 漂移重新出现
	if out, err := runCtl(ctx, token, "component", "set-drift-policy", "web", "none"); err != nil {
		t.Fatalf("清除覆盖失败: %v\n%s", err, out)
	}
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return len(driftedResources(ctx, t, token)) > 0
	}) {
		dumpDiagnostics(ctx, t)
		t.Fatal("清除覆盖之后应当恢复到 Pack 声明的 report，漂移要重新出现")
	}
}

// TestOrphanIsReportedNotDeleted 是 **M5 第 5 步的验收**。
//
// 孤儿 = 机器上还在、但下发里没有的实例。两条判据缺一不可：
//
//	① 在中心**看得到**——只写一条本机日志等于没有
//	② **没被删掉**——卸载不可逆，而「mechd 少发了一条」与「用户真的删了
//	   这个组件」在节点侧分辨不了（20-continuous-reconcile §2.4）
//
// 少了②，一次 mechd 侧的解析失败会静默卸载生产组件；少了①，那台机器上
// 多出来的东西没人知道，直到某天它和新部署抢端口。
func TestOrphanIsReportedNotDeleted(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	token, confPath := deployForDrift(ctx, t)

	// 造一个孤儿：往本机期望状态目录里放一份 mechd 从没下发过的规格，
	// 让它先被调和一次进本地台账，再把它从期望里拿走。
	//
	// 这样造出来的与真实成因（组件被 remove、或某次解析失败漏发）
	// 在 mechlet 看来完全一样——它只知道「台账里有、期望里没有」。
	orphanKey := "ghost__default"
	src := filepath.Join(saDataDir, "mechlet", "instances")
	live, err := os.ReadFile(filepath.Join(src, "web__default.json"))
	if err != nil {
		t.Fatalf("读本机台账: %v", err)
	}
	ghost := strings.ReplaceAll(string(live), `"component": "web"`, `"component": "ghost"`)
	if err := os.WriteFile(filepath.Join(src, orphanKey+".json"), []byte(ghost), 0o644); err != nil {
		t.Fatal(err)
	}

	// 等它被报上去
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return strings.Contains(nodeShow(ctx, t, token), orphanKey)
	}) {
		t.Fatalf("孤儿实例应当出现在 node show 里，实际:\n%s", nodeShow(ctx, t, token))
	}

	// ② **没被删掉**：台账文件还在
	if _, err := os.Stat(filepath.Join(src, orphanKey+".json")); err != nil {
		t.Errorf("孤儿不该被自动清理: %v", err)
	}
	// 正常的那个实例也不该受影响
	if _, err := os.Stat(confPath); err != nil {
		t.Errorf("正常实例的配置不该被动: %v", err)
	}
	if !isActive(ctx, saUnit) {
		t.Error("正常实例的服务不该被停掉")
	}

	// node list 里要有数字——只有 show 才看得到的问题等于没人会发现
	out, err := runCtl(ctx, token, "node", "list")
	if err != nil {
		t.Fatalf("node list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ORPHANED") {
		t.Errorf("node list 应当有孤儿列，实际:\n%s", out)
	}
}

func nodeShow(ctx context.Context, t *testing.T, token string) string {
	t.Helper()
	out, err := runCtl(ctx, token, "node", "show", saNode)
	if err != nil {
		return ""
	}
	return out
}

// TestStatusExplainsDriftAndWorkloadAction 是 **M5 第 6 步的验收**。
//
// 状态输出是给现场的人看的，那时他多半正忙。两件事必须一眼看到：
//
//	① 漂移**会不会被自动改回**——那取决于 driftPolicy（含站点覆盖）。
//	   只印一个资源 id 等于让他去翻 Pack 源码，再翻一遍站点配置。
//	② 工作负载**被拉起来过**——那一轮资源全都没变，不说清楚的话，
//	   一个每分钟崩一次又被拉起的服务与健康的没有区别。
func TestStatusExplainsDriftAndWorkloadAction(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	token, confPath := deployForDrift(ctx, t)

	// ── ① 漂移带着策略 ──
	if err := os.WriteFile(confPath, []byte("log_level: debug\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return strings.Contains(componentStatus(ctx, t, token), "Drift:")
	}) {
		t.Fatalf("漂移应当出现在 status 里，实际:\n%s", componentStatus(ctx, t, token))
	}
	got := componentStatus(ctx, t, token)
	if !strings.Contains(got, "won't be reverted automatically") {
		t.Errorf("status 要说清这条漂移会不会被改回（go-webapp 默认 report），实际:\n%s", got)
	}

	// 放松到 ignore 之后不再比对，漂移消失
	if out, err := runCtl(ctx, token, "component", "set-drift-policy", "web", "ignore"); err != nil {
		t.Fatalf("放松策略: %v\n%s", err, out)
	}
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return !strings.Contains(componentStatus(ctx, t, token), "Drift:")
	}) {
		t.Errorf("放松到 ignore 之后不该再显示漂移，实际:\n%s",
			componentStatus(ctx, t, token))
	}

	// ── ② 工作负载被拉起来过 ──
	runSystemctl(ctx, t, "stop", saUnit)
	if !waitUntil(ctx, 60*time.Second, func() bool { return isActive(ctx, saUnit) }) {
		dumpDiagnostics(ctx, t)
		t.Fatal("服务应当被拉起来")
	}
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return strings.Contains(componentStatus(ctx, t, token), "brought back up")
	}) {
		t.Errorf("status 要说清工作负载被拉起来过，实际:\n%s",
			componentStatus(ctx, t, token))
	}
}

func componentStatus(ctx context.Context, t *testing.T, token string) string {
	t.Helper()
	out, err := runCtl(ctx, token, "component", "status", "web")
	if err != nil {
		return ""
	}
	return out
}

// TestAckDriftSuppressesThenExpires 是 **M5 第 8 步的验收**，
// 覆盖验收表第 6、7 行。
//
// `ack-drift` 给「临时修改」一个名分：运维凌晨救火改了一个值，此前只有
// 两个坏选择——被永远报成异常，或者凌晨三点走一次正式变更。
//
// 三条性质缺一不可，这条测试逐条核对：
//
//	有期限   到点自动恢复告警，不会悄悄变永久
//	有理由   进审计，事后查得到是谁、为什么
//	仍然检测 只是不告警，status 里照常显示「已抑制」
func TestAckDriftSuppressesThenExpires(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	token, confPath := deployForDrift(ctx, t)
	resourceID := "template:" + confPath

	// 运维手工改了一个值
	if err := os.WriteFile(confPath, []byte("log_level: debug\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return instanceResult(ctx, t, token) == "drift"
	}) {
		t.Fatalf("手改之后应当报 drift，实际 %q", instanceResult(ctx, t, token))
	}

	// ── 第 6 行：确认之后不再告警，但仍然显示 ──
	const reason = "排查慢查询，临时调日志级别，待变更单 #1234"
	out, err := runCtl(ctx, token, "component", "ack-drift", "web",
		"--resource", resourceID, "--duration", "20s", "--reason", reason)
	if err != nil {
		t.Fatalf("ack-drift 失败: %v\n%s", err, out)
	}

	if !waitUntil(ctx, 60*time.Second, func() bool {
		return instanceResult(ctx, t, token) != "drift"
	}) {
		t.Fatalf("确认之后不该再报 drift，实际 %q（status:\n%s）",
			instanceResult(ctx, t, token), componentStatus(ctx, t, token))
	}
	// **仍然检测**：status 里照常显示，只是标成已抑制
	if got := componentStatus(ctx, t, token); !strings.Contains(got, "Suppressed") {
		t.Errorf("status 应当显示「已抑制」，实际:\n%s", got)
	}
	// 确认过的修改不该被改回
	if b, _ := os.ReadFile(confPath); string(b) != "log_level: debug\n" {
		t.Errorf("已确认的修改不该被改回，实际 %q", b)
	}
	// ── 第 7 行：到期后恢复告警 ──
	//
	// **这是「有期限」这条性质的唯一验收方式**。少了它，抑制会悄悄变成
	// 永久——那等于把这个组件从管理里摘掉了，而没人会发现。
	if !waitUntil(ctx, 90*time.Second, func() bool {
		return instanceResult(ctx, t, token) == "drift"
	}) {
		dumpDiagnostics(ctx, t)
		t.Fatalf("抑制到期后应当恢复告警，实际 %q（status:\n%s）",
			instanceResult(ctx, t, token), componentStatus(ctx, t, token))
	}
	if got := componentStatus(ctx, t, token); strings.Contains(got, "Suppressed") {
		t.Errorf("抑制已到期，不该再显示「已抑制」，实际:\n%s", got)
	}
}

// instanceResult 取第一个实例上报的调和结论。
//
// 用它而不是看 status 的文字：**「有没有告警」的判据是结论本身**
// （ok / changed / drift），而不是某一行的措辞。
func instanceResult(ctx context.Context, t *testing.T, token string) string {
	t.Helper()
	out, err := runCtl(ctx, token, "component", "status", "web", "-o", "json")
	if err != nil {
		return ""
	}
	var st struct {
		Instances []struct {
			Result string `json:"result"`
		} `json:"instances"`
	}
	if json.Unmarshal([]byte(out), &st) != nil || len(st.Instances) == 0 {
		return ""
	}
	return st.Instances[0].Result
}

// TestRollbackPointIsRecorded 是 **M6 第 2 步的验收**。
//
// 自动回滚需要节点上有一份**自足的旧规格**：只切软链不够——unit、渲染出的
// 配置、容器的 env 与挂载都是按新规格写的，只切软链会得到「旧的二进制配上
// 新的 unit」，一个从没被测试过的组合。
//
// 这条测试确认那份规格在一次正常部署之后就已经在盘上，而且**不会被当成
// 一个额外的实例**去调和。
func TestRollbackPointIsRecorded(t *testing.T) {
	requireEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	token, _ := deployForDrift(ctx, t)

	applied := filepath.Join(saDataDir, "desired", "web__default.applied.json")
	if !waitUntil(ctx, 60*time.Second, func() bool {
		_, err := os.Stat(applied)
		return err == nil
	}) {
		entries, _ := os.ReadDir(filepath.Join(saDataDir, "desired"))
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("一次成功部署之后应当留下回滚点，desired 目录里只有 %v", names)
	}

	// 它必须是一份**完整可用**的规格，不是半截东西
	body, err := os.ReadFile(applied)
	if err != nil {
		t.Fatal(err)
	}
	var sp struct {
		Digest    string `json:"digest"`
		Component string `json:"component"`
		Workload  *struct {
			Runtime string `json:"runtime"`
		} `json:"workload"`
	}
	if err := json.Unmarshal(body, &sp); err != nil {
		t.Fatalf("回滚点不是合法规格: %v", err)
	}
	if sp.Digest == "" || sp.Component != "web" || sp.Workload == nil {
		t.Errorf("回滚点内容不完整: digest=%q component=%q workload=%v",
			sp.Digest, sp.Component, sp.Workload)
	}

	// **不该被当成第二个实例**：status 里仍然只有一个
	out, err := runCtl(ctx, token, "component", "status", "web", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Instances []struct {
			Role string `json:"role"`
		} `json:"instances"`
	}
	if json.Unmarshal([]byte(out), &st) != nil {
		t.Fatalf("status 解析失败:\n%s", out)
	}
	if len(st.Instances) != 1 {
		t.Errorf("回滚点不该变成一个额外的实例，实际 %d 个", len(st.Instances))
	}
}
