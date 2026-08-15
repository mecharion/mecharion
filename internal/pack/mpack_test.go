package pack

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// `.mpack` 的判据分两类，而它们的读者不同：
//
//	可复现   —— 给「摘要能不能当身份用」把关
//	路径校验 —— 给「别人打的包」把关（唯一一处新增的供应链入口）
//
// 第二类必须拿**手工构造的恶意归档**来测：一个正常打包流程产出的包
// 里永远不会有 `../`，因此只用自己打的包去测等于什么都没测。

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func packTo(t *testing.T, dir string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteMpack(dir, &buf); err != nil {
		t.Fatalf("打包: %v", err)
	}
	return buf.Bytes()
}

// TestMpackIsReproducible 是「摘要能当身份用」的前提。
//
// 同样的内容打两次必须得到**同一个字节流**。做不到的话，一份包在两台
// 机器上算出两个摘要，而这个系统里一切信任都锚在摘要上。
func TestMpackIsReproducible(t *testing.T) {
	files := map[string]string{
		"pack.yaml":         "schema: pack/v1\n",
		"templates/a.tmpl":  "A",
		"blobs/sha256/aaaa": "payload",
		"files/z.conf":      "z",
	}
	a := packTo(t, writeTree(t, files))
	b := packTo(t, writeTree(t, files))

	if !bytes.Equal(a, b) {
		t.Fatal("同样的内容打两次得到了不同的字节流——摘要不能当身份用了")
	}
}

// TestMpackEntriesAreSorted 守的是可复现的**来源**。
//
// 目录遍历顺序在不同文件系统上不同；不排序的话上一条测试会变成
// 「在这台机器上碰巧一致」。
func TestMpackEntriesAreSorted(t *testing.T) {
	// **样本要能分辨「排过序」与「WalkDir 的遍历顺序」。**
	//
	// 第一版用的是 z/a/m-b/m-a，而 WalkDir 本身就按字典序走，于是
	// 拿掉 sort.Strings 之后这条测试照样通过——变异测试当场证明它
	// 什么都没守住。
	//
	// 关键在于：**深度优先的遍历顺序不等于全路径的字典序**。
	// 根目录下 `m`（目录）排在 `m.txt` 前面，WalkDir 会先递归进 m、
	// 产出 `m/a.txt`，再产出 `m.txt`；而全路径字典序里 `.`(0x2E)
	// 小于 `/`(0x2F)，因此 `m.txt` 在前。
	dir := writeTree(t, map[string]string{
		"z.txt": "z", "a.txt": "a", "m/a.txt": "a", "m.txt": "m",
	})
	names := namesIn(t, packTo(t, dir))

	want := []string{"a.txt", "m.txt", "m/a.txt", "z.txt"}
	if len(names) != len(want) {
		t.Fatalf("条目数不对: %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("条目没有按路径排序:\n  期望 %v\n  实际 %v", want, names)
		}
	}
}

// TestMpackZeroesTimestampsAndOwners 守的是另外两条可复现要求。
//
// 顺带一件事：不把打包机器的用户信息带进交付物。
func TestMpackZeroesTimestampsAndOwners(t *testing.T) {
	dir := writeTree(t, map[string]string{"pack.yaml": "x"})
	tr := readerFor(t, packTo(t, dir))

	h, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	// 按 Unix() 判而不是 IsZero()：零值 time.Time 是公元 1 年，
	// 写进 tar 再读出来是 epoch 0（且带本地时区），IsZero() 会是假
	if h.ModTime.Unix() != 0 {
		t.Errorf("时间戳没归零: %v", h.ModTime)
	}
	if h.Uid != 0 || h.Gid != 0 {
		t.Errorf("uid/gid 没归零: %d/%d", h.Uid, h.Gid)
	}
	if h.Uname != "" || h.Gname != "" {
		t.Errorf("带上了打包机器的用户名: %q/%q", h.Uname, h.Gname)
	}
}

// TestMpackKeepsExecutableBit 守的是**唯一保留的那一位权限**。
//
// 二进制载荷丢了可执行位，装上去就是一个 "permission denied"，而那时
// 没人会想到是打包环节。
func TestMpackKeepsExecutableBit(t *testing.T) {
	// **在 Windows 上跳过，并说清为什么。**
	//
	// Windows 建不出带可执行位的文件（Go 在那里只反映只读属性），
	// 因此打包侧探测不到它——这条判据在那里无从成立，不是实现的问题。
	// 它在容器套件里跑（Linux），那才是 Pack 的目标平台。
	if runtime.GOOS == "windows" {
		t.Skip("Windows 表达不了可执行位；这条在容器套件里验（Linux）")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.conf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// **判归档头里的 mode，不判解出来的文件。**
	//
	// Windows 表达不了可执行位（Go 在那里只反映只读属性），而这个包
	// 的目标机器是 Linux——归档里那一位才是决定行为的东西。
	tr := readerFor(t, packTo(t, dir))
	modes := map[string]int64{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		modes[h.Name] = h.Mode
	}
	if modes["run.sh"]&0o111 == 0 {
		t.Error("可执行位丢了——装上去会是一句 permission denied")
	}
	if modes["a.conf"]&0o111 != 0 {
		t.Error("普通文件被加上了可执行位")
	}
}

func TestMpackRoundTrip(t *testing.T) {
	files := map[string]string{
		"pack.yaml":        "schema: pack/v1\nname: demo\n",
		"templates/a.tmpl": "hello {{ .X }}",
		"blobs/main.tgz":   "binary-ish\x00\xff",
	}
	out := t.TempDir()
	if err := ExtractMpack(bytes.NewReader(packTo(t, writeTree(t, files))), out); err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s 内容不对", name)
		}
	}
}

