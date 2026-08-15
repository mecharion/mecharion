package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/protocol"
	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/vault"
)

// TestSaveRejectsPathEscapingKey 验证的是路径转义防护的第二道防线。
//
// Component 名此时已经被 internal/ident 在 mechd 落库时挡住了，但 Role
// 名来自 Pack 定义，只在可选的 `mechpack lint` 时被校验——一个没 lint
// 过的恶意 Pack 仍可能带着一个包含 ".." 的 Role 名被部署到这台机器。
// pathFor 必须在这种情况下拒绝写出边界之外的文件，而不是把它当成
// 一次普通的 os.WriteFile 失败悄悄放过。
//
// **算清楚需要几层 "../" 是这条测试的关键，写错会得到一条假绿。**
// InstanceKey 是 `Component + "__" + Role`，`pathFor` 直接把它当文件名
// （不加分隔符），所以 key 的第一个路径段永远是 "pg-main.."——这不是
// 字面量 ".."，filepath.Clean 不会把它当成「向上一级」，而是当成一个
// 古怪但合法的目录名字面量。第一个真正的 ".." 段只会把这个字面量段
// 顶掉，等于抵消，人还站在原地；要真正跳出 desired 目录，需要**再多
// 一层**。用 3 层 "../"：第 1 层抵消前缀，第 2 层跳出 desired，落到
// `root`（`newDesired` 建的目录结构是 root/desired，只有一层深）——
// `root` 是 t.TempDir()，真实存在且可写，因此逃逸与否能被准确观察到，
// 不会跟「目标目录不存在导致 WriteFile 本来就会失败」混在一起产生假绿。
func TestSaveRejectsPathEscapingKey(t *testing.T) {
	ds, root := newDesired(t)

	in := instSpec("pg-main", strings.Repeat("../", 3)+"evil")
	if err := ds.Save(in); err == nil {
		t.Fatal("越界的实例键应当被拒绝，实际写入成功")
	}

	escaped := filepath.Join(root, "evil.json")
	if _, statErr := os.Stat(escaped); statErr == nil {
		t.Fatalf("发现越界写入的文件: %s", escaped)
	}
}

