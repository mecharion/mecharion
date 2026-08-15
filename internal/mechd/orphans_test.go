package mechd

import (
	"strings"
	"testing"
	"time"

	"github.com/mecharion/mecharion/internal/protocol"
)

// reportOrphan 让某台机器报告一个孤儿。
func reportOrphan(t *testing.T, f *fixture, node, key string, paths ...string) {
	t.Helper()
	// **每一轮上报要有自己的时刻。** 「本轮没报到的删掉」靠时间戳区分轮次，
	// 而夹具的时钟是冻住的——不推的话每一轮都长得一模一样，那条清理
	// 永远不会生效，测试测的就成了另一回事。
	f.clock.advance(time.Second)
	b := &Backend{S: f.svc}
	err := b.Report(ctx(), protocol.Report{
		Node:    node,
		Orphans: []string{key},
		OrphanRecords: []protocol.OrphanRecord{{
			Key: key, RetainedPaths: paths, Pack: "go-webapp", Version: "1.2.0",
		}},
	})
	if err != nil {
		t.Fatalf("上报失败: %v", err)
	}
}

func orphansOf(t *testing.T, f *fixture, node string) []OrphanEntry {
	t.Helper()
	got, err := f.svc.ListOrphans(ctx(), "", node)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// ── 发现 ────────────────────────────────────────────────────────────────

// TestOrphanCarriesItsPaths 是这一步的地基。
//
// 一条只有实例键的记录回答不了唯一要紧的问题：**那台机器上到底还剩
// 什么、在哪儿**。10-cli §4.3 明写「保留而不提供发现机制等于把问题
// 推给未来」——只记一个键，那个机制就只完成了一半。
func TestOrphanCarriesItsPaths(t *testing.T) {
	f := newFixture(t, "n1", "n2")
	reportOrphan(t, f, "n1", "web__default", "/var/lib/mecharion/apps/web")

	got := orphansOf(t, f, "")
	if len(got) != 1 {
		t.Fatalf("应当有 1 个孤儿，实际 %d", len(got))
	}
	if got[0].Node != "n1" || got[0].Instance != "web__default" {
		t.Errorf("孤儿定位不对: %+v", got[0])
	}
	if len(got[0].Paths) != 1 || got[0].Paths[0] != "/var/lib/mecharion/apps/web" {
		t.Errorf("路径没带上来: %+v", got[0].Paths)
	}
	if got[0].Installed() {
		t.Error("有路径的应当被判为「数据残留」，不是「仍装着」")
	}
}

// TestOrphanWithoutPathsIsStillInstalled 守两类孤儿的区分。
//
// 有路径 = remove 留下的数据残留，purge 就是删几个目录。
// 无路径 = 下发里没有它了，但机器上**还装着、可能还在跑**。
//
// 混为一谈会让人以为 purge 能停掉一个还在跑的服务。
func TestOrphanWithoutPathsIsStillInstalled(t *testing.T) {
	f := newFixture(t, "n1")
	reportOrphan(t, f, "n1", "web__default") // 没有路径

	got := orphansOf(t, f, "n1")
	if len(got) != 1 || !got[0].Installed() {
		t.Fatalf("没有路径的孤儿应当被判为「仍装着」: %+v", got)
	}
}

// TestOrphansDisappearWhenNoLongerReported：整体替换，不是只增不减。
//
// 孤儿消失时记录要跟着消失，否则列表只增不减，很快就没人看了。
func TestOrphansDisappearWhenNoLongerReported(t *testing.T) {
	f := newFixture(t, "n1")
	reportOrphan(t, f, "n1", "web__default", "/var/lib/x")
	if len(orphansOf(t, f, "n1")) != 1 {
		t.Fatal("先得有一个")
	}

	// 下一轮什么都没报
	f.clock.advance(time.Second)
	b := &Backend{S: f.svc}
	if err := b.Report(ctx(), protocol.Report{Node: "n1"}); err != nil {
		t.Fatal(err)
	}
	if got := orphansOf(t, f, "n1"); len(got) != 0 {
		t.Errorf("不再上报的孤儿应当消失，实际还剩 %+v", got)
	}
}

// TestOldAgentWithoutRecordsStillRegisters 守向后兼容。
//
// 旧 mechlet 只报 Orphans 不报 OrphanRecords。那时少了路径细节，但
// **「有个东西在那儿」仍然必须看得见**——否则升级 mechd 会让一批孤儿
// 凭空消失，而它们在机器上一个没少。
func TestOldAgentWithoutRecordsStillRegisters(t *testing.T) {
	f := newFixture(t, "n1")
	b := &Backend{S: f.svc}
	if err := b.Report(ctx(), protocol.Report{
		Node: "n1", Orphans: []string{"web__default"}, // 没有 OrphanRecords
	}); err != nil {
		t.Fatal(err)
	}
	got := orphansOf(t, f, "n1")
	if len(got) != 1 || got[0].Instance != "web__default" {
		t.Fatalf("旧 agent 报的孤儿也得记下来: %+v", got)
	}
}

// ── 清理 ────────────────────────────────────────────────────────────────

// TestPurgeRecordsIntentAndDeliversIt 是 purge 那条链路。
//
// 它**不当场删任何东西**：中心不执行部署动作，而那台机器很可能此刻
// 联系不上——一个「只能在线时执行」的清理会把最需要清理的机器排除在外。
func TestPurgeRecordsIntentAndDeliversIt(t *testing.T) {
	f := newFixture(t, "n1", "n2")
	reportOrphan(t, f, "n1", "web__default", "/var/lib/mecharion/apps/web")

	if err := f.svc.PurgeOrphan(ctx(), PurgeOrphanRequest{
		Node: "n1", Instance: "web__default", Actor: "test",
	}); err != nil {
		t.Fatal(err)
	}

	// 下发里带上了
	b := &Backend{S: f.svc}
	keys, err := b.PurgeOrphans(ctx(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "web__default" {
		t.Fatalf("下发里应当带上待清理的键，实际 %v", keys)
	}
	// **别的机器不受影响**：同一个键会出现在多台机器上
	if other, _ := b.PurgeOrphans(ctx(), "n2"); len(other) != 0 {
		t.Errorf("n2 不该被牵连: %v", other)
	}
	// 列表里要看得出「正在等清理」
	if got := orphansOf(t, f, "n1"); len(got) != 1 || !got[0].PurgeRequested {
		t.Errorf("列表里应当标出待清理: %+v", got)
	}
}

// TestPurgeIntentSurvivesFurtherReports 是自限设计的另一半。
//
// 节点每 15 秒上报一次，而 purge 意图必须活到它真的被执行为止。
// upsert 若顺手把 purge_requested_at 冲掉，这条意图会在下一次上报时
// 消失——表现为「敲了 purge 但什么也没发生」，且没有任何错误。
func TestPurgeIntentSurvivesFurtherReports(t *testing.T) {
	f := newFixture(t, "n1")
	reportOrphan(t, f, "n1", "web__default", "/var/lib/x")
	if err := f.svc.PurgeOrphan(ctx(), PurgeOrphanRequest{
		Node: "n1", Instance: "web__default", Actor: "test",
	}); err != nil {
		t.Fatal(err)
	}

	// 节点又报了几轮，孤儿还在（它还没来得及清）
	for i := 0; i < 3; i++ {
		reportOrphan(t, f, "n1", "web__default", "/var/lib/x")
	}

	b := &Backend{S: f.svc}
	keys, _ := b.PurgeOrphans(ctx(), "n1")
	if len(keys) != 1 {
		t.Fatalf("purge 意图被上报冲掉了——敲了却什么也不会发生。实际 %v", keys)
	}
}

// TestPurgeIntentClearsItselfWhenTheOrphanIsGone：自限。
//
// 节点清完之后本地收据就没了，孤儿不再出现在上报里——这一行跟着消失，
// purge 意图自动失效。不需要任何确认序号或一次性标记。
func TestPurgeIntentClearsItselfWhenTheOrphanIsGone(t *testing.T) {
	f := newFixture(t, "n1")
	reportOrphan(t, f, "n1", "web__default", "/var/lib/x")
	if err := f.svc.PurgeOrphan(ctx(), PurgeOrphanRequest{
		Node: "n1", Instance: "web__default", Actor: "test",
	}); err != nil {
		t.Fatal(err)
	}

	// 节点清干净了，不再报这个孤儿
	f.clock.advance(time.Second)
	b := &Backend{S: f.svc}
	if err := b.Report(ctx(), protocol.Report{Node: "n1"}); err != nil {
		t.Fatal(err)
	}

	keys, _ := b.PurgeOrphans(ctx(), "n1")
	if len(keys) != 0 {
		t.Errorf("孤儿没了，purge 意图应当跟着失效，实际 %v", keys)
	}
}

// TestPurgeRefusesAnInstalledOrphan 守一条会误导人的路。
//
// 一个「还装着」的孤儿（机器上进程可能还在跑）不能靠 purge 解决——
// purge 只删目录，停不掉它。放行的话，人会以为问题解决了。
func TestPurgeRefusesAnInstalledOrphan(t *testing.T) {
	f := newFixture(t, "n1")
	reportOrphan(t, f, "n1", "web__default") // 没有路径 = 仍装着

	err := f.svc.PurgeOrphan(ctx(), PurgeOrphanRequest{
		Node: "n1", Instance: "web__default", Actor: "test",
	})
	if err == nil {
		t.Fatal("还装着的实例不该能被 purge")
	}
	if !strings.Contains(err.Error(), "still-installed") {
		t.Errorf("要说清为什么不行，得到: %v", err)
	}
}

func TestPurgeUnknownOrphanFails(t *testing.T) {
	f := newFixture(t, "n1")
	err := f.svc.PurgeOrphan(ctx(), PurgeOrphanRequest{
		Node: "n1", Instance: "nope__default", Actor: "test",
	})
	if err == nil {
		t.Fatal("不存在的孤儿应当报错")
	}
	if !strings.Contains(err.Error(), "orphans list") {
		t.Errorf("要给出下一步，得到: %v", err)
	}
}
