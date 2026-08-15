package mechd

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
)

// PasswordBody 是设定/重设口令的请求体。
//
// **口令只走请求体，不进路径也不进查询串**：后两者会进访问日志与浏览器
// 历史。这与 CLI 那边「口令不走命令行参数」是同一条纪律。
type PasswordBody struct {
	Password string `json:"password"`
}

// BootstrapBody 是首次初始化的请求体。
//
// **Token 是唯一真正起作用的防护**（ADR-0039）：初始化抢注是「谁先提交
// 谁赢」的一次性竞赛，PoW/滑块只让「反复尝试」变贵，对第一次也是唯一
// 有意义的那次提交没有任何影响——套错了威胁模型。Token 复用的是首次
// 启动就打印过的那个 admin token：知道它，就证明是刚装完这台机器、
// 看得到它输出或读得到它文件系统的那个人。
type BootstrapBody struct {
	Password string `json:"password"`
	Token    string `json:"token"`
}

// authState 告诉初始化页与登录页这台机器处在什么状态。
//
// **不在 guard 之下**：它要在有任何凭据之前就被问到。只回一个布尔与那个
// 固定的账号名，不泄漏别的。
func (a *API) authState(w http.ResponseWriter, r *http.Request) {
	done, err := a.S.Initialized(r.Context())
	if err != nil {
		a.writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"initialized": done,
		// 账号名回给前端，让初始化页与登录页把它显示成不可编辑的固定值
		"user": AdminUser,
	})
}

// bootstrapAdmin 是**首次访问时设定管理员口令**的入口。
//
// **刻意不在 guard 之下**，与 /join 同理：这台机器上此刻还不存在任何凭据。
// 代价是初始化之前有一个抢注窗口（ADR-0037 记在案）——因此这里做四件事：
//
//	认令牌  必须带上首次启动时打印过的 admin token（ADR-0039）
//	一次性  设过之后永久 409
//	记来源  审计里留下是从哪个地址初始化的
//	喊一声  日志里 WARN
//
// 放在 guard 之外**必须显式写出来**：一条悄悄绕过认证的路由是这类代码里
// 最危险的东西，它得在路由表上一眼可见。
func (a *API) bootstrapAdmin(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	if err := a.Limiter.Check(ip); err != nil {
		a.writeErr(w, r, http.StatusTooManyRequests, err)
		return
	}

	var body BootstrapBody
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}

	// **「已经初始化过」在令牌校验之前判**：那不是权限问题，是这件事
	// 已经做完了——与 GET /auth/state 一样，谁都答得出来，不因为没带
	// 令牌就该变成一句无关的「未授权」。
	if done, err := a.S.Initialized(r.Context()); err != nil {
		a.writeErr(w, r, http.StatusInternalServerError, err)
		return
	} else if done {
		// **409 而不是 403**：这不是「你没权限」，是「这件事已经做过了」
		a.writeErr(w, r, http.StatusConflict, ErrAlreadyInitialized)
		return
	}

	// **先验令牌再碰任何有状态的东西**：与 login 里「先验挑战再验口令」
	// 是同一条纪律。令牌不对时，连 Service.InitializeAdmin 都不该被调用
	// ——那是唯一在真正决定「这台机器谁说了算」的地方。
	if !validBootstrapToken(a.BootstrapTokenHash, body.Token) {
		a.Limiter.Fail(ip)
		a.writeErr(w, r, http.StatusUnauthorized, errBadBootstrapToken)
		return
	}

	v, err := a.S.InitializeAdmin(r.Context(), body.Password, ip)
	if err != nil {
		if errors.Is(err, ErrAlreadyInitialized) {
			// 竞态兜底：两个请求几乎同时通过了上面的检查，其中一个
			// 在真正写库时才撞上——InitializeAdmin 自己的一次性保证
			// 仍然是权威判据，这里的预检只是给常见情况一个更早的 409。
			a.writeErr(w, r, http.StatusConflict, err)
			return
		}
		a.Limiter.Fail(ip)
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	a.Limiter.Succeed(ip)
	writeJSON(w, http.StatusCreated, v)
}

// errBadBootstrapToken 不说「令牌不对」与「你没带令牌」的区别——那没有
// 意义，两者的补救办法是同一个：去看 mechd 启动时打印的内容，或读
// `<confDir>/admin.token`。
var errBadBootstrapToken = errors.New(
	"bootstrap token is invalid -- see what mechd printed on first startup, or <confDir>/admin.token")

// validBootstrapToken 用定长比较判断，且**空哈希一律拒绝**：
// BootstrapTokenHash 没被正确接线时，不能让任何 token（包括空字符串）
// 意外通过（ADR-0039）。
func validBootstrapToken(wantHash, got string) bool {
	if wantHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(HashToken(got)), []byte(wantHash)) == 1
}

// adminStatus 返回管理员账号状态（需认证）。
func (a *API) adminStatus(w http.ResponseWriter, r *http.Request, _ string) {
	v, err := a.S.AdminStatus(r.Context())
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) setAdminPassword(w http.ResponseWriter, r *http.Request, actor string) {
	var body PasswordBody
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	if err := a.S.SetAdminPassword(r.Context(), body.Password, actor); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) resetAdmin(w http.ResponseWriter, r *http.Request, actor string) {
	if err := a.S.ResetAdmin(r.Context(), actor); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
}

// remoteIP 取请求来源地址，用于审计。
//
// **不信任 X-Forwarded-For**：它是客户端可以随便写的。真要支持反代得由
// 部署方显式声明可信代理，而那还没有做——写进审计的必须是我们确实看到的
// 那个地址，不是别人告诉我们的。
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
