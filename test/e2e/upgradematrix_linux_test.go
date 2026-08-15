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

// 本文件是 **M6 第 8 步的验收**：把 22-upgrade §4 的验收表**整张**过一遍，
// 三个 Runtime 一起。
//
// 之前每一步各自验了自己那一格，结果是「每条都验过，整张表没人看过」——
// 而这个里程碑承诺的是一整套性质在三种运行形态下都成立，不是九个孤立的
// 断言。表在这里收成一张，缺哪一格一眼可见。
//
// 三个 Runtime 共用同一段断言、只换夹具：**这正是 Runtime 接缝要证明的
// 事**。任何一格需要为某个 Runtime 单独放宽，都说明抽象漏了。
//
// 判据一律取「服务真的在应答」而不是「报告说成功了」。跨三个 Runtime 时
// 这一点尤其要紧：unit active、容器 running、project up 各说各话，
// 只有端口上有没有人应答是同一件事。

// upgradeCase 是一个 Runtime 在这张表里的全部夹具。
type upgradeCase struct {
	name    string
	require func(*testing.T)
	dataDir string
	key     string
	port    int
	// home 是组件的安装根，generation 目录在它下面。
	home string
	// appData 是组件的数据目录——「升级不碰数据」核对的就是它。
	appData string
	cleanup func(context.Context, *testing.T)
	// setup 装好夹具，返回「第 gen 代的规格」。
	setup func(context.Context, *testing.T) func(gen int) map[string]any
	// breakStart 把规格改成**起不来**的样子。
	breakStart func(map[string]any)
}

func upgradeCases() []upgradeCase {
	return []upgradeCase{
		{
			name:    "systemd",
			require: requireEnv,
			dataDir: dataDir,
			key:     component + "__" + role,
			port:    port,
			home:    "/opt/mecharion-e2e/apps/webapp",
			appData: dataDir + "/apps/" + component,
			cleanup: func(_ context.Context, t *testing.T) { cleanup(t) },
			setup: func(_ context.Context, t *testing.T) func(int) map[string]any {
				home := "/opt/mecharion-e2e/apps/webapp"
				confDir := "/etc/mecharion-e2e/apps/webapp"
				sum := installBlob(t, buildTarball(t))
				return func(gen int) map[string]any {
					return specOf(home, confDir, sum, "info", gen)
				}
			},
			breakStart: func(s map[string]any) {
				sd := s["workload"].(map[string]any)["systemd"].(map[string]any)
				sd["exec"] = "/opt/mecharion-e2e/apps/webapp/current/bin/" +
					"does-not-exist --config /etc/mecharion-e2e/apps/webapp/app.yaml"
			},
		},
		{
			name:    "docker",
			require: requireDocker,
			dataDir: dkDataDir,
			key:     dkComponent + "__default",
			port:    dkPort,
			home:    filepath.Join(dkDataDir, "apps", dkComponent),
			appData: filepath.Join(dkDataDir, "data", dkComponent),
			cleanup: cleanupDocker,
			setup: func(ctx context.Context, t *testing.T) func(int) map[string]any {
				sum := stageImageBlob(ctx, t)
				confDir := filepath.Join("/etc/mecharion-e2e-docker/apps", dkComponent)
				return func(gen int) map[string]any {
					s := dockerSpec(sum, confDir)
					s["pack"].(map[string]any)["revision"] = gen
					return s
				}
			},
			breakStart: func(s map[string]any) {
				d := s["workload"].(map[string]any)["docker"].(map[string]any)
				d["command"] = []string{"/does-not-exist"}
			},
		},
		{
			name: "compose",
			require: func(t *testing.T) {
				requireDocker(t)
				requireCompose(t)
			},
			dataDir: cpDataDir,
			key:     cpComponent + "__default",
			port:    cpWebPort,
			home:    filepath.Join(cpDataDir, "apps", cpComponent),
			appData: filepath.Join(cpDataDir, "data", cpComponent),
			cleanup: cleanupCompose,
			setup: func(ctx context.Context, t *testing.T) func(int) map[string]any {
				sum := stageImageBlobRoot(ctx, t, cpDataDir)
				confDir := filepath.Join(cpConfRoot, "apps", cpComponent)
				return func(gen int) map[string]any {
					s := composeSpec(sum, confDir)
					s["pack"].(map[string]any)["revision"] = gen
					return s
				}
			},
			breakStart: func(s map[string]any) {
				// compose 的「起不来」在**文件里**：改那条渲染出来的
				// compose.yaml，让 web 服务指向一个不存在的程序。
				confDir := filepath.Join(cpConfRoot, "apps", cpComponent)
				body := strings.Replace(composeFileBody(confDir),
					`command: ["--config", "/etc/app/web.yaml"]`,
					`entrypoint: ["/does-not-exist"]`, 1)
				setResourceContent(s, "template:"+confDir+"/compose.yaml", body)
			},
		},
	}
}

