//go:build linux

package multinode

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 本文件是 **M7 第 3 步的验收**：一台新机器用 token 加进来。
//
// 与第 2 步的区别是**没有人搬证书**：那一步为了先验传输，用
// `mechd ca issue` 手工签了一张再拷过去。这一步走真实路径——
// 节点自己生成密钥、发 CSR、拿回证书，全程私钥不出那台机器。

// TestNodeJoinsWithToken 是第 3 步的验收。
func TestNodeJoinsWithToken(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	n1, n2 := c.node(1), c.node(2)
	installOnce(ctx, t, c, n1)
	resetNode(ctx, t, c, n2)

	// ── ① 建一张绑定 n2 的 token ──
	tok := createToken(ctx, t, c, n1, "--node", n2)
	if tok.Token == "" {
		t.Fatal("创建 token 应当返回明文（且只这一次）")
	}
	if tok.JoinCommand == "" || !strings.Contains(tok.JoinCommand, "--ca-hash") {
		t.Errorf("应当打印可粘贴的加入命令并带上 CA 指纹，实际:\n%s", tok.JoinCommand)
	}
	caHash := caHashOf(ctx, t, c, n1)

	// 清掉可能存在的离线签发产物。
	//
	// `mechd ca issue`（第 2 步用的那条离线路径）**确实**会在 mechd 上
	// 生成私钥——那正是它被定为「次选」的原因。不先清掉的话，下面那条
	// 断言测的是「有没有人用过离线路径」，而不是 join 干了什么。
	keyOnMechd := m7ConfDir + "/pki/nodes/" + n2 + ".key"
	if _, err := c.sh(ctx, n1, "rm -f "+keyOnMechd); err != nil {
		t.Fatalf("清理旧的离线签发产物: %v", err)
	}

	// ── ② 在 n2 上加入 ──
	out := c.mustRun(ctx, t, n2, "mechlet", "install", "--join",
		"https://"+n1+":"+mechdPort,
		"--token", tok.Token, "--ca-hash", caHash,
		"--prefix", m7Prefix, "--link-dir", m7Prefix+"/bin",
		"--conf-dir", m7ConfDir, "--data-dir", m7DataDir, "--node", n2)
	if !strings.Contains(out, "Install complete") {
		t.Fatalf("[%s] 加入失败:\n%s", n2, out)
	}

	// **私钥必须在本机生成**：join 这条路不该在 mechd 上留下它。
	if _, err := c.readFile(ctx, n1, keyOnMechd); err == nil {
		t.Errorf("mechd 上不该存在 %s 的私钥——join 走的是 CSR，私钥不过网", n2)
	}
	for _, f := range []string{"node.crt", "node.key", "ca.crt"} {
		if _, err := c.readFile(ctx, n2, m7ConfDir+"/pki/"+f); err != nil {
			t.Errorf("[%s] 加入之后应当有 %s: %v", n2, f, err)
		}
	}

	// ── ③ 节点已在册，且能用那张证书连上来 ──
	if got := nodeStatus(ctx, t, c, n1, n2); got == "" {
		t.Fatalf("加入之后 %s 应当出现在节点列表里", n2)
	}
	logPath := startAgent(ctx, t, c, n2, n1+":"+grpcPort)
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return strings.Contains(readLog(ctx, c, n2, logPath), "registered with mechd")
	}) {
		t.Fatalf("[%s] 加入之后应当连得上，日志:\n%s",
			n2, tailOf(readLog(ctx, c, n2, logPath), 30))
	}
	if got := nodeStatus(ctx, t, c, n1, n2); got != "online" {
		t.Errorf("%s 的状态应为 online，实际 %q", n2, got)
	}

	// ── ④ 同一张 token 不能再用一次 ──
	//
	// 默认 uses=1。用尽之后**必须给出「用尽」而不是「无效」**——
	// 那是运维现场第一个要分清的事。
	tokens := listTokens(ctx, t, c, n1)
	var found bool
	for _, x := range tokens {
		if x.ID != tok.ID {
			continue
		}
		found = true
		if x.Used != 1 {
			t.Errorf("token 用量应为 1，实际 %d", x.Used)
		}
		if x.Usable {
			t.Error("单次 token 用过之后不该还可用")
		}
		if !strings.Contains(x.Reason, "exhausted") {
			t.Errorf("失效原因应当说清是「用尽」，实际 %q", x.Reason)
		}
	}
	if !found {
		t.Error("列表里应当有这张 token")
	}
}

