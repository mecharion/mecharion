//go:build linux

package multinode

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 本文件是 **M7 第 1 步的验收**：三台容器互通，mechd 在其中一台。
//
// 判据不是「docker ps 里有三行」——那种验收下一步就会失效。真正要成立的是
// **控制面跨机可达**：站在 n2 上，用 mechctl 连 n1 的 HTTPS API，拿到答案。
// 那一条命令同时把四件事验了，而它们各自都可能单独坏掉：
//
//	网络      两个容器在同一个用户自定义网络上
//	DNS       m7n-n1 这个名字解析得到
//	证书 SAN  自签服务端证书里有 m7n-n1，用名字连才验得过
//	认证      CA 与 admin token 跨机之后仍然可用
//
// 后面九步全都建立在这四件事上。它们在这里一起验，比各自等到用上时才发现
// 便宜得多——尤其是证书 SAN：它坏掉时的症状是一句 TLS 错误，**看起来像
// 网络问题**。

// 安装到一套**隔离的路径**下，与 test/e2e 的做法一致。
//
// 不是洁癖：测试镜像里 /usr/local/lib/mecharion/current **是个真目录**
// （Dockerfile 建它来接宿主 bin 的挂载），而 install 要把 current 做成软链，
// 于是原样安装必然撞在那里。换个前缀比改镜像便宜，也不影响这一步要验的东西。
const (
	m7Prefix  = "/usr/local/lib/mecharion-m7"
	m7ConfDir = "/etc/mecharion-m7"
	m7DataDir = "/var/lib/mecharion-m7"

	mechdUnit = "mecharion-mechd.service"
	caPath    = m7ConfDir + "/pki/ca.crt"
	tokenPath = m7ConfDir + "/admin.token"

	// **走真实默认值**：0.0.0.0 + HTTPS。单节点 e2e 用的是回环上的明文
	// （那里只需要本机可达），而这一步要验的恰恰是「跨机 + TLS」。
	mechdAddr = "0.0.0.0:8443"
	mechdPort = "8443"
)

// TestClusterControlPlaneIsReachableAcrossNodes 是第 1 步的验收。
func TestClusterControlPlaneIsReachableAcrossNodes(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	if len(c.nodes) < 3 {
		t.Fatalf("这一步要三台机器，实际 %d 台：%v", len(c.nodes), c.nodes)
	}
	n1, n2, n3 := c.node(1), c.node(2), c.node(3)

	// ── ① 三台机器都能跑 sshd ──
	//
	// 集群用的是**带 ssh 的镜像**，不是那个贫瘠的（test/node-ssh）：
	// `mechctl node bootstrap ssh://…` 的前提就是目标机器上有 sshd，
	// 而真实用户的机器本来也不是贫瘠的。
	//
	// **hermetic 约束没有因此失效**——它由单节点套件强制执行，
	// test/node 那个「没有 curl」的镜像原样保留。两处约束各在该在的
	// 地方，而不是靠一条写在错误位置的断言假装还在保。
	for _, n := range c.nodes {
		if out, err := c.run(ctx, n, "systemctl", "is-active", "ssh"); err != nil ||
			!strings.HasPrefix(strings.TrimSpace(out), "active") {
			t.Errorf("[%s] sshd 应当在跑（bootstrap 那一步要用）: %v\n%s", n, err, out)
		}
	}

	// ── ② n1 装成单机：mechd 起来 ──
	installOnce(ctx, t, c, n1)

	// ── ③ 名字解析得到，且端口通 ──
	//
	// 分两条断言而不是一条：解析不了与连不上是两种完全不同的故障，
	// 合成一条「连不上 m7n-n1」会让排查从头开始。
	for _, from := range []string{n2, n3} {
		if out, err := c.sh(ctx, from,
			"getent hosts "+n1); err != nil || !strings.Contains(out, n1) {
			t.Fatalf("[%s] 解析不到 %s（用户自定义网络才有内嵌 DNS）: %v\n%s",
				from, n1, err, out)
		}
	}

	// ── ④ 站在 n2 / n3 上问 n1 要节点列表 ──
	ca := mustRead(ctx, t, c, n1, caPath)
	token := strings.TrimSpace(mustRead(ctx, t, c, n1, tokenPath))
	if token == "" {
		t.Fatal("n1 上没有 admin token —— mechd 首次启动应当生成它")
	}

	for _, from := range []string{n2, n3} {
		// CA 拷过去：远程连接要显式带 --ca-file（08-security §3.2
		// 明确不做 TOFU）
		remoteCA := "/tmp/n1-ca.crt"
		writeFile(ctx, t, c, from, remoteCA, ca)

		out, err := c.run(ctx, from, "mechctl", "node", "list",
			"--server", "https://"+n1+":"+mechdPort,
			"--token", token, "--ca-file", remoteCA)
		if err != nil {
			t.Fatalf("[%s] 连不上 %s 的控制面: %v\n%s", from, n1, err, out)
		}
		if !strings.Contains(out, n1) {
			t.Errorf("[%s] 节点列表里应当有 %s，实际:\n%s", from, n1, out)
		}
	}

	// ── ⑤ 不带 CA 必须失败 ──
	//
	// **不验这一条，上面那条就说明不了什么**：如果客户端根本没在校验
	// 证书，带不带 --ca-file 都能连上，而「证书 SAN 是对的」这个结论
	// 就是假的。
	if out, err := c.run(ctx, n2, "mechctl", "node", "list",
		"--server", "https://"+n1+":"+mechdPort, "--token", token,
		"--ca-file", "/dev/null"); err == nil {
		t.Errorf("[%s] 用一个空 CA 也连上了 —— 证书没有被真的校验:\n%s", n2, out)
	}
}

