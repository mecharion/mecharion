package reconcile

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/resource"
	"github.com/mecharion/mecharion/internal/spec"
)

// kafkaLike 构造一份「配置必须位于安装目录内」的规格——Kafka / Tomcat /
// Elasticsearch / ZooKeeper 都是这个形状。
func (f *fixture) kafkaLike(mut ...func(*spec.ResolvedSpec)) *spec.ResolvedSpec {
	f.t.Helper()
	return f.webappSpec(append([]func(*spec.ResolvedSpec){func(x *spec.ResolvedSpec) {
		p := x.Paths["config"]
		p.LinkInto = spec.GenerationPlaceholder + "/config"
		p.DistDir = "config"
		x.Paths["config"] = p
	}}, mut...)...)
}

// TestLinkIntoCreatesSymlink 钉住 linkInto 的核心行为。
//
// 应用看到 $HOME/config，配置真身在 /etc 下——跨版本存活、可备份、
// 可审计、升级不丢（04-paths-and-storage §3）。
func TestLinkIntoCreatesSymlink(t *testing.T) {
	f := newFixture(t)
	s := f.kafkaLike()

	rep := f.MustReconcile(s)

	link := filepath.Join(rep.GenerationDir, "config")
	got := readLink(t, link)
	want := s.Paths["config"].First()
	if got != want {
		t.Errorf("%s → %q，期望 %q", link, got, want)
	}

	// 透过软链能读到渲染出来的配置
	body, err := os.ReadFile(filepath.Join(link, "app.yaml"))
	if err != nil {
		t.Fatalf("应当能透过软链读到配置: %v", err)
	}
	if string(body) != "port: 8080\n" {
		t.Errorf("内容 = %q", body)
	}
}

// TestLinkIntoPreservesDistDir 钉住载荷自带的配置目录被改名保留。
//
// 升级时新版本可能新增或重命名配置项。保留每个 generation 的默认值基线，
// `config diff --from --to` 才有得比。所有包管理器都在这件事上栽过跟头
// （Debian 的 .dpkg-dist、RPM 的 .rpmnew）。
func TestLinkIntoPreservesDistDir(t *testing.T) {
	f := newFixture(t)
	s := f.kafkaLike()

	// 模拟 tarball 自带 config/ ——用一个 file 资源在 generation 里造出来
	s.Resources = append([]spec.Resource{{
		ID: "file:dist", Type: "file",
		Args: mustJSON(map[string]any{
			"path":    spec.GenerationPlaceholder + "/config/server.properties",
			"content": "# 上游默认值\nnum.partitions=1\n",
		}),
	}}, s.Resources...)

	rep := f.MustReconcile(s)

	dist := filepath.Join(rep.GenerationDir, "config.dist", "server.properties")
	body, err := os.ReadFile(dist)
	if err != nil {
		t.Fatalf("载荷自带的 config/ 应当被改名为 config.dist/ 保留: %v", err)
	}
	if !strings.Contains(string(body), "num.partitions=1") {
		t.Errorf("基线内容 = %q", body)
	}

	// 原位置现在是软链
	link := filepath.Join(rep.GenerationDir, "config")
	if _, err := os.Readlink(link); err != nil {
		t.Errorf("%s 应当是软链: %v", link, err)
	}
}

// TestLinkIntoWithoutDistDirIsRejected 钉住「有同名目录却没声明 distDir」报错。
//
// 引擎不能替 Pack 作者决定那个目录是删是留——删掉可能丢基线，
// 留着又建不了软链。
func TestLinkIntoWithoutDistDirIsRejected(t *testing.T) {
	f := newFixture(t)
	s := f.kafkaLike(func(x *spec.ResolvedSpec) {
		p := x.Paths["config"]
		p.DistDir = ""
		x.Paths["config"] = p
	})
	s.Resources = append([]spec.Resource{{
		ID: "file:dist", Type: "file",
		Args: mustJSON(map[string]any{
			"path":    spec.GenerationPlaceholder + "/config/x.properties",
			"content": "x\n",
		}),
	}}, s.Resources...)

	_, err := f.Reconcile(s)
	if err == nil {
		t.Fatal("载荷自带同名目录却没声明 distDir，应当报错")
	}
	if !strings.Contains(err.Error(), "distDir") {
		t.Errorf("错误信息应指出该声明 distDir: %v", err)
	}
}

// TestLinkIntoIsIdempotent 钉住重复调和不会把软链变成 .dist。
func TestLinkIntoIsIdempotent(t *testing.T) {
	f := newFixture(t)
	s := f.kafkaLike()
	rep := f.MustReconcile(s)

	for i := 0; i < 3; i++ {
		f.MustReconcile(s)
	}

	link := filepath.Join(rep.GenerationDir, "config")
	if got := readLink(t, link); got != s.Paths["config"].First() {
		t.Errorf("重复调和后软链 = %q", got)
	}
	if exists(filepath.Join(rep.GenerationDir, "config.dist")) {
		t.Error("软链不该被反复改名成 .dist")
	}
}

// TestLinkIntoMustStayInsideGeneration 钉住 linkInto 不能指到 generation 之外。
func TestLinkIntoMustStayInsideGeneration(t *testing.T) {
	f := newFixture(t)
	s := f.kafkaLike(func(x *spec.ResolvedSpec) {
		p := x.Paths["config"]
		p.LinkInto = f.path("tmp", "somewhere-else")
		x.Paths["config"] = p
	})

	_, err := f.Reconcile(s)
	if err == nil {
		t.Fatal("linkInto 指到 generation 之外应当被拒绝")
	}
	if !strings.Contains(err.Error(), "generation") {
		t.Errorf("错误信息应说明原因: %v", err)
	}
}