// TestUpgradeMatrix 逐格核对 22-upgrade §4 的验收表。
func TestUpgradeMatrix(t *testing.T) {
	for _, c := range upgradeCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Run("升级_回滚_数据不动", func(t *testing.T) { runUpgradeRow(t, c) })
			t.Run("新版起不来自动切回", func(t *testing.T) { runRollbackRow(t, c) })
			t.Run("超出保留数的旧版被回收", func(t *testing.T) { runPruneRow(t, c) })
		})
	}
}

// runUpgradeRow 覆盖验收表第 1、5、6 行。
//
//	1  正常升级：新版在跑，旧 generation 保留
//	5  手工回滚：秒级切回，**不重新物化**
//	6  升级过程中数据目录：一个字节都没动
//
// 三条放在一次部署里连着核对，是因为它们本来就是同一段时间线上的事：
// 分成三个测试要把同一套夹具装三遍，而**装夹具本身**是这里最慢的部分。
func runUpgradeRow(t *testing.T, c upgradeCase) {
	c.require(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	c.cleanup(ctx, t)
	t.Cleanup(func() { c.cleanup(context.Background(), t) })
	specOfGen := c.setup(ctx, t)

	// ── 第 1 代 ──
	mustApply(ctx, t, c, specOfGen(1))
	mustServe(t, c.port)

	// 数据目录里放一个标记：升级不该碰它一个字节
	marker := filepath.Join(c.appData, "keep-me.dat")
	body := []byte("升级前写下的数据\n")
	if err := os.WriteFile(marker, body, 0o644); err != nil {
		t.Fatalf("写数据目录标记（%s 应当已由 paths 建出来）: %v", c.appData, err)
	}
	before, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}

	gen1 := onlyGeneration(t, c)
	gen1Dir := gen1.Dir
	gen1Mod := dirModTime(t, gen1Dir)

	// ── 升级到第 2 代 ──
	mustApply(ctx, t, c, specOfGen(2))
	mustServe(t, c.port)

	led := readLedger(t, c.dataDir, c.key)
	if len(led.Generations) != 2 {
		t.Fatalf("升级之后应当有 2 代（旧的要保留），实际 %d: %+v",
			len(led.Generations), led.Generations)
	}
	if led.CurrentGeneration != 2 {
		t.Errorf("current 应当是第 2 代，实际 %d", led.CurrentGeneration)
	}
	// **旧 generation 目录必须还在**——回滚的落脚点就是它
	if !dirExistsAt(gen1Dir) {
		t.Errorf("旧 generation 目录 %s 不该被删——没有它就回滚不了", gen1Dir)
	}

	// ── 第 6 行：数据目录一个字节都没动 ──
	after, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("数据目录里的文件在升级后消失了: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Errorf("升级碰了数据目录：mtime %s → %s，size %d → %d",
			before.ModTime(), after.ModTime(), before.Size(), after.Size())
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != string(body) {
		t.Errorf("数据目录里的内容被改了：%q（err=%v）", got, err)
	}

	// ── 第 5 行：手工回到第 1 代，秒级且不重新物化 ──
	//
	// 判据是**没有产生新的 generation**：解析是纯函数，回到旧版本必然
	// 算出与当年一模一样的 digest，于是命中已保留的那一代，只切一次软链。
	// 多出一代就说明走的是「重新物化」那条路——那才是慢的来源。
	start := time.Now()
	mustApply(ctx, t, c, specOfGen(1))
	elapsed := time.Since(start)
	mustServe(t, c.port)

	led = readLedger(t, c.dataDir, c.key)
	if len(led.Generations) != 2 {
		t.Errorf("回滚不该产生新的 generation，实际有 %d 代: %+v",
			len(led.Generations), led.Generations)
	}
	if led.CurrentGeneration != gen1.Seq {
		t.Errorf("回滚之后 current 应当是第 %d 代，实际 %d", gen1.Seq, led.CurrentGeneration)
	}
	if got := dirModTime(t, gen1Dir); !got.Equal(gen1Mod) {
		t.Errorf("旧 generation 目录被重新物化了（mtime %s → %s）——"+
			"命中已保留的那一代时只该切软链", gen1Mod, got)
	}
	t.Logf("回滚耗时 %s", elapsed.Round(time.Millisecond))
}

