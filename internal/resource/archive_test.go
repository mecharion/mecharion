package resource

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// tarEntry 是构造测试归档的一条条目。
type tarEntry struct {
	name string
	body string
	mode int64
	typ  byte
	link string
}

// makeTar 造一个 tar（gz 可选）。
func makeTar(t *testing.T, gzipped bool, entries []tarEntry) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{
			Name: e.name, Mode: mode, Typeflag: typ,
			Size: int64(len(e.body)), Linkname: e.link,
		}
		if typ == tar.TypeDir || typ == tar.TypeSymlink {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if !gzipped {
		return raw.Bytes()
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

func TestArchiveLifecycle(t *testing.T) {
	env := testEnv(t)
	root := t.TempDir()
	dest := filepath.Join(root, "generations", "0001-1.2.0-1")

	putBlob(t, env, "main", makeTar(t, true, []tarEntry{
		{name: "webapp-1.2.0/", typ: tar.TypeDir, mode: 0o755},
		{name: "webapp-1.2.0/bin/", typ: tar.TypeDir, mode: 0o755},
		{name: "webapp-1.2.0/bin/webapp", body: "#!/bin/sh\necho hi\n", mode: 0o755},
		{name: "webapp-1.2.0/README.md", body: "读我\n", mode: 0o644},
	}))

	a := build(t, env, mk(t, "archive:main", TypeArchive, map[string]any{
		"blob": "main", "dest": dest, "strip": 1,
	}))

	requireAbsent(t, a)
	requireIdempotent(t, a, root)
	requireClean(t, a)

	body, err := os.ReadFile(filepath.Join(dest, "bin", "webapp"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "#!/bin/sh\necho hi\n" {
		t.Errorf("解出的内容 = %q", body)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dest, "bin", "webapp"))
		if fi.Mode().Perm() != 0o755 {
			t.Errorf("可执行位应当保留，实际 %04o", fi.Mode().Perm())
		}
	}
	// strip: 1 应当剥掉版本目录
	if _, err := os.Stat(filepath.Join(dest, "webapp-1.2.0")); err == nil {
		t.Error("strip: 1 应当剥掉最外层的版本目录")
	}

	// Remove 是 no-op —— generation 由调和器整体回收
	if err := a.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "bin", "webapp")); err != nil {
		t.Error("archive.Remove 应当是 no-op")
	}
}

// TestArchiveDetectsChangedBlob 钉住「用标记文件而非目录非空判定幂等」。
//
// blob 变了但路径没变时，目录非空，只有标记文件能看出内容是旧版本的。
func TestArchiveDetectsChangedBlob(t *testing.T) {
	env := testEnv(t)
	dest := filepath.Join(t.TempDir(), "gen")

	putBlob(t, env, "main", makeTar(t, true, []tarEntry{
		{name: "app/v", body: "1.0\n"},
	}))
	a := build(t, env, mk(t, "archive:main", TypeArchive, map[string]any{
		"blob": "main", "dest": dest, "strip": 1,
	}))
	if err := a.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireClean(t, a)

	// 换一个 blob（目录仍然非空）
	env2 := testEnv(t)
	env2.BlobDir = env.BlobDir
	putBlob(t, env2, "main", makeTar(t, true, []tarEntry{
		{name: "app/v", body: "2.0\n"},
	}))
	a2 := build(t, env2, mk(t, "archive:main", TypeArchive, map[string]any{
		"blob": "main", "dest": dest, "strip": 1,
	}))
	requireOnlyField(t, a2, "blob")

	if err := a2.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dest, "v"))
	if string(got) != "2.0\n" {
		t.Errorf("重解后内容 = %q，期望 2.0", got)
	}
	requireClean(t, a2)
}

// TestArchiveRejectsPathTraversal 钉住 tar-slip 防护。
//
// Pack 可以由第三方提供，一条 `../../etc/cron.d/x` 条目就是直接的 root
// 提权。系统不做 Pack 来源校验（ADR-0040），safeJoin 是唯一的防线。
func TestArchiveRejectsPathTraversal(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{"相对路径逃逸", []tarEntry{{name: "../../etc/cron.d/evil", body: "* * * * * root sh\n"}}},
		{"绝对路径", []tarEntry{{name: "/etc/cron.d/evil", body: "x\n"}}},
		{"软链逃逸", []tarEntry{
			{name: "link", typ: tar.TypeSymlink, link: "../../../etc/shadow"},
		}},
		{"绝对软链", []tarEntry{
			{name: "link", typ: tar.TypeSymlink, link: "/etc/shadow"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			root := t.TempDir()
			dest := filepath.Join(root, "gen")
			putBlob(t, env, "evil", makeTar(t, true, tc.entries))

			a := build(t, env, mk(t, "archive:evil", TypeArchive, map[string]any{
				"blob": "evil", "dest": dest,
			}))
			err := a.Apply(context.Background())
			if err == nil {
				t.Fatal("逃逸条目必须被拒绝")
			}
			if ClassOf(err) != ErrPermanent {
				t.Errorf("恶意归档应归为 permanent，实际 %s", ClassOf(err))
			}
			// 拒绝之后不该留下标记文件——下次要重新判定
			if _, serr := os.Stat(filepath.Join(dest, archiveMarker)); serr == nil {
				t.Error("失败的解压不该写下完成标记")
			}
		})
	}
}

