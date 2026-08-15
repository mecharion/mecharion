//go:build linux

package multinode

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 本文件是 **M7 第 5 步的验收**：证书轮换与吊销。
//
// 两件事都必须在真机上验，理由相同：**它们的正确性依赖 TLS 握手之后
// 发生了什么**，而那是单元测试碰不到的一层。
//
// 尤其是吊销：ADR-0034 选了应用层检查而不是 CRL，代价是**被吊销的证书
// 仍能完成 TLS 握手**。这条验收要证明的正是「握手成功 ≠ 获准」——
// 如果哪天有人把那道门挪到传输层，这里会红。

// TestRevokedNodeCannotTalk 是吊销的验收。
func TestRevokedNodeCannotTalk(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	n1, n2 := c.node(1), c.node(2)
	installOnce(ctx, t, c, n1)
	resetNode(ctx, t, c, n2)
	joinNode(ctx, t, c, n1, n2)

	// ── ① 先证明它本来连得上 ──
	//
	// 少了这一步，「吊销之后连不上」可能只是因为它从来就没连上过。
	logPath := startAgent(ctx, t, c, n2, n1+":"+grpcPort)
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return strings.Contains(readLog(ctx, c, n2, logPath), "registered with mechd")
	}) {
		t.Fatalf("[%s] 吊销之前应当连得上，日志:\n%s",
			n2, tailOf(readLog(ctx, c, n2, logPath), 30))
	}

	// **agent 一直开着。**
	//
	// 吊销要在**已经连着**的 agent 上立刻生效，而不是等它下次重连。
	// 停掉再起会让这条测试退化成「重新 Register 时被拒」——那是 M3 就
	// 有的检查，不是这一步加的每-RPC 门禁。

	// ── ② 吊销 ──
	out := c.mustRun(ctx, t, n1, "mechctl", "node", "revoke", n2, "-y",
		"--server", "https://"+n1+":"+mechdPort,
		"--token", adminToken(ctx, t, c, n1), "--ca-file", caPath)
	if !strings.Contains(out, "handshake will still succeed") {
		t.Errorf("输出应当说清应用层吊销的代价，实际:\n%s", out)
	}

	// ── ③ 它仍然在册（这正是 revoke 与 remove 的区别）──
	if got := nodeStatus(ctx, t, c, n1, n2); got == "" {
		t.Errorf("revoke 不该把节点从册子上抹掉——那是 remove 干的事")
	}

	// ── ④ 那个**还开着的** agent 立刻被切断 ──
	//
	// 判据是日志里出现拒绝，不是「进程退出」：agent 被拒会一直退避重试
	// 而不退出（那正是它该做的）。
	if !waitUntil(ctx, 90*time.Second, func() bool {
		return strings.Contains(readLog(ctx, c, n2, logPath), "revoked")
	}) {
		t.Fatalf("[%s] 吊销应当在**已连接**的 agent 上立刻生效，日志:\n%s",
			n2, tailOf(readLog(ctx, c, n2, logPath), 30))
	}
	stopAgent(ctx, c, n2)

	// ── ⑤ 恢复之后又能连了 ──
	//
	// 不验这一条，「吊销」与「彻底弄坏了」分不开。
	c.mustRun(ctx, t, n1, "mechctl", "node", "unrevoke", n2,
		"--server", "https://"+n1+":"+mechdPort,
		"--token", adminToken(ctx, t, c, n1), "--ca-file", caPath)

	logPath = startAgent(ctx, t, c, n2, n1+":"+grpcPort)
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return strings.Contains(readLog(ctx, c, n2, logPath), "registered with mechd")
	}) {
		t.Fatalf("[%s] 恢复之后应当又能连上，日志:\n%s",
			n2, tailOf(readLog(ctx, c, n2, logPath), 30))
	}
}

// TestRemovedNodeCannotTalk 钉住 remove 也当场切断**已连接**的 agent。
//
// 「重连时被 Register 拒绝」是 M3 就有的行为，不需要这一步。这一步买到的
// 是**不必等它重连**：少了它，一台被 remove 的机器会带着一张仍然有效的
// 证书继续上报，而中心已经不认识它了——那个窗口有多长取决于它什么时候
// 恰好断线一次，可能是几天。
func TestRemovedNodeCannotTalk(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	n1, n3 := c.node(1), c.node(3)
	installOnce(ctx, t, c, n1)
	resetNode(ctx, t, c, n3)
	joinNode(ctx, t, c, n1, n3)

	logPath := startAgent(ctx, t, c, n3, n1+":"+grpcPort)
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return strings.Contains(readLog(ctx, c, n3, logPath), "registered with mechd")
	}) {
		t.Fatalf("[%s] 移除之前应当连得上", n3)
	}

	// **agent 一直开着**——见上面的理由。
	forgetNode(ctx, t, c, n1, n3)

	if !waitUntil(ctx, 90*time.Second, func() bool {
		return strings.Contains(readLog(ctx, c, n3, logPath), "is not registered")
	}) {
		t.Fatalf("[%s] 移除应当在**已连接**的 agent 上立刻生效，日志:\n%s",
			n3, tailOf(readLog(ctx, c, n3, logPath), 30))
	}
}

