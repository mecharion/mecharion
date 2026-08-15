//go:build linux

package multinode

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 本文件是 **M7 第 2 步的验收**：gRPC TCP + mTLS 监听，证书 CN 作为身份。
//
// 判据是**带证书连得上、不带连不上**，且拿别人的证书冒名会被拒。
// 三条都要真的跨机跑：`nodeOf` 的单元测试证明的是判据本身，
// 而这里证明**那条判据真的接在了传输上**——一个忘了配
// RequireAndVerifyClientCert 的服务端，单元测试全绿。

const grpcPort = "8444"

// TestRemoteNodeJoinsOverMTLS 是第 2 步的验收。
func TestRemoteNodeJoinsOverMTLS(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	n1, n2 := c.node(1), c.node(2)
	installOnce(ctx, t, c, n1)

	// ── ① 没有证书：连不上 ──
	//
	// 先验这一条。反过来的话，一次「其实根本没启用 mTLS」的实现会先让
	// 后面那条通过，而这条永远没人去看。
	//
	// **resetNode 不能省**：同一个集群跨测试复用，别的测试可能已经把 n2
	// 加进来了。少了它，这条断言测的是「一台已经有证书的机器」——
	// 那正好是它要排除的情况。（这一条是被真实的测试间干扰抓出来的。）
	resetNode(ctx, t, c, n2)

	// 用 timeout 包一层**只是防挂**，不作为判据：agent 有证书时会一直跑，
	// 而这条断言要的是那句「读不到证书」。判据是输出内容。
	out, _ := c.run(ctx, n2, "timeout", "15", "mechlet", "agent",
		"--upstream", n1+":"+grpcPort,
		"--data-dir", m7DataDir,
		"--conf-dir", m7ConfDir,
		"--node", n2)
	if !strings.Contains(out, "certificate") {
		t.Fatalf("[%s] 没有证书时应当明确报「读不到证书」，实际:\n%s", n2, out)
	}

	// ── ② 签一张给 n2，放到它的 pki 目录 ──
	issueNodeCert(ctx, t, c, n1, n2, n2)

	// ── ③ 带证书：连得上，且 mechd 认得出它是谁 ──
	//
	// 判据不是「进程没退出」——那太弱了。要的是 **mechd 侧真的多出一个
	// 已注册的节点**，而它的名字来自证书。
	seedNode(ctx, t, c, n1, n2)
	logPath := startAgent(ctx, t, c, n2, n1+":"+grpcPort)

	if !waitUntil(ctx, 60*time.Second, func() bool {
		return strings.Contains(readLog(ctx, c, n2, logPath), "registered with mechd")
	}) {
		t.Fatalf("[%s] 带证书应当能注册上去，日志:\n%s",
			n2, tailOf(readLog(ctx, c, n2, logPath), 30))
	}
	if got := nodeStatus(ctx, t, c, n1, n2); got != "online" {
		t.Errorf("mechd 侧 %s 的状态应为 online，实际 %q", n2, got)
	}
}

// TestCertificateIdentityCannotBeSpoofed 钉住 ADR-0034 的核心：
// **拿 A 的证书自称是 B，必须被拒**。
//
// 这是 mTLS 真正买到的那件事。少了它，一张任意合法证书就能冒充任何节点，
// 而「谁上报了这条状态」将不再可信——那会让整个多节点的状态视图失去意义。
func TestCertificateIdentityCannotBeSpoofed(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	n1, n3 := c.node(1), c.node(3)
	installOnce(ctx, t, c, n1)

	// n3 拿到的是**自己名字**的证书
	issueNodeCert(ctx, t, c, n1, n3, n3)
	seedNode(ctx, t, c, n1, n3)

	// 却自称是 n1
	//
	// **判据是日志内容，不是退出码。** agent 连不上时会一直重试而不是退出，
	// 因此用 `timeout` 包一层的话退出码恒为非零——那样这条断言在
	// 「冒名成功了」时也会通过，什么都没验证。（这一条是被变异测试
	// 抓出来的：去掉 nodeOf 里的比对之后，原来的写法照样绿。）
	logPath := startAgentAs(ctx, t, c, n3, n1+":"+grpcPort, n1)

	if !waitUntil(ctx, 30*time.Second, func() bool {
		return strings.Contains(readLog(ctx, c, n3, logPath), "证书身份是")
	}) {
		t.Fatalf("[%s] 冒名 %s 应当被拒，日志:\n%s",
			n3, n1, tailOf(readLog(ctx, c, n3, logPath), 30))
	}
	// **绝不能注册成功**
	if log := readLog(ctx, c, n3, logPath); strings.Contains(log, "registered with mechd") {
		t.Fatalf("[%s] 冒名 %s 竟然注册成功了:\n%s", n3, n1, tailOf(log, 30))
	}
	// 错误信息要指名两个名字，否则现场只能看到一句「注册失败」
	log := readLog(ctx, c, n3, logPath)
	for _, want := range []string{n1, n3} {
		if !strings.Contains(log, want) {
			t.Errorf("错误信息里应当同时有 %q，实际:\n%s", want, tailOf(log, 20))
		}
	}
}

