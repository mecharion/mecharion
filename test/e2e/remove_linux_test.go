//go:build linux

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// M9 第 1 步的验收：**一个实例收到 runState: removed 之后真的被卸载干净**。
//
// 这里用真的 systemd、真的 unit、真的目录。单元测试用的是 fakeRuntime，
// 它证明得了编排顺序，证明不了「unit 文件真的没了、systemd 真的忘了它」
// ——而那正是卸载唯一要交付的东西。
//
// 判据分两半，缺一不可：
//
//	拆掉了什么：unit 消失、进程没了、generation 与 config 目录没了
//	留下了什么：数据目录还在，且留下了一张说明来历的收据
//
// 只验前一半的话，「rm -rf 全删光」也能通过——而那正是我们最不想要的
// 那种实现。

// removedSpec 在一份正常规格上加卸载意图。
func removedSpec(s map[string]any, opts map[string]any) map[string]any {
	s["runState"] = "removed"
	if opts != nil {
		s["removal"] = opts
	}
	return s
}

// applyRemoved 下发一次卸载，返回 mechlet 的输出。
func applyRemoved(ctx context.Context, t *testing.T, s map[string]any) string {
	t.Helper()
	out, err := runMechlet(ctx, "apply", "-f", writeSpec(t, s), "--data-dir", dataDir)
	if err != nil {
		t.Fatalf("卸载失败: %v\n%s", err, out)
	}
	return out
}

// TestRemovedUninstallsForReal 是这一步的主验收。
func TestRemovedUninstallsForReal(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	cleanup(t)
	t.Cleanup(func() { cleanup(t) })

	home := "/opt/mecharion-e2e/apps/" + component
	confDir := "/etc/mecharion-e2e/apps/" + component
	dataPath := dataDir + "/apps/" + component
	blobSum := installBlob(t, buildTarball(t))

	// ── 先真的装起来 ──
	base := func() map[string]any { return specOf(home, confDir, blobSum, "info", 1) }
	if out, err := runMechlet(ctx, "apply", "-f", writeSpec(t, base()), "--data-dir", dataDir); err != nil {
		t.Fatalf("先得装起来: %v\n%s", err, out)
	}
	waitUnitActive(t, 20*time.Second)
	// 数据目录里放一个记号：只有「没被删」才留得下来
	marker := filepath.Join(dataPath, "keepme")
	if err := os.WriteFile(marker, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	// ── 卸载 ──
	out := applyRemoved(ctx, t, removedSpec(base(), nil))
	t.Logf("mechlet apply（卸载）输出:\n%s", out)

	// ① systemd 真的忘了它。
	//
	// **不能只看 is-active**：一个 stop 掉但 unit 文件还在的服务同样是
	// inactive，而它会在下次 daemon-reload 或重启时重新出现在 systemd 的
	// 世界里。判据是 LoadState=not-found。
	if st := systemctl(t, "show", "-p", "LoadState", "--value", unitName); st != "not-found" {
		t.Errorf("unit 的 LoadState = %q，期望 not-found（unit 文件应当已被删除）", st)
	}
	if _, err := os.Stat("/etc/systemd/system/" + unitName); !os.IsNotExist(err) {
		t.Errorf("unit 文件还在: %v", err)
	}

	// ② 端口真的不通了——「进程没了」的唯一诚实判据。
	//
	// httpGet 连不上时返回空串，正是这里要的：连得上就说明进程还在。
	if body := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", port)); body != "" {
		t.Errorf("端口还通着，进程没死: %q", body)
	}

	// ③ generation 与配置目录没了
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("home（含 generations 与 current 软链）应当已删除: %v", err)
	}
	if _, err := os.Stat(confDir); !os.IsNotExist(err) {
		t.Errorf("配置目录默认删: %v", err)
	}

	// ④ 数据目录**还在**，而且内容没被动过
	b, err := os.ReadFile(marker)
	if err != nil || string(b) != "payload" {
		t.Errorf("数据目录默认保留且不该被改动，读 %s 得到 %q / %v", marker, b, err)
	}

	// ⑤ 留下了一张说明来历的收据——没有它，那个目录就是无主垃圾
	rec := loadRemovalReceipt(t)
	if rec == nil {
		t.Fatal("保留了数据却没留收据，orphans 将永远列不出这个目录")
	}
	if rec.Pack != "go-webapp" || rec.Version != "1.2.0" {
		t.Errorf("收据要记下是谁、哪一版留下的: %+v", rec)
	}
	if !hasPath(rec.RetainedPaths, dataPath) {
		t.Errorf("收据里没有数据目录 %s: %+v", dataPath, rec)
	}
}

