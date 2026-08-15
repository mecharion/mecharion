package ctlcmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/pack"
)

// mustBuildMpack 把一个最小合法 Pack 打成 .mpack 字节，写到临时文件，
// 返回文件路径。与 internal/mechd/upload_test.go 的 mpackOf/goodPack
// 是同一种手法——两边都在验证 `POST /packs` 这同一个接口，只是分别站在
// 服务端与客户端。
func mustBuildMpack(t *testing.T, name string) string {
	t.Helper()
	src := t.TempDir()
	packYAML := `schema: pack/v1
name: ` + name + `
version: "1.0.0"
revision: 1
platforms: [linux/amd64]
params:
  greeting:
    type: string
    default: hi
paths:
  conf:
    default: "{{ .Node.Roots.etc }}/` + name + `"
roles:
  - name: main
    cardinality: "1-N"
    resources:
      - file:
          path: "{{ .Paths.conf }}/a.conf"
          content: "greeting={{ .Params.greeting }}\n"
          mode: "0644"
`
	if err := os.WriteFile(filepath.Join(src, "pack.yaml"), []byte(packYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := pack.WriteMpack(src, &buf); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), name+".mpack")
	if err := os.WriteFile(dst, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return dst
}

// TestPackUploadPutsAPackIntoTheRegistry 钉住这条命令补上的路径：
// 命令行现在有路径能把一个真实 .mpack 送进 mechd 的 Pack 集合——此前
// mechctl 完全没有能打到 POST /packs 的命令，`component deploy` 找到的
// 本地 pack-dir 定义只是元数据，没有这一步 blob 永远入不了库。
func TestPackUploadPutsAPackIntoTheRegistry(t *testing.T) {
	w := newWired(t)
	file := mustBuildMpack(t, "hello")

	out := w.mustRunFull("pack", "upload", file)
	if !strings.Contains(out, "Uploaded hello 1.0.0") {
		t.Errorf("上传成功的输出不对:\n%s", out)
	}
	if strings.Contains(out, "Overwrote") {
		t.Errorf("第一次上传不该报「已覆盖」:\n%s", out)
	}
}

// TestPackUploadOverwritesSameVersion 钉住同名同版本再传一次是覆盖，
// 不是报错——本地反复迭代 Pack 时的正常用法。
func TestPackUploadOverwritesSameVersion(t *testing.T) {
	w := newWired(t)
	file := mustBuildMpack(t, "hello")

	w.mustRunFull("pack", "upload", file)
	out := w.mustRunFull("pack", "upload", file)
	if !strings.Contains(out, "Overwrote hello 1.0.0") {
		t.Errorf("第二次上传同一个包应当报「已覆盖」:\n%s", out)
	}
}

// TestPackUploadRejectsMissingFile 钉住本地文件读不到时报的是清楚的
// 客户端错误，而不是把「文件不存在」当成网络请求发出去再报一个更难懂
// 的服务端错误。
func TestPackUploadRejectsMissingFile(t *testing.T) {
	w := newWired(t)
	_, _, err := w.runFull("pack", "upload", filepath.Join(t.TempDir(), "not-there.mpack"))
	if err == nil {
		t.Fatal("文件不存在应当报错")
	}
	if !strings.Contains(err.Error(), "open") {
		t.Errorf("错误信息应说明是打开文件失败，实际: %v", err)
	}
}

// TestPackUploadRejectsInvalidPack 钉住服务端的 lint 失败原样传回来给
// 用户——那句话本来就是写给用户看的（与其它命令的错误契约一致）。
func TestPackUploadRejectsInvalidPack(t *testing.T) {
	w := newWired(t)
	src := t.TempDir()
	// 缺 version/platforms/roles，lint 过不了
	if err := os.WriteFile(filepath.Join(src, "pack.yaml"),
		[]byte("schema: pack/v1\nname: bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := pack.WriteMpack(src, &buf); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "bad.mpack")
	if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := w.runFull("pack", "upload", file)
	if err == nil {
		t.Fatal("过不了 lint 的 Pack 应当被拒绝")
	}
}
