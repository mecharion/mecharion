package ctlcmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// `--set` 用于 secret 参数**直接报错**（10-cli §4.3）——这是一条
// 用户定过的硬约束，而在这次审查之前它只存在于文档里。

func cmdWithStdin(in string) *cobra.Command {
	c := &cobra.Command{}
	c.SetIn(strings.NewReader(in))
	c.SetOut(&bytes.Buffer{})
	c.SetErr(&bytes.Buffer{})
	return c
}

func TestReadStdinSecretsFromPipe(t *testing.T) {
	got, err := readStdinSecrets(cmdWithStdin("hunter2\n"), []string{"pw"})
	if err != nil {
		t.Fatal(err)
	}
	if got["pw"] != "hunter2" {
		t.Errorf("应当读到 hunter2，得到 %q", got["pw"])
	}
}

// TestStdinTrailingNewlineIsStripped 守的是最常见的造值方式。
//
// `echo secret | mechctl …` 会带一个换行，而它几乎从来不是口令的一部分。
// 不去掉的话症状是「口令明明对却连不上」——最难查的一类。
func TestStdinTrailingNewlineIsStripped(t *testing.T) {
	got, _ := readStdinSecrets(cmdWithStdin("hunter2\n"), []string{"pw"})
	if strings.HasSuffix(got["pw"].(string), "\n") {
		t.Error("结尾换行没去掉")
	}
	// CRLF 也要处理：Windows 上生成的文件很常见
	got, _ = readStdinSecrets(cmdWithStdin("hunter2\r\n"), []string{"pw"})
	if got["pw"] != "hunter2" {
		t.Errorf("CRLF 没处理干净，得到 %q", got["pw"])
	}
}

func TestMultipleStdinSecretsTakeOneLineEach(t *testing.T) {
	got, err := readStdinSecrets(cmdWithStdin("a-pw\nb-pw\n"), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if got["a"] != "a-pw" || got["b"] != "b-pw" {
		t.Errorf("按顺序一行一个，得到 %+v", got)
	}
}

// TestEmptyStdinIsRefused 是这一组里最要紧的一条。
//
// **不能静默用空值**：一个自动化脚本里少接了管道，空口令会让组件带着
// 空密码起来——而那要到很久以后才被发现，且发现时已经在生产上。
func TestEmptyStdinIsRefused(t *testing.T) {
	_, err := readStdinSecrets(cmdWithStdin(""), []string{"pw"})
	if err == nil {
		t.Fatal("stdin 是空的时候必须报错，不能静默用空值")
	}
	// 错误里要给出可行动的两条路
	for _, want := range []string{"--set-stdin", "--set-file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里应当给出 %s 的用法，得到: %v", want, err)
		}
	}
}

func TestTooFewLinesIsRefused(t *testing.T) {
	_, err := readStdinSecrets(cmdWithStdin("only-one\n"), []string{"a", "b"})
	if err == nil {
		t.Fatal("给了两个参数却只有一行时应当报错")
	}
	if !strings.Contains(err.Error(), "one value per line") {
		t.Errorf("错误要说清格式，得到: %v", err)
	}
}

func TestNoStdinParamsIsNoop(t *testing.T) {
	got, err := readStdinSecrets(cmdWithStdin(""), nil)
	if err != nil {
		t.Fatalf("没给 --set-stdin 时不该去读 stdin: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("不该返回任何值，得到 %+v", got)
	}
}