// TestArchiveAllowsInternalSymlink 钉住「指向归档内部的相对软链是合法的」。
//
// JDK 一类的发行包大量使用这种软链，一刀切拒绝会让它们无法安装。
func TestArchiveAllowsInternalSymlink(t *testing.T) {
	requireSymlinkSupport(t)
	env := testEnv(t)
	root := t.TempDir()
	dest := filepath.Join(root, "gen")

	putBlob(t, env, "jdk", makeTar(t, true, []tarEntry{
		{name: "lib/", typ: tar.TypeDir, mode: 0o755},
		{name: "lib/libjvm.so", body: "假装是 so\n"},
		{name: "jre/", typ: tar.TypeDir, mode: 0o755},
		{name: "jre/lib", typ: tar.TypeSymlink, link: "../lib"},
	}))

	a := build(t, env, mk(t, "archive:jdk", TypeArchive, map[string]any{
		"blob": "jdk", "dest": dest,
	}))
	requireIdempotent(t, a, root)

	got, err := os.Readlink(filepath.Join(dest, "jre", "lib"))
	if err != nil {
		t.Fatal(err)
	}
	// Windows 的符号链接不接受 "/"（os.Symlink 会转成 "\"），
	// 读回的目标天然是本机分隔符，这里按平台比较是正确行为，不是缺陷。
	if want := filepath.FromSlash("../lib"); got != want {
		t.Errorf("软链目标 = %q，期望 %q", got, want)
	}
}

func TestArchiveExclude(t *testing.T) {
	env := testEnv(t)
	root := t.TempDir()
	dest := filepath.Join(root, "gen")

	putBlob(t, env, "kafka", makeTar(t, true, []tarEntry{
		{name: "kafka-3.6.0/bin/kafka-server-start.sh", body: "start\n"},
		{name: "kafka-3.6.0/bin/windows/kafka-server-start.bat", body: "start\n"},
		{name: "kafka-3.6.0/site-docs/index.html", body: "docs\n"},
	}))

	a := build(t, env, mk(t, "archive:kafka", TypeArchive, map[string]any{
		"blob": "kafka", "dest": dest, "strip": 1,
		"exclude": []string{"site-docs", "bin/windows"},
	}))
	if err := a.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dest, "bin", "kafka-server-start.sh")); err != nil {
		t.Error("未被排除的文件应当解出来")
	}
	for _, p := range []string{"site-docs", filepath.Join("bin", "windows")} {
		if _, err := os.Stat(filepath.Join(dest, p)); err == nil {
			t.Errorf("%s 应当被排除（含其下全部内容）", p)
		}
	}
}