// runRollbackRow 覆盖验收表第 2、4 行。
//
//	2  新版**起不来** → 自动切回旧版，**服务不丢**
//	4  回滚之后再调和一轮 → **不反复重试**，停在旧版
//
// 第 3 行（起来了但健康检查不过）在 rollback_linux_test.go 里三个 Runtime
// 各有一条，不在这里重复。
func runRollbackRow(t *testing.T, c upgradeCase) {
	c.require(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	c.cleanup(ctx, t)
	t.Cleanup(func() { c.cleanup(context.Background(), t) })
	specOfGen := c.setup(ctx, t)

	v1 := specOfGen(1)
	readLog := deployAndWatch(ctx, t, c.dataDir, v1, c.key)

	v2 := specOfGen(2)
	c.breakStart(v2)
	seedDesired(ctx, t, c.dataDir, c.key, writeSpec(t, v2))

	assertRolledBack(ctx, t, readLog, c.port)

	// ── 第 4 行：停在旧版，不反复横跳 ──
	//
	// 判据不是「没再回滚」，而是**回滚次数不再增长**：一个每轮重试一次的
	// 实现会每轮都停一次服务、再回滚一次，而每一轮的瞬间快照看起来都很正常。
	// 计数才分得出「停住了」与「一直在跳」。
	n1 := strings.Count(readLog(), "已回滚")
	cur := readLedger(t, c.dataDir, c.key).CurrentGeneration
	waitFor(ctx, 20*time.Second)

	if n2 := strings.Count(readLog(), "已回滚"); n2 != n1 {
		t.Errorf("回滚之后不该再反复尝试：回滚次数 %d → %d\n%s",
			n1, n2, tailOf(readLog(), 30))
	}
	if got := readLedger(t, c.dataDir, c.key).CurrentGeneration; got != cur {
		t.Errorf("回滚之后应当稳定停在第 %d 代，实际变成了 %d", cur, got)
	}
	// 服务从头到尾没丢
	mustServe(t, c.port)
}

// runPruneRow 覆盖验收表第 7 行：超出 `retainGenerations` 的旧版，
// 目录被回收。
//
// 镜像那一半（第 7、8 行的容器部分）在 imagegc_linux_test.go 里，
// 那里能分辨「删对了哪一个」；这里只核对目录与台账一致。
func runPruneRow(t *testing.T, c upgradeCase) {
	c.require(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	c.cleanup(ctx, t)
	t.Cleanup(func() { c.cleanup(context.Background(), t) })
	specOfGen := c.setup(ctx, t)

	for gen := 1; gen <= 4; gen++ {
		mustApply(ctx, t, c, specOfGen(gen))
	}
	mustServe(t, c.port)

	led := readLedger(t, c.dataDir, c.key)
	if len(led.Generations) != 3 {
		t.Fatalf("retainGenerations=3，应当只剩 3 代，实际 %d: %+v",
			len(led.Generations), led.Generations)
	}

	// **台账与磁盘必须一致**：留一条指向已删目录的记录，会让回滚在
	// 「命中 digest」之后才发现目录不在，那时已经停了服务。
	for _, g := range led.Generations {
		if g.Dir != "" && !dirExistsAt(g.Dir) {
			t.Errorf("台账里的 generation %04d 目录已不存在: %s", g.Seq, g.Dir)
		}
	}
	entries, err := os.ReadDir(filepath.Join(c.home, "generations"))
	if err != nil {
		t.Fatalf("读 generations 目录: %v", err)
	}
	if len(entries) != 3 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("磁盘上也应当只剩 3 个 generation 目录，实际 %v", names)
	}
}

// ── 夹具 ────────────────────────────────────────────────────────────────

func mustApply(ctx context.Context, t *testing.T, c upgradeCase, s map[string]any) {
	t.Helper()
	out, err := runMechlet(ctx, "apply", "-f", writeSpec(t, s), "--data-dir", c.dataDir)
	if err != nil {
		t.Fatalf("[%s] apply 失败: %v\n%s", c.name, err, out)
	}
}

// mustServe 用**同一条判据**核对三个 Runtime：端口上有没有人应答。
//
// unit active / 容器 running / project up 各说各话，只有这一条是同一件事。
func mustServe(t *testing.T, port int) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	if body := waitHTTP(t, url, 60*time.Second); body == "" {
		t.Fatalf("服务没有应答：%s", url)
	}
}

