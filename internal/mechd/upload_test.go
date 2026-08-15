package mechd

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/packindex"
)

// 上传是**唯一一处新增的供应链入口**（23-web-ui §2.7）。这些测试守的是
// 它的两半：合法的包能进来，不合法的包**什么都不留下**。

// uploadFixture 起一套控制面，Pack 集合是一个空的临时目录。
func uploadFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t, "n1")
	dir := t.TempDir()
	idx := packindex.New()
	if err := idx.AddDir(dir); err != nil {
		t.Fatal(err)
	}
	f.svc.Packs = idx
	return f, dir
}

// mpackOf 把一个 Pack 源目录打成 .mpack 字节流。
func mpackOf(t *testing.T, files map[string]string) []byte {
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
	var buf bytes.Buffer
	if err := pack.WriteMpack(dir, &buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// goodPack 是一个能通过 lint 的最小 Pack。
func goodPack() map[string]string {
	return map[string]string{"pack.yaml": `schema: pack/v1
name: uploaded
version: "1.0.0"
revision: 1
platforms: [linux/amd64]
params:
  greeting:
    type: string
    default: hi
paths:
  conf:
    default: "{{ .Node.Roots.etc }}/uploaded"
roles:
  - name: main
    cardinality: "1-N"
    resources:
      - file:
          path: "{{ .Paths.conf }}/a.conf"
          content: "greeting={{ .Params.greeting }}\n"
          mode: "0644"
`}
}

// TestUploadAcceptsAValidPack 是验收表第 17 条。
func TestUploadAcceptsAValidPack(t *testing.T) {
	f, dir := uploadFixture(t)

	out, err := f.svc.UploadPack(ctx(), bytes.NewReader(mpackOf(t, goodPack())), dir, "admin")
	if err != nil {
		t.Fatalf("合法的包应当被接受: %v", err)
	}
	if out.Name != "uploaded" || out.Version != "1.0.0" {
		t.Errorf("回的元数据不对: %+v", out)
	}
	if out.Digest == "" || out.Size == 0 {
		t.Error("摘要与大小要回出去——它们进审计")
	}

	// **落进 Pack 集合，且立刻可用**（不必重启 mechd）
	if _, err := f.svc.Packs.Resolve("uploaded", ""); err != nil {
		t.Errorf("上传之后应当立刻能解析到它: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "uploaded-1.0.0-1", "pack.yaml")); err != nil {
		t.Errorf("目录名应当是 name-version-revision: %v", err)
	}
}

// TestUploadRejectsBadPackAndLeavesNothing 是验收表第 16 条。
//
// **两条都要断言**：拒绝了，而且**不留下半截 Pack**。只断言「返回了错误」
// 的话，一个先解到 Pack 集合再校验的实现照样通过——而它留下的那个半截
// 目录会被放置阶段看见。
func TestUploadRejectsBadPackAndLeavesNothing(t *testing.T) {
	f, dir := uploadFixture(t)

	bad := map[string]string{"pack.yaml": `schema: pack/v1
name: broken
version: "1.0.0"
platforms: [linux/amd64]
roles:
  - name: main
    workload:
      runtime: nosuchruntime
`}
	_, err := f.svc.UploadPack(ctx(), bytes.NewReader(mpackOf(t, bad)), dir, "admin")
	if err == nil {
		t.Fatal("lint 不过的包应当被拒绝")
	}

	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, e := range entries {
		t.Errorf("被拒之后 Pack 集合里不该留下 %s —— "+
			"半截 Pack 会被放置阶段看见", e.Name())
	}
}

// TestUploadRejectsPathEscapeAndLeavesNothing 守的是供应链那一半。
//
// 路径校验在 pack.ExtractMpack 里验过；这里验的是**上传这条路径上
// 它真的被调用了**，以及被拒之后集合是干净的。
func TestUploadRejectsPathEscape(t *testing.T) {
	f, dir := uploadFixture(t)

	// 手工造一个带 ../ 的归档：WriteMpack 自己会拒绝这种输入
	evil := evilArchive(t, "../escaped.sh")
	_, err := f.svc.UploadPack(ctx(), bytes.NewReader(evil), dir, "admin")
	if err == nil {
		t.Fatal("带 ../ 的归档应当被拒绝")
	}
	if !strings.Contains(err.Error(), "..") {
		t.Errorf("错误里应当说清是路径问题，得到: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("被拒之后不该留下 %s", e.Name())
	}
}

func TestUploadRejectsEmptyBody(t *testing.T) {
	f, dir := uploadFixture(t)
	if _, err := f.svc.UploadPack(ctx(), bytes.NewReader(nil), dir, "admin"); err == nil {
		t.Fatal("空文件应当被拒绝")
	}
}

func TestUploadRejectsNonMpack(t *testing.T) {
	f, dir := uploadFixture(t)
	_, err := f.svc.UploadPack(ctx(),
		strings.NewReader("PK\x03\x04 这是个 zip"), dir, "admin")
	if err == nil {
		t.Fatal("非 .mpack 应当被拒绝")
	}
	if !strings.Contains(err.Error(), ".mpack") {
		t.Errorf("错误要指向「这不是 .mpack」，得到: %v", err)
	}
}

// TestUploadReplacingIsAtomic 守的是覆盖同名同版本时不留混合状态。
func TestUploadReplacingIsAtomic(t *testing.T) {
	f, dir := uploadFixture(t)

	first := goodPack()
	if _, err := f.svc.UploadPack(ctx(), bytes.NewReader(mpackOf(t, first)), dir, "admin"); err != nil {
		t.Fatal(err)
	}
	// 第二次带一个多出来的文件：若是「往上盖」，旧文件会留下
	second := goodPack()
	second["templates/new.tmpl"] = "x"
	out, err := f.svc.UploadPack(ctx(), bytes.NewReader(mpackOf(t, second)), dir, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Replaced {
		t.Error("覆盖同名同版本时应当报 Replaced")
	}

	// 集合里只有一份
	entries, _ := os.ReadDir(dir)
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("覆盖之后应当只剩一份，实际 %d 份", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "uploaded-1.0.0-1", "templates", "new.tmpl")); err != nil {
		t.Errorf("新版本的文件应当在: %v", err)
	}
}

// TestUploadedPackIsDeployable 是这一步真正的判据。
//
// 「进了 Pack 集合」不等于「能用」。一个通过 lint 却装不了的包会在部署
// 那一刻才露馅，而那时用户已经离开了上传的上下文。
func TestUploadedPackIsDeployable(t *testing.T) {
	f, dir := uploadFixture(t)
	if _, err := f.svc.UploadPack(ctx(), bytes.NewReader(mpackOf(t, goodPack())), dir, "admin"); err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "uploaded", Nodes: []string{"n1"}, Actor: "test",
	}); err != nil {
		t.Fatalf("刚上传的包应当能直接部署: %v", err)
	}
	st, err := f.svc.Status(ctx(), DefaultSite, "uploaded")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Instances) != 1 {
		t.Errorf("应当有 1 个实例，实际 %d", len(st.Instances))
	}
}

// evilArchive 手工造一个带指定路径的 .mpack。
//
// **不能用 pack.WriteMpack 造**：它自己就拒绝这种输入，那样测的是打包侧，
// 而这里要守的是「别人打的包」。
func evilArchive(t *testing.T, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	body := []byte("#!/bin/sh\necho pwned\n")
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

// TestTruncatedUploadIsAUserError 守的是**状态码与措辞**。
//
// 一个传坏了的文件是**客户端**的问题，不是服务端故障。第一版回的是
// HTTP 500 + 一句 "unexpected EOF"：状态码指向服务端，措辞指向 tar 库，
// 而用户要知道的是「这个文件不完整，重传一次」。
func TestTruncatedUploadIsAUserError(t *testing.T) {
	f, dir := uploadFixture(t)
	full := mpackOf(t, goodPack())

	_, err := f.svc.UploadPack(ctx(), bytes.NewReader(full[:len(full)/2]), dir, "admin")
	if err == nil {
		t.Fatal("截断的文件应当被拒绝")
	}
	if !isUserError(err) {
		t.Errorf("传坏的文件是客户端问题，应当映射成 4xx，得到: %v", err)
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("错误要说清「文件不完整」，得到: %v", err)
	}
}

// TestLintFailureIsAUserError 同理：包写错了是用户的问题。
func TestLintFailureIsAUserError(t *testing.T) {
	f, dir := uploadFixture(t)
	bad := map[string]string{"pack.yaml": "schema: pack/v1\nname: x\nversion: \"1\"\n"}
	_, err := f.svc.UploadPack(ctx(), bytes.NewReader(mpackOf(t, bad)), dir, "admin")
	if err == nil {
		t.Fatal("残缺的 pack.yaml 应当被拒绝")
	}
	if !isUserError(err) {
		t.Errorf("包写错了是客户端问题，应当映射成 4xx，得到: %v", err)
	}
}

// TestUploadImportsBlobsIntoTheStore 覆盖以下缺陷。
//
// thick pack 里带着 `blobs/sha256-<hex>`，而节点是按 sha256 向 mechd 的
// 载荷库要的（`<BlobDir>/sha256/<xx>/<hex>`）。两处布局不同，不搬过去
// 的话 agent 拿不到载荷。
//
// **症状极其难查**：deploy 成功、没有任何地方报错，只是那个实例
// 「装上了、一直不收敛」。
//
// 之前的单元测试抓不到它，因为那个最小 Pack 根本没有载荷——**夹具越
// 干净，它掩盖的形状越多**（与第 5 步 go-webapp 那次是同一条）。
func TestUploadImportsBlobsIntoTheStore(t *testing.T) {
	f, dir := uploadFixture(t)

	payload := []byte("这是一份假装是二进制的载荷\x00\xff")
	sum := sha256.Sum256(payload)
	hexsum := hex.EncodeToString(sum[:])

	files := goodPack()
	files["blobs/sha256-"+hexsum] = string(payload)
	if _, err := f.svc.UploadPack(ctx(), bytes.NewReader(mpackOf(t, files)), dir, "admin"); err != nil {
		t.Fatal(err)
	}

	stored := filepath.Join(f.svc.BlobDir, "sha256", hexsum[:2], hexsum)
	got, err := os.ReadFile(stored)
	if err != nil {
		t.Fatalf("载荷没有入库——节点会拿不到它，症状是「装上了一直不收敛」: %v", err)
	}
	if string(got) != string(payload) {
		t.Error("入库的内容与包里的不一致")
	}
}

// TestUploadRejectsMislabeledBlob 守的是供应链那一半。
//
// 文件名里的 sha256 是**归档自己声称的**。名不副实的载荷进了库，之后
// 每一次按摘要取都会拿到错的东西——而那时已经查不到是上传引入的。
func TestUploadRejectsMislabeledBlob(t *testing.T) {
	f, dir := uploadFixture(t)

	files := goodPack()
	// 名字说是全 a 的摘要，内容却是别的
	files["blobs/sha256-"+strings.Repeat("a", 64)] = "内容与名字对不上"
	_, err := f.svc.UploadPack(ctx(), bytes.NewReader(mpackOf(t, files)), dir, "admin")
	if err == nil {
		t.Fatal("名不副实的载荷应当被拒绝")
	}
	if !strings.Contains(err.Error(), "mislabeled") {
		t.Errorf("错误要说清是摘要对不上，得到: %v", err)
	}
}
