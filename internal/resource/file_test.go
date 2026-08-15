package resource

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFileFromContent(t *testing.T) {
	env := testEnv(t)
	root := t.TempDir()
	path := filepath.Join(root, "etc", "app.yaml")

	f := build(t, env, mk(t, "file:app", TypeFile, map[string]any{
		"path": path, "content": "port: 8080\n", "mode": "0640",
	}))

	requireAbsent(t, f)
	requireIdempotent(t, f, root)
	requireClean(t, f)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "port: 8080\n" {
		t.Errorf("内容 = %q", got)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o640 {
			t.Errorf("权限 = %04o，期望 0640", fi.Mode().Perm())
		}
	}

	if err := f.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireAbsent(t, f)
	// Remove 幂等
	if err := f.Remove(context.Background()); err != nil {
		t.Errorf("重复 Remove 应当无害: %v", err)
	}
}

// TestFileDoesNotRewriteWhenIdentical 钉住「内容一致时不重写」。
//
// 重写会改动 mtime，从而惊动 inotify 类的自动重载——一份内容没变的配置
// 不该让服务每 60 秒重载一次。
func TestFileDoesNotRewriteWhenIdentical(t *testing.T) {
	env := testEnv(t)
	root := t.TempDir()
	path := filepath.Join(root, "app.conf")

	f := build(t, env, mk(t, "file:conf", TypeFile, map[string]any{
		"path": path, "content": "a=1\n",
	}))
	ctx := context.Background()
	if err := f.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("内容一致时不应重写文件（mtime %v → %v）",
			before.ModTime(), after.ModTime())
	}
}

