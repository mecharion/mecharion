package mechd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mecharion/mecharion/internal/authn"
)

// TestChallengeIsRateLimited 验证：/auth/challenge 不需要凭据，
// 而每次出题都要服务端真算一次 Argon2、画两张图。超过单 IP 的速率上限
// 之后必须直接 429，**且不能再去调用 Challenges.Issue()**——限流查了却
// 不真正拒绝，等于没查。
func TestChallengeIsRateLimited(t *testing.T) {
	f := newAuthFixture(t)

	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", APIPrefix+"/auth/challenge", nil)
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		return rec
	}

	for i := 0; i < authn.ChallengePerIPLimit; i++ {
		rec := get()
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次不该被拒，实际 %d: %s", i+1, rec.Code, rec.Body)
		}
	}
	if f.stub.issued != authn.ChallengePerIPLimit {
		t.Fatalf("Issue() 应当被调用 %d 次，实际 %d", authn.ChallengePerIPLimit, f.stub.issued)
	}

	rec := get()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次应当被限流拒绝，实际 %d: %s",
			authn.ChallengePerIPLimit+1, rec.Code, rec.Body)
	}
	// **最要紧的断言**：被拒的这一次不该让 Issue() 又被调用一遍——
	// 限流必须在真正出题（Argon2 + 画图）之前生效，不是「查了但不管用」。
	if f.stub.issued != authn.ChallengePerIPLimit {
		t.Errorf("被限流的请求不该触发 Issue()，调用次数从 %d 变成了 %d",
			authn.ChallengePerIPLimit, f.stub.issued)
	}
}
