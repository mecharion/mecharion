package ctlcmd

import (
	"bytes"
	"encoding/json"
	"github.com/mecharion/mecharion/internal/cli"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/mecharion/mecharion/internal/spec"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "examples", "packs")); err != nil {
		t.Skip("没有源码树，跳过（容器内运行）")
	}
	// 必须是绝对路径：输入文件写在 TempDir 里，而 pack 的相对路径是
	// 相对**输入文件所在目录**解析的
	return root
}

// runCmd 跑一次 render 并返回 stdout / stderr。
func runCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewRenderCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errBuf.String(), err
}

func writePlan(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRenderZooKeeperOffline 是第 5 步的验收：离线产出 ResolvedSpec。
//
// 走的是与真实部署完全相同的管线，只少了「落库 + 下发」两步。
func TestRenderZooKeeperOffline(t *testing.T) {
	root := repoRoot(t)
	plan := writePlan(t, `
site: { name: site1, kind: cluster }
component: zk-main
pack: `+filepath.ToSlash(filepath.Join(root, "examples", "packs", "zookeeper"))+`
nodes:
  - { name: n1, address: 10.0.0.1, facts: { memory: { total: 32Gi } } }
  - { name: n2, address: 10.0.0.2, facts: { memory: { total: 32Gi } } }
  - { name: n3, address: 10.0.0.3, facts: { memory: { total: 32Gi } } }
instances:
  - { role: server, node: n1, ordinal: 0 }
  - { role: server, node: n2, ordinal: 1 }
  - { role: server, node: n3, ordinal: 2 }
requires:
  jdk11:
    pack: jdk11
    component: jdk11
    version: "11.0.22"
    scope: node
    paths:
      current: ["/opt/mecharion/apps/jdk11/current"]
`)

	outDir := filepath.Join(t.TempDir(), "out")
	stdout, _, err := runCmd(t, "-f", plan, "--out", outDir)
	if err != nil {
		t.Fatalf("render 失败: %v\n%s", err, stdout)
	}

	for _, want := range []string{"server_n1.json", "server_n2.json", "server_n3.json"} {
		b, err := os.ReadFile(filepath.Join(outDir, want))
		if err != nil {
			t.Fatalf("没有产出 %s: %v", want, err)
		}
		s, err := spec.Parse(b)
		if err != nil {
			t.Fatalf("%s 不是合法规格: %v", want, err)
		}
		if err := spec.VerifyDigest(s); err != nil {
			t.Errorf("%s: %v", want, err)
		}
		if s.Component != "zk-main" {
			t.Errorf("%s: Component 应为 zk-main，实际 %s", want, s.Component)
		}
	}

	// myid 必须是各自固化的 ordinal —— 三份规格里不能是同一个数字
	seen := map[string]bool{}
	for i, f := range []string{"server_n1.json", "server_n2.json", "server_n3.json"} {
		b, _ := os.ReadFile(filepath.Join(outDir, f))
		s, _ := spec.Parse(b)
		if s.Ordinal != i {
			t.Errorf("%s 的 ordinal 应为 %d，实际 %d", f, i, s.Ordinal)
		}
		seen[s.Digest] = true
	}
	if len(seen) != 3 {
		t.Error("三个实例的 digest 应当各不相同（myid / 本机路径都不同）")
	}
}

// TestRenderIsOffline 钉住「不碰任何机器」。
//
// 这条命令的价值就在于此：事故复盘时集群可能已经不在了。
func TestRenderIsOffline(t *testing.T) {
	root := repoRoot(t)
	plan := writePlan(t, `
component: demo
pack: `+filepath.ToSlash(filepath.Join(root, "examples", "packs", "go-webapp"))+`
nodes:
  - { name: n1, address: 10.0.0.1 }
instances:
  - { role: default, node: n1, ordinal: 0 }
`)
	stdout, _, err := runCmd(t, "-f", plan)
	if err != nil {
		t.Fatalf("render 失败: %v", err)
	}
	// 输出直接是规格，用不着任何服务
	if !strings.Contains(stdout, `"component": "demo"`) {
		t.Errorf("标准输出里应当有规格，实际:\n%s", stdout)
	}
	var body string
	if i := strings.Index(stdout, "{"); i >= 0 {
		body = stdout[i:]
	}
	var s spec.ResolvedSpec
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
}

// TestRenderRejectsUnknownFields 钉住输入文件的拼写错误不被静默忽略。
func TestRenderRejectsUnknownFields(t *testing.T) {
	root := repoRoot(t)
	plan := writePlan(t, `
component: demo
pack: `+filepath.ToSlash(filepath.Join(root, "examples", "packs", "go-webapp"))+`
nodes:
  - { name: n1, address: 10.0.0.1 }
instances:
  - { role: default, node: n1, ordinal: 0 }
prams:
  component: { port: 9090 }
`)
	_, _, err := runCmd(t, "-f", plan)
	if err == nil {
		t.Fatal("拼错的顶层字段应当报错——静默忽略会让人以为设置生效了")
	}
	if ExitCodeOf(err) != ExitValidation {
		t.Errorf("校验类错误的退出码应为 %d，实际 %d", ExitValidation, ExitCodeOf(err))
	}
}

// TestRenderReportsUnknownNode 钉住放置与节点表不一致时的错误信息。
func TestRenderReportsUnknownNode(t *testing.T) {
	root := repoRoot(t)
	plan := writePlan(t, `
component: demo
pack: `+filepath.ToSlash(filepath.Join(root, "examples", "packs", "go-webapp"))+`
nodes:
  - { name: n1, address: 10.0.0.1 }
instances:
  - { role: default, node: n9, ordinal: 0 }
`)
	_, _, err := runCmd(t, "-f", plan)
	if err == nil {
		t.Fatal("实例落在未声明的节点上应当报错")
	}
	for _, want := range []string{"n9", "n1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应含 %q（缺什么、有什么都要说），实际:\n%v", want, err)
		}
	}
}

