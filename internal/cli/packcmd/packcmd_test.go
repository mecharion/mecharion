package packcmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// run 用给定参数执行一个子命令，返回 stdout 与错误。
//
// stdout 与 stderr 分开：`-o json` 的输出必须是**纯净的** JSON，
// 混进 cobra 的错误行会让脚本解析失败。
func run(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	// 与 mechpack 主程序一致：错误由调用方展示，cobra 不重复打印
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	err := cmd.Execute()
	return stdout.String(), err
}

// TestInitAssembleLintRoundTrip 是 M1 的端到端冒烟：
// init 出来的骨架，填上产物后必须能 assemble，产物必须能通过 lint。
//
// 这条链路是 Pack 作者的第一次真实体验——它断了，其他都没意义。
func TestInitAssembleLintRoundTrip(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "myapp")
	output := OutputTable

	// ① init
	if _, err := run(t, NewInitCmd(), "myapp", "--dir", src); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := os.Stat(filepath.Join(src, "pack.yaml")); err != nil {
		t.Fatalf("init 未生成 pack.yaml: %v", err)
	}

	// ② 造出骨架里 sources 指向的产物（真实场景由作者的构建工具产出）
	dist := filepath.Join(src, "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"myapp-linux-amd64.tar.gz": "AMD64",
		"myapp-linux-arm64.tar.gz": "ARM64",
	} {
		if err := os.WriteFile(filepath.Join(dist, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// ③ assemble
	out := filepath.Join(base, "out")
	stdout, err := run(t, NewAssembleCmd(&output), src, "--out", out)
	if err != nil {
		t.Fatalf("assemble: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "artifact validation passed") {
		t.Errorf("assemble 应报告产物校验通过:\n%s", stdout)
	}

	// ④ lint 产物
	stdout, err = run(t, NewLintCmd(&output), out)
	if err != nil {
		t.Fatalf("lint 产物: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "1/1 Pack(s) passed") {
		t.Errorf("产物应通过 lint:\n%s", stdout)
	}
}

func TestInitRejectsBadName(t *testing.T) {
	for _, bad := range []string{"MyApp", "my_app", "-app", "app-", ""} {
		if _, err := run(t, NewInitCmd(), bad, "--dir", t.TempDir()); err == nil {
			t.Errorf("名字 %q 应当被拒绝", bad)
		}
	}
}

func TestInitRefusesNonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, NewInitCmd(), "app", "--dir", dir); err == nil {
		t.Fatal("非空目录应当被拒绝")
	}
	if _, err := run(t, NewInitCmd(), "app", "--dir", dir, "--force"); err != nil {
		t.Fatalf("--force 应当允许写入: %v", err)
	}
}

func TestLintExitCodeIsValidation(t *testing.T) {
	dir := t.TempDir()
	manifest := "schema: pack/v2\nname: bad\nversion: \"1.0\"\nplatforms: [linux/amd64]\nroles: []\n"
	if err := os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	output := OutputTable
	_, err := run(t, NewLintCmd(&output), dir)
	if err == nil {
		t.Fatal("校验失败时应返回错误")
	}
	if got := ExitCode(err); got != ExitValidation {
		t.Errorf("退出码 = %d, 期望 %d（对齐 CLI 规范中的「校验失败」）", got, ExitValidation)
	}
}

func TestLintJSONOutput(t *testing.T) {
	dir := t.TempDir()
	// R31：command 缺守卫
	manifest := `schema: pack/v1
name: demo
version: "1.0.0"
platforms: [linux/amd64]
roles:
  - resources:
      - command: { run: "/bin/true" }
`
	if err := os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	output := OutputJSON
	stdout, err := run(t, NewLintCmd(&output), dir)
	if err == nil {
		t.Fatal("应当校验失败")
	}

	// 字段名必须是 lowerCamelCase —— 这是对脚本的兼容性承诺
	var reports []struct {
		Pack     string `json:"pack"`
		OK       bool   `json:"ok"`
		Findings []struct {
			Rule    string `json:"rule"`
			Message string `json:"message"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(stdout), &reports); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, stdout)
	}
	if len(reports) != 1 || reports[0].OK {
		t.Fatalf("期望一份未通过的报告，实际 %+v", reports)
	}
	found := false
	for _, f := range reports[0].Findings {
		if f.Rule == "R31" {
			found = true
		}
	}
	if !found {
		t.Errorf("JSON 输出中应含 R31，实际 %s", stdout)
	}
}

func TestInspect(t *testing.T) {
	dir := t.TempDir()
	manifest := `schema: pack/v1
name: demo
version: "2.1.0"
revision: 3
description: "示例"
platforms: [linux/amd64]
roles:
  - name: server
    cardinality: "1-N"
    quorum: true
    workload:
      runtime: systemd
      systemd: { exec: /bin/demo }
`
	if err := os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	output := OutputTable
	stdout, err := run(t, NewInspectCmd(&output), dir)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	for _, want := range []string{"demo 2.1.0-3", "server", "1-N", "quorum"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("输出缺少 %q:\n%s", want, stdout)
		}
	}
}
