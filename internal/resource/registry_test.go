package resource

import (
	"errors"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/spec"
)

// TestUnknownVsPlannedType 钉住「拼错类型名」与「类型还没做」的区分。
//
// 前者用户改一个字母就好，后者只能等版本——把两者混成同一句话，
// 会让用户在自己没错的时候反复检查拼写。
func TestUnknownVsPlannedType(t *testing.T) {
	env := testEnv(t)

	_, err := New(env, mk(t, "x", "directroy", map[string]any{"path": absPath("/tmp/x")}))
	if err == nil || !strings.Contains(err.Error(), "未知的资源类型") {
		t.Errorf("拼错的类型名应报「未知」，实际: %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "本版本支持") {
		t.Errorf("未知类型的错误应列出可用类型，实际: %v", err)
	}

	_, err = New(env, mk(t, "x", "sysctl", map[string]any{"key": "vm.swappiness", "value": "1"}))
	if err == nil || !strings.Contains(err.Error(), "尚未实现") {
		t.Errorf("规范里有但没实现的类型应报「尚未实现」，实际: %v", err)
	}
}

// TestBuildReportsAllErrors 钉住「构造阶段一次报全」。
//
// 参数写错是配置问题，应当在动手改机器之前全部暴露，而不是执行到
// 第三条时才发现第四条写错了——那时前三条已经落地了。
func TestBuildReportsAllErrors(t *testing.T) {
	env := testEnv(t)
	_, err := Build(env, []spec.Resource{
		mk(t, "directory:ok", TypeDirectory, map[string]any{"path": absPath("/tmp/ok")}),
		mk(t, "file:bad1", TypeFile, map[string]any{"path": absPath("/tmp/a")}),
		mk(t, "symlink:bad2", TypeSymlink, map[string]any{"path": absPath("/tmp/l")}),
	})
	if err == nil {
		t.Fatal("应当报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "file:bad1") || !strings.Contains(msg, "symlink:bad2") {
		t.Errorf("两条错误都该出现在同一条消息里，实际:\n%s", msg)
	}
	if !strings.Contains(msg, "2 条资源声明有问题") {
		t.Errorf("应当报出错误条数，实际:\n%s", msg)
	}
}

func TestBuildPreservesOrder(t *testing.T) {
	env := testEnv(t)
	rs := []spec.Resource{
		mk(t, "user:webapp", TypeUser, map[string]any{"name": "webapp"}),
		mk(t, "archive:main", TypeArchive, map[string]any{
			"blob": "main", "dest": absPath("/tmp/gen"),
		}),
		mk(t, "directory:data", TypeDirectory, map[string]any{"path": absPath("/tmp/data")}),
	}
	built, err := Build(env, rs)
	if err != nil {
		t.Fatal(err)
	}
	// 声明顺序即应用顺序：建用户必须早于解包，解包早于建数据目录
	for i, want := range []string{"user:webapp", "archive:main", "directory:data"} {
		if built[i].ID() != want {
			t.Errorf("第 %d 条 = %s，期望 %s", i, built[i].ID(), want)
		}
	}
}

// TestErrorClassDefaultsToPermanent 钉住「未分类错误按不可重试处理」。
func TestErrorClassDefaultsToPermanent(t *testing.T) {
	if got := ClassOf(errors.New("裸错误")); got != ErrPermanent {
		t.Errorf("未分类错误应按 permanent 处理（当成可重试会无限重试一个配置错误），实际 %s", got)
	}
	if got := ClassOf(nil); got != ErrPermanent {
		t.Errorf("ClassOf(nil) = %s", got)
	}

	// 包装后仍可识别
	wrapped := Transient("读取", errors.New("超时"))
	if !IsTransient(wrapped) {
		t.Error("Transient 标记应当可识别")
	}
	if !strings.Contains(wrapped.Error(), "读取") {
		t.Errorf("错误信息应带上操作名: %v", wrapped)
	}
}

// TestUndeclaredFieldsAreNotDrift 钉住「Pack 没声明的字段不参与比对」。
//
// 否则每个没写 owner 的资源都会在每轮调和里报一次漂移。
func TestUndeclaredFieldsAreNotDrift(t *testing.T) {
	var b diffBuilder
	o := ownership{Mode: "0644"} // 只声明了 mode
	o.diffInto(&b, present(map[string]any{
		"mode": "0644", "owner": "nobody", "group": "nogroup",
	}))
	if len(b.changes) != 0 {
		t.Errorf("未声明的 owner/group 不该报差异，实际 %v", b.changes)
	}
}

// TestModeIsComparedNormalized 钉住「750 与 0750 是同一件事」。
func TestModeIsComparedNormalized(t *testing.T) {
	var b diffBuilder
	ownership{Mode: "750"}.diffInto(&b, present(map[string]any{"mode": "0750"}))
	if len(b.changes) != 0 {
		t.Errorf("mode 应当归一化后比较，实际 %v", b.changes)
	}
}

func TestSupportedTypesIsSorted(t *testing.T) {
	got := SupportedTypes()
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("SupportedTypes 应按字典序（错误信息要稳定）: %v", got)
		}
	}
	if len(got) != 7 {
		t.Errorf("本版本实现了 7 种类型，实际 %d: %v", len(got), got)
	}
}
