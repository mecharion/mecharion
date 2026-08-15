// 这个文件叫 _linux_test.go 是**故意的**，不要改回 purge_test.go。
//
// purge.go 里的 isAbs 只认 `/` 开头（那是刻意的：判据要跟节点上真实的
// 路径一致，不随构建平台变）。于是在 Windows 上跑这些测试时：
//
//	TestPurgeDeletesRetainedPathsAndReceipt  C:\... 被当成相对路径跳过 → 红
//	TestPurgeSkipsWhenTheInstanceWasRedeployed  同样被跳过 → **绿，但绿错了**
//
// 后一条更糟：它想验的是「有真实实例时不删」，实际验成了「Windows 上
// 什么都删不动」。一个照删不误的实现也能通过。
//
// mechlet 只跑在 Linux 上，CI 也在 Linux 上跑，因此这里限定平台，而不是
// 去把 isAbs 改成 filepath.IsAbs——那会为了让开发机变绿而放宽产品代码。
package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mecharion/mecharion/internal/state"
)

// purgeFixture 起一个只带本地状态的 agent——purge 不需要别的东西。
func purgeFixture(t *testing.T) (*Agent, *state.Store, string) {
	t.Helper()
	root := t.TempDir()
	st, err := state.New(filepath.Join(root, "mechlet"))
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{opts: Options{
		State: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
	return a, st, root
}

// receipt 造一个「已卸载、留了数据」的实例，返回那些目录。
func receipt(t *testing.T, st *state.Store, root, key string) []string {
	t.Helper()
	data := filepath.Join(root, "data", key)
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "payload"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveInstance(&state.Instance{
		Component: key, Role: "default",
		Removed: &state.Removal{
			At: time.Now(), Pack: "p", Version: "1",
			RetainedPaths: []string{data},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return []string{data}
}

// TestPurgeDeletesRetainedPathsAndReceipt 是这一步的主判据。
func TestPurgeDeletesRetainedPathsAndReceipt(t *testing.T) {
	a, st, root := purgeFixture(t)
	paths := receipt(t, st, root, "web")
	key := state.InstanceKey("web", "default")

	a.purgeOrphans(context.Background(), []string{key})

	for _, p := range paths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("目录应当被删掉: %s (%v)", p, err)
		}
	}
	in, err := st.LoadInstance(key)
	if err != nil {
		t.Fatal(err)
	}
	if in != nil {
		t.Error("收据也该没了——它是这个孤儿存在的唯一证据")
	}
}

// TestPurgeSkipsWhenNoReceipt 是整个 purge 设计的**安全性所在**。
//
// 时序：
//
//	t0  中心下发 purge web__default，这台机器正好失联
//	t1  运维用同一个名字重新部署了 web，新数据写进了同一个目录
//	t2  这台机器回来了，收到那条 purge
//
// t2 那一刻本地状态已经是一个**真实实例**（Removed 为 nil），不是收据。
// purge 必须直接空跑——否则它会把 t1 写进去的新数据删掉，而那正是
// 这个项目反复在防的那类事故。
func TestPurgeSkipsWhenTheInstanceWasRedeployed(t *testing.T) {
	a, st, root := purgeFixture(t)
	data := filepath.Join(root, "data", "web")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	// 一个**真实装着的**实例，不是收据
	if err := st.SaveInstance(&state.Instance{
		Component: "web", Role: "default",
		Paths:       map[string][]string{"data": {data}},
		Generations: []state.Generation{{Seq: 1, Digest: "d"}},
	}); err != nil {
		t.Fatal(err)
	}
	key := state.InstanceKey("web", "default")

	a.purgeOrphans(context.Background(), []string{key})

	if _, err := os.Stat(data); err != nil {
		t.Fatalf("重新部署过的实例，purge 必须空跑——它的数据被删了: %v", err)
	}
	if in, _ := st.LoadInstance(key); in == nil {
		t.Error("实例状态也不该被删掉")
	}
}

// TestPurgeIsIdempotent：清过的、不存在的键都走到同一个终态。
//
// purge 是**状态**：只要 mechd 还认为该清，每一轮下发都会再带一次。
// 任何一处「已经没了 → 报错」都会变成日志里每分钟一条的噪声。
func TestPurgeIsIdempotent(t *testing.T) {
	a, st, root := purgeFixture(t)
	receipt(t, st, root, "web")
	key := state.InstanceKey("web", "default")

	for i := 0; i < 3; i++ {
		a.purgeOrphans(context.Background(), []string{key})
	}
	a.purgeOrphans(context.Background(), []string{"never__heard-of-it"})
}

// TestPurgeLeavesReceiptWhenADirectoryCannotBeRemoved 守一条会让残留
// 变成幽灵的错误。
//
// 收据是这个孤儿存在的唯一证据。删目录失败却把收据删掉，剩下的目录就
// 再也不会出现在 `orphans list` 里——它变成了一个谁也不知道的残留。
//
// **失败用一个带 NUL 字节的路径造出来**，不用「去掉父目录写权限」：
// 后者对 root 无效，而这个项目的测试常常以 root 跑（容器、WSL）——
// 那条测试会被静默 skip，而 skip 掉的测试看起来和通过一模一样。
// NUL 路径在任何身份下都会让 RemoveAll 返回 EINVAL。
func TestPurgeLeavesReceiptWhenADirectoryCannotBeRemoved(t *testing.T) {
	a, st, root := purgeFixture(t)

	good := filepath.Join(root, "data")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	bad := "/tmp/m7n-purge-" + string(rune(0)) + "-invalid"

	if err := st.SaveInstance(&state.Instance{
		Component: "web", Role: "default",
		Removed: &state.Removal{
			At: time.Now(), RetainedPaths: []string{good, bad},
		},
	}); err != nil {
		t.Fatal(err)
	}
	key := state.InstanceKey("web", "default")

	a.purgeOrphans(context.Background(), []string{key})

	// 删得掉的那个照删——一个目录失败不该让其余的也留着
	if _, err := os.Stat(good); !os.IsNotExist(err) {
		t.Errorf("能删的那个应当已经删掉: %v", err)
	}
	// 但收据必须留着，否则剩下的东西就再也没人知道了
	in, err := st.LoadInstance(key)
	if err != nil {
		t.Fatal(err)
	}
	if in == nil || in.Removed == nil {
		t.Fatal("有目录没删成功时收据必须留着，否则那个残留就成了幽灵")
	}
}

// TestPurgeIgnoresRelativePaths：只删绝对路径。
//
// `os.RemoveAll("relative/path")` 是**相对 mechlet 的工作目录**解析的。
// 一条畸形的收据因此能让 purge 在一个完全无关的地方删东西——而 mechlet
// 的 cwd 是 systemd 给的，谁也说不清那是哪儿。
//
// 判据必须是「那个相对路径真的还在」：只断言「没 panic」的话，一个
// 照删不误的实现同样能通过。
func TestPurgeIgnoresRelativePaths(t *testing.T) {
	a, st, _ := purgeFixture(t)

	// 把 cwd 换到一个临时目录，在里面造出那个相对路径
	cwd := t.TempDir()
	t.Chdir(cwd)
	victim := filepath.Join(cwd, "relative", "path")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := st.SaveInstance(&state.Instance{
		Component: "web", Role: "default",
		Removed: &state.Removal{
			At: time.Now(), RetainedPaths: []string{"relative/path", ""},
		},
	}); err != nil {
		t.Fatal(err)
	}
	a.purgeOrphans(context.Background(), []string{state.InstanceKey("web", "default")})

	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("相对路径必须被跳过，它却在工作目录下被删了: %v", err)
	}
}