// TestArchiveDetectsFormatByMagic 钉住「按魔数而非后缀识别格式」。
func TestArchiveDetectsFormatByMagic(t *testing.T) {
	env := testEnv(t)
	root := t.TempDir()
	dest := filepath.Join(root, "gen")

	// 内容是未压缩的 tar，但 filename 写成了 .tar.gz
	ref := putBlob(t, env, "mislabeled", makeTar(t, false, []tarEntry{
		{name: "a.txt", body: "内容\n"},
	}))
	ref.Filename = "thing-1.0.tar.gz"
	env.Blobs["mislabeled"] = ref

	a := build(t, env, mk(t, "archive:x", TypeArchive, map[string]any{
		"blob": "mislabeled", "dest": dest,
	}))
	if err := a.Apply(context.Background()); err != nil {
		t.Fatalf("后缀写错不该导致解压失败: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "a.txt")); string(got) != "内容\n" {
		t.Errorf("内容 = %q", got)
	}
}

// TestArchiveCorruptMarkerTriggersReextract 钉住「标记文件坏了就重解」。
func TestArchiveCorruptMarkerTriggersReextract(t *testing.T) {
	env := testEnv(t)
	dest := filepath.Join(t.TempDir(), "gen")
	putBlob(t, env, "main", makeTar(t, true, []tarEntry{{name: "a.txt", body: "x\n"}}))

	a := build(t, env, mk(t, "archive:main", TypeArchive, map[string]any{
		"blob": "main", "dest": dest,
	}))
	if err := a.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, archiveMarker), []byte("{坏了"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 坏掉的标记应当读成 absent 而非 unknown——重解一次是安全且能自愈的，
	// unknown 会让这条资源被永远跳过。
	requireAbsent(t, a)
	if err := a.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireClean(t, a)
}

// makeZip 造一个 zip。
func makeZip(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		mode := fs.FileMode(e.mode)
		if mode == 0 {
			mode = 0o644
		}
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		body := e.body
		switch e.typ {
		case tar.TypeDir:
			h.Name = strings.TrimSuffix(e.name, "/") + "/"
			mode |= fs.ModeDir
		case tar.TypeSymlink:
			mode |= fs.ModeSymlink
			body = e.link
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestArchiveZip(t *testing.T) {
	env := testEnv(t)
	root := t.TempDir()
	dest := filepath.Join(root, "gen")

	putBlob(t, env, "tool", makeZip(t, []tarEntry{
		{name: "tool-2.1/", typ: tar.TypeDir, mode: 0o755},
		{name: "tool-2.1/bin/tool", body: "#!/bin/sh\n", mode: 0o755},
		{name: "tool-2.1/conf/app.ini", body: "[main]\n", mode: 0o644},
		{name: "tool-2.1/docs/readme.txt", body: "读我\n"},
	}))

	a := build(t, env, mk(t, "archive:tool", TypeArchive, map[string]any{
		"blob": "tool", "dest": dest, "strip": 1, "exclude": []string{"docs"},
	}))

	requireAbsent(t, a)
	requireIdempotent(t, a, root)
	requireClean(t, a)

	if got, err := os.ReadFile(filepath.Join(dest, "bin", "tool")); err != nil {
		t.Fatal(err)
	} else if string(got) != "#!/bin/sh\n" {
		t.Errorf("内容 = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "conf", "app.ini")); err != nil {
		t.Error("conf/app.ini 应当解出来")
	}
	if _, err := os.Stat(filepath.Join(dest, "docs")); err == nil {
		t.Error("docs 应当被 exclude 排除")
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(filepath.Join(dest, "bin", "tool"))
		if fi.Mode().Perm() != 0o755 {
			t.Errorf("zip 里的可执行位应当保留，实际 %04o", fi.Mode().Perm())
		}
	}
}

// TestArchiveZipRejectsTraversal 钉住 zip-slip 防护。
func TestArchiveZipRejectsTraversal(t *testing.T) {
	env := testEnv(t)
	dest := filepath.Join(t.TempDir(), "gen")
	putBlob(t, env, "evil", makeZip(t, []tarEntry{
		{name: "../../etc/cron.d/evil", body: "x\n"},
	}))

	a := build(t, env, mk(t, "archive:evil", TypeArchive, map[string]any{
		"blob": "evil", "dest": dest,
	}))
	if err := a.Apply(context.Background()); err == nil {
		t.Fatal("zip 中的逃逸条目必须被拒绝")
	}
}

// TestArchiveHardLink 覆盖 tar 的硬链条目。
func TestArchiveHardLink(t *testing.T) {
	env := testEnv(t)
	root := t.TempDir()
	dest := filepath.Join(root, "gen")

	putBlob(t, env, "busybox", makeTar(t, true, []tarEntry{
		{name: "pkg/bin/busybox", body: "#!/bin/sh\n", mode: 0o755},
		{name: "pkg/bin/sh", typ: tar.TypeLink, link: "pkg/bin/busybox"},
	}))

	a := build(t, env, mk(t, "archive:busybox", TypeArchive, map[string]any{
		"blob": "busybox", "dest": dest, "strip": 1,
	}))
	if err := a.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "bin", "sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "#!/bin/sh\n" {
		t.Errorf("硬链内容 = %q", got)
	}

	// 硬链要能重解——第二次 Apply 时目标已存在
	if err := a.Apply(context.Background()); err != nil {
		t.Fatalf("重复解压硬链失败: %v", err)
	}
}

// TestArchiveSkipsSpecialEntries 钉住「设备文件等一律跳过」。
func TestArchiveSkipsSpecialEntries(t *testing.T) {
	env := testEnv(t)
	dest := filepath.Join(t.TempDir(), "gen")
	putBlob(t, env, "weird", makeTar(t, true, []tarEntry{
		{name: "pkg/normal.txt", body: "ok\n"},
		{name: "pkg/fifo", typ: tar.TypeFifo},
		{name: "pkg/dev", typ: tar.TypeChar},
	}))

	a := build(t, env, mk(t, "archive:weird", TypeArchive, map[string]any{
		"blob": "weird", "dest": dest, "strip": 1,
	}))
	if err := a.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "normal.txt")); err != nil {
		t.Error("普通文件应当解出来")
	}
	for _, n := range []string{"fifo", "dev"} {
		if _, err := os.Stat(filepath.Join(dest, n)); err == nil {
			t.Errorf("%s 这类特殊条目应当被跳过——组件载荷里出现它们要么是"+
				"打包失误，要么就该走 package 资源", n)
		}
	}
}

func TestArchiveRejectsBadArgs(t *testing.T) {
	env := testEnv(t)
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"缺 blob", map[string]any{"dest": absPath("/tmp/d")}, "缺少 blob"},
		{"缺 dest", map[string]any{"blob": "main"}, "缺少 dest"},
		{"strip 为负", map[string]any{"blob": "m", "dest": absPath("/tmp/d"), "strip": -1}, "不能为负"},
		{"exclude 非法", map[string]any{
			"blob": "m", "dest": absPath("/tmp/d"), "exclude": []string{"["},
		}, "glob"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(env, mk(t, "archive:x", TypeArchive, tc.args))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("期望包含 %q 的错误，实际: %v", tc.want, err)
			}
		})
	}
}
