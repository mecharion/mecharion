//go:build linux

package multinode

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 本文件是 **M7 第 4 步的验收**：两条加入路径都通。
//
// 第 3 步验的是「在目标机器上手工敲 install --join」。这一步验的是
// **运维不必登上那台机器**：一条 `mechctl node bootstrap ssh://…`
// 把二进制推过去、把 install 跑起来，然后 SSH 到此为止。
//
// 「到此为止」不是一句口号，是要验的性质之一：装完之后 mechlet 主动
// 拨向 mechd，那台机器上不开任何入站端口——这正是 Agent 模式相对
// Ansible 的核心安全收益（ADR-0001）。

// stagingDir 与 ctlcmd.bootstrapDir 对应：二进制在目标机器上的落脚点。
//
// **不在 /tmp**：加固过的机器普遍把它挂成 noexec，推过去的二进制根本
// 执行不了（本项目的测试容器恰好就是这样，第一次跑就撞上了）。
const stagingDir = "/usr/local/lib/mecharion/.bootstrap"

// TestBootstrapOverSSH 是第 4 步的验收。
func TestBootstrapOverSSH(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	n1, n3 := c.node(1), c.node(3)
	installOnce(ctx, t, c, n1)
	resetNode(ctx, t, c, n3)
	setupSSH(ctx, t, c, n1, n3)

	tok := createToken(ctx, t, c, n1, "--node", n3)

	// ── 一条命令，不登那台机器 ──
	//
	// **不给 --ca-hash、不给 --join-url**：两者都该由 mechctl 从本机已有
	// 的配置推出来。多要一次的唯一后果是多一次贴错。
	out, err := c.run(ctx, n1, "mechctl", "node", "bootstrap",
		"ssh://root@"+n3,
		"--token", tok.Token,
		"--identity", "/root/.ssh/id_ed25519",
		"--node", n3,
		"--prefix", m7Prefix, "--conf-dir", m7ConfDir, "--data-dir", m7DataDir,
		"--link-dir", m7Prefix+"/bin",
		"--server", "https://"+n1+":"+mechdPort,
		"--ca-file", caPath)
	if err != nil {
		t.Fatalf("bootstrap 失败: %v\n%s", err, out)
	}
	if !strings.Contains(out, "has joined") {
		t.Fatalf("bootstrap 输出不对:\n%s", out)
	}

	// ── ① 证书真的落在了目标机器上 ──
	for _, f := range []string{"node.crt", "node.key", "ca.crt"} {
		if _, err := c.readFile(ctx, n3, m7ConfDir+"/pki/"+f); err != nil {
			t.Errorf("[%s] 应当有 %s: %v", n3, f, err)
		}
	}

	// ── ② 推过去的二进制被清理掉了 ──
	//
	// 那条命令行里带着 token，留一堆二进制在安装根下还会让人误以为
	// 那是安装位置。失败的 bootstrap 也要清——因此这条断言同时钉住了
	// 「清理走 defer 而不是成功路径」。
	if ls, err := c.sh(ctx, n3, "ls "+stagingDir); err == nil {
		t.Errorf("[%s] bootstrap 目录应当被清理，实际还在:\n%s\nbootstrap 输出:\n%s",
			n3, ls, out)
	}

	// ── ③ 节点在册，且能用那张证书连上来 ──
	if got := nodeStatus(ctx, t, c, n1, n3); got == "" {
		t.Fatalf("bootstrap 之后 %s 应当出现在节点列表里", n3)
	}
	logPath := startAgent(ctx, t, c, n3, n1+":"+grpcPort)
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return strings.Contains(readLog(ctx, c, n3, logPath), "registered with mechd")
	}) {
		t.Fatalf("[%s] bootstrap 之后应当连得上，日志:\n%s",
			n3, tailOf(readLog(ctx, c, n3, logPath), 30))
	}
	if got := nodeStatus(ctx, t, c, n1, n3); got != "online" {
		t.Errorf("%s 的状态应为 online，实际 %q", n3, got)
	}
}