// ── 夹具 ────────────────────────────────────────────────────────────────

// issueNodeCert 在 mechd 那台机器上签一张证书，搬到目标机器的 pki 目录。
//
// 走的是真实的 `mechd ca issue`，不是测试自己拿 CA 私钥签一张——
// 那样会绕开生产的签发逻辑，而签发正是这一步要验的一半。
// TestAnonymousClientIsRejectedAtHandshake 是 §5 第 5 行的验收。
//
// **原来那条是假的**：它测的是「agent 没有证书时自己拒绝启动」，判据是
// 那句「读不到证书」——客户端压根没走到握手。一个把
// `RequireAndVerifyClientCert` 改成 `VerifyClientCertIfGiven` 的 mechd
// 照样能让它通过，而那是一次彻底的 mTLS 失效。
//
// 这条去撞真的服务端：不带客户端证书连 8444，必须被拒。
func TestAnonymousClientIsRejectedAtHandshake(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	n1, n2 := c.node(1), c.node(2)
	installOnce(ctx, t, c, n1)

	// 从**另一台机器**上发起，走的是真实的跨机路径
	out, err := c.run(ctx, n2, binDir+"/tlsprobe", n1+":"+grpcPort)
	if err == nil {
		t.Fatalf("[%s] 不带客户端证书竟然连上了 %s —— mTLS 没有真的要求证书:\n%s",
			n2, n1, out)
	}

	// **判据必须是 TLS 告警，不能是「反正失败了」。**
	//
	// 这一条是被变异测试抓出来的：把 `RequireAndVerifyClientCert` 降级成
	// `VerifyClientCertIfGiven`（一次彻底的 mTLS 失效）之后，这条测试
	// **照样通过**——因为探针发的是 HTTP/1.0，而 gRPC 认不出那个前导也会
	// 关连接。两种「被拒」在退出码上长得一模一样。
	//
	// 真正能分开它们的是错误的**来源**：TLS 层拒绝会回一个告警
	// （`remote error: tls: certificate required`），应用层关连接只会给
	// EOF。
	if !strings.Contains(out, "tls:") || !strings.Contains(out, "certificate") {
		t.Fatalf("[%s] 应当在 **TLS 层**被拒（certificate required），"+
			"而不是握手之后被应用层关掉——后者说明 mTLS 其实没在要求证书:\n%s",
			n2, out)
	}

	// **对照组**：同一个探针撞 HTTPS 那个口要能连上。
	//
	// 少了它，一个「探针本身就是坏的」（拼错主机名、端口不通）会让上面
	// 那条永远绿——而它什么都没验。
	if out, err := c.run(ctx, n2, binDir+"/tlsprobe", n1+":"+mechdPort); err != nil {
		t.Errorf("[%s] HTTPS 端口不要求客户端证书，探针应当连得上；"+
			"连不上说明探针或网络本身有问题: %v\n%s", n2, err, out)
	}
}