// TestRenderSecretsStayReferences 钉住离线产出可以直接传阅。
func TestRenderSecretsStayReferences(t *testing.T) {
	root := repoRoot(t)
	plan := writePlan(t, `
component: minio
pack: `+filepath.ToSlash(filepath.Join(root, "examples", "packs", "minio"))+`
nodes:
  - { name: n1, address: 10.0.0.1 }
  - { name: n2, address: 10.0.0.2 }
  - { name: n3, address: 10.0.0.3 }
  - { name: n4, address: 10.0.0.4 }
instances:
  - { role: server, node: n1, ordinal: 0 }
  - { role: server, node: n2, ordinal: 1 }
  - { role: server, node: n3, ordinal: 2 }
  - { role: server, node: n4, ordinal: 3 }
params:
  component:
    root_password: "operator-secret-value"
`)
	stdout, stderr, err := runCmd(t, "-f", plan)
	if err != nil {
		t.Fatalf("render 失败: %v", err)
	}
	if strings.Contains(stdout, "operator-secret-value") {
		t.Fatal("离线产出里出现了口令明文——这份输出是要拿去传阅的")
	}
	if !strings.Contains(stdout, spec.SecretPrefix) {
		t.Error("产出里应当是密钥引用")
	}
	// 限制必须明说，不能让人自己发现
	if !strings.Contains(stderr, "isn't comparable") {
		t.Errorf("应提示离线 digest 与生产不可比，实际 stderr:\n%s", stderr)
	}
}

// TestRenderCommandWiresIntoRealTree 钉住 `mechctl component render` 真的能跑。
//
// 上面所有测试都单独构造 NewRenderCmd()，因此**看不到命令树合并那一步**——
// 而 cobra 正是在那一步把父命令的持久 flag 并进来。`--out` 曾经占着 `-o`
// 简写，与 root 和 component 上的 `--output` 撞车，表现是**一执行就 panic**：
//
//	panic: unable to redefine 'o' shorthand in "render" flagset
//
// 单测全绿、命令根本没法用。因此这条测试走完整的树，一个 flag 都不能少。
func TestRenderCommandWiresIntoRealTree(t *testing.T) {
	root := repoRoot(t)

	comp := NewComponentCmd(&ClientFlags{Global: &cli.GlobalFlags{}})
	// 复刻 cli.NewRoot 会挂的那个持久 flag——冲突就发生在它与子命令之间
	tree := &cobra.Command{Use: "mechctl"}
	var output string
	tree.PersistentFlags().StringVarP(&output, "output", "o", "table", "输出格式")
	tree.AddCommand(comp)

	plan := writePlan(t, `
site: { name: site1, kind: standalone }
component: web
pack: `+filepath.ToSlash(filepath.Join(root, "examples", "packs", "go-webapp"))+`
nodes:
  - { name: n1, address: 10.0.0.1 }
instances:
  - { role: default, node: n1, ordinal: 0 }
`)
	outDir := filepath.Join(t.TempDir(), "out")

	var buf bytes.Buffer
	tree.SetOut(&buf)
	tree.SetErr(&buf)
	tree.SetArgs([]string{"component", "render", "-f", plan, "--out", outDir})
	if err := tree.Execute(); err != nil {
		t.Fatalf("mechctl component render 应当可用: %v\n%s", err, buf.String())
	}

	files, _ := filepath.Glob(filepath.Join(outDir, "*.json"))
	if len(files) != 1 {
		t.Fatalf("应当写出 1 份规格，实际 %v\n%s", files, buf.String())
	}
}
