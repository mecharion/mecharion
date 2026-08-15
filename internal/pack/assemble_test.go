package pack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// srcManifest 是一个带 sources 段的源 Pack，platforms 有意省略以验证推导。
const srcManifest = `schema: pack/v1
name: demo
version: "1.0.0"

# 这条注释必须出现在产物中
sources:
  main:
    linux/amd64: dist/demo-amd64.tar.gz
    linux/arm64: dist/demo-arm64.tar.gz

params:
  port: { type: port, default: 8080 }

roles:
  - resources:
      # 角色内的注释同样应当保留
      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }
    workload:
      runtime: systemd
      systemd:
        exec: "{{ .Paths.Current }}/bin/demo"
`

func writeSrcPack(t *testing.T, manifest string, artifacts map[string]string) string {
	t.Helper()
	extra := map[string]string{}
	for rel, content := range artifacts {
		extra[rel] = content
	}
	return writePack(t, manifest, extra)
}

func TestAssembleBasic(t *testing.T) {
	dir := writeSrcPack(t, srcManifest, map[string]string{
		"dist/demo-amd64.tar.gz": "AMD64 PAYLOAD",
		"dist/demo-arm64.tar.gz": "ARM64 PAYLOAD",
	})
	out := filepath.Join(t.TempDir(), "out")

	res, err := Assemble(dir, AssembleOptions{Out: out})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}

	// 平台从 sources 推导
	if got := strings.Join(res.Platforms, ","); got != "linux/amd64,linux/arm64" {
		t.Errorf("platforms = %q", got)
	}
	if len(res.Blobs) != 2 {
		t.Fatalf("期望 2 个 blob，实际 %d", len(res.Blobs))
	}

	// 产物能被解析且通过 lint
	if res.Lint == nil || !res.Lint.OK() {
		t.Errorf("产物未通过 lint:\n%s", dump(res.Lint))
	}

	// blob 按内容寻址落盘
	for _, b := range res.Blobs {
		p := filepath.Join(out, DirBlobs, BlobFileName(b.SHA256))
		if _, err := os.Stat(p); err != nil {
			t.Errorf("blob 文件缺失: %s", p)
		}
	}

	manifest := readFile(t, filepath.Join(out, PackFile))

	// sources 已被移除，blobs 已生成。
	// 注意按顶层键（行首无缩进）判断——`  - resources:` 含有 "sources:" 子串。
	if hasTopKey(manifest, "sources") {
		t.Error("产物中不应保留 sources 段")
	}
	if !hasTopKey(manifest, "blobs") {
		t.Error("产物中应当有 blobs 段")
	}
	// platforms 与 revision 显式落盘——产物不依赖「默认值」这一实现细节
	if !hasTopKey(manifest, "platforms") {
		t.Error("产物中 platforms 必须显式存在")
	}
	if !hasTopKey(manifest, "revision") {
		t.Error("产物中 revision 必须显式存在")
	}
	// 注释保留——产物是人会打开阅读的东西
	if !strings.Contains(manifest, "角色内的注释同样应当保留") {
		t.Error("产物丢失了作者写的注释")
	}
}

func TestAssembleDeduplicatesIdenticalPayloads(t *testing.T) {
	// 两个平台指向内容完全相同的文件
	dir := writeSrcPack(t, srcManifest, map[string]string{
		"dist/demo-amd64.tar.gz": "SAME BYTES",
		"dist/demo-arm64.tar.gz": "SAME BYTES",
	})
	out := filepath.Join(t.TempDir(), "out")

	res, err := Assemble(dir, AssembleOptions{Out: out})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(out, DirBlobs))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("内容相同的载荷应只存一份，实际 %d 个文件", len(entries))
	}

	reused := 0
	for _, b := range res.Blobs {
		if b.Reused {
			reused++
		}
	}
	if reused != 1 {
		t.Errorf("应有 1 个 blob 被标记为去重，实际 %d", reused)
	}
}

func TestAssembleRejectsInconsistentPlatforms(t *testing.T) {
	manifest := `schema: pack/v1
name: demo
version: "1.0.0"
sources:
  main:
    linux/amd64: dist/a
    linux/arm64: dist/b
  image:
    linux/amd64: dist/c
params:
  port: { type: port, default: 8080 }
roles:
  - resources:
      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }
    workload:
      runtime: systemd
      systemd:
        exec: /bin/demo
`
	dir := writeSrcPack(t, manifest, map[string]string{
		"dist/a": "a", "dist/b": "b", "dist/c": "c",
	})

	_, err := Assemble(dir, AssembleOptions{Out: filepath.Join(t.TempDir(), "out")})
	if err == nil {
		t.Fatal("各 blob 平台键不一致时应当报错")
	}
	// 错误必须指出是哪个 blob 缺哪个平台，否则作者无从下手
	for _, want := range []string{"image", "linux/arm64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q，实际: %v", want, err)
		}
	}
}

func TestAssembleRejectsNoPlatformsNoBlobs(t *testing.T) {
	manifest := `schema: pack/v1
name: hosttune
version: "1.0.0"
roles:
  - resources:
      - sysctl: { key: vm.swappiness, value: "1" }
`
	dir := writeSrcPack(t, manifest, nil)
	_, err := Assemble(dir, AssembleOptions{Out: filepath.Join(t.TempDir(), "out")})
	if err == nil {
		t.Fatal("无载荷且未声明 platforms 时应当报错")
	}
	if !strings.Contains(err.Error(), "declare platforms explicitly") {
		t.Errorf("错误应提示显式声明 platforms，实际: %v", err)
	}
}

