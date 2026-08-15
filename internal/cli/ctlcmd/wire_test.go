package ctlcmd

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	rootcli "github.com/mecharion/mecharion/internal/cli"
	"github.com/mecharion/mecharion/internal/logging"
	"github.com/mecharion/mecharion/internal/mechd"
	"github.com/mecharion/mecharion/internal/packindex"
	"github.com/mecharion/mecharion/internal/protocol"
	"github.com/mecharion/mecharion/internal/store"
	"github.com/mecharion/mecharion/internal/vault"
)

// ── 夹具 ────────────────────────────────────────────────────────────────

// wired 起一套真实的服务端（HTTP + 服务层 + 库），并给出连它的 flag。
//
// 走的是**真的 HTTP**而不是直接调服务层：这一步验收的正是「接线」，
// 而接线的错误（路径写错、body 字段名对不上、状态码映射错）恰恰只在
// 跨过那条边界时才暴露。
type wired struct {
	t    *testing.T
	srv  *httptest.Server
	svc  *mechd.Service
	site store.Site
	args []string
}

func newWired(t *testing.T, nodes ...string) *wired {
	t.Helper()
	root := repoRoot(t)
	dir := t.TempDir()

	st, err := store.Open(t.Context(), store.Options{
		Path: filepath.Join(dir, "mechd.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	v, err := vault.Open(t.Context(), st, vault.Options{
		KeyPath: filepath.Join(dir, "secret.key"),
	})
	if err != nil {
		t.Fatal(err)
	}

	idx := packindex.New()
	if err := idx.AddDir(filepath.Join(root, "examples", "packs")); err != nil {
		t.Fatal(err)
	}

	svc := &mechd.Service{
		Store: st, Repos: st.Repos(), Vault: v, Packs: idx,
		BlobDir: filepath.Join(dir, "blobs"),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	site, err := svc.Repos.Sites().Create(t.Context(), store.Site{
		Name: mechd.DefaultSite, Kind: "cluster",
	})
	if err != nil {
		t.Fatal(err)
	}

	for i, name := range nodes {
		n, err := svc.Repos.Nodes().Upsert(t.Context(), store.Node{
			SiteID: site.ID, Name: name,
			Address: "10.0.0." + string(rune('1'+i)), Status: "online",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.Repos.Status().PutFacts(t.Context(), store.NodeFacts{
			NodeID: n.ID, CollectedAt: time.Now(),
			Facts: map[string]any{"memory": map[string]any{"total": "32Gi"}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	token := mechd.TokenPrefix + "wire-test"
	api := &mechd.API{S: svc, Auth: mechd.NewTokenAuth(token), PackDir: filepath.Join(dir, "packs")}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	return &wired{
		t: t, srv: srv, svc: svc, site: site,
		args: []string{"--server", srv.URL, "--token", token},
	}
}

// newMechctlRoot 起一棵最小但真实的 root 命令树，把 ClientFlags 只在
// 根命令上 Bind 一次——`--server`/`--token`/`--local`/`--site`/`-o/--output`
// 都按同一处理，物理上只有一处能注册，
// 独立测试用的命令树也必须照这个真实接线搭，否则测的不是用户真实会
// 敲的那条命令行（这条测试基础设施本身就是防「文档能用、代码不能用」
// 这类回归的地方——mechctl --local 那次正是没测到位）。
func newMechctlRoot(nouns ...func(*ClientFlags) *cobra.Command) *cobra.Command {
	root, gf := rootcli.NewRoot(rootcli.Options{
		Name: "mechctl", DefaultLogFormat: logging.FormatText,
	})
	flags := &ClientFlags{Global: gf}
	flags.Bind(root)
	for _, n := range nouns {
		root.AddCommand(n(flags))
	}
	return root
}

// run 跑一条 mechctl 子命令，返回 stdout / stderr。
//
// **经由真实的 root 命令树**，不是孤立地跑 `NewComponentCmd()`——
// `-o/--output` 只在根命令上注册一次，孤立跑子命令树
// 拿不到这个 flag，测的也就不是用户真实会敲的那条命令行。
func (w *wired) run(args ...string) (string, string, error) {
	w.t.Helper()
	root := newMechctlRoot(NewComponentCmd)
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(append(append([]string{"component"}, args...), w.args...))
	err := root.Execute()
	return out.String(), errBuf.String(), err
}

// runFull 跟 run 一样走真实 root 命令树，但挂全部名词命令，且不隐式
// 加上 "component" 前缀——调用方自己写完整的 `<名词> <动词>`。
// 给需要跨名词（node/rollout/user/...）验证 -o 的测试用，run 只挂了
// component 一个名词。
func (w *wired) runFull(args ...string) (string, string, error) {
	w.t.Helper()
	root := newMechctlRoot(
		NewComponentCmd, NewNodeCmd, NewRolloutCmd,
		NewUserCmd, NewOrphansCmd, NewConfigCmd, NewPackCmd,
	)
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(append(args, w.args...))
	err := root.Execute()
	return out.String(), errBuf.String(), err
}

func (w *wired) mustRunFull(args ...string) string {
	w.t.Helper()
	out, errBuf, err := w.runFull(args...)
	if err != nil {
		w.t.Fatalf("mechctl %s 失败: %v\n%s\n%s", strings.Join(args, " "), err, out, errBuf)
	}
	return out
}

func (w *wired) mustRun(args ...string) string {
	w.t.Helper()
	out, errBuf, err := w.run(args...)
	if err != nil {
		w.t.Fatalf("mechctl component %s 失败: %v\n%s\n%s",
			strings.Join(args, " "), err, out, errBuf)
	}
	return out
}

// ── 验收 ────────────────────────────────────────────────────────────────

// TestDeployStatusDiffAckDrift 是第 8 步的验收判据：
// component deploy / status / diff / ack-drift 四条命令全部走通。
func TestDeployStatusDiffAckDrift(t *testing.T) {
	w := newWired(t, "n1", "n2")

	// deploy
	out := w.mustRun("deploy", "go-webapp", "-c", "web", "--nodes", "n1,n2")
	if !strings.Contains(out, "Deployed web") {
		t.Errorf("deploy 输出不对:\n%s", out)
	}
	if !strings.Contains(out, "default@n1") || !strings.Contains(out, "default@n2") {
		t.Errorf("应列出两个实例:\n%s", out)
	}

	// list
	out = w.mustRun("list")
	if !strings.Contains(out, "web") || !strings.Contains(out, "go-webapp") {
		t.Errorf("list 输出不对:\n%s", out)
	}

	// status —— 还没有上报，应当未收敛
	out = w.mustRun("status", "web")
	if !strings.Contains(out, "Not converged") {
		t.Errorf("没有上报时应报未收敛:\n%s", out)
	}

	// diff —— 还没有任何上报，两个实例都是待创建
	out = w.mustRun("diff", "web")
	if !strings.Contains(out, "create") {
		t.Errorf("没有上报时 diff 应报 create:\n%s", out)
	}

	// 让两个实例报上对得上的 digest，此时应当没有待下发的变化
	w.reportConverged("web", "default", "n1", "n2")
	out = w.mustRun("diff", "web")
	if !strings.Contains(out, "has no pending changes") {
		t.Errorf("已收敛时 diff 不该有变化:\n%s", out)
	}
	out = w.mustRun("status", "web")
	if !strings.Contains(out, "Converged") {
		t.Errorf("digest 一致且健康时应报已收敛:\n%s", out)
	}

	// 改一个参数：期望变了、机器上还没变 → update
	w.mustRun("deploy", "go-webapp", "-c", "web", "--nodes", "n1,n2",
		"--update", "--set", "log_level=debug")
	out = w.mustRun("diff", "web")
	if !strings.Contains(out, "update") {
		t.Errorf("改了参数后 diff 应报 update:\n%s", out)
	}

	// ack-drift
	out = w.mustRun("ack-drift", "web",
		"--resource", "template:app.yaml",
		"--duration", "4h", "--reason", "凌晨排障临时调了日志级别")
	if !strings.Contains(out, "Suppressed drift alerts for 2 instance") {
		t.Errorf("ack-drift 输出不对:\n%s", out)
	}
	out = w.mustRun("status", "web")
	if !strings.Contains(out, "Suppressed") {
		t.Errorf("status 里应显示已抑制:\n%s", out)
	}
}

// TestAckDriftRequiresReasonAndDuration 钉住抑制不能是无名无期的。
func TestAckDriftRequiresReasonAndDuration(t *testing.T) {
	w := newWired(t, "n1")
	w.mustRun("deploy", "go-webapp", "-c", "web", "--nodes", "n1")

	if _, _, err := w.run("ack-drift", "web", "--duration", "4h"); err == nil {
		t.Error("没有 --reason 应当被拒绝")
	}
	if _, _, err := w.run("ack-drift", "web", "--reason", "x"); err == nil {
		t.Error("没有 --duration 应当被拒绝")
	}
}

// TestDryRunPrintsPlanWithoutPersisting 钉住 --dry-run 的两条性质。
func TestDryRunPrintsPlanWithoutPersisting(t *testing.T) {
	w := newWired(t, "n1")

	out := w.mustRun("deploy", "go-webapp", "-c", "web", "--nodes", "n1", "--dry-run")
	if !strings.Contains(out, "--dry-run, nothing changed") {
		t.Errorf("干跑要说清什么都没改:\n%s", out)
	}
	if !strings.Contains(out, "default@n1") {
		t.Errorf("干跑也要给出完整计划:\n%s", out)
	}

	if _, err := w.svc.Repos.Components().GetByName(t.Context(), w.site.ID, "web"); err == nil {
		t.Error("干跑不该把组件写进库")
	}
}

// TestSetFileKeepsSecretsOffTheCommandLine 钉住 --set-file。
//
// 命令行会出现在同机任何用户的 ps 输出里，也会进 shell 历史。
// 敏感参数必须有一条不经过命令行的路（spec §7.7）。
func TestSetFileKeepsSecretsOffTheCommandLine(t *testing.T) {
	w := newWired(t, "n1", "n2", "n3", "n4")

	pwFile := filepath.Join(t.TempDir(), "pw")
	// echo 造出来的文件带换行，那几乎从来不是口令的一部分
	if err := os.WriteFile(pwFile, []byte("minio-root-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := w.mustRun("deploy", "minio", "-c", "obj", "--profile", "distributed",
		"--role", "server=n1,n2,n3,n4",
		"--set-file", "root_password=@"+pwFile)
	if !strings.Contains(out, "Deployed obj") {
		t.Fatalf("部署失败:\n%s", out)
	}
	// 口令不该出现在任何输出里
	if strings.Contains(out, "minio-root-secret") {
		t.Error("口令出现在了命令输出里")
	}

	// 值确实被用上了（结尾换行已去掉）
	comp, err := w.svc.Repos.Components().GetByName(t.Context(), w.site.ID, "obj")
	if err != nil {
		t.Fatal(err)
	}
	if got := comp.Params["root_password"]; got != "minio-root-secret" {
		t.Errorf("--set-file 应去掉结尾换行，实际 %q", got)
	}
}

// TestSetCoercesScalars 钉住 --set port=9090 是数字而非字符串。
//
// 参数按声明的 type 校验，一个字符串会在 port 上直接报错。
func TestSetCoercesScalars(t *testing.T) {
	for in, want := range map[string]any{
		"9090":  int64(9090),
		"true":  true,
		"false": false,
		"1.5":   1.5,
		"debug": "debug",
		"":      "",
	} {
		if got := coerceScalar(in); got != want {
			t.Errorf("coerceScalar(%q) = %#v，期望 %#v", in, got, want)
		}
	}
}

func TestParseRolesRejectsBadSyntax(t *testing.T) {
	if _, err := parseRoles([]string{"server"}); err == nil {
		t.Error("缺少 = 的 --role 应当报错")
	}
	if _, err := parseRoles([]string{"=n1"}); err == nil {
		t.Error("缺少角色名的 --role 应当报错")
	}
	got, err := parseRoles([]string{"server=n1, n2 ,n3"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got["server"], ",") != "n1,n2,n3" {
		t.Errorf("应当去掉空格，实际 %v", got["server"])
	}
}

// TestServerErrorReachesUserVerbatim 钉住服务端的错误文案原样端给用户。
//
// 那句话本来就是写给用户看的；再包一层「请求失败:」只会把真正有用的
// 内容挤到后面。
func TestServerErrorReachesUserVerbatim(t *testing.T) {
	w := newWired(t, "n1")
	_, _, err := w.run("deploy", "go-webapp", "--nodes", "ghost")
	if err == nil {
		t.Fatal("落在未注册节点上应当失败")
	}
	if !strings.Contains(err.Error(), "ghost") ||
		!strings.Contains(err.Error(), "is not registered") {
		t.Errorf("错误信息应原样来自服务端，实际: %v", err)
	}
}

// TestMissingTokenIsExplained 钉住没有 token 时的提示可操作。
func TestMissingTokenIsExplained(t *testing.T) {
	t.Setenv("MECHARION_ROOT", t.TempDir()) // 让默认 token 路径读不到东西
	_, err := NewClient(ClientConfig{Server: "https://127.0.0.1:1"})
	if err == nil {
		t.Fatal("没有 token 应当报错")
	}
	if !strings.Contains(err.Error(), "--token") {
		t.Errorf("错误信息应告诉用户怎么办，实际: %v", err)
	}
}

// TestJSONOutput 钉住 -o json 产出机器可读的结构。
func TestJSONOutput(t *testing.T) {
	w := newWired(t, "n1")
	w.mustRun("deploy", "go-webapp", "-c", "web", "--nodes", "n1")

	out := w.mustRun("status", "web", "-o", "json")
	var st mechd.StatusView
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("-o json 应产出合法 JSON: %v\n%s", err, out)
	}
	if st.Component != "web" || len(st.Instances) != 1 {
		t.Errorf("JSON 内容不对: %s", out)
	}
}

// reportConverged 模拟 mechlet 把期望的 digest 报回来。
func (w *wired) reportConverged(component, role string, nodes ...string) {
	w.t.Helper()
	st, err := w.svc.Status(w.t.Context(), "", component)
	if err != nil {
		w.t.Fatal(err)
	}
	want := map[string]string{}
	for _, in := range st.Instances {
		want[in.Node] = in.Want
	}
	b := &mechd.Backend{S: w.svc}
	for _, node := range nodes {
		if err := b.Report(w.t.Context(), protocol.Report{
			Node: node,
			Instances: []protocol.InstanceStatus{{
				Component: component, Role: role,
				Digest: want[node], Generation: 1, Result: "ok",
				Health: &protocol.HealthStatus{State: "healthy"},
				Workload: &protocol.WorkloadStatus{
					Runtime: "systemd", State: "running",
				},
			}},
		}); err != nil {
			w.t.Fatal(err)
		}
	}
}
