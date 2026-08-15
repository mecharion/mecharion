package reconcile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/state"
)

// TestDriftPolicyDoesNotBlockDesiredChange 钉住「漂移」与「期望变了」的区别。
//
// 这两者容易被混成一件事，但后果完全相反：
//
//	期望状态变了 → 必须落地。一个 driftPolicy: report 的模板若拦住它，
//	               用户改了参数却发现配置没变，而且没有任何报错。
//	期望没变、机器变了 → 那才是漂移，默认只上报不动手。
//
// 判据是 generation 有没有换：digest 变了说明期望变了。
func TestDriftPolicyDoesNotBlockDesiredChange(t *testing.T) {
	f := newFixture(t)
	confPath := filepath.Join(f.path("etc", "mecharion", "apps", "webapp"), "app.yaml")

	f.MustReconcile(f.webappSpec()) // driftPolicy 默认 report

	changed := f.webappSpec(func(x *spec.ResolvedSpec) { setContent(x, "port: 9090\n") })
	rep := f.MustReconcile(changed)

	body, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "port: 9090\n" {
		t.Errorf("期望状态变了就必须落地，实际文件内容 = %q", body)
	}
	if rep.Resources[0].Action != ActionApplied {
		t.Errorf("动作 = %s，期望 applied", rep.Resources[0].Action)
	}
}

