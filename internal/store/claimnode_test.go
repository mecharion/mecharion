package store

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestClaimNode 系列验证：token 消耗与 Node 落库必须是同一个
// 原子操作，此前是四次独立的读写，中间的窗口能被并发请求同时钻进去。

func newClaimFixture(t *testing.T) (*Store, Repos, Site) {
	t.Helper()
	s := openTest(t)
	r := s.Repos()
	site, err := r.Sites().Create(context.Background(), Site{Name: "s1", Kind: "cluster"})
	if err != nil {
		t.Fatal(err)
	}
	return s, r, site
}

func makeToken(t *testing.T, r Repos, siteID int64, nodeName string, maxUses int) JoinToken {
	t.Helper()
	tok, err := r.JoinTokens().Create(bg(), JoinToken{
		SiteID: siteID, Hash: "hash-" + t.Name(), NodeName: nodeName,
		ExpiresAt: time.Now().Add(time.Hour), MaxUses: maxUses,
		CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestClaimNodeSucceedsAndConsumesToken(t *testing.T) {
	_, r, site := newClaimFixture(t)
	tok := makeToken(t, r, site.ID, "", 1)

	n, err := r.ClaimNode(bg(), tok.ID, Node{SiteID: site.ID, Name: "n1", Address: "10.0.0.1"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n.Name != "n1" {
		t.Errorf("Name = %q", n.Name)
	}

	got, err := r.Nodes().GetByName(bg(), site.ID, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != n.ID {
		t.Errorf("落库的行与返回的不是同一条")
	}

	list, err := r.JoinTokens().List(bg(), site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Used != 1 {
		t.Errorf("token used = %d，期望 1", list[0].Used)
	}
}

func TestClaimNodeRejectsExhaustedToken(t *testing.T) {
	_, r, site := newClaimFixture(t)
	tok := makeToken(t, r, site.ID, "", 1)

	if _, err := r.ClaimNode(bg(), tok.ID, Node{SiteID: site.ID, Name: "n1"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	// 第二次用同一张（已用尽的）token 认领另一个名字
	_, err := r.ClaimNode(bg(), tok.ID, Node{SiteID: site.ID, Name: "n2"}, time.Now())
	if !errors.Is(err, ErrTokenUnavailable) {
		t.Fatalf("err = %v，期望 ErrTokenUnavailable", err)
	}
	if _, err := r.Nodes().GetByName(bg(), site.ID, "n2"); !errors.Is(err, ErrNotFound) {
		t.Error("token 用尽后不该有 n2 这一行——事务应当整体回滚")
	}
}

func TestClaimNodeRejectsExpiredToken(t *testing.T) {
	_, r, site := newClaimFixture(t)
	tok, err := r.JoinTokens().Create(bg(), JoinToken{
		SiteID: site.ID, Hash: "hash-" + t.Name(),
		ExpiresAt: time.Now().Add(-time.Minute), // 已过期
		MaxUses:   1, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.ClaimNode(bg(), tok.ID, Node{SiteID: site.ID, Name: "n1"}, time.Now())
	if !errors.Is(err, ErrTokenUnavailable) {
		t.Fatalf("err = %v，期望 ErrTokenUnavailable（token 已过期）", err)
	}
}

func TestClaimNodeRejectsTakenNameWithoutBurningToken(t *testing.T) {
	_, r, site := newClaimFixture(t)
	tok := makeToken(t, r, site.ID, "", 5)

	if _, err := r.ClaimNode(bg(), tok.ID, Node{SiteID: site.ID, Name: "n1"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	// 同一个名字再认领一次——名字冲突，不该消耗 token 的使用次数
	_, err := r.ClaimNode(bg(), tok.ID, Node{SiteID: site.ID, Name: "n1"}, time.Now())
	if !errors.Is(err, ErrNodeTaken) {
		t.Fatalf("err = %v，期望 ErrNodeTaken", err)
	}

	list, err := r.JoinTokens().List(bg(), site.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 只有第一次成功的那次算数：输给名字冲突的那次不该额外计一次使用，
	// 否则一个总在抢已占用名字的攻击者能白白耗尽别人的合法 token
	if list[0].Used != 1 {
		t.Errorf("used = %d，期望 1（名字冲突那次不该计入）", list[0].Used)
	}
}

// TestClaimNode 的 reserved 认领系列验证：node add 预登记
// （status=reserved，从未发过证书）的行必须能被 Join 认领，而已经发过
// 证书或被吊销的行必须继续被拒绝——这与并发认领要挡的是同一类风险，
// 只是多认得一种「可以原地认领」的既有行。

func TestClaimNodeClaimsReservedNode(t *testing.T) {
	_, r, site := newClaimFixture(t)
	reserved, err := r.Nodes().Upsert(bg(), Node{
		SiteID: site.ID, Name: "n1", Address: "10.0.0.1", Status: NodeReserved,
	})
	if err != nil {
		t.Fatal(err)
	}
	tok := makeToken(t, r, site.ID, "", 1)

	claimed, err := r.ClaimNode(bg(), tok.ID, Node{
		SiteID: site.ID, Name: "n1", Address: "10.0.0.2", Status: NodePending,
	}, time.Now())
	if err != nil {
		t.Fatalf("认领 reserved 行不该报错: %v", err)
	}
	if claimed.ID != reserved.ID {
		t.Errorf("认领应当原地改行，ID = %d，期望仍是 %d", claimed.ID, reserved.ID)
	}
	if claimed.Status != NodePending {
		t.Errorf("Status = %q，期望 %q", claimed.Status, NodePending)
	}
	if claimed.Address != "10.0.0.2" {
		t.Errorf("Address = %q，期望换成认领时提交的新地址", claimed.Address)
	}

	list, err := r.JoinTokens().List(bg(), site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Used != 1 {
		t.Errorf("token used = %d，期望 1——认领 reserved 行也要消耗 token", list[0].Used)
	}

	got, err := r.Nodes().GetByName(bg(), site.ID, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != NodePending || got.Address != "10.0.0.2" {
		t.Errorf("落库的行 = %+v，与认领返回的不一致", got)
	}
}

func TestClaimNodeRejectsRevokedReservedNode(t *testing.T) {
	_, r, site := newClaimFixture(t)
	reserved, err := r.Nodes().Upsert(bg(), Node{
		SiteID: site.ID, Name: "n1", Status: NodeReserved,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := r.Nodes().SetRevoked(bg(), reserved.ID, &now); err != nil {
		t.Fatal(err)
	}
	tok := makeToken(t, r, site.ID, "", 1)

	_, err = r.ClaimNode(bg(), tok.ID, Node{SiteID: site.ID, Name: "n1", Status: NodePending}, now)
	if !errors.Is(err, ErrNodeTaken) {
		t.Fatalf("err = %v，期望 ErrNodeTaken——预登记之后被吊销的行不能再被认领", err)
	}

	list, err := r.JoinTokens().List(bg(), site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Used != 0 {
		t.Errorf("used = %d，期望 0——认领被拒不该消耗 token", list[0].Used)
	}
}

func TestClaimNodeRejectsAlreadyIssuedNode(t *testing.T) {
	_, r, site := newClaimFixture(t)
	first, err := r.JoinTokens().Create(bg(), JoinToken{
		SiteID: site.ID, Hash: "hash-first", ExpiresAt: time.Now().Add(time.Hour),
		MaxUses: 1, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimNode(bg(), first.ID, Node{
		SiteID: site.ID, Name: "n1", Status: NodePending,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}

	second, err := r.JoinTokens().Create(bg(), JoinToken{
		SiteID: site.ID, Hash: "hash-second", ExpiresAt: time.Now().Add(time.Hour),
		MaxUses: 1, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.ClaimNode(bg(), second.ID, Node{
		SiteID: site.ID, Name: "n1", Status: NodePending,
	}, time.Now())
	if !errors.Is(err, ErrNodeTaken) {
		t.Fatalf("err = %v，期望 ErrNodeTaken——已经发过证书的行不是 reserved，不能再被认领", err)
	}
}

// TestClaimNodeConcurrentReservedOnlyOneWins 是
// TestClaimNodeConcurrentSameNameOnlyOneWins 的 reserved 认领版本：
// 并发认领同一个预登记的 reserved 行，只有一次能把它变成 pending。
func TestClaimNodeConcurrentReservedOnlyOneWins(t *testing.T) {
	_, r, site := newClaimFixture(t)
	if _, err := r.Nodes().Upsert(bg(), Node{
		SiteID: site.ID, Name: "n1", Status: NodeReserved,
	}); err != nil {
		t.Fatal(err)
	}
	tok := makeToken(t, r, site.ID, "", 50)

	const n = 50
	now := time.Now()
	var wg sync.WaitGroup
	var succeeded, nameTaken, other int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.ClaimNode(bg(), tok.ID, Node{
				SiteID: site.ID, Name: "n1", Status: NodePending,
			}, now)
			switch {
			case err == nil:
				atomic.AddInt64(&succeeded, 1)
			case errors.Is(err, ErrNodeTaken):
				atomic.AddInt64(&nameTaken, 1)
			default:
				atomic.AddInt64(&other, 1)
			}
		}()
	}
	wg.Wait()

	if other != 0 {
		t.Fatalf("有 %d 次失败原因不是 ErrNodeTaken", other)
	}
	if succeeded != 1 {
		t.Fatalf("成功次数 = %d，期望恰好 1", succeeded)
	}
	if nameTaken != n-1 {
		t.Errorf("ErrNodeTaken 次数 = %d，期望 %d", nameTaken, n-1)
	}

	got, err := r.Nodes().GetByName(bg(), site.ID, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != NodePending {
		t.Errorf("Status = %q，期望恰好一次认领把它变成 %q", got.Status, NodePending)
	}
}

func TestClaimNodeRejectsUnknownToken(t *testing.T) {
	_, r, site := newClaimFixture(t)
	_, err := r.ClaimNode(bg(), 999999, Node{SiteID: site.ID, Name: "n1"}, time.Now())
	if !errors.Is(err, ErrTokenUnavailable) {
		t.Fatalf("err = %v，期望 ErrTokenUnavailable", err)
	}
	if _, err := r.Nodes().GetByName(bg(), site.ID, "n1"); !errors.Is(err, ErrNotFound) {
		t.Error("不存在的 token 不该能认领任何节点")
	}
}

// TestClaimNodeConcurrentSameNameOnlyOneWins 验证：
// -race 下并发 50 次同 token/同名字，只有一次成功——不能出现两台机器
// 都拿到 CN 相同的有效证书。
func TestClaimNodeConcurrentSameNameOnlyOneWins(t *testing.T) {
	_, r, site := newClaimFixture(t)
	tok := makeToken(t, r, site.ID, "", 50) // token 本身不是瓶颈，名字冲突才是

	const n = 50
	now := time.Now()
	var wg sync.WaitGroup
	var succeeded, tokenUnavail, nameTaken, other int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.ClaimNode(bg(), tok.ID, Node{SiteID: site.ID, Name: "same-node"}, now)
			switch {
			case err == nil:
				atomic.AddInt64(&succeeded, 1)
			case errors.Is(err, ErrNodeTaken):
				atomic.AddInt64(&nameTaken, 1)
			case errors.Is(err, ErrTokenUnavailable):
				atomic.AddInt64(&tokenUnavail, 1)
			default:
				atomic.AddInt64(&other, 1)
			}
		}()
	}
	wg.Wait()

	if other != 0 {
		t.Fatalf("有 %d 次失败原因既不是 ErrNodeTaken 也不是 ErrTokenUnavailable", other)
	}
	if succeeded != 1 {
		t.Fatalf("成功次数 = %d，期望恰好 1", succeeded)
	}
	if nameTaken != n-1 {
		t.Errorf("ErrNodeTaken 次数 = %d，期望 %d", nameTaken, n-1)
	}

	list, err := r.Nodes().List(bg(), site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("库里的 Node 行数 = %d，期望 1——不能出现重复身份", len(list))
	}
}

// TestClaimNodeConcurrentMaxUsesStrictlyEnforced 钉住验收表的第二条：
// maxUses 在不同节点名并发下严格不超限。用不同名字避免与上一条测试
// 混在一起验同一件事——这里要单独隔离出「token 计数」这个维度。
func TestClaimNodeConcurrentMaxUsesStrictlyEnforced(t *testing.T) {
	_, r, site := newClaimFixture(t)
	const maxUses = 5
	const n = 50
	tok := makeToken(t, r, site.ID, "", maxUses)

	now := time.Now()
	var wg sync.WaitGroup
	var succeeded int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := r.ClaimNode(bg(), tok.ID, Node{
				SiteID: site.ID, Name: "node-" + strconv.Itoa(i),
			}, now)
			if err == nil {
				atomic.AddInt64(&succeeded, 1)
			} else if !errors.Is(err, ErrTokenUnavailable) {
				t.Errorf("goroutine %d: 意外的错误 %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if succeeded != maxUses {
		t.Fatalf("成功次数 = %d，期望恰好 %d（maxUses）", succeeded, maxUses)
	}

	list, err := r.Nodes().List(bg(), site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != maxUses {
		t.Fatalf("库里的 Node 行数 = %d，期望 %d", len(list), maxUses)
	}

	toks, err := r.JoinTokens().List(bg(), site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if toks[0].Used != maxUses {
		t.Fatalf("token.used = %d，期望恰好 %d，不能超限也不能少计", toks[0].Used, maxUses)
	}
}