func newDesired(t *testing.T) (*DesiredStore, string) {
	t.Helper()
	root := t.TempDir()
	fv, err := vault.OpenFile(filepath.Join(root, "vault"), vault.FileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ds, err := NewDesiredStore(filepath.Join(root, "desired"), fv)
	if err != nil {
		t.Fatal(err)
	}
	return ds, root
}

// instSpec 造一份最小的已解析规格。
func instSpec(component, role string, refs ...spec.SecretRef) protocol.InstanceSpec {
	s := &spec.ResolvedSpec{
		SchemaVersion: spec.SchemaVersion,
		Component:     component, Role: role, ConfigGroup: "default",
		Node:       spec.NodeRef{Name: "n1"},
		Pack:       spec.PackRef{Name: component, Version: "1.0.0"},
		Site:       spec.SiteRef{Name: "site1"},
		SecretRefs: refs,
	}
	in := protocol.InstanceSpec{Spec: s}
	if len(refs) > 0 {
		in.Secrets = map[string]string{}
		for _, r := range refs {
			in.Secrets[r.ID] = "value-of-" + r.Param
		}
	}
	return in
}

// TestDesiredSurvivesRestart 是 M5 第 1 步的核心：**重启后还知道该维持什么**。
//
// 没有这条，mechlet 一重启就只能干等下一次推送——而断连 + 重启正是最需要
// 它自己顶上的时候（ADR-0033）。
func TestDesiredSurvivesRestart(t *testing.T) {
	ds, root := newDesired(t)

	in := instSpec("pg-main", "primary",
		spec.SecretRef{ID: "pg-main.admin_password", Version: 1, Param: "admin_password"})
	if err := ds.Save(in); err != nil {
		t.Fatal(err)
	}

	// 重新打开一整套 —— 模拟 mechlet 重启
	fv2, err := vault.OpenFile(filepath.Join(root, "vault"), vault.FileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ds2, err := NewDesiredStore(filepath.Join(root, "desired"), fv2)
	if err != nil {
		t.Fatal(err)
	}

	got, err := ds2.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("应当读回 1 个实例，实际 %d", len(got))
	}
	if got[0].Spec.InstanceKey() != "pg-main__primary" {
		t.Errorf("实例键不对: %q", got[0].Spec.InstanceKey())
	}
	// **密钥也要回来**：没有它，带 secret 的组件重启后渲染不出配置
	if got[0].Secrets["pg-main.admin_password"] != "value-of-admin_password" {
		t.Errorf("密钥没读回来，实际 %v", got[0].Secrets)
	}

	// 读回来的规格必须能直接喂给调和器
	if _, err := spec.ResolveSecrets(got[0].Spec, got[0].Secrets); err != nil {
		t.Errorf("读回的规格应当可以解析密钥: %v", err)
	}
}

// TestDesiredKeepsNoPlaintextOnDisk 钉住规格文件里没有明文口令。
//
// 规格会进日志、进诊断包、被人 cat。密钥必须走 vault 那一侧。
func TestDesiredKeepsNoPlaintextOnDisk(t *testing.T) {
	ds, root := newDesired(t)
	if err := ds.Save(instSpec("app", "default",
		spec.SecretRef{ID: "app.pw", Version: 1, Param: "pw"})); err != nil {
		t.Fatal(err)
	}

	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		body, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(body), "value-of-pw") {
			t.Errorf("%s 里出现了明文口令", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestDesiredFileNamesAreReadable 钉住目录可被人直接看懂。
//
// 用哈希做文件名会让排障时的 `ls` 毫无信息量——而「这台机器上应该跑什么」
// 恰恰是现场第一个要问的问题（原则七 现场可诊断）。
func TestDesiredFileNamesAreReadable(t *testing.T) {
	ds, root := newDesired(t)
	if err := ds.Save(instSpec("pg-main", "primary")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "desired"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("应当有 1 个文件，实际 %d", len(entries))
	}
	if got := entries[0].Name(); got != "pg-main__primary.json" {
		t.Errorf("文件名应当一眼能看懂，实际 %q", got)
	}
}

// TestDesiredKeepRemovesGoneInstances 钉住全量之后的清理。
func TestDesiredKeepRemovesGoneInstances(t *testing.T) {
	ds, _ := newDesired(t)
	for _, in := range []protocol.InstanceSpec{
		instSpec("a", "default", spec.SecretRef{ID: "a.pw", Version: 1, Param: "pw"}),
		instSpec("b", "default", spec.SecretRef{ID: "b.pw", Version: 1, Param: "pw"}),
	} {
		if err := ds.Save(in); err != nil {
			t.Fatal(err)
		}
	}

	// **用真实调用方传的东西**：Apply 传的是 spec.InstanceKey()。
	// 测试里自己拼一个格式不同的键，会把「Keep 认不出还在用的实例」
	// 这类 bug 完全遮住——那正是这条测试要防的事故
	if err := ds.Keep(map[string]bool{instSpec("a", "default").Spec.InstanceKey(): true}); err != nil {
		t.Fatal(err)
	}
	got, err := ds.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Spec.Component != "a" {
		t.Fatalf("只该剩下 a，实际 %d 个", len(got))
	}
	// **凭据也要跟着走**：留在盘上就是一份没有主人的口令
	if _, ok, _ := ds.vault.Get("b.pw"); ok {
		t.Error("被移除实例的密钥应当一并清掉")
	}
	if _, ok, _ := ds.vault.Get("a.pw"); !ok {
		t.Error("还在的实例，密钥不该被清")
	}
}

// TestDesiredReportsMissingSecret 钉住密钥缺失是**错误**而不是静默跳过。
//
// 一个「因为密钥读不出来所以一直没被调和」的实例，从外部看与「一切正常」
// 完全一样——那是最坏的一类失败。
func TestDesiredReportsMissingSecret(t *testing.T) {
	ds, root := newDesired(t)
	if err := ds.Save(instSpec("app", "default",
		spec.SecretRef{ID: "app.pw", Version: 1, Param: "pw"})); err != nil {
		t.Fatal(err)
	}

	// 把保管库清掉，只留规格
	if err := os.Remove(filepath.Join(root, "vault", "secrets.json")); err != nil {
		t.Fatal(err)
	}
	_, err := ds.Load()
	if err == nil {
		t.Fatal("密钥缺失时应当报错，而不是返回一份缺值的规格")
	}
	if !strings.Contains(err.Error(), "pw") {
		t.Errorf("错误应指明缺的是哪个参数，实际: %v", err)
	}
}

// TestAppliedIsOnlyTheLastSuccessful 钉住回滚点的语义（M6 第 2 步）。
//
// 「上一个不同的规格」与「最后一次成功应用的规格」在正常路径上一样，
// 一旦出过错就分道扬镳：
//
//	v1 成功 → v2 失败 → 用户改参数成 v3
//	  上一版         = v2  ← 从来没成功过，回滚到它等于回滚到另一个坏版本
//	  最后成功应用的  = v1  ← 这才是能落脚的地方
//
// MarkApplied 只在调和成功之后被调用，因此 v2 永远不会成为回滚点。
func TestAppliedIsOnlyTheLastSuccessful(t *testing.T) {
	ds, _ := newDesired(t)

	v1 := instSpec("web", "default")
	v1.Spec.Pack.Version = "1.0.0"
	if err := ds.Save(v1); err != nil {
		t.Fatal(err)
	}
	if err := ds.MarkApplied(v1); err != nil { // v1 装成功了
		t.Fatal(err)
	}

	// v2 下发了，但**没有** MarkApplied——它失败了
	v2 := instSpec("web", "default")
	v2.Spec.Pack.Version = "2.0.0"
	if err := ds.Save(v2); err != nil {
		t.Fatal(err)
	}

	got, err := ds.Applied("web__default")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("应当有一个回滚点")
	}
	if got.Spec.Pack.Version != "1.0.0" {
		t.Errorf("回滚点应当是最后一次**成功**的 v1，实际 %s", got.Spec.Pack.Version)
	}
}

// TestAppliedIsNotLoadedAsDesired 钉住回滚点不会被当成期望状态。
//
// 它的文件名也以 .json 结尾。漏过滤的话，同一个组件会被调和两遍，
// 而第二遍用的是**旧版本**——机器会在新旧之间来回翻。
func TestAppliedIsNotLoadedAsDesired(t *testing.T) {
	ds, _ := newDesired(t)
	in := instSpec("web", "default")
	if err := ds.Save(in); err != nil {
		t.Fatal(err)
	}
	if err := ds.MarkApplied(in); err != nil {
		t.Fatal(err)
	}

	got, err := ds.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("回滚点不该被当成一个实例，期望 1 个，实际 %d", len(got))
	}
}

// TestKeepRetainsRollbackSecrets 钉住回滚点引用的密钥不被清掉。
//
// 升级时轮换了口令：新规格引用新 id，旧规格引用旧 id。只按新规格收敛的话，
// 旧 id 当场被清——而回滚要重新渲染配置文件，那时解不出明文，
// **回滚本身失败**。那是最不该失败的一步。
func TestKeepRetainsRollbackSecrets(t *testing.T) {
	ds, _ := newDesired(t)

	old := instSpec("pg", "primary",
		spec.SecretRef{ID: "pg.pw.v1", Version: 1, Param: "pw"})
	if err := ds.Save(old); err != nil {
		t.Fatal(err)
	}
	if err := ds.MarkApplied(old); err != nil {
		t.Fatal(err)
	}

	// 轮换：新规格引用一条新 id
	rotated := instSpec("pg", "primary",
		spec.SecretRef{ID: "pg.pw.v2", Version: 2, Param: "pw"})
	if err := ds.Save(rotated); err != nil {
		t.Fatal(err)
	}

	if err := ds.Keep(map[string]bool{"pg__primary": true}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := ds.vault.Get("pg.pw.v1"); !ok {
		t.Error("回滚点引用的旧密钥被清掉了——回滚时会解不出明文")
	}
	if _, ok, _ := ds.vault.Get("pg.pw.v2"); !ok {
		t.Error("当前规格引用的密钥不该被清")
	}
}

// TestKeepRemovesOrphanedRollbackPoint 钉住实例被移除时回滚点跟着走。
//
// 留下一个没有主人的回滚点，下次同名组件被部署时会拿到一份来路不明的旧规格。
func TestKeepRemovesOrphanedRollbackPoint(t *testing.T) {
	ds, root := newDesired(t)
	in := instSpec("gone", "default")
	if err := ds.Save(in); err != nil {
		t.Fatal(err)
	}
	if err := ds.MarkApplied(in); err != nil {
		t.Fatal(err)
	}

	if err := ds.Keep(map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "desired", "gone__default"+appliedSuffix)); err == nil {
		t.Error("实例已被移除，它的回滚点也该一并清掉")
	}
}