// TestClusterNodesAreIsolated 钉住「三台是三台」。
//
// 同一个镜像起三份，很容易写出「其实一直在同一台上跑」的夹具——
// 那样后面每一条多节点验收都会通过，而它们什么都没验证。
func TestClusterNodesAreIsolated(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	seen := map[string]string{}
	for _, n := range c.nodes {
		host := strings.TrimSpace(c.mustRun(ctx, t, n, "hostname"))
		if host != n {
			t.Errorf("[%s] 主机名应当与容器名一致（证书 SAN 依赖它），实际 %q", n, host)
		}
		// 在一台上写个文件，别的机器不该看得见
		marker := "/var/tmp/who-am-i"
		c.mustRun(ctx, t, n, "sh", "-c", "echo "+n+" > "+marker)
		seen[n] = marker
	}
	for _, n := range c.nodes {
		got := strings.TrimSpace(c.mustRun(ctx, t, n, "cat", seen[n]))
		if got != n {
			t.Errorf("[%s] 读回来的标记是 %q —— 三台机器可能共用了文件系统", n, got)
		}
	}
}

// ── 夹具 ────────────────────────────────────────────────────────────────

// TestMechdStopsPromptly 钉住 mechd **响应 SIGTERM**。
//
// 缺陷长这样：`Subscribe` 是长连的服务端流，正常情况下永远不返回；而 grpc
// 的 `GracefulStop` 会等所有活跃 RPC 结束。两者撞在一起，mechd 收到 SIGTERM
// 之后一直挂到 systemd 的 `TimeoutStopSec` 再吃一发 SIGKILL。
//
// **这条必须在容器里验。** 症状的每一样都在 systemd 那一侧：等了多久、
// 有没有被 KILL、退出码是什么。宿主上的单元测试只能看到「GracefulStop
// 返回了」，看不到这些。
//
// 代价也是真的，不只是慢：SIGKILL 意味着 `defer st.Close()` 走不到，
// SQLite 的 WAL 没有干净关闭。
func TestMechdStopsPromptly(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	n1, n2 := c.node(1), c.node(2)
	installOnce(ctx, t, c, n1)

	// **要有一条活跃的订阅流**，否则这条测试什么都没验——挂住的正是
	// 「有节点连着」这个正常情况。
	resetNode(ctx, t, c, n2)
	joinNode(ctx, t, c, n1, n2)
	logPath := startAgent(ctx, t, c, n2, n1+":"+grpcPort)
	if !waitUntil(ctx, 60*time.Second, func() bool {
		return strings.Contains(readLog(ctx, c, n2, logPath), "registered with mechd")
	}) {
		t.Fatalf("[%s] 需要一条活跃连接才谈得上优雅关闭", n2)
	}

	// **取容器自己的时钟**，用来把日志范围收到这一次停机。
	//
	// `journalctl -n 30` 是不行的：它跨多次启停，会把上一轮留下的
	// 「优雅关闭超时」也算进来。第一版就是这么写的，结果一条已经修好的
	// 代码被上一次变异运行的日志判成红的——**一条会闪的测试比没有更糟**。
	since := strings.TrimSpace(runOn(ctx, t, c, n1, "date", "+%Y-%m-%d %H:%M:%S"))

	start := time.Now()
	if out, err := c.run(ctx, n1, "systemctl", "stop", mechdUnit); err != nil {
		t.Fatalf("停 mechd 失败: %v\n%s", err, out)
	}
	took := time.Since(start)
	t.Cleanup(func() {
		_, _ = c.run(context.Background(), n1, "systemctl", "start", mechdUnit)
	})

	// systemd 的 TimeoutStopSec 是 30 秒。**10 秒是个有余量的判据**：
	// 修好之后实际是一两秒，而没修好一定会撞满 30 秒。
	if took > 10*time.Second {
		t.Errorf("停 mechd 用了 %s——SIGTERM 没有被及时响应", took.Round(time.Second))
	}

	// **判据不只是快。** 一个 `TimeoutStopSec=1s` 的 unit 也会「很快停下」，
	// 而那只是把 SIGKILL 提前了。真正要钉的是它**自己退的**。
	logs, _ := c.run(ctx, n1, "journalctl", "-u", mechdUnit, "--no-pager",
		"--since", since)
	if strings.Contains(logs, "Killing process") {
		t.Errorf("mechd 是被 SIGKILL 掉的，不是自己退的：\n%s", tailOf(logs, 20))
	}
	if strings.Contains(logs, "stop-sigterm") {
		t.Errorf("撞上了 stop-sigterm 超时：\n%s", tailOf(logs, 20))
	}

	// **必须是 Drain 干的活，不是兜底救的场。**
	//
	// 关闭路径有两层：Drain 让订阅流自己收摊（机制），超时后 Stop 强切
	// （兜底）。少了这一条断言，两层就分不开——Drain 哪天悄悄失效了，
	// 每次停机会静默地多花一个宽限期，而上面那几条照样全绿。
	//
	// 这不是假想：写这条测试时把 Drain 摘掉做变异，前面三条断言**一条都
	// 没红**，因为兜底把 SIGKILL 挡住了。
	if strings.Contains(logs, "graceful shutdown timed out") {
		t.Errorf("走的是强制关闭的兜底路径，说明 Drain 没起作用：\n%s",
			tailOf(logs, 20))
	}
}