// TestCertRotatesBeforeExpiry 是轮换的验收。
//
// 用一张**短有效期**的证书把一年的等待压成几秒：`mechd ca issue --validity`
// 是真实需求（高安全环境用几天一换的证书），不是为测试开的后门。
//
// 判据是**证书真的换了**（NotAfter 前移），且换完之后**不必重启**就能
// 继续通信——后者靠拨号时每次握手从盘上读当前证书。
func TestCertRotatesBeforeExpiry(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	n1, n2 := c.node(1), c.node(2)
	installOnce(ctx, t, c, n1)
	resetNode(ctx, t, c, n2)
	joinNode(ctx, t, c, n1, n2)

	before := certNotAfter(ctx, t, c, n2)

	// 阈值设成 400 天：那张证书（1 年）立刻落进「该续了」的窗口。
	logPath := startAgentRenewing(ctx, t, c, n2, n1+":"+grpcPort, "9600h")
	if !waitUntil(ctx, 90*time.Second, func() bool {
		return strings.Contains(readLog(ctx, c, n2, logPath), "certificate renewed")
	}) {
		t.Fatalf("[%s] 应当自动续期，日志:\n%s",
			n2, tailOf(readLog(ctx, c, n2, logPath), 30))
	}

	after := certNotAfter(ctx, t, c, n2)
	if after == before {
		t.Errorf("续期之后 NotAfter 应当往后移，实际都是 %q", before)
	}

	// **换完还得能用**：不重启 agent，继续跑到下一轮调和。
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return nodeStatus(ctx, t, c, n1, n2) == "online"
	}) {
		t.Errorf("[%s] 续期之后应当继续在线（拨号每次握手都读盘上的当前证书）", n2)
	}
}

// TestExpiredCertSaysWhatToDo 钉住「已经过期就别装作能自愈」。
//
// 过期的证书连不上 mechd，而续期本身要走 mTLS——这是个**人必须介入**的
// 状态。悄悄每轮重试一次只会把日志刷满，而它不会自己好转。
func TestExpiredCertSaysWhatToDo(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	n1, n3 := c.node(1), c.node(3)
	installOnce(ctx, t, c, n1)
	resetNode(ctx, t, c, n3)

	// 签一张 1 秒后就过期的证书，等它过期
	issueNodeCert(ctx, t, c, n1, n3, n3, "--validity", "1s")
	seedNode(ctx, t, c, n1, n3)
	time.Sleep(2 * time.Second)

	logPath := startAgentRenewing(ctx, t, c, n3, n1+":"+grpcPort, "720h")
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return strings.Contains(readLog(ctx, c, n3, logPath), "cannot auto-renew")
	}) {
		t.Fatalf("[%s] 证书过期时应当明确说出路，日志:\n%s",
			n3, tailOf(readLog(ctx, c, n3, logPath), 30))
	}
	if log := readLog(ctx, c, n3, logPath); !strings.Contains(log, "rejoin") {
		t.Errorf("[%s] 应当告诉运维下一步怎么做:\n%s", n3, tailOf(log, 20))
	}
}

// ── 夹具 ────────────────────────────────────────────────────────────────

// joinNode 用一张 token 把节点加进来（走真实的 join 路径）。
func joinNode(ctx context.Context, t *testing.T, c *cluster, mechdNode, node string) {
	t.Helper()
	tok := createToken(ctx, t, c, mechdNode, "--node", node)
	out := c.mustRun(ctx, t, node, "mechlet", "install", "--join",
		"https://"+mechdNode+":"+mechdPort,
		"--token", tok.Token, "--ca-hash", caHashOf(ctx, t, c, mechdNode),
		"--prefix", m7Prefix, "--link-dir", m7Prefix+"/bin",
		"--conf-dir", m7ConfDir, "--data-dir", m7DataDir, "--node", node)
	if !strings.Contains(out, "Install complete") {
		t.Fatalf("[%s] 加入失败:\n%s", node, out)
	}
}

// startAgentRenewing 起一个 agent，并把续期阈值调到能立刻触发。
func startAgentRenewing(
	ctx context.Context, t *testing.T, c *cluster, node, upstream, renewBefore string,
) string {
	t.Helper()
	log := "/var/tmp/agent-" + node + ".log"
	stopAgent(ctx, c, node)
	script := "rm -f " + log + "; nohup mechlet agent" +
		" --upstream " + upstream +
		" --data-dir " + m7DataDir +
		" --conf-dir " + m7ConfDir +
		" --node " + node +
		" --reconcile-interval 3s" +
		" --cert-renew-before " + renewBefore +
		" >" + log + " 2>&1 & echo started"
	if out, err := c.sh(ctx, node, script); err != nil {
		t.Fatalf("[%s] 起 agent: %v\n%s", node, err, out)
	}
	t.Cleanup(func() { stopAgent(context.Background(), c, node) })
	return log
}

// certNotAfter 读本机证书的到期时间。
func certNotAfter(ctx context.Context, t *testing.T, c *cluster, node string) string {
	t.Helper()
	// 贫瘠镜像里没有 openssl，用 mechlet 自己不行（没有这条命令）——
	// 退而求其次：证书文件本身变了就说明换过了。
	body := mustRead(ctx, t, c, node, m7ConfDir+"/pki/node.crt")
	return body
}