// TestBootstrapNeedsUsableToken 钉住 bootstrap **没有绕过那道门**。
//
// 它只是把 install --join 送到别人机器上执行，用的是同一条 join 路径。
// 若它另开一条口子（比如直接用 admin token 签证书），第 3 步那五条校验
// 就全被绕过去了。
func TestBootstrapNeedsUsableToken(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	n1, n3 := c.node(1), c.node(3)
	installOnce(ctx, t, c, n1)
	resetNode(ctx, t, c, n3)
	setupSSH(ctx, t, c, n1, n3)

	out, err := c.run(ctx, n1, "mechctl", "node", "bootstrap",
		"ssh://root@"+n3,
		"--token", "m7n_join_deadbeef",
		"--identity", "/root/.ssh/id_ed25519",
		"--node", n3,
		"--prefix", m7Prefix, "--conf-dir", m7ConfDir, "--data-dir", m7DataDir,
		"--link-dir", m7Prefix+"/bin",
		"--server", "https://"+n1+":"+mechdPort,
		"--ca-file", caPath)
	if err == nil {
		t.Fatalf("伪造的 token 竟然装成了:\n%s", out)
	}
	if !strings.Contains(out, "invalid token") {
		t.Errorf("应当把远端那句拒绝原样带回来，实际:\n%s", out)
	}
	if _, err := c.readFile(ctx, n3, m7ConfDir+"/pki/node.crt"); err == nil {
		t.Errorf("[%s] 失败的 bootstrap 不该留下证书", n3)
	}
	// **失败时更要清干净**：那条命令行里带着 token。
	if out, err := c.sh(ctx, n3, "ls "+stagingDir); err == nil {
		t.Errorf("[%s] 失败的 bootstrap 也该清掉推过去的二进制，实际还在:\n%s", n3, out)
	}
}

// TestBootstrapRejectsPasswordOnlyTarget 钉住「只支持公钥认证」。
//
// 口令认证要么写进命令行（进 shell 历史），要么交互输入（脚本里走不通）。
// 两者都不是好的默认，因此**明确不支持**——而不是悄悄退化。
func TestBootstrapRejectsPasswordOnlyTarget(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	n1, n3 := c.node(1), c.node(3)
	// 故意指一把不存在的私钥
	out, _ := c.run(ctx, n1, "mechctl", "node", "bootstrap",
		"ssh://root@"+n3,
		"--token", "m7n_join_whatever",
		"--identity", "/root/.ssh/does-not-exist",
		"--server", "https://"+n1+":"+mechdPort,
		"--ca-file", caPath)
	if !strings.Contains(out, "private key") {
		t.Errorf("找不到私钥时应当说清是私钥的问题，实际:\n%s", out)
	}
}

// setupSSH 在 n1 上生成一把密钥并装到目标机器的 authorized_keys。
//
// 这一步**刻意由测试做**而不是打进镜像：它模拟的是运维本来就有的那条
// 带外通道（他能 ssh 上去，所以才用得了 bootstrap）。把固定密钥打进镜像
// 会让「bootstrap 到底需要什么前提」在验收里变得含糊。
func setupSSH(ctx context.Context, t *testing.T, c *cluster, from, to string) {
	t.Helper()
	key := "/root/.ssh/id_ed25519"
	if _, err := c.sh(ctx, from,
		"mkdir -p /root/.ssh && chmod 700 /root/.ssh && "+
			"[ -f "+key+" ] || ssh-keygen -t ed25519 -N '' -f "+key+" -q"); err != nil {
		t.Fatalf("[%s] 生成 SSH 密钥失败", from)
	}
	pub := mustRead(ctx, t, c, from, key+".pub")

	writeFile(ctx, t, c, to, "/root/.ssh/authorized_keys", pub)
	c.mustRun(ctx, t, to, "chmod", "600", "/root/.ssh/authorized_keys")
	if out, err := c.run(ctx, to, "systemctl", "restart", "ssh"); err != nil {
		t.Fatalf("[%s] 起 sshd: %v\n%s", to, err, out)
	}
	// 等 sshd 真的接受连接：systemctl 返回只说明 unit 起来了
	if !waitUntil(ctx, 30*time.Second, func() bool {
		_, err := c.sh(ctx, to, "ss -ltn 2>/dev/null | grep -q ':22 ' || "+
			"grep -q . /proc/net/tcp")
		return err == nil
	}) {
		t.Logf("[%s] 未能确认 sshd 端口就绪，继续试", to)
	}
}
