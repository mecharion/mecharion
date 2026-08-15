package ctlcmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/spf13/cobra"
)

// 一次命令里的多档确认**必须共用同一个带缓冲的读取器**。
//
// `bufio.Reader` 会**预读**：它一次从底层读一大块，`ReadString('\n')`
// 只是从那块里切出一行。因此每一档各自 `bufio.NewReader(stdin)` 的话，
// 第一档会把后面几行一起吞进它自己的缓冲区，第二档拿到的是一个已经
// 空掉的 stdin——症状是「明明喂了两行，第二问却说读不到回答」。
//
// 这条在 `component remove --purge-data`（组件名 ＋ 确认删数据，
// 10-cli §7）上是致命的：脚本里那条命令**永远走不完**。
//
// 早先没暴露出来，是因为当时只有一处会读 stdin。
var (
	readersMu sync.Mutex
	readers   = map[io.Reader]*bufio.Reader{}
)

// promptReader 返回这条命令的输入读取器，同一个 stdin 只包一层。
func promptReader(c *cobra.Command) *bufio.Reader {
	in := c.InOrStdin()
	readersMu.Lock()
	defer readersMu.Unlock()
	if r, ok := readers[in]; ok {
		return r
	}
	r := bufio.NewReader(in)
	readers[in] = r
	return r
}

// confirmYN 是危险操作的最低一档确认（10-cli §7）。
//
// 更高的两档——「输入 Component 名」「输入 purge-all」——留给它们各自的
// 命令，因为那几档的意义正是**不能被一个通用的 y 放过去**。
//
// **判据是「有没有人真的回答」，不是「stdin 是不是终端」。** 探测终端听起来
// 更直接，却把 `/dev/null` 判成了终端（它确实是字符设备），于是脚本里那条
// 没带 `-y` 的命令会拿到一个空回答——按 y/N 的默认走，就是沉默照做。
// 读不到回答就拒绝，这条判据不依赖任何平台细节，也不会有那种误判。
//
// 拒绝而不是默认放行：脚本跑到一条危险命令却没带 `-y`，最可能的解释是
// 写脚本的人没意识到它危险。让它停下来，比让它照做便宜得多。
func confirmYN(c *cobra.Command, prompt string, yes bool) error {
	if yes {
		return nil
	}
	fmt.Fprintf(c.ErrOrStderr(), "%s [y/N] ", prompt)

	line, err := promptReader(c).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer == "" && (err == nil || errors.Is(err, io.EOF)) {
		if errors.Is(err, io.EOF) {
			return validationErr(fmt.Errorf(
				"couldn't read a confirmation (non-interactive environment). This is a dangerous operation, pass -y explicitly"))
		}
		return errors.New("cancelled")
	}
	if err != nil && answer == "" {
		return fmt.Errorf("failed to read confirmation: %w", err)
	}
	switch answer {
	case "y", "yes":
		return nil
	}
	return errors.New("cancelled")
}

// confirmName 是危险操作的第二档确认：**要求把名字打出来**。
//
// 与 y/N 的区别不是「更烦一点」，而是它证明了**用户知道自己在删哪一个**。
// 一个 y 只证明他按了一个键——而这一档用在「删错了就回不来」的地方
// （10-cli §7）。
//
// **`-y` 不能跳过这一档**（10-cli §7 明写「-y 不能全部跳过」，表里
// `component remove` 与 `node remove --force` 都在这一档）。这两件事
// 挡的根本不是同一种错误：
//
//	-y      挡「手滑敲了回车」
//	输名字  挡「删错了对象」
//
// 运维在两个终端之间切换、把生产的名字敲进了测试那个窗口——这是真实
// 发生过的事故形态，而它唯一挡得住的方式就是让人把名字亲手打一遍。
// 一个 `-y` 不该同时买断这两件事。
//
// 早先这里写的是 `if yes { return nil }`，与 §7 直接冲突。这处是
// M9 第 3 步做 `component remove` 时发现的。
//
// 脚本里怎么办：**把名字从标准输入喂进来**，`echo pg-main | mechctl …`。
// 那仍然要求写脚本的人把对象名写对一次，正是这一档想要的。
func confirmName(c *cobra.Command, prompt, want string) error {
	fmt.Fprintf(c.ErrOrStderr(), "%s\n  Type %q to confirm: ", prompt, want)

	line, err := promptReader(c).ReadString('\n')
	got := strings.TrimSpace(line)
	if got == "" && errors.Is(err, io.EOF) {
		return validationErr(fmt.Errorf(
			"couldn't read a confirmation (non-interactive environment). -y can't skip this "+
				"tier — it guards against \"deleted the wrong thing\", not \"fat-fingered enter\".\n"+
				"  In a script, write:  echo %s | <this command>", want))
	}
	if got != want {
		return fmt.Errorf("got %q, not %q — cancelled", got, want)
	}
	return nil
}