// ledger 是本地台账里这里用得到的那一部分。
type ledger struct {
	CurrentGeneration int `json:"currentGeneration"`
	Generations       []struct {
		Seq    int      `json:"seq"`
		Digest string   `json:"digest"`
		Dir    string   `json:"dir"`
		State  string   `json:"state"`
		Images []string `json:"images"`
	} `json:"generations"`
}

func readLedger(t *testing.T, dataDir, key string) ledger {
	t.Helper()
	path := filepath.Join(dataDir, "mechlet", "instances", key+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读台账 %s: %v", path, err)
	}
	var l ledger
	if err := json.Unmarshal(body, &l); err != nil {
		t.Fatalf("解析台账: %v\n%s", err, body)
	}
	return l
}

// onlyGeneration 断言此刻只有一代，并返回它。
func onlyGeneration(t *testing.T, c upgradeCase) struct {
	Seq    int      `json:"seq"`
	Digest string   `json:"digest"`
	Dir    string   `json:"dir"`
	State  string   `json:"state"`
	Images []string `json:"images"`
} {
	t.Helper()
	led := readLedger(t, c.dataDir, c.key)
	if len(led.Generations) != 1 {
		t.Fatalf("首装之后应当只有 1 代，实际 %d: %+v",
			len(led.Generations), led.Generations)
	}
	return led.Generations[0]
}

func dirExistsAt(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func dirModTime(t *testing.T, p string) time.Time {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("读 %s: %v", p, err)
	}
	return fi.ModTime()
}

// waitFor 睡一段时间，但 ctx 结束就提前返回。
func waitFor(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// setResourceContent 换掉某条 template 资源的内容。
func setResourceContent(s map[string]any, id, content string) {
	for _, raw := range s["resources"].([]map[string]any) {
		if raw["id"] != id {
			continue
		}
		raw["args"].(map[string]any)["content"] = content
		return
	}
	panic("找不到资源 " + id + " —— 夹具与规格不同步了")
}
