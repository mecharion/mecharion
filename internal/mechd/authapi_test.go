package mechd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/authn"
)

// 本文件钉住 **M8 第 3 步**的 HTTP 面：会话、cookie 属性、CSRF。
//
// 这几条坏掉的时候都**不会报错**——登录照样成功，界面照样能用，只是少了
// 一层防护。所以每一条都得单独验。

// stubChallenger 是挑战的桩。
//
// **谜题本身在 authn 包里验透了**（Argon2、一次性核销、滑块容差、过期）；
// 这一层要验的是「登录有没有真的去问它、以及它说不通过时会不会放行」。
// 用桩还避免了每条 HTTP 测试都真跑一遍 Argon2——那会让这个包慢得没人愿意跑。
type stubChallenger struct {
	// answer 是唯一被接受的答案。
	answer authn.Answer
	// issued 记录出过几道题。
	issued int
}

func (s *stubChallenger) Issue() (*authn.Challenge, error) {
	s.issued++
	return &authn.Challenge{
		ID: s.answer.ID, PoWSalt: "aabb", PoWDifficulty: 10,
		Background: "data:image/png;base64,AA", Piece: "data:image/png;base64,AA",
	}, nil
}

func (s *stubChallenger) Verify(a authn.Answer) error {
	if a != s.answer {
		return authn.ErrChallengeUnknown
	}
	return nil
}

// authFixture 起一个带 API 的夹具，并把 admin 初始化好。
type authFixture struct {
	*fixture
	api  *API
	h    http.Handler
	stub *stubChallenger
}

// bootstrapTestToken 是测试里固定用的初始化令牌明文——真实场景里它是
// ensureToken 随机生成的，这里固定成常量纯粹是为了断言方便。
const bootstrapTestToken = "m7n_bootstrap-test-token"

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	f := newFixture(t)
	if _, err := f.svc.InitializeAdmin(ctx(), goodPW, "test"); err != nil {
		t.Fatal(err)
	}
	stub := &stubChallenger{answer: authn.Answer{ID: "ch-1", PoW: 7, SliderX: 42}}
	api := &API{
		S: f.svc, Auth: NewTokenAuth("m7n_test-token"),
		Challenges: stub, Limiter: authn.NewLimiter(f.svc.now),
		BootstrapTokenHash: HashToken(bootstrapTestToken),
	}
	return &authFixture{fixture: f, api: api, h: api.Handler(), stub: stub}
}

// newUninitializedAuthFixture 起一套**还没设过 admin 口令**的夹具，
// 专供 bootstrap 相关测试用——newAuthFixture 为了让其它测试拿到手就能
// 登录，已经在夹具搭建阶段直接调用 Service.InitializeAdmin 把口令设好了。
func newUninitializedAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	f := newFixture(t)
	stub := &stubChallenger{answer: authn.Answer{ID: "ch-1", PoW: 7, SliderX: 42}}
	api := &API{
		S: f.svc, Auth: NewTokenAuth("m7n_test-token"),
		Challenges: stub, Limiter: authn.NewLimiter(f.svc.now),
		BootstrapTokenHash: HashToken(bootstrapTestToken),
	}
	return &authFixture{fixture: f, api: api, h: api.Handler(), stub: stub}
}

func (f *authFixture) login(t *testing.T, pw string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(LoginBody{
		User: AdminUser, Password: pw, Challenge: f.stub.answer,
	})
	req := httptest.NewRequest("POST", APIPrefix+"/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func sessionCookieOf(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie {
			return c
		}
	}
	t.Fatal("响应里没有会话 cookie")
	return nil
}

// TestLoginSetsHardenedCookie 钉住 cookie 的四个属性。
//
// 少任何一个都不会报错，只会安静地少一层防护：
//
//	HttpOnly  少了它，一次 XSS 就能偷走会话
//	SameSite  少了它，CSRF 的第一道没了
//	Path      少了它，cookie 的作用域不确定
func TestLoginSetsHardenedCookie(t *testing.T) {
	f := newAuthFixture(t)
	rec := f.login(t, goodPW)
	if rec.Code != http.StatusOK {
		t.Fatalf("登录应当成功，实际 %d: %s", rec.Code, rec.Body)
	}

	c := sessionCookieOf(t, rec)
	if !c.HttpOnly {
		t.Error("会话 cookie 必须是 HttpOnly——否则一次 XSS 就能偷走它")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite 应当是 Strict，实际 %v", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path 应当是 /，实际 %q", c.Path)
	}
	if c.Value == "" {
		t.Error("cookie 值不该是空的")
	}
	// **明文模式下不能置 Secure**：置了浏览器根本不存，登录会表现成「密码错」
	if c.Secure {
		t.Error("明文 HTTP 下不该置 Secure，否则浏览器不会保存这个 cookie")
	}
}

// TestSessionAuthenticatesRequests 钉住会话真的能用来调接口。
func TestSessionAuthenticatesRequests(t *testing.T) {
	f := newAuthFixture(t)
	c := sessionCookieOf(t, f.login(t, goodPW))

	req := httptest.NewRequest("GET", APIPrefix+"/nodes", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("带会话的读请求应当通过，实际 %d: %s", rec.Code, rec.Body)
	}
}