// TestLinkIntoRejectedForMultiPath 钉住多盘路径不能声明 linkInto。
func TestLinkIntoRejectedForMultiPath(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec(func(x *spec.ResolvedSpec) {
		x.Paths["dataDirs"] = spec.PathValue{
			Name: "dataDirs", Kind: "multi",
			Values:   []string{f.path("data1", "dfs"), f.path("data2", "dfs")},
			LinkInto: spec.GenerationPlaceholder + "/data",
		}
	})

	_, err := f.Reconcile(s)
	if err == nil {
		t.Fatal("一条软链指不了多块盘，应当报错")
	}
}

// TestMultiPathCreatesEveryDisk 钉住多盘路径逐个创建。
//
// 这条规则消除了「为每块盘写一个 directory 资源」的需求，因而
// resources 不需要迭代机制（spec §8.3）。
func TestMultiPathCreatesEveryDisk(t *testing.T) {
	f := newFixture(t)
	dirs := []string{
		f.path("data1", "apps", "hdfs", "dfs", "dn"),
		f.path("data2", "apps", "hdfs", "dfs", "dn"),
		f.path("data3", "apps", "hdfs", "dfs", "dn"),
	}
	s := f.webappSpec(func(x *spec.ResolvedSpec) {
		x.Paths["dataDirs"] = spec.PathValue{
			Name: "dataDirs", Kind: "multi", Values: dirs, Mode: "0700",
		}
	})

	f.MustReconcile(s)

	for _, d := range dirs {
		fi, err := os.Stat(d)
		if err != nil {
			t.Errorf("%s 未创建: %v", d, err)
			continue
		}
		if !fi.IsDir() {
			t.Errorf("%s 不是目录", d)
		}
	}
}

// TestRelativePathIsRejected 钉住相对路径被拒。
func TestRelativePathIsRejected(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec(func(x *spec.ResolvedSpec) {
		x.Paths["bad"] = spec.PathValue{
			Name: "bad", Values: []string{"relative/dir"}, Kind: "single",
		}
	})

	_, err := f.Reconcile(s)
	if err == nil {
		t.Fatal("相对路径应当被拒绝——mechlet 的工作目录不是任何组件的目录")
	}
	if !strings.Contains(err.Error(), "绝对路径") {
		t.Errorf("错误信息 = %v", err)
	}
}

// TestCurrentLinkRefusesToClobberRealDir 钉住不覆盖真实目录。
func TestCurrentLinkRefusesToClobberRealDir(t *testing.T) {
	f := newFixture(t)
	s := f.webappSpec()
	home := s.Paths["home"].First()

	// 有人在 current 的位置放了一个真实目录
	if err := os.MkdirAll(filepath.Join(home, CurrentLink, "用户的东西"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := f.Reconcile(s)
	if err == nil {
		t.Fatal("current 被真实目录占着时应当拒绝，而不是删掉它")
	}
	if !strings.Contains(err.Error(), "不是软链") {
		t.Errorf("错误信息 = %v", err)
	}
	if !exists(filepath.Join(home, CurrentLink, "用户的东西")) {
		t.Error("失败路径不该动到用户的东西")
	}
}

// TestGenerationDirNaming 钉住目录名带上版本与 revision。
//
// `ls generations/` 就能看出装过哪些版本——这是给现场排障的人看的。
func TestGenerationDirNaming(t *testing.T) {
	cases := []struct {
		version  string
		revision int
		want     string
	}{
		{"16.4", 1, "0007-16.4-1"},
		{"1.2.0", 3, "0007-1.2.0-3"},
		{"", 0, "0007"},
		{"3.6.0-rc1", 1, "0007-3.6.0-rc1-1"},
		{"v1/2", 1, "0007-v1-2-1"}, // 斜杠不能进目录名
	}
	for _, tc := range cases {
		got := generationDir("/opt/x", 7, spec.PackRef{
			Version: tc.version, Revision: tc.revision,
		})
		want := filepath.Join("/opt/x", "generations", tc.want)
		if got != want {
			t.Errorf("version=%q revision=%d → %q，期望 %q",
				tc.version, tc.revision, got, want)
		}
	}
}

// TestPathModeYieldsToDirectoryResource 钉住「引擎不和自己打架」。
//
// 同一路径既在 paths 里（默认 mode 0755）、又有一条 directory 资源
// （mode 0750 + owner）。这是 go-webapp 的真实写法，也是规范允许的。
//
// 两处都强制的话，阶段① 每轮把它 chmod 回 0755，阶段② 报一次「漂移」但
// 按 report 策略不改——**Pack 声明的 0750 从此永不生效**，而运维每轮看到
// 一条假漂移。这个 bug 在只有推送触发时完全不可见，是周期调和第一天就
// 暴露出来的东西。
func TestPathModeYieldsToDirectoryResource(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "data")

	args, err := json.Marshal(map[string]any{"path": target, "mode": "0750"})
	if err != nil {
		t.Fatal(err)
	}
	s := &spec.ResolvedSpec{
		Paths: map[string]spec.PathValue{
			"data": {Name: "data", Values: []string{target}, Mode: "0755"},
		},
		Resources: []spec.Resource{
			{ID: "directory:" + target, Type: "directory", Args: args},
		},
	}

	env := &resource.Env{}
	if err := createPaths(context.Background(), env, s); err != nil {
		t.Fatal(err)
	}

	// 先手工设成资源声明的那个值，再跑一次阶段①——它不该被改回去
	if err := os.Chmod(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := createPaths(context.Background(), env, s); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不强制 Unix 权限位")
	}
	if got := fi.Mode().Perm(); got != 0o750 {
		t.Errorf("有 directory 资源接管时，阶段① 不该按 paths 的 mode 改回去；"+
			"期望 0750，实际 %04o", got)
	}
}