// ── 供应链入口：拿手工构造的恶意归档来测 ──────────────────────────────

// evilMpack 手工造一个带指定条目的归档。
//
// **不能用 WriteMpack 造**：它自己就拒绝这些条目，那样测的是打包侧，
// 而要守的是「别人打的包」。
func evilMpack(t *testing.T, headers ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	for _, h := range headers {
		if h.Typeflag == tar.TypeReg && h.Size == 0 {
			h.Size = int64(len("x"))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func TestExtractRejectsPathEscape(t *testing.T) {
	cases := []struct {
		name string
		h    *tar.Header
		want string
	}{
		{"上级目录", &tar.Header{
			Name: "../evil.sh", Mode: 0o644, Typeflag: tar.TypeReg}, ".."},
		{"深层上级", &tar.Header{
			Name: "a/b/../../../evil", Mode: 0o644, Typeflag: tar.TypeReg}, ".."},
		{"绝对路径", &tar.Header{
			Name: "/etc/cron.d/evil", Mode: 0o644, Typeflag: tar.TypeReg}, "absolute path"},
		{"符号链接", &tar.Header{
			Name: "link", Linkname: "/etc/shadow", Typeflag: tar.TypeSymlink}, "symlink"},
		{"硬链接", &tar.Header{
			Name: "hard", Linkname: "/etc/shadow", Typeflag: tar.TypeLink}, "hard link"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()
			err := ExtractMpack(bytes.NewReader(evilMpack(t, tc.h)), out)
			if err == nil {
				t.Fatalf("%s 应当被拒绝", tc.name)
			}
			// **错误里要说清是哪一条**：「包有问题」说不出该去看什么
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误里应当指明原因 %q，得到: %v", tc.want, err)
			}
			// 而且什么都不该留下
			entries, _ := os.ReadDir(out)
			for _, e := range entries {
				t.Errorf("被拒之后不该留下 %s", e.Name())
			}
		})
	}
}

// TestExtractRejectsHugeEntry 是解压炸弹的第一道。
//
// 一个几 KB 的 zstd 流可以解出上百 GB。这里判的是**声称的大小**——
// 真正的拷贝也有限长，两道都要。
func TestExtractRejectsHugeEntry(t *testing.T) {
	h := &tar.Header{Name: "big", Mode: 0o644, Typeflag: tar.TypeReg, Size: mpackMaxEntry + 1}
	// 不真的写那么多字节：checkEntry 在读内容之前就该拒掉
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	_ = tw.WriteHeader(h)
	tw.Flush()
	zw.Close()

	if err := ExtractMpack(bytes.NewReader(buf.Bytes()), t.TempDir()); err == nil {
		t.Fatal("超大的条目应当在读内容之前就被拒")
	}
}

func TestExtractRejectsNonZstd(t *testing.T) {
	err := ExtractMpack(strings.NewReader("这不是 zstd"), t.TempDir())
	if err == nil {
		t.Fatal("非 zstd 输入应当被拒")
	}
	// 错误要指向**外面那层**。"magic number mismatch" 指向 tar，
	// 而问题是「这根本不是 .mpack」——那句话会让人去查错的地方
	if !strings.Contains(err.Error(), ".mpack") {
		t.Errorf("错误应当说清这不像一个 .mpack，得到: %v", err)
	}
}

func TestExtractRejectsEmptyArchive(t *testing.T) {
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.Close()
	zw.Close()

	if err := ExtractMpack(bytes.NewReader(buf.Bytes()), t.TempDir()); err == nil {
		t.Fatal("空归档应当被拒——它不是一个 Pack")
	}
}

func TestMpackNameFollowsSpec(t *testing.T) {
	got := MpackName(&Pack{Name: "postgresql", Version: "16.4", Revision: 2})
	if got != "postgresql-16.4-2.mpack" {
		t.Errorf("文件名不符合规范 §3: %s", got)
	}
}

// ── 辅助 ────────────────────────────────────────────────────────────────

func readerFor(t *testing.T, blob []byte) *tar.Reader {
	t.Helper()
	zr, err := zstd.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	return tar.NewReader(zr)
}

func namesIn(t *testing.T, blob []byte) []string {
	t.Helper()
	tr := readerFor(t, blob)
	var out []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h.Name)
	}
	return out
}
