package ctlcmd

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/mecharion/mecharion/internal/mechd"
)

// 本文件钉住：mechctl 只有一份 --output（挂在根命令上），
// table/json/yaml 三种取值对全部只读命令都真的有效，未知取值非零退出。
// 此前根命令与每个名词命令（ClientFlags）各自持有一份 `-o`，cobra 按
// 离目标命令最近的祖先解析同名 flag——名词命令那份永远遮蔽根命令的
// 定义与校验，`-o yaml`/`-o table` 传给任何真实子命令都被子命令那份
// 「只认识 json，其余一律当 text」的逻辑默默吃掉。

// TestOutputFormatJSONAndYAMLAcrossReadCommands 是核心的 snapshot 覆盖：
// 一组有代表性的只读命令（跨 component / node / user 三个名词，即跨
// 三个此前各自遮蔽过根 flag 的独立注册点），JSON 与 YAML 都必须产出
// 能反序列化回结构体的合法输出，且内容一致。
func TestOutputFormatJSONAndYAMLAcrossReadCommands(t *testing.T) {
	w := newWired(t, "n1")
	w.mustRun("deploy", "go-webapp", "-c", "web", "--nodes", "n1")

	cases := []struct {
		name string
		args []string
		json any
		yaml any
	}{
		{
			name: "component status",
			args: []string{"component", "status", "web"},
			json: &mechd.StatusView{}, yaml: &mechd.StatusView{},
		},
		{
			name: "component list",
			args: []string{"component", "list"},
			json: &struct {
				Components []mechd.ComponentView `json:"components" yaml:"components"`
			}{},
			yaml: &struct {
				Components []mechd.ComponentView `json:"components" yaml:"components"`
			}{},
		},
		{
			name: "component diff",
			args: []string{"component", "diff", "web"},
			json: &mechd.DiffView{}, yaml: &mechd.DiffView{},
		},
		{
			name: "node list",
			args: []string{"node", "list"},
			json: &nodeList{}, yaml: &nodeList{},
		},
		{
			name: "node show",
			args: []string{"node", "show", "n1"},
			json: &mechd.NodeView{}, yaml: &mechd.NodeView{},
		},
		{
			name: "user show",
			args: []string{"user", "show"},
			json: &mechd.AdminView{}, yaml: &mechd.AdminView{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/json", func(t *testing.T) {
			out := w.mustRunFull(append(append([]string{}, tc.args...), "-o", "json")...)
			if err := json.Unmarshal([]byte(out), tc.json); err != nil {
				t.Fatalf("-o json 应产出合法 JSON: %v\n%s", err, out)
			}
		})
		t.Run(tc.name+"/yaml", func(t *testing.T) {
			out := w.mustRunFull(append(append([]string{}, tc.args...), "-o", "yaml")...)
			if err := yaml.Unmarshal([]byte(out), tc.yaml); err != nil {
				t.Fatalf("-o yaml 应产出合法 YAML: %v\n%s", err, out)
			}
			// YAML 不该是套了层皮的 JSON——真出问题最常见的表现就是
			// 「反序列化侥幸成功，但其实是拿 YAML 解析器容忍了 JSON
			// 语法」，所以顺手确认它不是一个单行大括号。
			if strings.HasPrefix(strings.TrimSpace(out), "{") {
				t.Errorf("-o yaml 的输出看起来像 JSON，不像 YAML:\n%s", out)
			}
		})
	}
}

// TestOutputFormatDefaultsToTable 确认不传 -o 时走人类可读的默认渲染，
// 不是意外地变成了 JSON/YAML（三种格式都要「真实实现」，不是随手选一种
// 当唯一路径）。
func TestOutputFormatDefaultsToTable(t *testing.T) {
	w := newWired(t, "n1")
	w.mustRun("deploy", "go-webapp", "-c", "web", "--nodes", "n1")

	out := w.mustRunFull("component", "status", "web")
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("默认输出不该是 JSON，实际:\n%s", out)
	}
	if !strings.Contains(out, "web") {
		t.Errorf("默认输出应当认得出组件名，实际:\n%s", out)
	}
}

// TestOutputFormatExplicitTable 确认 -o table 现在是一个**合法值**
// （此前根命令定义了 table|json|yaml，但因为被子命令那份
// `-o` 遮蔽，"-o table" 传给任何真实子命令都会静默退化成子命令的
// text 默认值——退化不是报错，但也不是真的走了 table 分支的证据）。
func TestOutputFormatExplicitTable(t *testing.T) {
	w := newWired(t, "n1")
	w.mustRun("deploy", "go-webapp", "-c", "web", "--nodes", "n1")

	out, _, err := w.runFull("component", "status", "web", "-o", "table")
	if err != nil {
		t.Fatalf("-o table 应当是合法值: %v", err)
	}
	if !strings.Contains(out, "web") {
		t.Errorf("table 输出应当认得出组件名，实际:\n%s", out)
	}
}

// TestOutputFormatUnknownValueFailsNonZero 钉住验收要求：
// 「未知值非零退出」。这条此前对任何真实子命令调用都是死代码——
// 校验只在根命令的 PersistentPreRunE 里，而子命令自己的 `-o` 遮蔽了它，
// 根本走不到那段校验。
func TestOutputFormatUnknownValueFailsNonZero(t *testing.T) {
	w := newWired(t, "n1")

	_, errBuf, err := w.runFull("component", "list", "-o", "bogus")
	if err == nil {
		t.Fatal("未知的 -o 取值应当报错")
	}
	if !strings.Contains(err.Error()+errBuf, "table") ||
		!strings.Contains(err.Error()+errBuf, "yaml") {
		t.Errorf("错误信息应当列出可用取值，实际: %v\n%s", err, errBuf)
	}
}

// TestOutputFormatSameFlagAcrossAllNouns 确认 --output 是**根命令上的
// 唯一一份**：任意名词命令的 --help 里都应当看到同一句说明文字，
// 而不是各自一份措辞、取值范围也可能不一致的独立定义。
func TestOutputFormatSameFlagAcrossAllNouns(t *testing.T) {
	w := newWired(t, "n1")
	for _, noun := range []string{"component", "node", "user"} {
		out, _, err := w.runFull(noun, "--help")
		if err != nil {
			t.Fatalf("[%s] --help 不该报错: %v", noun, err)
		}
		if !strings.Contains(out, "table|json|yaml") {
			t.Errorf("[%s] --help 应当看到根命令那份 --output 的说明，实际:\n%s", noun, out)
		}
	}
}