// TestFileContentDrift 钉住「内容漂移只报 content 一项」。
func TestFileContentDrift(t *testing.T) {
	env := testEnv(t)
	root := t.TempDir()
	path := filepath.Join(root, "app.conf")

	f := build(t, env, mk(t, "file:conf", TypeFile, map[string]any{
		"path": path, "content": "log_level: info\n", "mode": "0644",
	}))
	if err := f.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("log_level: debug\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	requireOnlyField(t, f, "content")

	// 小文件应带上全文，供 CLI 做行级 diff
	obs, _ := f.Read(context.Background())
	c := f.Diff(obs)[0]
	if c.Kind != KindText {
		t.Errorf("内容差异应标记为 KindText，实际 %s", c.Kind)
	}
	if !strings.Contains(c.Want, "info") || !strings.Contains(c.Got, "debug") {
		t.Errorf("小文件的差异应携带全文，实际 want=%q got=%q", c.Want, c.Got)
	}

	if err := f.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireClean(t, f)
}

// TestFileLargeContentReportsDigestOnly 钉住「超过阈值只报摘要」。
func TestFileLargeContentReportsDigestOnly(t *testing.T) {
	env := testEnv(t)
	root := t.TempDir()
	path := filepath.Join(root, "big.bin")

	big := strings.Repeat("A", textDiffLimit+1)
	f := build(t, env, mk(t, "file:big", TypeFile, map[string]any{
		"path": path, "content": big,
	}))
	if err := os.WriteFile(path, []byte(strings.Repeat("B", textDiffLimit+1)), 0o644); err != nil {
		t.Fatal(err)
	}

	obs, err := f.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := obs.Fields["content"]; ok {
		t.Error("超过阈值不该把全文读进内存")
	}
	c := f.Diff(obs)
	if len(c) != 1 || !strings.HasPrefix(c[0].Want, "sha256:") {
		t.Errorf("应当只报摘要，实际 %v", c)
	}
}

func TestFileFromBlob(t *testing.T) {
	env := testEnv(t)
	root := t.TempDir()
	putBlob(t, env, "minio", []byte("\x7fELF 假装我是一个二进制"))

	path := filepath.Join(root, "bin", "minio")
	f := build(t, env, mk(t, "file:minio", TypeFile, map[string]any{
		"path": path, "blob": "minio", "mode": "0755",
	}))

	requireIdempotent(t, f, root)
	requireClean(t, f)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "\x7fELF 假装我是一个二进制" {
		t.Errorf("内容 = %q", got)
	}
}

// TestFileMissingBlobIsTransient 钉住「载荷没就位是可重试的」。
//
// Read 不需要载荷就位——blob 的摘要在规格里就有，因此「目标文件已经是
// 对的」这种情况根本不必等载荷下载完。错误只在真要写的时候才出现。
func TestFileMissingBlobIsTransient(t *testing.T) {
	env := testEnv(t)
	env.Blobs["late"] = blobRef("late", strings.Repeat("ab", 32))

	f := build(t, env, mk(t, "file:late", TypeFile, map[string]any{
		"path": filepath.Join(t.TempDir(), "x"), "blob": "late",
	}))
	requireAbsent(t, f)

	err := f.Apply(context.Background())
	if err == nil {
		t.Fatal("载荷不在本地时 Apply 应当报错")
	}
	if !IsTransient(err) {
		t.Errorf("「还没下载完」应归为 transient，实际 %s", ClassOf(err))
	}
	if !strings.Contains(err.Error(), "尚未就位") {
		t.Errorf("错误信息应说清是载荷没到，实际: %v", err)
	}
}

func TestFileFromSource(t *testing.T) {
	env := testEnv(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(env.PackRoot, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(env.PackRoot, "files", "logrotate.conf"),
		[]byte("rotate 7\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(root, "logrotate.conf")
	f := build(t, env, mk(t, "file:lr", TypeFile, map[string]any{
		"path": path, "source": "files/logrotate.conf",
	}))
	requireIdempotent(t, f, root)
	requireClean(t, f)
}

// TestFileSourceEscapingPackRoot 钉住「source 不能逃出 Pack 根」。
func TestFileSourceEscapingPackRoot(t *testing.T) {
	env := testEnv(t)
	f := build(t, env, mk(t, "file:x", TypeFile, map[string]any{
		"path": filepath.Join(t.TempDir(), "x"), "source": "../../etc/shadow",
	}))
	if _, err := f.Read(context.Background()); err == nil {
		t.Fatal("source 逃出 Pack 根必须被拒绝")
	}
}

// TestTemplateIsFileWithRenderedContent 钉住 template 在 mechlet 侧与 file 同构。
func TestTemplateIsFileWithRenderedContent(t *testing.T) {
	env := testEnv(t)
	root := t.TempDir()
	path := filepath.Join(root, "app.yaml")

	tpl := build(t, env, mk(t, "template:app", TypeTemplate, map[string]any{
		"dest": path, "content": "port: 8080\n", "mode": "0640",
	}))
	if tpl.Type() != TypeTemplate {
		t.Errorf("Type 应保留 template，供 status 区分静态与渲染: %s", tpl.Type())
	}
	requireIdempotent(t, tpl, root)
	requireClean(t, tpl)
}

// TestTemplateWithSrcIsRejected 钉住「未渲染的模板不该出现在已解析规格里」。
func TestTemplateWithSrcIsRejected(t *testing.T) {
	env := testEnv(t)
	_, err := New(env, mk(t, "template:x", TypeTemplate, map[string]any{
		"dest": absPath("/tmp/x"), "src": "app.yaml.tmpl",
	}))
	if err == nil {
		t.Fatal("已解析规格中出现 src 是 mechd 的 bug，必须报出来")
	}
	if !strings.Contains(err.Error(), "mechd") {
		t.Errorf("错误信息应指出渲染是 mechd 的职责: %v", err)
	}
}

func TestFileRejectsBadArgs(t *testing.T) {
	env := testEnv(t)
	p := absPath("/tmp/x")
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"没有来源", map[string]any{"path": p}, "恰好声明一个"},
		{"两个来源", map[string]any{"path": p, "content": "a", "blob": "b"}, "恰好声明一个"},
		{"path 与 dest 同时给", map[string]any{
			"path": p, "dest": p, "content": "a",
		}, "只能有一个"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(env, mk(t, "file:x", TypeFile, tc.args))
			if err == nil {
				t.Fatal("应当构造失败")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息应包含 %q，实际: %v", tc.want, err)
			}
		})
	}
}

// TestFileEmptyContentIsNotUnset 钉住 content:"" 与未声明 content 的区别。
func TestFileEmptyContentIsNotUnset(t *testing.T) {
	env := testEnv(t)
	path := filepath.Join(t.TempDir(), "empty")
	f := build(t, env, mk(t, "file:empty", TypeFile, map[string]any{
		"path": path, "content": "",
	}))
	if err := f.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal("content: \"\" 应当创建一个空文件")
	}
	if fi.Size() != 0 {
		t.Errorf("大小 = %d，期望 0", fi.Size())
	}
}
