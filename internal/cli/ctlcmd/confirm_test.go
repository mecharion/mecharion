package ctlcmd

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runRollout 跑一条 `mechctl rollout` 子命令。
//
// **故意指向一个连不上的地址**：这样「确认拦下来了」与「确认被跳过了」
// 的错误完全不同——前者是那句提示，后者是一句连接失败。否则一个
// 恒返回错误的实现也能让测试通过。
func runRollout(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newMechctlRoot(NewRolloutCmd)
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader(""))
	root.SetArgs(append(append([]string{"rollout"}, args...),
		"--server", "http://127.0.0.1:1", "--token", "m7n_confirm-test"))
	err := root.Execute()
	return out.String() + errBuf.String(), err
}

// TestRolloutAbortNeedsConfirmation 钉住：非交互环境下不带 -y 的 abort
// **在发出请求之前**就被拒绝。
//
// 「危险操作要确认」这条约定唯一真正会被用到的地方就是脚本，而脚本
// 恰恰没有终端。此时沉默照做等于把这条约定取消掉。
func TestRolloutAbortNeedsConfirmation(t *testing.T) {
	out, err := runRollout(t, "abort", "web")
	if err == nil {
		t.Fatalf("不带 -y 的 abort 应当失败，实际:\n%s", out)
	}
	if !strings.Contains(err.Error(), "-y") {
		t.Errorf("错误信息应当告诉用户怎么继续（加 -y），实际: %v", err)
	}
	// 连接失败说明确认根本没拦——请求已经发出去了
	if strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("确认应当在发请求之前完成，实际已经连上游了: %v", err)
	}
}

// TestConfirmYN 逐条核对回答的判读。
func TestConfirmYN(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		yes   bool
		allow bool
	}{
		{"-y 直接放行", "", true, true},
		{"y", "y\n", false, true},
		{"yes 大小写不敏感", "YES\n", false, true},
		{"回车即取消", "\n", false, false},
		{"n", "n\n", false, false},
		{"读不到回答", "", false, false},
		{"含糊的回答不算同意", "yeah\n", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.SetErr(io.Discard)
			cmd.SetIn(strings.NewReader(tc.in))
			err := confirmYN(cmd, "确认？", tc.yes)
			if (err == nil) != tc.allow {
				t.Errorf("回答 %q（yes=%v）应当 allow=%v，实际 err=%v",
					tc.in, tc.yes, tc.allow, err)
			}
		})
	}
}

// pause / resume 可以再敲一次撤销，不该被同一道门挡住。
func TestRolloutPauseNeedsNoConfirmation(t *testing.T) {
	_, err := runRollout(t, "pause", "web")
	if err == nil {
		t.Fatal("连不上时 pause 应当报错")
	}
	if strings.Contains(err.Error(), "-y") {
		t.Errorf("pause 不该要求确认，实际: %v", err)
	}
}

// TestConfirmNameIgnoresYes 是 10-cli §7 的硬约束。
//
// **`-y` 挡的是「手滑敲了回车」，输名字挡的是「删错了对象」。** 一个 -y
// 不该同时买断这两件事——运维在两个终端之间切换、把生产的名字敲进了
// 测试那个窗口，这是真实发生过的事故形态。
//
// 这条测试之前不存在，而实现里当时写的正是 `if yes { return nil }`。
func TestConfirmNameIgnoresYes(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)
	cmd.SetIn(strings.NewReader("")) // 什么都没输入
	if err := confirmName(cmd, "删掉它？", "pg-main"); err == nil {
		t.Fatal("没有输入名字时必须拒绝——这一档不接受任何形式的跳过")
	}
}

func TestConfirmNameNeedsTheExactName(t *testing.T) {
	for _, tc := range []struct {
		in    string
		allow bool
	}{
		{"pg-main\n", true},
		{"  pg-main  \n", true}, // 两侧空白无所谓
		{"pg-mian\n", false},    // 打错一个字母
		{"pg-main-2\n", false},  // 相似的另一个组件
		{"y\n", false},          // y 在这一档不算数
		{"\n", false},
		{"", false}, // 非交互、没喂输入
	} {
		cmd := &cobra.Command{}
		cmd.SetErr(io.Discard)
		cmd.SetIn(strings.NewReader(tc.in))
		err := confirmName(cmd, "删掉它？", "pg-main")
		if (err == nil) != tc.allow {
			t.Errorf("输入 %q 应当 allow=%v，实际 err=%v", tc.in, tc.allow, err)
		}
	}
}

// TestConfirmNameTellsScriptsWhatToDo：拒绝之后要给出可行动的下一步。
//
// 一条只说「需要确认」的错误会让写脚本的人去找 -y——而那正是这一档
// 刻意不接受的东西。必须直接给出管道的写法。
func TestConfirmNameTellsScriptsWhatToDo(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)
	cmd.SetIn(strings.NewReader(""))
	err := confirmName(cmd, "删掉它？", "pg-main")
	if err == nil {
		t.Fatal("应当拒绝")
	}
	if !strings.Contains(err.Error(), "echo pg-main |") {
		t.Errorf("要给出脚本里的写法，得到: %v", err)
	}
}
