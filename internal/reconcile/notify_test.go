package reconcile

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	rt "github.com/mecharion/mecharion/internal/runtime"
	"github.com/mecharion/mecharion/internal/spec"
)

// ── 聚合规则（纯逻辑） ──────────────────────────────────────────────────

func TestNotifyDeduplicates(t *testing.T) {
	n := newNotifySet()
	n.add(NotifyReload, "template:a")
	n.add(NotifyReload, "template:b")
	n.add(NotifyReload, "template:c")

	if got := n.resolved(); !reflect.DeepEqual(got, []string{NotifyReload}) {
		t.Errorf("三个模板都 notify: reload，只该 reload 一次，实际 %v", got)
	}
	if got := n.causesOf(NotifyReload); len(got) != 3 {
		t.Errorf("触发来源应当都记下来供诊断，实际 %v", got)
	}
}

// TestRestartAbsorbsReload 钉住「restart 吸收 reload」。
//
// 进程既然要整个重来，再让它先热加载一次是纯粹的浪费，而且那一次
// reload 读的是即将被丢弃的进程状态。
func TestRestartAbsorbsReload(t *testing.T) {
	n := newNotifySet()
	n.add(NotifyReload, "template:conf")
	n.add(NotifyRestart, "template:port")

	got := n.resolved()
	if !reflect.DeepEqual(got, []string{NotifyRestart}) {
		t.Errorf("resolved = %v，期望只剩 restart", got)
	}
	if !reflect.DeepEqual(n.absorbed(), []string{NotifyReload}) {
		t.Errorf("被吸收的动作要记下来给人看，实际 %v", n.absorbed())
	}
}

func TestNotifyEmptyByDefault(t *testing.T) {
	n := newNotifySet()
	if !n.Empty() {
		t.Error("没登记过就该是空的")
	}
	n.add("", "template:x") // 资源没声明 notify
	if !n.Empty() {
		t.Error("空动作不该被登记")
	}
	if n.resolved() != nil {
		t.Error("空集合应当返回 nil")
	}
}

// TestNotifyOrderPutsRestartFirst 钉住 restart 排在最前。
//
// hook 类的 notify 多半是「配置生效后做点什么」，在进程重启之后执行
// 才有意义。
func TestNotifyOrderPutsRestartFirst(t *testing.T) {
	n := newNotifySet()
	n.add("warm-cache", "script:warm")
	n.add(NotifyRestart, "template:port")
	n.add("announce", "script:ann")

	got := n.resolved()
	if len(got) != 3 || got[0] != NotifyRestart {
		t.Errorf("restart 应当排最前，实际 %v", got)
	}
}

// ── 与调和的联动 ────────────────────────────────────────────────────────

// TestNotifyNotFiredWithoutDiff 是最重要的一条。
//
// 调和每 60 秒跑一次。若无差异也触发 notify，服务会每 60 秒被重启一次，
// 永远无法稳定运行——而且 `apply` 与周期性调和走同一条代码路径，
// 引擎无法区分「用户主动触发」与「定时器到期」。
func TestNotifyNotFiredWithoutDiff(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec()
	f.MustReconcile(s)

	for i := 0; i < 3; i++ {
		f.RT.Reset()
		rep := f.MustReconcile(s)
		if len(rep.Notified) > 0 {
			t.Fatalf("第 %d 轮无差异却触发了 notify: %v", i+1, rep.Notified)
		}
		for _, a := range f.RT.Actions() {
			if a == "stop" || a == "start" || a == "reload" {
				t.Fatalf("第 %d 轮无差异却惊动了进程: %v", i+1, f.RT.Actions())
			}
		}
	}
}

// TestNotifyReloadOnDriftReconcile 钉住「确有改动才 reload」。
func TestNotifyReloadOnDriftReconcile(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec(func(x *spec.ResolvedSpec) {
		x.Resources[0].DriftPolicy = "reconcile"
		x.Resources[0].Notify = NotifyReload
	})
	f.MustReconcile(s)

	confPath := filepath.Join(f.path("etc", "mecharion", "apps", "webapp"), "app.yaml")
	if err := os.WriteFile(confPath, []byte("port: 1234\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	f.RT.Reset()

	rep := f.MustReconcile(s)

	if len(rep.Notified) != 1 || rep.Notified[0] != NotifyReload {
		t.Errorf("Notified = %v，期望 [reload]", rep.Notified)
	}
	if !f.RT.actionsContain("reload") {
		t.Errorf("应当调用 Runtime.Reload，实际 %v", f.RT.Actions())
	}
}

// TestReloadFallsBackToRestart 钉住「不支持热加载就降级为重启」。
//
// Pack 声明了 reload 但工作负载不支持时，什么都不做等于变更没生效。
func TestReloadFallsBackToRestart(t *testing.T) {
	f := newFixture(t)
	f.RT.reloadErr = rt.ErrReloadUnsupported

	s := f.webappSpec(func(x *spec.ResolvedSpec) {
		x.Resources[0].DriftPolicy = "reconcile"
		x.Resources[0].Notify = NotifyReload
	})
	f.MustReconcile(s)

	confPath := filepath.Join(f.path("etc", "mecharion", "apps", "webapp"), "app.yaml")
	if err := os.WriteFile(confPath, []byte("port: 1234\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	f.RT.Reset()

	rep := f.MustReconcile(s)

	if !f.RT.actionsContain("stop") || !f.RT.actionsContain("start") {
		t.Errorf("应当降级为重启，实际 %v", f.RT.Actions())
	}
	if len(rep.Notified) != 1 || rep.Notified[0] != NotifyRestart {
		t.Errorf("报告里应当记成 restart（实际做的就是重启），实际 %v", rep.Notified)
	}
}

// TestGenerationSwitchSupersedesNotify 钉住「换 generation 不走 notify」。
//
// 那是 generation 切换流程本身处理的（停服务、切软链、起服务）。
// 再叠一次 notify 就是在刚起来的进程上又重启一遍。
func TestGenerationSwitchSupersedesNotify(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec()
	f.MustReconcile(s)

	f.RT.Reset()
	changed := f.webappSpec(func(x *spec.ResolvedSpec) { setContent(x, "port: 9090\n") })
	rep := f.MustReconcile(changed)

	if len(rep.Notified) > 0 {
		t.Errorf("切换 generation 时不该再执行 notify，实际 %v", rep.Notified)
	}

	// stop → start 各一次，不该有第二轮
	stops, starts := 0, 0
	for _, a := range f.RT.Actions() {
		switch a {
		case "stop":
			stops++
		case "start":
			starts++
		}
	}
	if stops != 1 || starts != 1 {
		t.Errorf("切换应当只停一次起一次，实际 %v", f.RT.Actions())
	}
}

// TestUnknownNotifyTargetIsRejected 钉住指向 hook 的 notify 明确报错。
//
// 静默跳过会让 Pack 作者以为它生效了。
func TestUnknownNotifyTargetIsRejected(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec(func(x *spec.ResolvedSpec) {
		x.Resources[0].DriftPolicy = "reconcile"
		x.Resources[0].Notify = "warm-cache"
	})
	f.MustReconcile(s)

	confPath := filepath.Join(f.path("etc", "mecharion", "apps", "webapp"), "app.yaml")
	if err := os.WriteFile(confPath, []byte("port: 1234\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	_, err := f.Reconcile(s)
	if err == nil {
		t.Fatal("指向未实现的 hook 应当报错，而不是静默跳过")
	}
}