func issueNodeCert(
	ctx context.Context, t *testing.T, c *cluster, ca, target, node string,
	extra ...string,
) {
	t.Helper()
	stage := "/tmp/issued-" + node
	args := append([]string{"mechd", "ca", "issue",
		"--conf-dir", m7ConfDir, "--node", node, "--out-dir", stage}, extra...)
	c.mustRun(ctx, t, ca, args...)

	dst := m7ConfDir + "/pki"
	c.mustRun(ctx, t, target, "mkdir", "-p", dst)
	for _, f := range []string{"node.crt", "node.key", "ca.crt"} {
		body := mustRead(ctx, t, c, ca, stage+"/"+f)
		writeFile(ctx, t, c, target, dst+"/"+f, body)
	}
	c.mustRun(ctx, t, target, "chmod", "600", dst+"/node.key")
}

// seedNode 在 mechd 里登记一个节点。
//
// M7 第 3 步之前还没有 join，而 Register **拒绝不在册的节点**——
// 那条拒绝是对的（backend.go），只是要到第 3 步才有自动入册的路。
// 这里用 mechd 自己的 API 补上，不去改生产代码迁就测试。
func seedNode(ctx context.Context, t *testing.T, c *cluster, mechdNode, node string) {
	t.Helper()
	token := strings.TrimSpace(mustRead(ctx, t, c, mechdNode, tokenPath))
	out, err := c.run(ctx, mechdNode, "mechctl", "node", "add", node,
		"--server", "https://"+mechdNode+":"+mechdPort,
		"--token", token, "--ca-file", caPath)
	if err != nil && !strings.Contains(out, "already exists") {
		t.Fatalf("登记节点 %s: %v\n%s", node, err, out)
	}
}

// nodeStatus 问 mechd 某个节点现在什么状态。
func nodeStatus(ctx context.Context, t *testing.T, c *cluster, mechdNode, node string) string {
	t.Helper()
	token := strings.TrimSpace(mustRead(ctx, t, c, mechdNode, tokenPath))
	out, err := c.run(ctx, mechdNode, "mechctl", "node", "list",
		"--server", "https://"+mechdNode+":"+mechdPort,
		"--token", token, "--ca-file", caPath)
	if err != nil {
		t.Fatalf("列节点: %v\n%s", err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), node) {
			continue
		}
		// 认全部三个值。只挑 online 出来、其余原样返回整行，会让
		// pending 与 offline 的失败信息变成一坨没法读的表格行——而它们
		// 恰恰是需要分辨的那两个（22-multi-node §6.13）。
		for _, st := range []string{"online", "offline", "pending"} {
			if strings.Contains(line, st) {
				return st
			}
		}
		return strings.TrimSpace(line)
	}
	return ""
}

// startAgent 在后台起一个 mechlet agent，日志落到文件。
func startAgent(ctx context.Context, t *testing.T, c *cluster, node, upstream string) string {
	return startAgentAs(ctx, t, c, node, upstream, node)
}

// startAgentAs 同上，但让它自称是另一个名字——冒名测试用。
func startAgentAs(
	ctx context.Context, t *testing.T, c *cluster, node, upstream, claim string,
) string {
	t.Helper()
	log := "/var/tmp/agent-" + node + ".log"
	stopAgent(ctx, c, node)
	script := "rm -f " + log + "; nohup mechlet agent" +
		" --upstream " + upstream +
		" --data-dir /var/lib/mecharion-m7" +
		" --conf-dir " + m7ConfDir +
		" --node " + claim +
		" --reconcile-interval 3s >" + log + " 2>&1 & echo started"
	if out, err := c.sh(ctx, node, script); err != nil {
		t.Fatalf("[%s] 起 agent: %v\n%s", node, err, out)
	}
	t.Cleanup(func() { stopAgent(context.Background(), c, node) })
	return log
}

func stopAgent(ctx context.Context, c *cluster, node string) {
	// pkill 不在贫瘠镜像里，用 /proc 找
	_, _ = c.sh(ctx, node,
		`for p in /proc/[0-9]*; do
		   case "$(cat $p/comm 2>/dev/null)" in mechlet) kill "${p#/proc/}" 2>/dev/null;; esac
		 done; true`)
}

func readLog(ctx context.Context, c *cluster, node, path string) string {
	out, _ := c.readFile(ctx, node, path)
	return out
}
