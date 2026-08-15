package packindex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/pack"
)

// writePack 在 dir 下造一个最小 Pack。
func writePack(t *testing.T, dir, name, version string, extra string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "schema: pack/v1\nname: " + name + "\nversion: \"" + version + "\"\n" +
		"platforms: [linux/amd64]\n" +
		"roles:\n  - resources: []\n" + extra
	if err := os.WriteFile(filepath.Join(dir, pack.PackFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadScansSubdirectories(t *testing.T) {
	root := t.TempDir()
	writePack(t, filepath.Join(root, "zookeeper"), "zookeeper", "3.9.1", "")
	writePack(t, filepath.Join(root, "jdk11"), "jdk11", "11.0.24", "")
	// 没有 pack.yaml 的目录应当被忽略，而不是报错
	if err := os.MkdirAll(filepath.Join(root, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}

	ix, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	names := ix.Names()
	if len(names) != 2 || names[0] != "jdk11" || names[1] != "zookeeper" {
		t.Errorf("Names = %v", names)
	}
}

// TestLoadAcceptsSinglePackDir 钉住直接指向一个 Pack 目录也行。
func TestLoadAcceptsSinglePackDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "zk")
	writePack(t, dir, "zookeeper", "3.9.1", "")

	ix, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Names()) != 1 {
		t.Errorf("Names = %v", ix.Names())
	}
}

// TestMissingDirIsNotAnError 钉住目录不存在等同于「本地没有 Pack」。
func TestMissingDirIsNotAnError(t *testing.T) {
	ix, err := Load(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("目录不存在不该是错误: %v", err)
	}
	if len(ix.Names()) != 0 {
		t.Error("应当是空索引")
	}
}

// TestBrokenPackIsRecordedNotSilent 钉住解析失败的目录被记下来。
//
// 静默跳过会让「为什么找不到依赖」无从查起。
func TestBrokenPackIsRecordedNotSilent(t *testing.T) {
	root := t.TempDir()
	bad := filepath.Join(root, "broken")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, pack.PackFile),
		[]byte("这不是合法的 yaml: ][\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ix, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Problems()) != 1 {
		t.Fatalf("应当记下 1 个问题目录，实际 %v", ix.Problems())
	}
	if !strings.Contains(ix.Problems()[0].Dir, "broken") {
		t.Errorf("问题目录 = %s", ix.Problems()[0].Dir)
	}
}

// TestResolvePicksHighestSatisfying 钉住取最高的满足版本。
//
// 依赖声明的是下限，用户装了更新的版本通常就是想用它。
func TestResolvePicksHighestSatisfying(t *testing.T) {
	root := t.TempDir()
	for _, v := range []string{"11.0.20", "11.0.24", "17.0.9"} {
		writePack(t, filepath.Join(root, "jdk-"+v), "jdk", v, "")
	}

	ix, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}

	e, err := ix.Resolve("jdk", ">=11")
	if err != nil {
		t.Fatal(err)
	}
	if e.Pack.Version != "17.0.9" {
		t.Errorf("解析到 %s，期望最高的 17.0.9", e.Pack.Version)
	}

	e, err = ix.Resolve("jdk", "~11")
	if err != nil {
		t.Fatal(err)
	}
	if e.Pack.Version != "11.0.24" {
		t.Errorf("~11 解析到 %s，期望 11.0.24", e.Pack.Version)
	}
}

// TestResolveErrorsAreActionable 钉住两类失败的错误信息可执行。
func TestResolveErrorsAreActionable(t *testing.T) {
	root := t.TempDir()
	writePack(t, filepath.Join(root, "jdk"), "jdk", "11.0.24", "")
	ix, _ := Load(root)

	// 本地根本没有
	_, err := ix.Resolve("postgresql", ">=14")
	if err == nil {
		t.Fatal("不存在的 Pack 应当报错")
	}
	for _, want := range []string{"postgresql", "本地已有", "不会联网"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应含 %q，实际:\n%v", want, err)
		}
	}

	// 有这个 Pack，但版本不满足
	_, err = ix.Resolve("jdk", ">=17")
	if err == nil {
		t.Fatal("版本不满足应当报错")
	}
	if !strings.Contains(err.Error(), "11.0.24") {
		t.Errorf("应当列出本地已有的版本，实际:\n%v", err)
	}
}

// TestExportsResolverContract 钉住 DepResolver 的 ok 语义。
//
// ok=false 表示**无法核对**，lint 据此降级为警告——依赖方可能单独发布，
// 缺席不代表 Pack 写错了。
func TestExportsResolverContract(t *testing.T) {
	root := t.TempDir()
	writePack(t, filepath.Join(root, "zookeeper"), "zookeeper", "3.9.1", `
exports:
  client: { role: default, port: "2181" }
  quorum: { role: default, port: "2888" }
`)
	ix, _ := Load(root)

	exports, ok := ix.Exports("zookeeper", ">=3.6")
	if !ok {
		t.Fatal("本地有这个 Pack，应当能核对")
	}
	if len(exports) != 2 || exports[0] != "client" || exports[1] != "quorum" {
		t.Errorf("Exports = %v", exports)
	}

	if _, ok := ix.Exports("postgresql", ">=14"); ok {
		t.Error("本地没有的 Pack 应当返回 ok=false")
	}
	if _, ok := ix.Exports("zookeeper", ">=99"); ok {
		t.Error("版本不满足时同样无法核对，应当 ok=false")
	}
}