// TestDriftIsOnlyReportedByDefault 钉住「默认只上报不动手」。
//
// 自动改回一个运维人员为了救火而临时修改的配置，是比漂移本身更严重的事故。
func TestDriftIsOnlyReportedByDefault(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec()
	f.MustReconcile(s)

	confPath := filepath.Join(f.path("etc", "mecharion", "apps", "webapp"), "app.yaml")
	if err := os.WriteFile(confPath, []byte("port: 1234\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	rep := f.MustReconcile(s) // 同一份规格 —— 期望没变

	if rep.Result != ResultDrift {
		t.Errorf("应当报 drift，实际 %s", rep.Result)
	}
	if rep.Resources[0].Action != ActionReported {
		t.Errorf("动作 = %s，期望 reported", rep.Resources[0].Action)
	}
	if len(rep.Resources[0].Changes) == 0 {
		t.Error("上报必须带着差异内容，否则人无从判断")
	}

	body, _ := os.ReadFile(confPath)
	if string(body) != "port: 1234\n" {
		t.Errorf("默认策略不该动手改回，实际 %q", body)
	}
	if len(rep.Notified) > 0 {
		t.Error("没有真的 Apply 就不该 notify")
	}
}

// TestDriftPolicyReconcileFixesIt 钉住 reconcile 策略会改回去。
func TestDriftPolicyReconcileFixesIt(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec(func(x *spec.ResolvedSpec) {
		x.Resources[0].DriftPolicy = "reconcile"
	})
	f.MustReconcile(s)

	confPath := filepath.Join(f.path("etc", "mecharion", "apps", "webapp"), "app.yaml")
	if err := os.WriteFile(confPath, []byte("port: 1234\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	rep := f.MustReconcile(s)

	if rep.Resources[0].Action != ActionApplied {
		t.Errorf("动作 = %s，期望 applied", rep.Resources[0].Action)
	}
	body, _ := os.ReadFile(confPath)
	if string(body) != "port: 8080\n" {
		t.Errorf("应当改回期望值，实际 %q", body)
	}
	// 改回去了，声明的 notify 就该跟着触发——否则配置回来了但进程还在用旧的
	if len(rep.Notified) == 0 {
		t.Error("确有改动就该触发 notify")
	}
}

// TestDriftPolicyIgnoreSkipsComparison 钉住 ignore 根本不比对。
func TestDriftPolicyIgnoreSkipsComparison(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec(func(x *spec.ResolvedSpec) {
		x.Resources[0].DriftPolicy = "ignore"
	})
	f.MustReconcile(s)

	rep := f.MustReconcile(s)
	if rep.Resources[0].Action != ActionIgnored {
		t.Errorf("动作 = %s，期望 ignored", rep.Resources[0].Action)
	}
}

// TestWorkloadRestoreCountsAsChange 钉住「恢复工作负载」不会被报成 ok。
//
// 少了这条，一次「服务被人停了、调和把它拉起来」在报告里不留任何痕迹：
// 没有资源被 Apply、没有切软链，于是结果是 ok。**一个每分钟崩一次又被
// 拉起的服务，从中心看完全健康**——而那正是最该被看见的一种故障。
func TestWorkloadRestoreCountsAsChange(t *testing.T) {
	quiet := &Report{Component: "web", Role: "default"}
	if quiet.Changed() {
		t.Fatal("什么都没做的一轮不该算变化")
	}

	restored := &Report{
		Component: "web", Role: "default",
		WorkloadAction: WorkloadRestored,
	}
	if !restored.Changed() {
		t.Error("拉起一个不该停的服务是一次变化，不是无事发生")
	}
	if got := restored.Summary(); !strings.Contains(got, "restored") {
		t.Errorf("摘要里要说清工作负载被恢复了，实际: %s", got)
	}

	stopped := &Report{
		Component: "web", Role: "default",
		WorkloadAction: WorkloadStopped,
	}
	if !stopped.Changed() {
		t.Error("把不该跑的服务停回去同样是一次变化")
	}
}

// TestIgnoreStillMaterializes 钉住 `driftPolicy: ignore` **不阻止首次物化**。
//
// 早先 ignore 是在读取之前就 continue 掉的，于是一个标了 ignore 的配置文件
// **从来不会被创建**——而 Pack 作者标它恰恰是因为「应用自己会改写这个
// 文件」，那个文件仍然得先有个初值。升级换 generation 时它也不会更新。
//
// §4 明说 **driftPolicy 只管漂移，不管期望变更**，这条曾经是反的。
func TestIgnoreStillMaterializes(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec(func(s *spec.ResolvedSpec) {
		for i := range s.Resources {
			s.Resources[i].DriftPolicy = "ignore"
		}
	})
	f.MustReconcile(s)

	// 资源必须真的落到盘上
	for _, r := range s.Resources {
		if !strings.HasPrefix(r.ID, "template:") {
			continue
		}
		var args struct {
			Dest string `json:"dest"`
		}
		if err := json.Unmarshal(r.Args, &args); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(args.Dest); err != nil {
			t.Errorf("driftPolicy=ignore 不该阻止首次物化，%s 没被创建: %v",
				args.Dest, err)
		}
	}
}

// TestHealthFailureDoesNotMarkActive 钉住台账顺序（M6 第 1 步）。
//
// 早先健康检查排在写台账之后，于是一个健康没过的 generation **已经被记成
// 当前版本**。下一轮读到「当前 digest == 期望 digest」，判定为复用——
// 不再切换、不再重试。机器停在坏版本上，而台账说一切正常。
func TestHealthFailureDoesNotMarkActive(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec(func(x *spec.ResolvedSpec) {
		// 探一个必然不通的端口，且宽限期极短
		x.Health = &spec.Health{
			TCP:          &spec.TCPProbe{Port: 1},
			StartupGrace: "1s",
			Interval:     "200ms",
			Timeout:      "200ms",
		}
	})

	if _, err := f.Reconcile(s); err == nil {
		t.Fatal("健康检查不通时调和应当失败")
	}

	in := f.Instance("webapp", "default")
	if in == nil {
		t.Fatal("应当写下了状态")
	}
	if a := in.Active(); a != nil && a.Digest == s.Digest {
		t.Errorf("健康没过的 generation 不该被记成 active，实际 %+v", a)
	}
	g := in.FindGeneration(s.Digest)
	if g == nil {
		t.Fatal("失败的 generation 仍应入账，供诊断与「不再重试」判定")
	}
	if g.State != state.GenFailed {
		t.Errorf("它应当被记为 failed，实际 %s", g.State)
	}
}

// TestFailedDigestIsNotRetried 钉住失败版本不会反复横跳（M6 第 4 步）。
//
// 不锁的话，一个起不来的版本会每个调和周期被重试一次，**而每次重试都要停
// 一次服务**——在两个坏状态之间来回，比稳定停在旧版糟得多。
func TestFailedDigestIsNotRetried(t *testing.T) {
	f := newFixture(t)

	// 先有一个装好的旧版本——**锁的前提是有地方可以停留**
	f.MustReconcile(f.webappSpec())

	// 升级到一个健康检查过不去的新版本
	bad := f.webappSpec(func(x *spec.ResolvedSpec) {
		x.Pack.Version = "1.3.0"
		x.Health = &spec.Health{
			TCP:          &spec.TCPProbe{Port: 1},
			StartupGrace: "1s",
			Interval:     "200ms",
			Timeout:      "200ms",
		}
	})
	s := bad

	if _, err := f.Reconcile(s); err == nil {
		t.Fatal("第一次升级应当失败")
	}

	// 同一份规格再来一轮：不该再尝试
	rep, err := f.Reconcile(s)
	if err == nil {
		t.Fatal("同一个失败过的 digest 应当被拒绝，而不是再试一次")
	}
	if !rep.Blocked {
		t.Errorf("报告要标出「因为上次失败而没有尝试」，实际 %+v", rep)
	}
	if !strings.Contains(err.Error(), "不再自动重试") {
		t.Errorf("错误要说清为什么没动，实际: %v", err)
	}
	// **必须给出解锁办法**，否则用户只知道卡住了
	if !strings.Contains(err.Error(), "换一个版本") {
		t.Errorf("错误要给出解锁办法，实际: %v", err)
	}
}

// TestFirstInstallFailureIsRetried 钉住失败锁**不适用于首次安装**。
//
// 这条是从一次真实的测试失败里长出来的：只判 GenFailed 的话，首装时一次
// 瞬时的健康检查失败（依赖服务还没起来、端口刚好被占、别的操作正在停这个
// 服务）会把组件**永久卡死**——而首装根本没有服务可丢。
//
// 锁要防的是「反复停机」，而首装时没有机可停。
func TestFirstInstallFailureIsRetried(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec(func(x *spec.ResolvedSpec) {
		x.Health = &spec.Health{
			TCP:          &spec.TCPProbe{Port: 1},
			StartupGrace: "1s",
			Interval:     "200ms",
			Timeout:      "200ms",
		}
	})

	if _, err := f.Reconcile(s); err == nil {
		t.Fatal("第一次应当失败")
	}
	rep, err := f.Reconcile(s)
	if err == nil {
		t.Fatal("健康仍然不通，第二次也应当失败")
	}
	if rep.Blocked {
		t.Error("首装失败不该被锁住——没有旧版本可以停留，重试是对的")
	}
}