// TestWriteNeedsCSRFHeader 是这一步的安全核心之一。
//
// 只靠 `SameSite=Strict` 等于把 CSRF 防护完全押在浏览器实现上。加一个
// 自定义头做第二道：浏览器不允许跨源请求带自定义头，因此带得上它的请求
// 一定来自我们自己的页面。
func TestWriteNeedsCSRFHeader(t *testing.T) {
	f := newAuthFixture(t)
	c := sessionCookieOf(t, f.login(t, goodPW))

	body := bytes.NewReader([]byte(`{"name":"n9"}`))
	req := httptest.NewRequest("POST", APIPrefix+"/nodes", body)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("缺自定义头的写请求应当被拒，实际 %d: %s", rec.Code, rec.Body)
	}

	// 带上头就放行
	body = bytes.NewReader([]byte(`{"name":"n9"}`))
	req = httptest.NewRequest("POST", APIPrefix+"/nodes", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(CSRFHeader, "1")
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("带了自定义头的写请求不该被拒: %s", rec.Body)
	}
}

// TestReadDoesNotNeedCSRFHeader 钉住读请求不受影响。
//
// 读也要头的话，用户在地址栏里直接打开一个页面就会失败。
func TestReadDoesNotNeedCSRFHeader(t *testing.T) {
	f := newAuthFixture(t)
	c := sessionCookieOf(t, f.login(t, goodPW))

	req := httptest.NewRequest("GET", APIPrefix+"/nodes", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("读请求不该要自定义头，实际 %d", rec.Code)
	}
}

// TestBearerTokenStillWorks 钉住 CLI 那条路没被会话挤掉。
//
// **两条认证路径并存**：脚本用 Bearer token，浏览器用 cookie。
// 而 token 那条**不受 CSRF 头约束**——它不是浏览器发的，不存在跨站问题。
func TestBearerTokenStillWorks(t *testing.T) {
	f := newAuthFixture(t)
	body := bytes.NewReader([]byte(`{"name":"n8"}`))
	req := httptest.NewRequest("POST", APIPrefix+"/nodes", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer m7n_test-token")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("Bearer token 的写请求不该被拒: %s", rec.Body)
	}
}

// TestSessionSurvivesRestart 是验收表第 7 条。
//
// 会话放内存的话，mechd 一次重启就把所有人登出——而重启在这个项目里是
// 常规操作（换二进制、改配置）。这条测试靠**新造一个 API 实例**来模拟
// 重启：底下是同一个库，但内存中的一切都没了。
func TestSessionSurvivesRestart(t *testing.T) {
	f := newAuthFixture(t)
	c := sessionCookieOf(t, f.login(t, goodPW))

	// 「重启」：全新的 API 与全新的挑战/限流状态，同一个 Store
	restarted := (&API{
		S: f.svc, Auth: NewTokenAuth("m7n_test-token"),
		Challenges: &stubChallenger{answer: f.stub.answer},
		Limiter:    authn.NewLimiter(f.svc.now),
	}).Handler()

	req := httptest.NewRequest("GET", APIPrefix+"/nodes", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	restarted.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("重启之后已登录的会话应当仍然有效，实际 %d: %s", rec.Code, rec.Body)
	}
}

// TestLogoutInvalidatesSession 钉住登出真的作废。
func TestLogoutInvalidatesSession(t *testing.T) {
	f := newAuthFixture(t)
	c := sessionCookieOf(t, f.login(t, goodPW))

	req := httptest.NewRequest("POST", APIPrefix+"/auth/logout", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("登出应当成功，实际 %d", rec.Code)
	}

	req = httptest.NewRequest("GET", APIPrefix+"/nodes", nil)
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("登出之后那个 cookie 应当失效，实际 %d", rec.Code)
	}
}

// TestPasswordChangeKillsSessions 钉住改口令把已有会话全清掉。
//
// 不清的话，一个已经被偷走的会话在改完口令之后仍然有效——而改口令的动机
// 通常正是「怀疑被偷了」。
func TestPasswordChangeKillsSessions(t *testing.T) {
	f := newAuthFixture(t)
	c := sessionCookieOf(t, f.login(t, goodPW))

	if err := f.svc.SetAdminPassword(ctx(), "a different long password", "tester"); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", APIPrefix+"/nodes", nil)
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("改口令之后旧会话应当失效，实际 %d", rec.Code)
	}
}

// TestLoginWithoutChallengeRejected 钉住绕开挑战登不进去。
func TestLoginWithoutChallengeRejected(t *testing.T) {
	f := newAuthFixture(t)
	body, _ := json.Marshal(LoginBody{User: AdminUser, Password: goodPW})
	req := httptest.NewRequest("POST", APIPrefix+"/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("没有挑战答案的登录应当被拒，实际 %d: %s", rec.Code, rec.Body)
	}
	if len(rec.Result().Cookies()) > 0 {
		t.Error("被拒的登录不该发出任何 cookie")
	}
}

// TestWrongPasswordLocksOutEventually 是验收表第 4 条。
//
// **限流才是挡住慢速撞库的那一层**：PoW 给每次尝试标价，但一秒一次的人工
// 尝试算力成本可以忽略。
func TestWrongPasswordLocksOutEventually(t *testing.T) {
	f := newAuthFixture(t)
	for i := 0; i < authn.MaxFailures; i++ {
		rec := f.login(t, "wrong password here")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错口令应当 401，实际 %d", i+1, rec.Code)
		}
	}

	// 现在连**对的**口令也该被挡住
	rec := f.login(t, goodPW)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("连续失败之后应当限流（429），实际 %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "try again in") {
		t.Errorf("限流信息该说清要等多久，实际: %s", rec.Body)
	}
}

// TestLoginErrorDoesNotSayWhichPartFailed 是验收表第 2 条。
func TestLoginErrorDoesNotSayWhichPartFailed(t *testing.T) {
	f := newAuthFixture(t)
	rec := f.login(t, "wrong password here")
	body := rec.Body.String()
	for _, leak := range []string{"不存在", "用户名错", "口令错误"} {
		if strings.Contains(body, leak) {
			t.Errorf("错误信息泄漏了是哪一半不对: %s", body)
		}
	}
}
