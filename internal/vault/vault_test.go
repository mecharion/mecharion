package vault

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/store"
)

type fixture struct {
	t       *testing.T
	dir     string
	store   *store.Store
	keyPath string
	compID  int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(context.Background(), store.Options{
		Path: filepath.Join(dir, "mechd.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	f := &fixture{t: t, dir: dir, store: s, keyPath: filepath.Join(dir, "secret.key")}
	f.compID = f.seedComponent()
	return f
}

// seedComponent 建一个 Component，密钥要挂在它下面（外键约束）。
func (f *fixture) seedComponent() int64 {
	f.t.Helper()
	now := store.FormatTime(f.store.Now())
	r, err := f.store.Writer().Exec(
		`INSERT INTO sites (name,kind,created_at) VALUES ('s1','standalone',?)`, now)
	if err != nil {
		f.t.Fatal(err)
	}
	siteID, _ := r.LastInsertId()

	r, err = f.store.Writer().Exec(
		`INSERT INTO components (site_id,name,pack_name,pack_version,created_at,updated_at)
		 VALUES (?,'pg-main','postgresql','16.4',?,?)`, siteID, now, now)
	if err != nil {
		f.t.Fatal(err)
	}
	id, _ := r.LastInsertId()
	return id
}

func (f *fixture) open() *Vault {
	f.t.Helper()
	v, err := Open(context.Background(), f.store, Options{KeyPath: f.keyPath})
	if err != nil {
		f.t.Fatalf("打开保管库: %v", err)
	}
	return v
}

func ctx() context.Context { return context.Background() }

// ── 基本往返 ────────────────────────────────────────────────────────────

func TestPutGetRoundTrip(t *testing.T) {
	f := newFixture(t)
	v := f.open()

	ver, err := v.Put(ctx(), f.compID, "app_password", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if ver != 1 {
		t.Errorf("首次写入版本 = %d，期望 1", ver)
	}

	got, ver, ok, err := v.Get(ctx(), f.compID, "app_password")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != "hunter2" || ver != 1 {
		t.Errorf("读回 (%q, %d, %v)", got, ver, ok)
	}

	_, _, ok, err = v.Get(ctx(), f.compID, "从来没写过")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("不存在的参数应当返回 ok=false")
	}
}

// TestCiphertextIsNotPlaintext 钉住库里存的不是明文。
func TestCiphertextIsNotPlaintext(t *testing.T) {
	f := newFixture(t)
	v := f.open()
	const secret = "correct-horse-battery-staple"

	if _, err := v.Put(ctx(), f.compID, "app_password", secret); err != nil {
		t.Fatal(err)
	}

	var blob []byte
	if err := f.store.Reader().QueryRow(
		`SELECT ciphertext FROM secrets WHERE component_id=? AND param='app_password'`,
		f.compID).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), secret) {
		t.Error("数据库里出现了明文——信封加密没生效")
	}
	if len(blob) <= len(secret) {
		t.Errorf("密文长度 %d 不合理（应含 nonce 与认证标签）", len(blob))
	}
}

// TestRotationBumpsVersion 钉住轮换让版本递增。
//
// version 参与 spec digest，因此轮换必然产生新 generation——否则默认
// driftPolicy: report 会让它永远发不出去（16-secrets §5）。
func TestRotationBumpsVersion(t *testing.T) {
	f := newFixture(t)
	v := f.open()

	v1, _, _, err := v.Generate(ctx(), f.compID, "pw", pack.Generate{Length: 32})
	if err != nil {
		t.Fatal(err)
	}

	v2, ver2, err := v.Rotate(ctx(), f.compID, "pw", pack.Generate{Length: 32})
	if err != nil {
		t.Fatal(err)
	}
	if ver2 != 2 {
		t.Errorf("轮换后版本 = %d，期望 2", ver2)
	}
	if v2 == v1 {
		t.Error("轮换必须换出新值")
	}

	got, ver, _, _ := v.Get(ctx(), f.compID, "pw")
	if got != v2 || ver != 2 {
		t.Errorf("读回 (%q, %d)，期望 (%q, 2)", got, ver, v2)
	}
}

// ── 验收点一：换机器解不开 ──────────────────────────────────────────────

// TestCiphertextUselessWithoutMatchingKey 钉住信封加密的**唯一实际收益**。
//
// 拷走数据库而没拷走主密钥时，密文毫无用处。这正是它针对的场景：
// 备份、快照、支持包、误配的同步。
func TestCiphertextUselessWithoutMatchingKey(t *testing.T) {
	f := newFixture(t)
	v := f.open()
	if _, err := v.Put(ctx(), f.compID, "pw", "hunter2"); err != nil {
		t.Fatal(err)
	}

	// 模拟「数据库被拷走，攻击者自己造了一把密钥」
	other := filepath.Join(f.dir, "attacker.key")
	if err := os.WriteFile(other,
		[]byte(strings.Repeat("ab", KeySize)+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}

	_, err := Open(ctx(), f.store, Options{KeyPath: other})
	if err == nil {
		t.Fatal("用另一把主密钥应当打不开")
	}
	if !strings.Contains(err.Error(), "don't match") {
		t.Errorf("错误信息应说明是密钥与数据库不匹配，实际: %v", err)
	}
}

// ── 验收点二：主密钥缺失时拒绝启动 ──────────────────────────────────────

// TestRefusesToStartWithoutKey 钉住「不静默把密钥当空值」。
//
// 静默降级会让所有依赖它的组件拿到空口令并在启动时莫名其妙地失败，
// 而根因在完全不相干的地方。
func TestRefusesToStartWithoutKey(t *testing.T) {
	f := newFixture(t)
	v := f.open()
	if _, err := v.Put(ctx(), f.compID, "pw", "hunter2"); err != nil {
		t.Fatal(err)
	}

	// 主密钥丢了（备份没带上它）
	if err := os.Remove(f.keyPath); err != nil {
		t.Fatal(err)
	}

	_, err := Open(ctx(), f.store, Options{KeyPath: f.keyPath})
	if err == nil {
		t.Fatal("库里有密文而密钥不在时必须拒绝启动")
	}
	if !errors.Is(err, ErrKeyMissing) {
		t.Errorf("应当是 ErrKeyMissing，实际 %v", err)
	}
	// 错误信息要能直接指导运维
	for _, want := range []string{f.keyPath, "backed up separately", "regenerated"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q，实际:\n%v", want, err)
		}
	}
}

// TestFreshInstallGeneratesKey 钉住首次安装能自己起来。
func TestFreshInstallGeneratesKey(t *testing.T) {
	f := newFixture(t)

	v := f.open()
	if !v.Enabled() {
		t.Error("默认应当开启加密")
	}
	fi, err := os.Stat(f.keyPath)
	if err != nil {
		t.Fatalf("应当生成主密钥: %v", err)
	}
	if perm := fi.Mode().Perm(); permEnforced && perm&0o077 != 0 {
		t.Errorf("主密钥权限 = %04o，对属主之外可读", perm)
	}

	// 重开一次应当复用同一把密钥，读得回原值
	if _, err := v.Put(ctx(), f.compID, "pw", "x"); err != nil {
		t.Fatal(err)
	}
	v2 := f.open()
	if got, _, _, _ := v2.Get(ctx(), f.compID, "pw"); got != "x" {
		t.Errorf("重开后读回 %q", got)
	}
}

// TestRejectsWorldReadableKey 钉住权限过松等同于没加密。
func TestRejectsWorldReadableKey(t *testing.T) {
	if !permEnforced {
		t.Skip("本平台不实现 Unix 权限位")
	}
	f := newFixture(t)
	_ = f.open() // 先生成密钥

	if err := os.Chmod(f.keyPath, 0o644); err != nil {
		t.Skipf("改不了权限: %v", err)
	}
	fi, _ := os.Stat(f.keyPath)
	if fi.Mode().Perm()&0o077 == 0 {
		t.Skip("本平台不支持该权限位")
	}

	_, err := Open(ctx(), f.store, Options{KeyPath: f.keyPath})
	if err == nil {
		t.Fatal("全体可读的主密钥应当被拒绝")
	}
	if !strings.Contains(err.Error(), "chmod") {
		t.Errorf("错误信息应给出可执行的修复命令，实际: %v", err)
	}
}

// ── AAD 绑定 ────────────────────────────────────────────────────────────

// TestCiphertextCannotBeMovedBetweenRows 钉住密文绑定到 (component, param)。
//
// 没有这条绑定，一条密文可以被从一行搬到另一行——把测试库里的弱口令挪到
// 生产组件上，或把低权限账号的口令挪成 superuser 的。
func TestCiphertextCannotBeMovedBetweenRows(t *testing.T) {
	f := newFixture(t)
	v := f.open()

	if _, err := v.Put(ctx(), f.compID, "app_password", "weak"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Put(ctx(), f.compID, "admin_password", "strong"); err != nil {
		t.Fatal(err)
	}

	// 把 app_password 的密文搬到 admin_password 上
	var blob []byte
	if err := f.store.Reader().QueryRow(
		`SELECT ciphertext FROM secrets WHERE component_id=? AND param='app_password'`,
		f.compID).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Writer().Exec(
		`UPDATE secrets SET ciphertext=? WHERE component_id=? AND param='admin_password'`,
		blob, f.compID); err != nil {
		t.Fatal(err)
	}

	_, _, _, err := v.Get(ctx(), f.compID, "admin_password")
	if err == nil {
		t.Fatal("被搬动过的密文必须解不开")
	}
	if !strings.Contains(err.Error(), "moved") {
		t.Errorf("错误信息应提示密文可能被搬动，实际: %v", err)
	}
}

// ── 明文模式 ────────────────────────────────────────────────────────────

func TestDisabledStoresPlaintext(t *testing.T) {
	f := newFixture(t)
	v, err := Open(ctx(), f.store, Options{KeyPath: f.keyPath, Disabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if v.Enabled() {
		t.Error("Disabled 时不该报告已启用")
	}
	if _, err := os.Stat(f.keyPath); err == nil {
		t.Error("关闭加密时不该生成主密钥")
	}

	if _, err := v.Put(ctx(), f.compID, "pw", "hunter2"); err != nil {
		t.Fatal(err)
	}
	if got, _, _, _ := v.Get(ctx(), f.compID, "pw"); got != "hunter2" {
		t.Errorf("明文模式读回 %q", got)
	}
}

// TestCannotDisableWithExistingCiphertext 钉住不能悄悄降级。
//
// 已有密文时关掉加密，会让它们变成读不出来的乱码，而错误出现在离根因
// 很远的地方。
func TestCannotDisableWithExistingCiphertext(t *testing.T) {
	f := newFixture(t)
	v := f.open()
	if _, err := v.Put(ctx(), f.compID, "pw", "hunter2"); err != nil {
		t.Fatal(err)
	}

	_, err := Open(ctx(), f.store, Options{KeyPath: f.keyPath, Disabled: true})
	if err == nil {
		t.Fatal("已有密文时不该允许关闭加密")
	}
	if !strings.Contains(err.Error(), "already has") {
		t.Errorf("错误信息 = %v", err)
	}
}
