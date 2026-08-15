package mechd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 本文件钉住 ADR-0039：首次初始化门禁靠一次性 admin token，不靠 PoW/滑块
// ——那套机制从未真正接到服务端，而且即便接上也验不动
// 「谁先提交谁赢」这种一次性竞赛。

func bootstrapReq(t *testing.T, f *authFixture, password, token string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(BootstrapBody{Password: password, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", APIPrefix+"/auth/bootstrap", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// TestBootstrapWithCorrectTokenSucceeds 是对照组：令牌对、口令合规时
// 应当正常完成初始化。
func TestBootstrapWithCorrectTokenSucceeds(t *testing.T) {
	f := newUninitializedAuthFixture(t)

	rec := bootstrapReq(t, f, goodPW, bootstrapTestToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，期望 201\n响应: %s", rec.Code, rec.Body.String())
	}

	done, err := f.svc.Initialized(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Error("初始化应当已经完成")
	}
}

// TestBootstrapAlreadyInitializedWinsOverBadToken 钉住一个真实撞过的
// 顺序问题：已经初始化过之后再调 bootstrap（无论带没带对令牌），必须
// 回 409（「已经做过了」），不能被令牌校验抢先答成 401（「未授权」）——
// 那不是权限问题，任何人（包括没有令牌的人）都问得到这个答案，
// GET /auth/state 本来就无鉴权地公开着同一件事。test/webui 的
// Test01b_BootstrapIsOneShot 撞上过这个问题：第一版实现先验令牌，
// 导致这条真机验收从「已初始化」误判成「令牌错」。
func TestBootstrapAlreadyInitializedWinsOverBadToken(t *testing.T) {
	f := newAuthFixture(t) // 已经初始化过

	rec := bootstrapReq(t, f, "some other long password", "not-the-real-token")
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409（已初始化应当优先于令牌校验）\n响应: %s",
			rec.Code, rec.Body.String())
	}
}

// TestBootstrapRejectsWrongToken 钉住 ADR-0039 的核心：令牌不对时，
// **即便口令完全合规**，初始化也不能发生。
func TestBootstrapRejectsWrongToken(t *testing.T) {
	f := newUninitializedAuthFixture(t)

	rec := bootstrapReq(t, f, goodPW, "m7n_totally-wrong-token")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401\n响应: %s", rec.Code, rec.Body.String())
	}

	done, err := f.svc.Initialized(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("令牌不对时不该完成初始化——这正是 ADR-0039 要挡的抢注")
	}
}

// TestBootstrapRejectsEmptyToken 钉住不给令牌等价于给错令牌，不是
// 「反正口令对就放行」的特例。
func TestBootstrapRejectsEmptyToken(t *testing.T) {
	f := newUninitializedAuthFixture(t)

	rec := bootstrapReq(t, f, goodPW, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401\n响应: %s", rec.Code, rec.Body.String())
	}
}

// TestBootstrapRejectsWhenTokenHashUnset 钉住 API.BootstrapTokenHash
// 没被正确接线（空字符串）时**一律拒绝**，而不是意外放行任何输入
// ——服务端配置错误的默认方向必须是「拒绝」，不能是「放行」。
func TestBootstrapRejectsWhenTokenHashUnset(t *testing.T) {
	f := newFixture(t)
	api := &API{S: f.svc} // 刻意不设 BootstrapTokenHash
	h := api.Handler()

	body, _ := json.Marshal(BootstrapBody{Password: goodPW, Token: ""})
	req := httptest.NewRequest("POST", APIPrefix+"/auth/bootstrap", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401（未接线时应当拒绝一切）\n响应: %s",
			rec.Code, rec.Body.String())
	}
}

// TestBootstrapWrongTokenEngagesLimiter 钉住重复猜错令牌最终会被
// 限流锁定——与 login 共用同一套 IP 失败锁定，即便令牌本身的熵大到
// 暴力破解不现实，限流仍然是现成的防线，不该因为换了个入口就漏接。
func TestBootstrapWrongTokenEngagesLimiter(t *testing.T) {
	f := newUninitializedAuthFixture(t)

	var last *httptest.ResponseRecorder
	for i := 0; i < 10; i++ {
		last = bootstrapReq(t, f, goodPW, "m7n_wrong")
		if last.Code == http.StatusTooManyRequests {
			break
		}
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("连续猜错应当最终被限流锁定，最后一次状态码 = %d", last.Code)
	}
}
