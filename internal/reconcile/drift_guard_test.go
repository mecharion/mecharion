package reconcile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mecharion/mecharion/internal/resource"
	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/state"
)

// tamper 手工改坏配置文件，制造一次漂移。
func (f *fixture) tamper(t *testing.T) string {
	t.Helper()
	p := filepath.Join(f.path("etc", "mecharion", "apps", "webapp"), "app.yaml")
	if err := os.WriteFile(p, []byte("port: 1234\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

// ── ① 显式确认的漂移不再告警 ────────────────────────────────────────────

// TestAckedDriftIsNotReported 钉住「运维说了算」。
//
// 临时修改在模型里本来没有名分：运维凌晨救火改了一个值，要么被永远报成
// 异常，要么得走一次正式变更。ack 给了它第三个选择。
func TestAckedDriftIsNotReported(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec(func(x *spec.ResolvedSpec) {
		x.Resources[0].DriftPolicy = "reconcile" // 即便策略要求改回
	})
	f.MustReconcile(s)
	confPath := f.tamper(t)

	// 运维确认：我知道，是我改的。
	//
	// 抑制**随规格下发**——早先这里写的是 state.Instance.Suppressions，
	// 而那个字段没有任何生产代码会写。测试自己填上去，于是这条用例
	// 一直是绿的，掩盖了「ack-drift 从来没到过节点」这件事。
	s.Suppressions = []spec.Suppression{{
		Resource: "template:app.yaml",
		Reason:   "排查慢查询，临时调 log_min_duration",
		Until:    time.Now().Add(4 * time.Hour).UTC().Format(time.RFC3339),
	}}

	rep := f.MustReconcile(s)

	if rep.Resources[0].Action != ActionSuppressed {
		t.Errorf("动作 = %s，期望 suppressed", rep.Resources[0].Action)
	}
	// 仍然检测得到，只是不告警——status 里照样看得见
	if !strings.Contains(rep.Resources[0].Reason, "排查慢查询") {
		t.Errorf("应当带上确认理由，实际 %q", rep.Resources[0].Reason)
	}
	if body, _ := os.ReadFile(confPath); string(body) != "port: 1234\n" {
		t.Errorf("确认过的修改不该被改回，实际 %q", body)
	}
	if rep.Result == ResultDrift {
		t.Error("已确认的漂移不该让整体结论变成 drift")
	}
}

// TestExpiredAckResumesReporting 钉住「有期限」——不会悄悄变成永久。
func TestExpiredAckResumesReporting(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec()
	f.MustReconcile(s)
	f.tamper(t)

	s.Suppressions = []spec.Suppression{{
		Resource: "template:app.yaml",
		Reason:   "已经过期了",
		Until:    time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	}}

	rep := f.MustReconcile(s)

	if rep.Resources[0].Action != ActionReported {
		t.Errorf("过期后应当恢复告警，实际 %s", rep.Resources[0].Action)
	}
}

// TestWholeInstanceAck 钉住「不指定资源时抑制整个实例」。
func TestWholeInstanceAck(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec()
	f.MustReconcile(s)
	f.tamper(t)

	s.Suppressions = []spec.Suppression{{
		Reason: "整机维护窗口",
		Until:  time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}}

	rep := f.MustReconcile(s)
	if rep.Resources[0].Action != ActionSuppressed {
		t.Errorf("未指定资源应当覆盖整个实例，实际 %s", rep.Resources[0].Action)
	}
}

// ── ② 自动改回不得顺带重启 ──────────────────────────────────────────────

// TestDriftReconcileWillNotRestartByDefault 钉住最不该自作主张的那个动作。
//
// 运维只是想试个参数，服务却在他手底下重启了——这比漂移本身严重得多。
func TestDriftReconcileWillNotRestartByDefault(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec(func(x *spec.ResolvedSpec) {
		x.Resources[0].DriftPolicy = "reconcile"
		x.Resources[0].Notify = NotifyRestart
	})
	f.MustReconcile(s)
	confPath := f.tamper(t)
	f.RT.Reset()

	rep := f.MustReconcile(s)

	if rep.Resources[0].Action != ActionReported {
		t.Errorf("会连带重启的改回应当降级为上报，实际 %s", rep.Resources[0].Action)
	}
	if !strings.Contains(rep.Resources[0].Reason, "重启") {
		t.Errorf("应当说清为什么没动手，实际 %q", rep.Resources[0].Reason)
	}
	if f.RT.actionsContain("stop") || f.RT.actionsContain("start") {
		t.Errorf("绝不能自作主张重启，实际 %v", f.RT.Actions())
	}
	if body, _ := os.ReadFile(confPath); string(body) != "port: 1234\n" {
		t.Error("没获准就不该改回文件")
	}
}

// TestDriftReconcileRestartWhenAllowed 钉住显式允许后确实会做。
func TestDriftReconcileRestartWhenAllowed(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec(func(x *spec.ResolvedSpec) {
		x.Resources[0].DriftPolicy = "reconcile"
		x.Resources[0].Notify = NotifyRestart
		x.Reconcile.AllowDriftRestart = true
	})
	f.MustReconcile(s)
	confPath := f.tamper(t)
	f.RT.Reset()

	rep := f.MustReconcile(s)

	if rep.Resources[0].Action != ActionApplied {
		t.Errorf("显式允许后应当改回，实际 %s", rep.Resources[0].Action)
	}
	if body, _ := os.ReadFile(confPath); string(body) != "port: 8080\n" {
		t.Errorf("应当改回期望值，实际 %q", body)
	}
	if !f.RT.actionsContain("start") {
		t.Errorf("应当执行 notify restart，实际 %v", f.RT.Actions())
	}
}

// TestReloadDriftStillWorks 确认只是 restart 受限，reload 不受影响。
//
// reload 不中断服务，没有「在运维手底下把服务重启了」的问题。
func TestReloadDriftStillWorks(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec(func(x *spec.ResolvedSpec) {
		x.Resources[0].DriftPolicy = "reconcile"
		x.Resources[0].Notify = NotifyReload
	})
	f.MustReconcile(s)
	f.tamper(t)
	f.RT.Reset()

	rep := f.MustReconcile(s)
	if rep.Resources[0].Action != ActionApplied {
		t.Errorf("reload 类的改回不该被拦，实际 %s", rep.Resources[0].Action)
	}
	if !f.RT.actionsContain("reload") {
		t.Errorf("应当执行 reload，实际 %v", f.RT.Actions())
	}
}

// ── ③ 检测开销 ──────────────────────────────────────────────────────────

// TestDigestCacheAvoidsRehashing 钉住「内容没变就不重复哈希」。
//
// 调和每 60 秒一轮。一个装了十个组件、各带 50MB 二进制的节点，不缓存
// 就是每分钟读并哈希 500MB——纯浪费。
func TestDigestCacheAvoidsRehashing(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec()
	f.MustReconcile(s)

	confPath := filepath.Join(f.path("etc", "mecharion", "apps", "webapp"), "app.yaml")
	in := f.Instance("webapp", "default")
	if _, ok := in.Digests[confPath]; !ok {
		t.Fatalf("首轮应当把摘要写进缓存，实际 %v", in.Digests)
	}

	fi, err := os.Stat(confPath)
	if err != nil {
		t.Fatal(err)
	}

	// 刚写完的条目不可信：mtime 与记录时刻挨得太近，无从排除
	// 「文件在同一 mtime 刻度内又被改过」
	if _, ok := newDigestCache(in).Get(confPath, fi.Size(), fi.ModTime()); ok {
		t.Error("刚写完的条目不该被信任——那正是 racily clean 窗口")
	}

	// 稳态：文件是上一轮写的，本轮在 60 秒之后。用改写记录时刻来模拟，
	// 免得让测试真的睡一秒。
	steady := in.Digests[confPath]
	steady.CachedAt = fi.ModTime().Add(2 * resource.RacyWindow)
	in.Digests[confPath] = steady

	if _, ok := newDigestCache(in).Get(confPath, fi.Size(), fi.ModTime()); !ok {
		t.Error("稳态下同样的 (size, mtime) 应当命中缓存，省掉一次全量哈希")
	}
	// 大小或 mtime 任一不同都必须重算
	if _, ok := newDigestCache(in).Get(confPath, fi.Size()+1, fi.ModTime()); ok {
		t.Error("大小变了必须重算")
	}
	if _, ok := newDigestCache(in).Get(confPath, fi.Size(), fi.ModTime().Add(time.Second)); ok {
		t.Error("mtime 变了必须重算")
	}
}

// TestDigestCacheClosesRacyWindow 钉住「同尺寸同刻度的改写不会被漏掉」。
//
// `port: 8080` 改成 `port: 1234` 恰好同样长度。只比 (size, mtime) 的话，
// 若改写落在记录摘要的同一刻度里，这处漂移会**永久**检测不到。
func TestDigestCacheClosesRacyWindow(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec()
	f.MustReconcile(s)

	confPath := filepath.Join(f.path("etc", "mecharion", "apps", "webapp"), "app.yaml")
	orig, err := os.Stat(confPath)
	if err != nil {
		t.Fatal(err)
	}

	// 同样长度、并把 mtime 恢复成原样——最坏情况
	if err := os.WriteFile(confPath, []byte("port: 1234\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(confPath, orig.ModTime(), orig.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(confPath)
	if after.Size() != orig.Size() {
		t.Fatalf("用例前提不成立：两份内容长度应当相同（%d vs %d）",
			orig.Size(), after.Size())
	}

	in := f.Instance("webapp", "default")
	if _, ok := newDigestCache(in).Get(confPath, after.Size(), after.ModTime()); ok {
		t.Error("mtime 与记录时刻同刻度的条目必须重算，否则这处漂移永远发现不了")
	}

	rep := f.MustReconcile(s)
	if rep.Result != ResultDrift {
		t.Errorf("应当检出漂移，实际 %s", rep.Result)
	}
}

// TestDigestCacheStillDetectsDrift 确认缓存没把漂移检测弄瞎。
func TestDigestCacheStillDetectsDrift(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec()
	f.MustReconcile(s)
	f.tamper(t)

	rep := f.MustReconcile(s)
	if rep.Result != ResultDrift {
		t.Fatalf("缓存不能让漂移检测失效，实际 %s", rep.Result)
	}
	if rep.Resources[0].Action != ActionReported {
		t.Errorf("动作 = %s", rep.Resources[0].Action)
	}
}

// TestDigestCacheEvictsStalePaths 钉住缓存不会无限增长。
func TestDigestCacheEvictsStalePaths(t *testing.T) {
	in := &state.Instance{Digests: map[string]state.DigestEntry{
		"/old/path": {Size: 1, SHA256: "aa"},
		"/kept":     {Size: 2, SHA256: "bb"},
	}}
	c := newDigestCache(in)
	// 本轮只碰了 /kept
	c.Get("/kept", 2, time.Time{})
	c.commit(in)

	if _, ok := in.Digests["/old/path"]; ok {
		t.Error("本轮没碰过的路径应当被淘汰——否则状态文件越滚越大")
	}
	if _, ok := in.Digests["/kept"]; !ok {
		t.Error("本轮用到的路径应当保留")
	}
}
