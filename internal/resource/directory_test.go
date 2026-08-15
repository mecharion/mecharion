package resource

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDirectoryLifecycle(t *testing.T) {
	env := testEnv(t)
	root := t.TempDir()
	path := filepath.Join(root, "apps", "webapp", "data")

	d := build(t, env, mk(t, "directory:data", TypeDirectory, map[string]any{
		"path": path, "mode": "0750",
	}))

	requireAbsent(t, d)

	// 不存在时应报「整个资源缺失」，而不是逐字段罗列
	obs, _ := d.Read(context.Background())
	changes := d.Diff(obs)
	if len(changes) != 1 || changes[0].Field != "exists" {
		t.Fatalf("absent 时应只报 exists，实际 %v", changes)
	}

	requireIdempotent(t, d, root)
	requireClean(t, d)

	if fi, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if !fi.IsDir() {
		t.Fatal("应当创建为目录")
	}

	// 手工改坏 mode，Diff 只应报 mode
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o777); err != nil {
			t.Fatal(err)
		}
		requireOnlyField(t, d, "mode")

		if err := d.Apply(context.Background()); err != nil {
			t.Fatal(err)
		}
		requireClean(t, d)
	}

	// 非空目录不删
	if err := os.WriteFile(filepath.Join(path, "keep.dat"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("非空目录不该被删除——里面可能是用户数据")
	}

	if err := os.Remove(filepath.Join(path, "keep.dat")); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireAbsent(t, d)
}

// TestDirectoryBlockedByFile 钉住「该是目录的地方躺着文件」是 error 而非漂移。
func TestDirectoryBlockedByFile(t *testing.T) {
	env := testEnv(t)
	root := t.TempDir()
	path := filepath.Join(root, "occupied")
	if err := os.WriteFile(path, []byte("我不是目录"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := build(t, env, mk(t, "directory:x", TypeDirectory, map[string]any{"path": path}))

	_, err := d.Read(context.Background())
	if err == nil {
		t.Fatal("必须报错——引擎删掉这个文件腾地方是破坏性的，得由人决定")
	}
	if ClassOf(err) != ErrPermanent {
		t.Errorf("这是配置问题，应归为 permanent，实际 %s", ClassOf(err))
	}
}

func TestDirectoryRecursive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不实现 Unix 权限位")
	}
	env := testEnv(t)
	root := t.TempDir()
	path := filepath.Join(root, "tree")

	if err := os.MkdirAll(filepath.Join(path, "sub"), 0o777); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(path, "sub", "f.txt")
	if err := os.WriteFile(inner, []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}

	d := build(t, env, mk(t, "directory:tree", TypeDirectory, map[string]any{
		"path": path, "mode": "0640", "recursive": true,
	}))
	if err := d.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(inner)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("文件权限 = %04o，期望 0640", fi.Mode().Perm())
	}

	// 目录必须补上执行位，否则 0640 的目录进不去
	di, err := os.Stat(filepath.Join(path, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o750 {
		t.Errorf("子目录权限 = %04o，期望 0750（由 0640 推出）", di.Mode().Perm())
	}
}

func TestDirectoryRejectsBadArgs(t *testing.T) {
	env := testEnv(t)
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"缺 path", map[string]any{"mode": "0755"}, "缺少 path"},
		{"相对路径", map[string]any{"path": "relative/dir"}, "必须是绝对路径"},
		{"mode 非法", map[string]any{"path": absPath("/tmp/x"), "mode": "rwxr-x"}, "八进制"},
		{"未知字段", map[string]any{"path": absPath("/tmp/x"), "ower": "webapp"}, "ower"},
		{"残留占位符", map[string]any{
			"path": absPath("/opt/x/{{ .Paths.Generation }}/y"),
		}, "ResolveGeneration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(env, mk(t, "directory:x", TypeDirectory, tc.args))
			if err == nil {
				t.Fatal("应当构造失败")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息应包含 %q，实际: %v", tc.want, err)
			}
		})
	}
}

// absPath 让测试用的绝对路径在 Windows 上也成立。
func absPath(p string) string {
	if runtime.GOOS == "windows" {
		return "C:" + filepath.FromSlash(p)
	}
	return p
}