// runOn 在节点上跑一条命令并返回输出，失败即终止。
func runOn(ctx context.Context, t *testing.T, c *cluster, node string, args ...string) string {
	t.Helper()
	out, err := c.run(ctx, node, args...)
	if err != nil {
		t.Fatalf("[%s] %v: %v\n%s", node, args, err, out)
	}
	return out
}

// installOnce 把 n1 装成单机，已经装过就跳过。
//
// 幂等是因为集群是**跨测试复用**的：起三台容器要十几秒，每个测试重来一遍
// 会让这个包慢到没人愿意跑。
func installOnce(ctx context.Context, t *testing.T, c *cluster, node string) {
	t.Helper()
	if out, err := c.run(ctx, node, "systemctl", "is-active", mechdUnit); err == nil &&
		strings.HasPrefix(strings.TrimSpace(out), "active") {
		return
	}
	out, err := c.run(ctx, node, "mechlet", "install", "--standalone",
		"--prefix", m7Prefix,
		// 不碰 /usr/bin：容器里那四条软链是夹具自己的
		"--link-dir", m7Prefix+"/bin",
		"--conf-dir", m7ConfDir,
		"--data-dir", m7DataDir,
		"--node", node,
		"--http", mechdAddr)
	if err != nil {
		t.Fatalf("[%s] 单机安装失败: %v\n%s", node, err, out)
	}
	// install **刻意不自动启动**，只打印这条命令——照着它做，
	// 走的就是用户会走的那条路。
	if out, err := c.run(ctx, node, "systemctl", "enable", "--now",
		"mecharion-mechd", "mecharion-mechlet"); err != nil {
		t.Fatalf("[%s] 启动 mechd/mechlet 失败: %v\n%s", node, err, out)
	}
	if !waitUntil(ctx, 60*time.Second, func() bool {
		o, err := c.run(ctx, node, "systemctl", "is-active", mechdUnit)
		return err == nil && strings.HasPrefix(strings.TrimSpace(o), "active")
	}) {
		logs, _ := c.run(ctx, node, "journalctl", "-u", mechdUnit, "--no-pager", "-n", "40")
		t.Fatalf("[%s] mechd 没起来:\n%s", node, tailOf(logs, 40))
	}
}

func mustRead(ctx context.Context, t *testing.T, c *cluster, node, path string) string {
	t.Helper()
	out, err := c.readFile(ctx, node, path)
	if err != nil {
		t.Fatalf("[%s] 读 %s: %v\n%s", node, path, err, out)
	}
	return out
}

// writeFile 把内容写到节点上。
//
// 走 stdin 而不是把内容拼进 shell 命令：证书里有换行与斜杠，
// 拼字符串迟早会在某个字符上炸掉，而那种失败看起来像「证书无效」。
func writeFile(ctx context.Context, t *testing.T, c *cluster, node, path, body string) {
	t.Helper()
	cmd := dockerStdin(ctx, node, "sh", "-c", "cat > "+path)
	cmd.Stdin = strings.NewReader(body)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("[%s] 写 %s: %v\n%s", node, path, err, out)
	}
}