// TestJoinRejectsBadTokens 逐条核对那道门。
//
// **这是整步的意义所在**：join 是唯一不需要既有身份的入口，它的每一条
// 校验就是那道门本身。少一条，这条路就成了「谁都能加进来」。
func TestJoinRejectsBadTokens(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	n1, n3 := c.node(1), c.node(3)
	installOnce(ctx, t, c, n1)
	caHash := caHashOf(ctx, t, c, n1)

	join := func(token, node, hash string) string {
		resetNode(ctx, t, c, n3)
		out, _ := c.run(ctx, n3, "mechlet", "install", "--join",
			"https://"+n1+":"+mechdPort,
			"--token", token, "--ca-hash", hash,
			"--prefix", m7Prefix, "--link-dir", m7Prefix+"/bin",
			"--conf-dir", m7ConfDir, "--data-dir", m7DataDir, "--node", node)
		return out
	}

	t.Run("伪造的 token", func(t *testing.T) {
		out := join("m7n_join_deadbeef", n3, caHash)
		if !strings.Contains(out, "invalid token") {
			t.Errorf("应当拒绝，实际:\n%s", out)
		}
	})

	t.Run("绑名的 token 换别的名字", func(t *testing.T) {
		tok := createToken(ctx, t, c, n1, "--node", "some-other-node")
		out := join(tok.Token, n3, caHash)
		if !strings.Contains(out, "bound to node name") {
			t.Errorf("应当拒绝并说清绑的是谁，实际:\n%s", out)
		}
	})

	t.Run("已吊销的 token", func(t *testing.T) {
		tok := createToken(ctx, t, c, n1, "--node", n3)
		revokeToken(ctx, t, c, n1, tok.ID)
		out := join(tok.Token, n3, caHash)
		if !strings.Contains(out, "revoked") {
			t.Errorf("应当说清是被吊销了，实际:\n%s", out)
		}
	})

	t.Run("已过期的 token", func(t *testing.T) {
		tok := createToken(ctx, t, c, n1, "--node", n3, "--ttl", "1s")
		time.Sleep(2 * time.Second)
		out := join(tok.Token, n3, caHash)
		if !strings.Contains(out, "expired") {
			t.Errorf("应当说清是过期了，实际:\n%s", out)
		}
	})

	// §5 第 2 行里的第三种：**用尽**。
	//
	// 前面两条（吊销、过期）都是「这张票作废了」，而用尽是「票还有效，
	// 只是配额没了」——它是批量预置那条路的全部安全边界（不绑名的
	// token 允许自报名字，靠 TTL + 次数上限 + 可吊销兜住，§2.1）。
	// 少了这条验收，`--uses` 就只是个显示字段。
	t.Run("次数用尽的 token", func(t *testing.T) {
		tok := createToken(ctx, t, c, n1, "--uses", "1")

		// 第一次：用掉那唯一一次
		resetNode(ctx, t, c, n3)
		if out := join(tok.Token, n3, caHash); strings.Contains(out, "拒绝") {
			t.Fatalf("第一次使用应当成功，实际:\n%s", out)
		}
		// 第二次：配额没了
		resetNode(ctx, t, c, n3)
		out := join(tok.Token, n3, caHash)
		if !strings.Contains(out, "exhausted") {
			t.Errorf("应当说清是次数用尽了，实际:\n%s", out)
		}
	})

	t.Run("CA 指纹对不上", func(t *testing.T) {
		tok := createToken(ctx, t, c, n1, "--node", n3)
		out := join(tok.Token, n3, "sha256:"+strings.Repeat("00", 32))
		if !strings.Contains(out, "doesn't match") {
			t.Errorf("指纹不符应当被拒——那是这一步唯一的信任锚，实际:\n%s", out)
		}
	})

	t.Run("不给指纹直接拒绝", func(t *testing.T) {
		tok := createToken(ctx, t, c, n1, "--node", n3)
		out := join(tok.Token, n3, "")
		if !strings.Contains(out, "ca-hash") {
			t.Errorf("缺指纹时应当拒绝而不是退化成 TOFU，实际:\n%s", out)
		}
	})
}

