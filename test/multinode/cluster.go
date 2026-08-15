//go:build linux

// Package multinode 是多节点验收的夹具与测试（M7）。
//
// **它跑在宿主侧，不在被测容器里** —— 这与 test/e2e 相反，而那个不同是
// 必须的：多节点验收要在三台机器上分别执行命令、比对三份状态，而节点镜像
// 里既没有 docker 也没有 ssh。装上任一个都会毁掉那个镜像存在的理由
// （test/node/Dockerfile：靠「没有 curl」强制执行 hermetic 约束）。
//
// 被测系统仍然完整地跑在容器里：mechd、mechlet、systemd 一个都没打桩。
// 挪到宿主的只有那双**观察的眼睛**——它本来就不能站在任何一台被测机器上。
//
// 代价照实说：这些测试依赖宿主有 docker CLI，因此 `go test ./...` 跑不到
// 它们（没有 M7N_CLUSTER_* 环境变量时整体 skip）。入口是
// `./hack/testenv.sh cluster test`。
package multinode

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// binDir 与容器里的真实安装路径一致（/usr/bin 下是软链）。
const binDir = "/usr/local/lib/mecharion/current/bin"

// cluster 是一组已经起好的节点容器。
type cluster struct {
	nodes []string
	net   string
}

// requireCluster 在集群不可用时跳过。
//
// **跳过而不是失败**：这个包会被 `go test ./...` 扫到，而那时多半没有集群。
// 但跳过的理由要说清楚，否则「一直在跳过」会被当成「一直在通过」。
func requireCluster(t *testing.T) *cluster {
	t.Helper()
	prefix := os.Getenv("M7N_CLUSTER_PREFIX")
	size, _ := strconv.Atoi(os.Getenv("M7N_CLUSTER_SIZE"))
	if prefix == "" || size <= 0 {
		t.Skip("没有多节点集群环境；入口是 ./hack/testenv.sh cluster test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("宿主没有 docker CLI —— 多节点验收的驱动程序跑在宿主侧")
	}

	c := &cluster{net: os.Getenv("M7N_NET")}
	for i := 1; i <= size; i++ {
		c.nodes = append(c.nodes, fmt.Sprintf("%s%d", prefix, i))
	}
	for _, n := range c.nodes {
		if err := exec.Command("docker", "inspect", n).Run(); err != nil {
			t.Skipf("节点 %s 不在运行；先 ./hack/testenv.sh cluster up", n)
		}
	}
	return c
}

func (c *cluster) node(i int) string { return c.nodes[i-1] }

// run 在某个节点上执行一条命令，返回合并的输出。
func (c *cluster) run(
	ctx context.Context, node string, args ...string,
) (string, error) {
	full := append([]string{"exec", node}, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

// mustRun 同上，失败即终止测试。
func (c *cluster) mustRun(
	ctx context.Context, t *testing.T, node string, args ...string,
) string {
	t.Helper()
	out, err := c.run(ctx, node, args...)
	if err != nil {
		t.Fatalf("[%s] %s 失败: %v\n%s", node, strings.Join(args, " "), err, out)
	}
	return out
}

// dockerStdin 构造一条可以喂 stdin 的 docker exec。
//
// `-i` 不能省：没有它 docker 不接管 stdin，写文件那类命令会拿到空输入
// 而**静默成功**——留下一个 0 字节的文件，症状出现在很远的地方。
func dockerStdin(ctx context.Context, node string, args ...string) *exec.Cmd {
	full := append([]string{"exec", "-i", node}, args...)
	return exec.CommandContext(ctx, "docker", full...)
}

// sh 在节点上跑一段 shell —— 需要管道或重定向时用它。
func (c *cluster) sh(ctx context.Context, node, script string) (string, error) {
	return c.run(ctx, node, "sh", "-c", script)
}

// runStdin 在节点上执行一条命令，并把 stdin 喂进去。
//
// 二档确认（输入组件名）唯一的驱动方式：`docker exec` 默认不接 stdin，
// 而没有 stdin 的确认会直接读到 EOF——那与「输错了名字」是两条不同的
// 路径，只测后者会漏掉真正会在脚本里发生的那一种。
func (c *cluster) runStdin(
	ctx context.Context, node, stdin string, args ...string,
) (string, error) {
	full := append([]string{"exec", "-i", node}, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Stdin = strings.NewReader(stdin)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

// mustRunStdin 是 runStdin 的「失败即终止」版本。
func (c *cluster) mustRunStdin(
	ctx context.Context, t *testing.T, node, stdin string, args ...string,
) string {
	t.Helper()
	out, err := c.runStdin(ctx, node, stdin, args...)
	if err != nil {
		t.Fatalf("[%s] %s 失败: %v\n%s", node, strings.Join(args, " "), err, out)
	}
	return out
}

// readFile 读节点上的一个文件。
func (c *cluster) readFile(ctx context.Context, node, path string) (string, error) {
	return c.sh(ctx, node, "cat "+path)
}

// waitUntil 轮询到条件成立或超时。
func waitUntil(ctx context.Context, d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		if cond() {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func tailOf(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
