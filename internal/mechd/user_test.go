package mechd

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/password"
)

// 本文件钉住 **M8 第 2 步**：单一 admin 账号与首访初始化。
//
// 这一步没有界面，但它决定了将来谁能进那个界面。安全相关的几条尤其要钉死
// ——它们坏掉的时候不会报错，只会安静地少一层防护。

const goodPW = "correct horse battery staple"

// TestBootstrapCreatesTheOnlyAdmin 钉住首访初始化这条主路径。
func TestBootstrapCreatesTheOnlyAdmin(t *testing.T) {
	f := newFixture(t)

	if done, _ := f.svc.Initialized(ctx()); done {
		t.Fatal("一台新机器不该已经初始化")
	}
	v, err := f.svc.InitializeAdmin(ctx(), goodPW, "10.0.0.9")
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != AdminUser || !v.Initialized {
		t.Fatalf("初始化结果不对: %+v", v)
	}
	if done, _ := f.svc.Initialized(ctx()); !done {
		t.Error("初始化之后状态应当变过来")
	}
}

// TestBootstrapIsOneShot 是这一步最重要的一条。
//
// 首访初始化的代价是**初始化之前有一个抢注窗口**（ADR-0037）。
// 唯一让那个窗口关得死的，就是「设过一次之后永久拒绝」——
// 少了它，任何人随时都能把管理员口令改成自己的。
func TestBootstrapIsOneShot(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.InitializeAdmin(ctx(), goodPW, "10.0.0.9"); err != nil {
		t.Fatal(err)
	}

	_, err := f.svc.InitializeAdmin(ctx(), "some other long password", "10.0.0.66")
	if !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("第二次初始化必须被拒，实际 %v", err)
	}
	// 原来的口令没被顶掉
	if err := f.svc.Authenticate(ctx(), AdminUser, goodPW); err != nil {
		t.Errorf("原口令应当仍然有效: %v", err)
	}
}

// TestBootstrapRecordsWhereItCameFrom 钉住来源进审计。
//
// 抢注窗口是接受了的风险，那么**事后至少要能回答「是谁初始化的」**。
func TestBootstrapRecordsWhereItCameFrom(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.InitializeAdmin(ctx(), goodPW, "10.0.0.9"); err != nil {
		t.Fatal(err)
	}
	entries, err := f.svc.Repos.Events().ListAudit(ctx(), 50)
	if err != nil {
		t.Fatal(err)
	}
	blob := mustJSON(t, entries)
	if !strings.Contains(blob, "10.0.0.9") {
		t.Errorf("审计里应当留下初始化的来源地址:\n%s", blob)
	}
	if strings.Contains(blob, goodPW) || strings.Contains(blob, "argon2") {
		t.Errorf("审计里不该出现口令或哈希:\n%s", blob)
	}
}

// TestAdminViewNeverCarriesTheHash 钉住哈希不出现在任何响应里。
//
// 它不可逆，但泄漏出去就等于把**离线爆破**的门打开了——攻击者可以拿去慢慢
// 试，不受限流约束。
func TestAdminViewNeverCarriesTheHash(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.InitializeAdmin(ctx(), goodPW, "local"); err != nil {
		t.Fatal(err)
	}
	u, err := f.svc.Repos.Users().GetByName(ctx(), AdminUser)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u.PasswordHash, "$argon2id$") {
		t.Fatalf("库里应当是 argon2id 编码串，实际 %q", u.PasswordHash)
	}

	v, err := f.svc.AdminStatus(ctx())
	if err != nil {
		t.Fatal(err)
	}
	blob := mustJSON(t, v)
	for _, leak := range []string{"argon2", u.PasswordHash, goodPW} {
		if strings.Contains(blob, leak) {
			t.Errorf("对外视图泄漏了 %q:\n%s", leak, blob)
		}
	}
}

