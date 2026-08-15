package mechd

import (
	"errors"
	"net/http"
	"time"

	"github.com/mecharion/mecharion/internal/authn"
	"github.com/mecharion/mecharion/internal/password"
)

// CSRFHeader 是写操作必须带的自定义头。
//
// **它挡的是跨站请求**：浏览器不允许跨源请求带自定义头（除非目标站点在
// CORS 预检里明确放行，而我们不放行任何跨源）。因此带得上这个头的请求，
// 一定来自我们自己的页面。
//
// 它与 `SameSite=Strict` 是**两道**：后者靠浏览器不发 cookie，前者靠
// 浏览器发不出这个头。任何一道单独失效，另一道还在。
const CSRFHeader = "X-Mecharion-Request"

// LoginBody 是登录请求体。
type LoginBody struct {
	User     string `json:"user"`
	Password string `json:"password"`
	// Challenge 是 PoW 与滑块的答案。
	Challenge authn.Answer `json:"challenge"`
}

// challenge 出一道题。
//
// **不在 guard 之下**：登录页在登录之前就要拿到它。
func (a *API) challenge(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)

	// 两层限流查的是不同的事，缺一都不够：
	//   Limiter          这个来源是不是正因为登录失败被锁定
	//   ChallengeLimiter 出题本身的速率——不失败、只是拼命出题也是一条
	//                    DoS，因为每道题都要服务端真算一次 Argon2、
	//                    画两张图。这里必须在做那些工作**之前**拒绝，
	//                    Check 之外还得有一层会真正记录这次请求。
	if err := a.Limiter.Check(ip); err != nil {
		a.writeErr(w, r, http.StatusTooManyRequests, err)
		return
	}
	if err := a.ChallengeLimiter.Allow(ip); err != nil {
		a.writeErr(w, r, http.StatusTooManyRequests, err)
		return
	}

	c, err := a.Challenges.Issue()
	if err != nil {
		if errors.Is(err, authn.ErrTooManyPending) {
			a.writeErr(w, r, http.StatusTooManyRequests, err)
			return
		}
		a.writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// login 校验口令并发一个会话 cookie。
func (a *API) login(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	if err := a.Limiter.Check(ip); err != nil {
		a.writeErr(w, r, http.StatusTooManyRequests, err)
		return
	}

	var body LoginBody
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}

	// **先验挑战再验口令**：挑战不过就不该消耗一次 argon2 口令校验，
	// 那正是 PoW 想省下的算力。
	if err := a.Challenges.Verify(body.Challenge); err != nil {
		a.Limiter.Fail(ip)
		a.writeErr(w, r, http.StatusUnauthorized, err)
		return
	}

	if err := a.S.Authenticate(r.Context(), body.User, body.Password); err != nil {
		a.Limiter.Fail(ip)
		// 措辞与「用户不对」完全一致——两者无从区分
		a.writeErr(w, r, http.StatusUnauthorized, password.ErrMismatch)
		return
	}

	tok, exp, err := a.S.NewSession(r.Context(), AdminUser)
	if err != nil {
		a.writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	a.Limiter.Succeed(ip)
	http.SetCookie(w, a.sessionCookie(r, tok, exp))
	writeJSON(w, http.StatusOK, map[string]any{
		"user": AdminUser, "expiresAt": exp.UTC().Format(time.RFC3339),
	})
}

// logout 结束会话。
func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		_ = a.S.EndSession(r.Context(), c.Value)
	}
	// 过期时间设成过去，让浏览器立刻丢掉它
	dead := a.sessionCookie(r, "", time.Unix(0, 0))
	dead.MaxAge = -1
	http.SetCookie(w, dead)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// sessionCookie 造会话 cookie。
//
// 四个属性缺一不可：
//
//	HttpOnly  脚本读不到它——一次 XSS 就偷不走会话
//	Secure    只走 HTTPS。明文模式（--insecure-http，仅本机调试）下必须关掉，
//	          否则浏览器根本不会存这个 cookie，登录会表现成「密码错」
//	SameSite  Strict：跨站请求浏览器不带它，这是 CSRF 的第一道
//	Path=/    整个站点
func (a *API) sessionCookie(r *http.Request, val string, exp time.Time) *http.Cookie {
	return &http.Cookie{
		Name: SessionCookie, Value: val, Path: "/",
		Expires: exp, HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	}
}

// sessionAuth 用会话 cookie 认证，是 Bearer token 之外的第二条路。
//
// 两条路**在 guard 一处收口**——散开的话，「某个接口忘了检查会话」就是
// 一个洞。
type sessionAuth struct{ a *API }

func (s sessionAuth) Authenticate(r *http.Request) (string, error) {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return "", ErrNoSession
	}
	user, err := s.a.S.LookupSession(r.Context(), c.Value)
	if err != nil {
		return "", err
	}
	// **cookie 认证的写操作必须带自定义头**。
	//
	// 只靠 SameSite=Strict 是把 CSRF 的防护完全押在浏览器实现上；
	// 老浏览器、或者某些客户端不遵守它时，那道防线就没了。
	if isWrite(r) && r.Header.Get(CSRFHeader) == "" {
		return "", errors.New("cross-site request rejected (missing " + CSRFHeader + ")")
	}
	return user, nil
}

func isWrite(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}