// TestRemovedWithPurgeDataLeavesNothing 验另一头：全清时不留收据。
//
// 一次真正干净的卸载不该在机器上留下任何痕迹，**包括那张收据**。
// 留着它，`orphans list` 里会永远挂着一条指向空地址的记录。
func TestRemovedWithPurgeDataLeavesNothing(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	cleanup(t)
	t.Cleanup(func() { cleanup(t) })

	home := "/opt/mecharion-e2e/apps/" + component
	confDir := "/etc/mecharion-e2e/apps/" + component
	dataPath := dataDir + "/apps/" + component
	blobSum := installBlob(t, buildTarball(t))
	base := func() map[string]any { return specOf(home, confDir, blobSum, "info", 1) }

	if out, err := runMechlet(ctx, "apply", "-f", writeSpec(t, base()), "--data-dir", dataDir); err != nil {
		t.Fatalf("先得装起来: %v\n%s", err, out)
	}
	waitUnitActive(t, 20*time.Second)

	applyRemoved(ctx, t, removedSpec(base(), map[string]any{"purgeData": true}))

	if _, err := os.Stat(dataPath); !os.IsNotExist(err) {
		t.Errorf("给了 purgeData，数据目录应当也被删: %v", err)
	}
	p := filepath.Join(dataDir, "mechlet", "instances", component+"__"+role+".json")
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("什么都没留下时状态文件应当整个删掉: %s", p)
	}
}

// TestRemovedIsIdempotentOnRealNode 是 removing 能不能收尾的前提。
//
// mechd 要等全部实例都报「已卸载」才删记录，在那之前每一轮调和（默认
// 60 秒一次）都会再走一遍这条路。任何一处「已经没了 → 报错」都会让
// 组件永远卡在 removing，而那是运维视角下最难解释的一种状态。
func TestRemovedIsIdempotentOnRealNode(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	cleanup(t)
	t.Cleanup(func() { cleanup(t) })

	home := "/opt/mecharion-e2e/apps/" + component
	confDir := "/etc/mecharion-e2e/apps/" + component
	blobSum := installBlob(t, buildTarball(t))
	base := func() map[string]any { return specOf(home, confDir, blobSum, "info", 1) }

	if out, err := runMechlet(ctx, "apply", "-f", writeSpec(t, base()), "--data-dir", dataDir); err != nil {
		t.Fatalf("先得装起来: %v\n%s", err, out)
	}
	waitUnitActive(t, 20*time.Second)

	for i := 1; i <= 3; i++ {
		out, err := runMechlet(ctx, "apply", "-f",
			writeSpec(t, removedSpec(base(), nil)), "--data-dir", dataDir)
		if err != nil {
			t.Fatalf("第 %d 次卸载失败——组件会永远卡在 removing: %v\n%s", i, err, out)
		}
	}
}

// TestRemovedOnNeverInstalledSucceedsOnRealNode：从没装过也要成功。
//
// 一台部署失败之后才加进来的节点会走到这里。报错的话，那个组件同样卡死。
func TestRemovedOnNeverInstalledSucceedsOnRealNode(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	cleanup(t)
	t.Cleanup(func() { cleanup(t) })

	home := "/opt/mecharion-e2e/apps/" + component
	confDir := "/etc/mecharion-e2e/apps/" + component
	blobSum := installBlob(t, buildTarball(t))

	s := removedSpec(specOf(home, confDir, blobSum, "info", 1), nil)
	if out, err := runMechlet(ctx, "apply", "-f", writeSpec(t, s), "--data-dir", dataDir); err != nil {
		t.Fatalf("从没装过的实例收到 removed 应当直接成功: %v\n%s", err, out)
	}
}

// ── 辅助 ────────────────────────────────────────────────────────────────

type removalReceipt struct {
	At            string   `json:"at"`
	Pack          string   `json:"pack"`
	Version       string   `json:"version"`
	RetainedPaths []string `json:"retainedPaths"`
}

// loadRemovalReceipt 读卸载收据；状态文件不存在时返回 nil。
func loadRemovalReceipt(t *testing.T) *removalReceipt {
	t.Helper()
	p := filepath.Join(dataDir, "mechlet", "instances", component+"__"+role+".json")
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("读取本地状态 %s: %v", p, err)
	}
	var in struct {
		Removed *removalReceipt `json:"removed"`
	}
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatalf("解析本地状态: %v\n%s", err, b)
	}
	return in.Removed
}

func hasPath(list []string, want string) bool {
	for _, v := range list {
		if strings.TrimRight(v, "/") == strings.TrimRight(want, "/") {
			return true
		}
	}
	return false
}
