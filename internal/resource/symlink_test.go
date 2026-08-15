package resource

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// requireSymlinkSupport 跳过没有建软链权限的环境。
//
// Windows 默认要求管理员权限或开发者模式才能建软链——这不是被测代码的
// 问题，mechlet 只跑在 Linux 上。
func requireSymlinkSupport(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		return
	}
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "target"), filepath.Join(dir, "probe")); err != nil {
		t.Skip("本机不允许创建软链（Windows 需开发者模式）")
	}
}

func TestSymlinkLifecycle(t *testing.T) {
	requireSymlinkSupport(t)
	env := testEnv(t)
	root := t.TempDir()

	gen1 := filepath.Join(root, "generations", "0001")
	gen2 := filepath.Join(root, "generations", "0002")
	for _, d := range []string{gen1, gen2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "current")

	s := build(t, env, mk(t, "symlink:current", TypeSymlink, map[string]any{
		"path": link, "target": gen1,
	}))

	requireAbsent(t, s)
	requireIdempotent(t, s, root)
	requireClean(t, s)

	if got, err := os.Readlink(link); err != nil || got != gen1 {
		t.Fatalf("软链目标 = %q（err=%v），期望 %q", got, err, gen1)
	}

	// 改指到新 generation——这正是原子切换要做的事
	s2 := build(t, env, mk(t, "symlink:current", TypeSymlink, map[string]any{
		"path": link, "target": gen2,
	}))
	requireOnlyField(t, s2, "target")
	if err := s2.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireClean(t, s2)
	if got, _ := os.Readlink(link); got != gen2 {
		t.Errorf("改指后目标 = %q，期望 %q", got, gen2)
	}

	if err := s2.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireAbsent(t, s2)
	// 目标不该被删掉
	if _, err := os.Stat(gen2); err != nil {
		t.Error("Remove 只该删链接自身，不该碰目标")
	}
}

// TestSymlinkBlockedByRealFile 钉住「不加 force 不覆盖真实文件」。
func TestSymlinkBlockedByRealFile(t *testing.T) {
	requireSymlinkSupport(t)
	env := testEnv(t)
	root := t.TempDir()
	link := filepath.Join(root, "conf")
	if err := os.WriteFile(link, []byte("用户手工放的东西"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := build(t, env, mk(t, "symlink:conf", TypeSymlink, map[string]any{
		"path": link, "target": filepath.Join(root, "real"),
	}))

	obs, err := s.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if obs.State != StatePresent {
		t.Fatalf("被真实文件占着应当读成 present（这样 Diff 能报出来），实际 %s", obs.State)
	}
	if len(s.Diff(obs)) == 0 {
		t.Error("被真实文件占着必须报差异")
	}

	err = s.Apply(context.Background())
	if err == nil {
		t.Fatal("不加 force 不该覆盖用户的真实文件")
	}
	if ClassOf(err) != ErrPermanent {
		t.Errorf("应归为 permanent，实际 %s", ClassOf(err))
	}
	if got, _ := os.ReadFile(link); string(got) != "用户手工放的东西" {
		t.Error("失败路径不该动到原文件")
	}

	// 加了 force 才允许覆盖
	sf := build(t, env, mk(t, "symlink:conf", TypeSymlink, map[string]any{
		"path": link, "target": filepath.Join(root, "real"), "force": true,
	}))
	if err := sf.Apply(context.Background()); err != nil {
		t.Fatalf("声明 force 后应当成功: %v", err)
	}
	requireClean(t, sf)
}

func TestSymlinkRejectsBadArgs(t *testing.T) {
	env := testEnv(t)
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"缺 target", map[string]any{"path": absPath("/tmp/l")}, "缺少 target"},
		{"缺 path", map[string]any{"target": "/x"}, "缺少 path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(env, mk(t, "symlink:x", TypeSymlink, tc.args))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("期望包含 %q 的错误，实际: %v", tc.want, err)
			}
		})
	}
}
