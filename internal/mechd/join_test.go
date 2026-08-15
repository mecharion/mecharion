package mechd

import (
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mecharion/mecharion/internal/pki"
	"github.com/mecharion/mecharion/internal/store"
)

// newJoinFixture 起一套用于 Join 测试的控制面，附带一个可签发证书的
// PKI 目录。
//
// **先手动建好 CA，不指望 SignNodeCSR 顺手建。** 真实运行时 mechd 启动
// 阶段就为自己的服务端证书建过 CA 了（08-security §3.2），pki 目录
// 早已存在；SignNodeCSR 本身不建目录（与 IssueNode 不同，那是刻意的
// 不对称，这里不去动它），所以测试要自己复现「CA 已经在」这个前提，
// 否则会撞上一个目录不存在错误。
func newJoinFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	confDir := t.TempDir()
	if err := os.MkdirAll(pki.Dir(confDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pki.EnsureCA(pki.Dir(confDir)); err != nil {
		t.Fatal(err)
	}
	return f, confDir
}

func csrFor(t *testing.T, name string) []byte {
	t.Helper()
	csr, _, err := pki.NewNodeCSR(name)
	if err != nil {
		t.Fatal(err)
	}
	return csr
}

func TestJoinHappyPath(t *testing.T) {
	f, confDir := newJoinFixture(t)

	tok, err := f.svc.CreateJoinToken(ctx(), f.site.Name, "", time.Hour, 1, confDir, "admin")
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.svc.Join(ctx(), JoinRequest{
		Token: tok.Token, Node: "n1", CSR: csrFor(t, "n1"),
		Address: "10.0.0.1", ConfDir: confDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Node != "n1" || len(res.Cert) == 0 || len(res.CA) == 0 {
		t.Errorf("结果不完整: %+v", res)
	}

	n, err := f.svc.Repos.Nodes().GetByName(ctx(), f.site.ID, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if n.Status != store.NodePending {
		t.Errorf("新加入的节点状态 = %q，期望 %q", n.Status, store.NodePending)
	}
}

func TestJoinRejectsAlreadyRegisteredName(t *testing.T) {
	f, confDir := newJoinFixture(t)
	// 已经拿到过证书的行（比如之前真的 Join 成功过一次）——reserved
	// 节点被放行后，这条守卫必须继续拒绝已发过证书的记录。
	f.addNode("n1", "10.0.0.1")

	tok, err := f.svc.CreateJoinToken(ctx(), f.site.Name, "", time.Hour, 1, confDir, "admin")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Join(ctx(), JoinRequest{
		Token: tok.Token, Node: "n1", CSR: csrFor(t, "n1"), ConfDir: confDir,
	})
	if err == nil {
		t.Fatal("已在册且已发过证书的名字不该能被 Join 顶替")
	}
}

// TestJoinClaimsPreRegisteredNode 验证：`mechctl node add` 预登记的名字
// （reserved，从未发过证书）必须能被后续的 Join 认领——这正是
// 22-multi-node §2.1 给 cloud-init 批量预置留的口子，此前被 §6.15 那道
// 一刀切的守卫挡住了。
func TestJoinClaimsPreRegisteredNode(t *testing.T) {
	f, confDir := newJoinFixture(t)
	if _, err := f.svc.AddNode(ctx(), f.site.Name, "n1", "", "admin"); err != nil {
		t.Fatal(err)
	}
	pre, err := f.svc.Repos.Nodes().GetByName(ctx(), f.site.ID, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if pre.Status != store.NodeReserved {
		t.Fatalf("node add 之后的状态 = %q，期望 %q", pre.Status, store.NodeReserved)
	}

	tok, err := f.svc.CreateJoinToken(ctx(), f.site.Name, "", time.Hour, 1, confDir, "admin")
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.Join(ctx(), JoinRequest{
		Token: tok.Token, Node: "n1", CSR: csrFor(t, "n1"),
		Address: "10.0.0.9", ConfDir: confDir,
	})
	if err != nil {
		t.Fatalf("预登记的名字应当能被 Join 认领: %v", err)
	}
	if res.Node != "n1" || len(res.Cert) == 0 {
		t.Errorf("结果不完整: %+v", res)
	}

	got, err := f.svc.Repos.Nodes().GetByName(ctx(), f.site.ID, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != pre.ID {
		t.Errorf("认领应当原地改行，ID = %d，期望仍是 %d", got.ID, pre.ID)
	}
	if got.Status != store.NodePending {
		t.Errorf("认领后的状态 = %q，期望 %q", got.Status, store.NodePending)
	}
	if got.Address != "10.0.0.9" {
		t.Errorf("Address = %q，期望换成 Join 提交的地址", got.Address)
	}
}

// TestJoinRejectsRevokedPreRegisteredNode 钉住 §6.16 明确排除的那种
// reserved：预登记之后又被吊销的名字，不能靠 Join 绕过吊销重新拿到证书。
func TestJoinRejectsRevokedPreRegisteredNode(t *testing.T) {
	f, confDir := newJoinFixture(t)
	if _, err := f.svc.AddNode(ctx(), f.site.Name, "n1", "", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetNodeRevoked(ctx(), f.site.Name, "n1", true, "admin"); err != nil {
		t.Fatal(err)
	}

	tok, err := f.svc.CreateJoinToken(ctx(), f.site.Name, "", time.Hour, 1, confDir, "admin")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Join(ctx(), JoinRequest{
		Token: tok.Token, Node: "n1", CSR: csrFor(t, "n1"), ConfDir: confDir,
	})
	if err == nil {
		t.Fatal("被吊销的预登记节点不该能被 Join 重新认领")
	}
}

func TestJoinBoundTokenRejectsMismatchedName(t *testing.T) {
	f, confDir := newJoinFixture(t)
	tok, err := f.svc.CreateJoinToken(ctx(), f.site.Name, "n1", time.Hour, 1, confDir, "admin")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Join(ctx(), JoinRequest{
		Token: tok.Token, Node: "n2", CSR: csrFor(t, "n2"), ConfDir: confDir,
	})
	if err == nil {
		t.Fatal("绑名 token 不该允许加入另一个名字")
	}
}

func TestJoinBoundTokenFillsInNameWhenOmitted(t *testing.T) {
	f, confDir := newJoinFixture(t)
	tok, err := f.svc.CreateJoinToken(ctx(), f.site.Name, "n1", time.Hour, 1, confDir, "admin")
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.Join(ctx(), JoinRequest{
		Token: tok.Token, CSR: csrFor(t, "n1"), ConfDir: confDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Node != "n1" {
		t.Errorf("Node = %q，期望绑定的 n1", res.Node)
	}
}

func TestJoinUnboundTokenRequiresExplicitName(t *testing.T) {
	f, confDir := newJoinFixture(t)
	tok, err := f.svc.CreateJoinToken(ctx(), f.site.Name, "", time.Hour, 1, confDir, "admin")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Join(ctx(), JoinRequest{Token: tok.Token, CSR: csrFor(t, "n1"), ConfDir: confDir})
	if err == nil {
		t.Fatal("不绑名的 token 必须要求调用方自己指定 --node")
	}
}

func TestJoinRejectsBadToken(t *testing.T) {
	f, confDir := newJoinFixture(t)
	_, err := f.svc.Join(ctx(), JoinRequest{
		Token: "m7n_join_不存在", Node: "n1", CSR: csrFor(t, "n1"), ConfDir: confDir,
	})
	if err == nil {
		t.Fatal("不存在的 token 应当被拒绝")
	}
}

// TestJoinConcurrentSameTokenSameNode 验证：-race 下并发 50 次同
// token/同 node 只有一次成功，且从来不会出现「两台机器都拿到有效证书」
// 的情形——失败的那 49 次要么在 Join 内部就没拿到证书，要么根本没走到
// 签发那一步。
func TestJoinConcurrentSameTokenSameNode(t *testing.T) {
	f, confDir := newJoinFixture(t)
	tok, err := f.svc.CreateJoinToken(ctx(), f.site.Name, "", time.Hour, 50, confDir, "admin")
	if err != nil {
		t.Fatal(err)
	}

	const n = 50
	var wg sync.WaitGroup
	var succeeded int64
	certs := make([][]byte, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := f.svc.Join(ctx(), JoinRequest{
				Token: tok.Token, Node: "same-node", CSR: csrFor(t, "same-node"),
				Address: "10.0.0." + strconv.Itoa(i), ConfDir: confDir,
			})
			if err == nil {
				atomic.AddInt64(&succeeded, 1)
				certs[i] = res.Cert
			}
		}(i)
	}
	wg.Wait()

	if succeeded != 1 {
		t.Fatalf("成功次数 = %d，期望恰好 1", succeeded)
	}

	list, err := f.svc.Repos.Nodes().List(ctx(), f.site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("库里 Node 行数 = %d，期望 1", len(list))
	}

	// 拿到证书的调用方只有一个——不存在「两份不同的 cert 都被调用方收到」
	got := 0
	for _, c := range certs {
		if len(c) > 0 {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("收到证书的调用方数 = %d，期望恰好 1", got)
	}
}

// TestJoinConcurrentMaxUsesStrictlyEnforced 钉住验收表的第二条：
// maxUses 在不同节点并发下严格不超限。
func TestJoinConcurrentMaxUsesStrictlyEnforced(t *testing.T) {
	f, confDir := newJoinFixture(t)
	const maxUses = 5
	const n = 30
	tok, err := f.svc.CreateJoinToken(ctx(), f.site.Name, "", time.Hour, maxUses, confDir, "admin")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var succeeded int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "node-" + strconv.Itoa(i)
			_, err := f.svc.Join(ctx(), JoinRequest{
				Token: tok.Token, Node: name, CSR: csrFor(t, name), ConfDir: confDir,
			})
			if err == nil {
				atomic.AddInt64(&succeeded, 1)
			}
		}(i)
	}
	wg.Wait()

	if succeeded != maxUses {
		t.Fatalf("成功次数 = %d，期望恰好 %d", succeeded, maxUses)
	}
	list, err := f.svc.Repos.Nodes().List(ctx(), f.site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != maxUses {
		t.Fatalf("库里 Node 行数 = %d，期望 %d", len(list), maxUses)
	}
}