// TestAuthenticate 钉住登录判定。
func TestAuthenticate(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.InitializeAdmin(ctx(), goodPW, "local"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Authenticate(ctx(), AdminUser, goodPW); err != nil {
		t.Errorf("对的口令应当通过: %v", err)
	}
	if err := f.svc.Authenticate(ctx(), AdminUser, "wrong password here"); err == nil {
		t.Error("错的口令应当被拒")
	}
}

// TestAuthChecksTheNameToo 钉住用户名也要对。
//
// 只有一个账号不等于「名字随便填都行」——那样的实现在将来加账号时会变成
// 一个洞，而那时没人会想起来这里从没校验过。
func TestAuthChecksTheNameToo(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.InitializeAdmin(ctx(), goodPW, "local"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Authenticate(ctx(), "root", goodPW); err == nil {
		t.Error("用别的名字配对的口令不该通过")
	}
}

// TestAuthDoesNotRevealWhetherUserExists 钉住两种失败无从区分。
//
// 「用户名不对」与「口令错」说成两句话，等于白送一个枚举接口。
func TestAuthDoesNotRevealWhetherUserExists(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.InitializeAdmin(ctx(), goodPW, "local"); err != nil {
		t.Fatal(err)
	}
	errNoUser := f.svc.Authenticate(ctx(), "nobody", "some password here")
	errBadPW := f.svc.Authenticate(ctx(), AdminUser, "some password here")

	if errNoUser == nil || errBadPW == nil {
		t.Fatal("两种情况都该失败")
	}
	if errNoUser.Error() != errBadPW.Error() {
		t.Errorf("两种失败的措辞必须一致：\n  名字不对: %v\n  口令错: %v",
			errNoUser, errBadPW)
	}
	if !errors.Is(errNoUser, password.ErrMismatch) {
		t.Errorf("应当是 ErrMismatch，实际 %v", errNoUser)
	}
}

// TestAuthFailsBeforeInitialization 钉住没初始化时登不进去。
func TestAuthFailsBeforeInitialization(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.Authenticate(ctx(), AdminUser, goodPW); !errors.Is(err, password.ErrMismatch) {
		t.Errorf("还没初始化时任何口令都该被拒，实际 %v", err)
	}
}

// TestPasswordLengthFloor 钉住长度下限。
//
// **只管长度，不管「必须含大写数字符号」**：那类规则被反复证明会把人推向
// `Passw0rd!` 这种可预测模式，而长度才真正拉高爆破成本。
func TestPasswordLengthFloor(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.InitializeAdmin(ctx(), "short", "local")
	if err == nil {
		t.Fatal("过短的口令应当被拒")
	}
	if !strings.Contains(err.Error(), "12") {
		t.Errorf("错误信息该说清下限，实际: %v", err)
	}
	// 长度够就行，不强制符号
	if _, err := f.svc.InitializeAdmin(ctx(), "aaaaaaaaaaaaaaa", "local"); err != nil {
		t.Errorf("长度够的纯字母口令应当被接受: %v", err)
	}
}

// TestSetPasswordInvalidatesOld 钉住改完口令旧的就不认了。
func TestSetPasswordInvalidatesOld(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.InitializeAdmin(ctx(), goodPW, "local"); err != nil {
		t.Fatal(err)
	}
	newPW := "a different long password"
	if err := f.svc.SetAdminPassword(ctx(), newPW, "tester"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Authenticate(ctx(), AdminUser, goodPW); err == nil {
		t.Error("旧口令改完之后不该还能用")
	}
	if err := f.svc.Authenticate(ctx(), AdminUser, newPW); err != nil {
		t.Errorf("新口令应当能用: %v", err)
	}
}

// TestSetPasswordBeforeInitSaysWhatToDo 钉住没初始化时改口令给得出出路。
func TestSetPasswordBeforeInitSaysWhatToDo(t *testing.T) {
	f := newFixture(t)
	err := f.svc.SetAdminPassword(ctx(), goodPW, "tester")
	if err == nil {
		t.Fatal("还没初始化时不该能改口令")
	}
	if !strings.Contains(err.Error(), "initialize") {
		t.Errorf("错误信息该告诉他去做什么，实际: %v", err)
	}
}

// TestResetReopensTheWindow 钉住 reset 之后能重新初始化。
//
// 它是「把机器交还给下一任」的出口——而它**重新打开抢注窗口**这件事，
// 由 CLI 的确认提示负责说清楚。
func TestResetReopensTheWindow(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.InitializeAdmin(ctx(), goodPW, "local"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ResetAdmin(ctx(), "tester"); err != nil {
		t.Fatal(err)
	}
	if done, _ := f.svc.Initialized(ctx()); done {
		t.Fatal("reset 之后应当回到未初始化")
	}
	// 旧口令彻底失效
	if err := f.svc.Authenticate(ctx(), AdminUser, goodPW); err == nil {
		t.Error("reset 之后旧口令不该还能用")
	}
	// 而且能重新初始化
	if _, err := f.svc.InitializeAdmin(ctx(), "brand new long password", "10.0.0.1"); err != nil {
		t.Errorf("reset 之后应当能重新初始化: %v", err)
	}
}

// mustJSON 把值序列化成字符串，供「不该出现在里面」这类断言用。
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