// TestBatchPreRegisteredNodesJoinWithUnboundToken 验证的是：
// 「预置批量节点 E2E 成功」（06-defect-register-and-roadmap.md §6）。
//
// 场景是 22-multi-node §2.1 给 cloud-init 留的口子：运维先用 `node add`
// 把机器名字批量登记进册子，再发一张**不绑名**、次数够用的 token 分发
// 给这批机器，每台开机时用 `--node <自己的名字>` 认领属于自己的那一行。
// 这条路在 §6.15 修复之前完全走不通——`node add` 之后任何 Join
// 都会被「已在册」拒绝，不管是不是同一张预登记的空位。
func TestBatchPreRegisteredNodesJoinWithUnboundToken(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	n1, n2, n3 := c.node(1), c.node(2), c.node(3)
	installOnce(ctx, t, c, n1)
	resetNode(ctx, t, c, n2)
	resetNode(ctx, t, c, n3)

	// ① 批量预登记：两台机器的名字先落进册子，此刻都还没有证书。
	seedNode(ctx, t, c, n1, n2)
	seedNode(ctx, t, c, n1, n3)
	if got := nodeStatus(ctx, t, c, n1, n2); got != "pending" {
		t.Fatalf("预登记之后 %s 的显示状态 = %q，期望 pending（reserved 与已发证书未连接对外显示相同）", n2, got)
	}

	// ② 一张不绑名、够两次用的 token——批量场景的样子。
	tok := createToken(ctx, t, c, n1, "--uses", "2")
	caHash := caHashOf(ctx, t, c, n1)

	// ③ 两台机器各自用自己的名字认领预登记的位置。
	for _, n := range []string{n2, n3} {
		out := c.mustRun(ctx, t, n, "mechlet", "install", "--join",
			"https://"+n1+":"+mechdPort,
			"--token", tok.Token, "--ca-hash", caHash,
			"--prefix", m7Prefix, "--link-dir", m7Prefix+"/bin",
			"--conf-dir", m7ConfDir, "--data-dir", m7DataDir, "--node", n)
		if !strings.Contains(out, "Install complete") {
			t.Fatalf("[%s] 认领预登记的名字应当成功，实际:\n%s", n, out)
		}
	}

	tokens := listTokens(ctx, t, c, n1)
	for _, x := range tokens {
		if x.ID != tok.ID {
			continue
		}
		if x.Used != 2 {
			t.Errorf("token 用量应为 2（两次认领各消耗一次），实际 %d", x.Used)
		}
	}

	// ④ 两台都能用刚认领到的证书真的连上来。
	for _, n := range []string{n2, n3} {
		logPath := startAgent(ctx, t, c, n, n1+":"+grpcPort)
		if !waitUntil(ctx, 60*time.Second, func() bool {
			return strings.Contains(readLog(ctx, c, n, logPath), "registered with mechd")
		}) {
			t.Fatalf("[%s] 认领之后应当连得上，日志:\n%s",
				n, tailOf(readLog(ctx, c, n, logPath), 30))
		}
		if got := nodeStatus(ctx, t, c, n1, n); got != "online" {
			t.Errorf("%s 的状态应为 online，实际 %q", n, got)
		}
	}
}

// TestJoinRejectsRevokedPreRegisteredName 钉住 §6.16 明确排除的那种
// reserved：预登记之后被吊销的名字，不能靠 Join 绕过吊销重新拿到证书。
func TestJoinRejectsRevokedPreRegisteredName(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	n1, n3 := c.node(1), c.node(3)
	installOnce(ctx, t, c, n1)
	resetNode(ctx, t, c, n3)

	seedNode(ctx, t, c, n1, n3)
	c.mustRun(ctx, t, n1, "mechctl", "node", "revoke", n3, "-y",
		"--server", "https://"+n1+":"+mechdPort,
		"--token", adminToken(ctx, t, c, n1), "--ca-file", caPath)

	tok := createToken(ctx, t, c, n1, "--node", n3)
	caHash := caHashOf(ctx, t, c, n1)
	out, _ := c.run(ctx, n3, "mechlet", "install", "--join",
		"https://"+n1+":"+mechdPort,
		"--token", tok.Token, "--ca-hash", caHash,
		"--prefix", m7Prefix, "--link-dir", m7Prefix+"/bin",
		"--conf-dir", m7ConfDir, "--data-dir", m7DataDir, "--node", n3)
	if !strings.Contains(out, "already registered") {
		t.Errorf("被吊销的预登记名字应当被拒绝，实际:\n%s", out)
	}
}

// ── 夹具 ────────────────────────────────────────────────────────────────

type tokenView struct {
	ID          int64  `json:"id"`
	Token       string `json:"token"`
	Node        string `json:"node"`
	MaxUses     int    `json:"maxUses"`
	Used        int    `json:"used"`
	Usable      bool   `json:"usable"`
	Reason      string `json:"reason"`
	JoinCommand string `json:"joinCommand"`
}

func createToken(
	ctx context.Context, t *testing.T, c *cluster, mechdNode string, args ...string,
) tokenView {
	t.Helper()
	full := append([]string{"mechctl", "node", "token", "create"}, args...)
	full = append(full, "-o", "json", "--server", "https://"+mechdNode+":"+mechdPort,
		"--token", adminToken(ctx, t, c, mechdNode), "--ca-file", caPath)
	out := c.mustRun(ctx, t, mechdNode, full...)
	var v tokenView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("解析 token: %v\n%s", err, out)
	}
	return v
}

