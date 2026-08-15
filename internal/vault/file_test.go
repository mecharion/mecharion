package vault

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openFile(t *testing.T, dir string) *FileVault {
	t.Helper()
	v, err := OpenFile(dir, FileOptions{})
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	return v
}

// TestFileVaultRoundTrip 是最基本的一条：存进去能取出来，且跨进程有效。
//
// 「跨进程」是重点——这个库存在的全部意义就是 mechlet 重启后还能用
// （ADR-0033）。只在同一个 FileVault 实例上验证，等于什么都没验证。
func TestFileVaultRoundTrip(t *testing.T) {
	dir := t.TempDir()

	v := openFile(t, dir)
	if err := v.Merge(map[string]string{
		"pg-main.admin_password": "s3cr3t",
		"minio.root_password":    "minio-pw",
	}); err != nil {
		t.Fatal(err)
	}

	// 重新打开 —— 模拟 mechlet 重启
	v2 := openFile(t, dir)
	got, ok, err := v2.Get("pg-main.admin_password")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != "s3cr3t" {
		t.Errorf("重开之后应当取回原值，实际 (%q, %v)", got, ok)
	}
}

// TestFileVaultStoresCiphertext 钉住**盘上没有明文**。
//
// 这条测的是这个包的存在理由。少了它，一个把 encrypt 写成恒等函数的
// 改动能让上面那条测试照常通过。
func TestFileVaultStoresCiphertext(t *testing.T) {
	dir := t.TempDir()
	v := openFile(t, dir)
	if !v.Enabled() {
		t.Fatal("默认应当开启加密")
	}
	if err := v.Merge(map[string]string{"x": "PLAINTEXT-CANARY"}); err != nil {
		t.Fatal(err)
	}

	// 整个目录里都不该出现明文
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		body, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(body), "PLAINTEXT-CANARY") {
			t.Errorf("%s 里出现了明文口令", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestFileVaultFilesArePrivate 钉住权限。
//
// 一份 0644 的密钥库等于没有密钥库——同机任何用户都读得到密文，
// 而 KEK 就在旁边。
func TestFileVaultFilesArePrivate(t *testing.T) {
	if !permEnforced {
		t.Skip("本平台不强制 Unix 权限位")
	}
	dir := t.TempDir()
	v := openFile(t, dir)
	if err := v.Merge(map[string]string{"x": "v"}); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]os.FileMode{
		"kek":       0o400,
		"dek":       0o600,
		secretsFile: 0o600,
	} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s 的权限应为 %04o，实际 %04o", name, want, got)
		}
	}
}

// TestFileVaultAADBindsToID 钉住密文不能被搬家。
//
// 没有 AAD 绑定的话，把测试组件的弱口令挪到生产组件的 id 下就能生效。
// 这条测试直接在盘上做那件事，确认解密失败而不是返回错值。
func TestFileVaultAADBindsToID(t *testing.T) {
	dir := t.TempDir()
	v := openFile(t, dir)
	if err := v.Merge(map[string]string{
		"weak.pw":   "123456",
		"strong.pw": "correct-horse-battery",
	}); err != nil {
		t.Fatal(err)
	}

	// 把 weak 的密文搬到 strong 的位置
	path := filepath.Join(dir, secretsFile)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]string
	if err := json.Unmarshal(body, &all); err != nil {
		t.Fatal(err)
	}
	all["strong.pw"] = all["weak.pw"]
	out, _ := json.Marshal(all)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}

	v2 := openFile(t, dir)
	if _, _, err := v2.Get("strong.pw"); err == nil {
		t.Fatal("被搬过来的密文应当解不开，而不是解出另一个口令")
	}
}

// TestFileVaultKeepRemovesOrphans 钉住组件移除后凭据不残留。
//
// 一份没有主人的口令留在盘上，没人会想起去清它。
func TestFileVaultKeepRemovesOrphans(t *testing.T) {
	dir := t.TempDir()
	v := openFile(t, dir)
	if err := v.Merge(map[string]string{"a.pw": "1", "b.pw": "2"}); err != nil {
		t.Fatal(err)
	}
	if err := v.Keep(map[string]bool{"a.pw": true}); err != nil {
		t.Fatal(err)
	}

	if _, ok, _ := v.Get("b.pw"); ok {
		t.Error("不在保留集合里的密钥应当被删掉")
	}
	if _, ok, _ := v.Get("a.pw"); !ok {
		t.Error("在保留集合里的密钥不该被删")
	}
}

// TestFileVaultRefusesNewKEKWhenDEKExists 钉住一个会毁掉全部密钥的场景。
//
// KEK 丢了而 DEK 还在：偷偷生成一把新 KEK 会让全部已存密钥变成解不开的
// 字节，而症状是**组件拿到了空口令**——根因离现场极远。必须当场拒绝，
// 并说清楚补救办法（节点侧只是缓存，让 mechd 重推即可）。
func TestFileVaultRefusesNewKEKWhenDEKExists(t *testing.T) {
	dir := t.TempDir()
	v := openFile(t, dir)
	if err := v.Merge(map[string]string{"a.pw": "1"}); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(dir, "kek")); err != nil {
		t.Fatal(err)
	}
	_, err := OpenFile(dir, FileOptions{})
	if err == nil {
		t.Fatal("KEK 丢失而 DEK 还在时应当拒绝启动")
	}
	if !strings.Contains(err.Error(), "resend") {
		t.Errorf("错误应给出补救办法，实际: %v", err)
	}
}

// TestFileVaultRefusesDisablingAfterEncrypting 钉住「原本加密过、现在关掉了」。
//
// 不拦的话已有密文会被当成明文读出来，变成一串乱码口令下发给组件。
func TestFileVaultRefusesDisablingAfterEncrypting(t *testing.T) {
	dir := t.TempDir()
	v := openFile(t, dir)
	if err := v.Merge(map[string]string{"a.pw": "1"}); err != nil {
		t.Fatal(err)
	}

	if _, err := OpenFile(dir, FileOptions{Disabled: true}); err == nil {
		t.Fatal("已有密文时不该允许关闭加密")
	}
}

// TestFileVaultDisabledStillPrivate 钉住关掉加密不等于放开权限。
func TestFileVaultDisabledStillPrivate(t *testing.T) {
	dir := t.TempDir()
	v, err := OpenFile(dir, FileOptions{Disabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if v.Enabled() {
		t.Fatal("Disabled 时不该报告已加密")
	}
	if err := v.Merge(map[string]string{"a.pw": "plain-value"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := v.Get("a.pw")
	if err != nil || !ok || got != "plain-value" {
		t.Fatalf("明文模式下也要能读写，实际 (%q, %v, %v)", got, ok, err)
	}
	if permEnforced {
		fi, err := os.Stat(filepath.Join(dir, secretsFile))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("明文模式下权限更要紧，应为 0600，实际 %04o", got)
		}
	}
}
