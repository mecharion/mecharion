package mechd

import (
	"testing"

	"github.com/mecharion/mecharion/internal/store"
)

// fakePresence 是一份「谁挂在线上」的名单。
type fakePresence struct{ up map[string]bool }

func (p *fakePresence) Connected(node string) bool { return p.up[node] }

// TestSeenMachineGoneQuietReadsOffline 是这一整块改动的理由。
//
// 缺陷的形态是：`status` 列只有写 online 的路径，没有写回去的路径，于是
// 一台 agent 早就没了的机器永远显示在线。因此这条测试盯的是**同一行数据
// 在连接断掉前后要给出不同的答案**——只断言「注册后是 online」是抓不住
// 它的，那正是缺陷版本能通过的部分。
func TestSeenMachineGoneQuietReadsOffline(t *testing.T) {
	f := newFixture(t, "n1")
	pres := &fakePresence{up: map[string]bool{"n1": true}}
	f.svc.Presence = pres

	// 机器连上来过：这一步把 status 落成 seen
	n, err := f.svc.Repos.Nodes().GetByName(ctx(), f.site.ID, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Repos.Nodes().SetStatus(ctx(), n.ID, store.NodeSeen); err != nil {
		t.Fatal(err)
	}

	if got := statusOf(t, f, "n1"); got != NodeOnline {
		t.Fatalf("连着的时候应当是 %s，得到 %s", NodeOnline, got)
	}

	// 唯一改变的是连接没了——库里那一行一个字节都没动
	pres.up["n1"] = false

	if got := statusOf(t, f, "n1"); got != NodeOffline {
		t.Fatalf("连接断掉之后应当是 %s，得到 %s\n"+
			"（这正是那个只进不出的门：库里存着 online 就一直报 online）", NodeOffline, got)
	}
}

// TestRegisteredMachineNeverReadsPending 守的是 pending 的边界。
//
// pending 说的是「那台机器上还没有人执行过 join」。一台掉线的机器不能
// 退回 pending——运维看到 pending 会去那台机器上重新加入，而它其实
// 已经在册、只是死了。
func TestRegisteredMachineNeverReadsPending(t *testing.T) {
	f := newFixture(t, "n1")
	f.svc.Presence = &fakePresence{}

	if got := statusOf(t, f, "n1"); got != NodePending {
		t.Fatalf("登记之后、连上来之前应当是 %s，得到 %s", NodePending, got)
	}

	n, err := f.svc.Repos.Nodes().GetByName(ctx(), f.site.ID, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Repos.Nodes().SetStatus(ctx(), n.ID, store.NodeSeen); err != nil {
		t.Fatal(err)
	}

	if got := statusOf(t, f, "n1"); got != NodeOffline {
		t.Fatalf("来过又掉线应当是 %s 而不是 %s——%q 会让人去重新加入一台已在册的机器",
			NodeOffline, got, NodePending)
	}
}

// TestUnwiredPresenceDoesNotClaimOnline 守的是 §6.13 里那条「错的方向里
// 安全的那个」：Presence 没接上时宁可全报离线，也不要沿用库里的值。
//
// 沿用会让一次接线遗漏看起来完全正常，而那是最难发现的一类回归——
// 集群跑起来一切正常，只是状态列不再反映现实。
func TestUnwiredPresenceDoesNotClaimOnline(t *testing.T) {
	f := newFixture(t, "n1")
	f.svc.Presence = nil

	n, err := f.svc.Repos.Nodes().GetByName(ctx(), f.site.ID, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Repos.Nodes().SetStatus(ctx(), n.ID, store.NodeSeen); err != nil {
		t.Fatal(err)
	}

	if got := statusOf(t, f, "n1"); got == NodeOnline {
		t.Fatal("没接 Presence 时报了 online——这会让一次接线遗漏毫无症状")
	}
}

func statusOf(t *testing.T, f *fixture, name string) string {
	t.Helper()
	views, err := f.svc.ListNodes(ctx(), DefaultSite)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		if v.Name == name {
			return v.Status
		}
	}
	t.Fatalf("节点 %s 不在列表里", name)
	return ""
}