func TestAssembleExplicitPlatformsWin(t *testing.T) {
	manifest := strings.Replace(srcManifest,
		`version: "1.0.0"`,
		"version: \"1.0.0\"\nplatforms: [linux/amd64, linux/arm64]", 1)
	dir := writeSrcPack(t, manifest, map[string]string{
		"dist/demo-amd64.tar.gz": "a", "dist/demo-arm64.tar.gz": "b",
	})
	res, err := Assemble(dir, AssembleOptions{Out: filepath.Join(t.TempDir(), "out")})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(res.Platforms) != 2 {
		t.Errorf("显式声明的 platforms 应被保留，实际 %v", res.Platforms)
	}
}

func TestAssembleSourceRoot(t *testing.T) {
	// 构建产物在 Pack 目录之外——这是真实项目的常态
	artifacts := t.TempDir()
	for _, n := range []string{"demo-amd64.tar.gz", "demo-arm64.tar.gz"} {
		if err := os.WriteFile(filepath.Join(artifacts, n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manifest := strings.ReplaceAll(srcManifest, "dist/", "")
	dir := writeSrcPack(t, manifest, nil)

	res, err := Assemble(dir, AssembleOptions{
		Out: filepath.Join(t.TempDir(), "out"), SourceRoot: artifacts,
	})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(res.Blobs) != 2 {
		t.Errorf("期望 2 个 blob，实际 %d", len(res.Blobs))
	}
}

func TestAssembleMissingArtifact(t *testing.T) {
	dir := writeSrcPack(t, srcManifest, map[string]string{
		"dist/demo-amd64.tar.gz": "a", // arm64 的产物没造
	})
	_, err := Assemble(dir, AssembleOptions{Out: filepath.Join(t.TempDir(), "out")})
	if err == nil {
		t.Fatal("载荷文件缺失时应当报错")
	}
	if !strings.Contains(err.Error(), "arm64") {
		t.Errorf("错误应指出是哪个来源缺失，实际: %v", err)
	}
}

func TestAssembleRefusesNonEmptyOutWithoutForce(t *testing.T) {
	dir := writeSrcPack(t, srcManifest, map[string]string{
		"dist/demo-amd64.tar.gz": "a", "dist/demo-arm64.tar.gz": "b",
	})
	out := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "leftover"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Assemble(dir, AssembleOptions{Out: out}); err == nil {
		t.Fatal("非空输出目录应当被拒绝")
	}
	res, err := Assemble(dir, AssembleOptions{Out: out, Force: true})
	if err != nil {
		t.Fatalf("--force 应当允许覆盖: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.Out, "leftover")); err == nil {
		t.Error("--force 应清空旧内容")
	}
}

func TestAssembleIsDeterministic(t *testing.T) {
	dir := writeSrcPack(t, srcManifest, map[string]string{
		"dist/demo-amd64.tar.gz": "a", "dist/demo-arm64.tar.gz": "b",
	})
	base := t.TempDir()

	first, err := Assemble(dir, AssembleOptions{Out: filepath.Join(base, "o1")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Assemble(dir, AssembleOptions{Out: filepath.Join(base, "o2")})
	if err != nil {
		t.Fatal(err)
	}

	m1 := readFile(t, filepath.Join(first.Out, PackFile))
	m2 := readFile(t, filepath.Join(second.Out, PackFile))
	if m1 != m2 {
		t.Error("同一输入两次 assemble 应产出完全相同的 pack.yaml")
	}
}

func TestAssembleCopiesLogicDirs(t *testing.T) {
	dir := writeSrcPack(t, srcManifest, map[string]string{
		"dist/demo-amd64.tar.gz": "a",
		"dist/demo-arm64.tar.gz": "b",
		"templates/a.tmpl":       "x\n",
		"files/static.txt":       "y\n",
		"hooks/post.sh":          "#!/bin/sh\ntrue\n",
	})
	out := filepath.Join(t.TempDir(), "out")
	if _, err := Assemble(dir, AssembleOptions{Out: out}); err != nil {
		t.Fatalf("assemble: %v", err)
	}

	for _, rel := range []string{"templates/a.tmpl", "files/static.txt", "hooks/post.sh"} {
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(rel))); err != nil {
			t.Errorf("产物缺少 %s", rel)
		}
	}
	// dist/ 是构建产物目录，不应被拷进 Pack
	if _, err := os.Stat(filepath.Join(out, "dist")); err == nil {
		t.Error("dist/ 不应出现在产物中")
	}
}

func TestAssembleFailsLintOnBadPack(t *testing.T) {
	// command 缺守卫 → 产物过不了 lint，assemble 必须如实报告
	manifest := strings.Replace(srcManifest,
		`      - archive: { blob: main, dest: "{{ .Paths.Generation }}" }`,
		`      - command: { run: "/bin/true" }`, 1)
	dir := writeSrcPack(t, manifest, map[string]string{
		"dist/demo-amd64.tar.gz": "a", "dist/demo-arm64.tar.gz": "b",
	})

	res, err := Assemble(dir, AssembleOptions{Out: filepath.Join(t.TempDir(), "out")})
	if err != nil {
		t.Fatalf("assemble 本身不应失败: %v", err)
	}
	if res.Lint == nil || res.Lint.OK() {
		t.Error("产物应当未通过 lint")
	}
	if !hasRule(res.Lint, "R31") {
		t.Errorf("应报 R31，实际:\n%s", dump(res.Lint))
	}
}

// hasTopKey 报告 YAML 文本中是否有某个顶层键（行首无缩进）。
func hasTopKey(manifest, key string) bool {
	for _, line := range strings.Split(manifest, "\n") {
		if strings.HasPrefix(line, key+":") {
			return true
		}
	}
	return false
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