func listTokens(ctx context.Context, t *testing.T, c *cluster, mechdNode string) []tokenView {
	t.Helper()
	out := c.mustRun(ctx, t, mechdNode, "mechctl", "node", "token", "list",
		"-o", "json", "--server", "https://"+mechdNode+":"+mechdPort,
		"--token", adminToken(ctx, t, c, mechdNode), "--ca-file", caPath)
	var v struct {
		Tokens []tokenView `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("解析 token 列表: %v\n%s", err, out)
	}
	return v.Tokens
}

func revokeToken(ctx context.Context, t *testing.T, c *cluster, mechdNode string, id int64) {
	t.Helper()
	c.mustRun(ctx, t, mechdNode, "mechctl", "node", "token", "revoke",
		itoa(id), "--server", "https://"+mechdNode+":"+mechdPort,
		"--token", adminToken(ctx, t, c, mechdNode), "--ca-file", caPath)
}

func adminToken(ctx context.Context, t *testing.T, c *cluster, node string) string {
	t.Helper()
	return strings.TrimSpace(mustRead(ctx, t, c, node, tokenPath))
}

func caHashOf(ctx context.Context, t *testing.T, c *cluster, node string) string {
	t.Helper()
	// 从那条可粘贴的加入命令里取，而不是另算一遍——**要验的正是它给的
	// 那个指纹能用**。自己重算等于绕过被测对象。
	tok := createToken(ctx, t, c, node, "--node", "throwaway-"+node, "--ttl", "1m")
	for _, f := range strings.Fields(tok.JoinCommand) {
		if strings.HasPrefix(f, "sha256:") {
			return f
		}
	}
	t.Fatalf("加入命令里没有 CA 指纹:\n%s", tok.JoinCommand)
	return ""
}

// resetNode 把一台机器擦回「还没加入」的样子。
//
// **两侧都要擦**：机器上的证书与安装，以及 mechd 册子里那一行。
// 只擦机器的话，重跑时 join 会以「节点已在册」被拒——那条拒绝是对的
// （ADR-0034 的名字占用规则），只是测试没把前提建立完整。
func resetNode(ctx context.Context, t *testing.T, c *cluster, node string) {
	t.Helper()
	stopAgent(ctx, c, node)
	if out, err := c.sh(ctx, node,
		"rm -rf "+m7ConfDir+" "+m7DataDir+" "+m7Prefix+" /usr/local/lib/mecharion/.bootstrap"); err != nil {
		t.Fatalf("[%s] 清理: %v\n%s", node, err, out)
	}
	// 把容器自己的 /usr/bin 软链复原。
	//
	// 一次没带 --link-dir 的安装会把它们改指到别的前缀，而上面刚把那个
	// 前缀删掉——于是 `mechlet` 变成一条悬空软链，**后面每条测试都拿到
	// 空输出**。那种失败看起来像「命令什么都没打印」，与真正的断言失败
	// 长得一模一样。
	if out, err := c.sh(ctx, node,
		"for b in mechctl mechpack mechd mechlet; do "+
			"ln -sfn /usr/local/lib/mecharion/current/bin/$b /usr/bin/$b; done"); err != nil {
		t.Fatalf("[%s] 复原软链: %v\n%s", node, err, out)
	}
	forgetNode(ctx, t, c, c.node(1), node)
}

// forgetNode 把一个节点从 mechd 的册子上抹掉；不在册时什么也不做。
func forgetNode(ctx context.Context, t *testing.T, c *cluster, mechdNode, node string) {
	t.Helper()
	if _, err := c.run(ctx, mechdNode, "systemctl", "is-active", mechdUnit); err != nil {
		return // mechd 还没起来，册子本来就是空的
	}
	// **节点名从标准输入喂进去。** `node remove --force` 在 10-cli §7 的
	// 二档确认表里，而那一档 `-y` 跳不过去——它挡的是「删错了对象」。
	//
	// 这里原本传的是 `-y`，M9 第 3 步把 confirmName 改成不认 -y 之后，
	// 这条清理就静默失效了（错误被下面那个「忽略」吞掉）。夹具的失败
	// 被吞掉最难发现：测试照样绿，只是前提悄悄不成立了。
	out, err := c.runStdin(ctx, mechdNode, node+"\n",
		"mechctl", "node", "remove", node, "--force",
		"--server", "https://"+mechdNode+":"+mechdPort,
		"--token", adminToken(ctx, t, c, mechdNode), "--ca-file", caPath)
	// 不在册是**正常情况**（第一次跑，或者上一条测试已经清过），不吵。
	if err != nil && !strings.Contains(out, "not found") {
		t.Logf("[%s] 移除节点 %s（忽略）: %v\n%s", mechdNode, node, err, out)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
