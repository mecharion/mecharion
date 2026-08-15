package mechd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mecharion/mecharion/internal/authn"
	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/store"
	"github.com/mecharion/mecharion/internal/webui"
)

// APIPrefix 是全部 HTTP 接口的前缀。版本在路径里，不在 header 里——
// 后者在 curl 与浏览器地址栏里都看不见。
const APIPrefix = "/api/v1"

// TokenPrefix 让一个泄漏的 token 一眼可辨（GitHub 的 ghp_ 是同一个用意）。
const TokenPrefix = "m7n_"

// Authenticator 校验请求的身份。
type Authenticator interface {
	// Authenticate 返回调用者标识；失败返回 error。
	Authenticate(r *http.Request) (string, error)
}

// TokenAuth 是基于 bearer token 的认证。
//
// **只存哈希**：库被拖走时 token 不能直接拿去用。
type TokenAuth struct{ hashes [][32]byte }

// NewTokenAuth 由若干明文 token 构造认证器。
func NewTokenAuth(tokens ...string) *TokenAuth {
	a := &TokenAuth{}
	for _, t := range tokens {
		if t == "" {
			continue
		}
		a.hashes = append(a.hashes, sha256.Sum256([]byte(t)))
	}
	return a
}

// HashToken 返回一个 token 的存储形态。
func HashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// Authenticate 实现 Authenticator。
func (a *TokenAuth) Authenticate(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || tok == "" {
		return "", errors.New("missing Authorization: Bearer <token>")
	}
	got := sha256.Sum256([]byte(tok))
	for _, want := range a.hashes {
		// 定长比较：普通的 == 会因为提前返回而泄漏前缀匹配了多少位
		if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
			return "token", nil
		}
	}
	return "", errors.New("invalid token")
}

// ── 路由 ────────────────────────────────────────────────────────────────

// Challenger 出题与核销。见 API.Challenges 上的说明。
type Challenger interface {
	Issue() (*authn.Challenge, error)
	Verify(authn.Answer) error
}

// API 是 HTTP 接口。
type API struct {
	S    *Service
	Auth Authenticator
	// ConfDir 是 pki 所在的配置目录，join 与 token 展示要用它读 CA。
	ConfDir string
	// PackDir 是本地 Pack 集合目录，上传落到这里。
	PackDir string
	// Challenges 出登录前的 PoW 与滑块题；nil 时由 Handler 兜底构造。
	//
	// **是接口而不是具体类型**：谜题本身（Argon2、图像、容差）在 authn 包
	// 里验透了，这一层要验的是「登录有没有真的去问它」。用接口让 HTTP 测试
	// 能塞一个桩，而不必在 authn 上开一个「能解出答案」的导出方法——
	// 那种方法一旦存在，迟早会有人把它接到别处。
	Challenges Challenger
	// Limiter 挡慢速撞库；nil 时由 Handler 兜底构造。
	Limiter *authn.Limiter
	// ChallengeLimiter 挡出题本身被刷；nil 时由 Handler 兜底构造。
	ChallengeLimiter *authn.ChallengeLimiter
	// BootstrapTokenHash 是首次初始化门禁用的一次性 admin token 的哈希
	// （HashToken 的输出）。空值时 bootstrapAdmin 拒绝一切请求——这是
	// 刻意的：宁可首次初始化用不了，也不要在忘记接线时悄悄放行任何
	// token（ADR-0039）。真实启动路径（serve.go）总是会把它设好。
	BootstrapTokenHash string
	// EnableHSTS 为 true 时响应带 Strict-Transport-Security。
	//
	// **只应在 HTTPS 模式下打开**：`--insecure-http` 时打开没有意义——
	// 浏览器规范本就无视在明文连接上收到的这个头，打开了也不生效，
	// 但会让人误以为它在起作用。serve.go 按 `!o.Insecure` 设置这个值，
	// 不是让调用方各自判断一遍。
	EnableHSTS bool
}

// Handler 构造带认证的路由。
//
// **认证不是可选的**：默认绑 0.0.0.0 是为了「拿笔记本连门店那台机看 UI」，
// 而一旦对外监听，无认证的写接口等于把机器交出去（08-security §3.3）。
func (a *API) Handler() http.Handler {
	if a.Challenges == nil {
		a.Challenges = authn.NewStore(a.S.now)
	}
	if a.Limiter == nil {
		a.Limiter = authn.NewLimiter(a.S.now)
	}
	if a.ChallengeLimiter == nil {
		a.ChallengeLimiter = authn.NewChallengeLimiter(a.S.now)
	}
	mux := http.NewServeMux()

	// **liveness**：进程能不能应个声。无条件 ok 是对的语义——能走到这行
	// 代码执行，本身就证明了 HTTP server 活着；不查数据库、不查任何
	// 依赖，查了就不是 liveness 是 readiness 了——此前只有这一个端点，
	// 没说清楚它到底答的是哪个问题。
	mux.HandleFunc("GET "+APIPrefix+"/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// **readiness**：真的探一下依赖——目前唯一有意义的依赖是 SQLite。
	// mechd 没有第二个实例做负载均衡（ADR-0014：v1 不做控制面 HA），
	// 这条的价值不是给反向代理挑实例，而是给运维/脚本一个可信的
	// "mechd 现在真能处理请求了吗"的信号：重启后数据库还没打开、
	// 或者磁盘/句柄出问题导致查询挂死，这两种情况下 /healthz 会照常
	// 答 ok（进程确实活着），但 /readyz 会如实答不行。
	mux.HandleFunc("GET "+APIPrefix+"/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := a.S.Store.Reader().PingContext(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "not ready", "reason": "database: " + err.Error(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.Handle("GET "+APIPrefix+"/components", a.guard(a.listComponents))
	mux.Handle("POST "+APIPrefix+"/components", a.guard(a.deploy))
	mux.Handle("GET "+APIPrefix+"/components/{name}/status", a.guard(a.status))
	// SSE：组件详情页的实时流（23-web-ui §4.5）。**认证只能靠 cookie**
	// ——EventSource 不能设请求头。guard 对 GET 不要求 CSRF 头，
	// 因此不需要为它开任何口子；放宽那条检查会让这条流当场断掉。
	mux.Handle("GET "+APIPrefix+"/components/{name}/watch", a.guard(a.watch))
	mux.Handle("GET "+APIPrefix+"/components/{name}/diff", a.guard(a.diff))
	// 表单只读；写回是第 6 步（23-web-ui §4.2）
	mux.Handle("GET "+APIPrefix+"/components/{name}/params", a.guard(a.componentForm))
	mux.Handle("PATCH "+APIPrefix+"/components/{name}/params", a.guard(a.setParams))
	mux.Handle("GET "+APIPrefix+"/components/{name}/groups", a.guard(a.listGroups))
	mux.Handle("PUT "+APIPrefix+"/components/{name}/groups/{group}", a.guard(a.saveGroup))
	mux.Handle("DELETE "+APIPrefix+"/components/{name}/groups/{group}", a.guard(a.removeGroup))
	mux.Handle("POST "+APIPrefix+"/components/{name}/ack-drift", a.guard(a.ackDrift))
	mux.Handle("POST "+APIPrefix+"/components/{name}/stop", a.guard(a.stop))
	mux.Handle("POST "+APIPrefix+"/components/{name}/start", a.guard(a.start))
	mux.Handle("POST "+APIPrefix+"/components/{name}/restart", a.guard(a.restart))
	mux.Handle("POST "+APIPrefix+"/components/{name}/drift-policy", a.guard(a.setDriftPolicy))
	// remove 用 DELETE 而不是 POST …/remove：它是整个 API 里唯一真正
	// 销毁东西的动词，用 HTTP 自己的销毁方法能让「这条请求会删东西」
	// 在网关日志、审计与任何中间件里都一眼可见。
	mux.Handle("DELETE "+APIPrefix+"/components/{name}", a.guard(a.removeComponent))
	mux.Handle("POST "+APIPrefix+"/components/{name}/upgrade", a.guard(a.upgrade))
	mux.Handle("POST "+APIPrefix+"/components/{name}/rollback", a.guard(a.rollback))
	mux.Handle("GET "+APIPrefix+"/components/{name}/rollout-policy", a.guard(a.rolloutPolicy))
	mux.Handle("POST "+APIPrefix+"/components/{name}/rollout-policy", a.guard(a.setRolloutPolicy))
	mux.Handle("GET "+APIPrefix+"/components/{name}/rollout", a.guard(a.rolloutStatus))
	mux.Handle("GET "+APIPrefix+"/components/{name}/rollout/history", a.guard(a.rolloutHistory))
	mux.Handle("POST "+APIPrefix+"/components/{name}/rollout/{action}", a.guard(a.rolloutAction))
	mux.Handle("GET "+APIPrefix+"/packs", a.guard(a.listPacks))
	mux.Handle("GET "+APIPrefix+"/packs/{name}/params", a.guard(a.packForm))
	// **唯一一处新增的供应链入口**（23-web-ui §2.7）。
	mux.Handle("POST "+APIPrefix+"/packs", a.guard(a.uploadPack))
	// apply 不挂在任何名词下：它作用于一份可能同时含 Site / Component /
	// ConfigGroup 的声明文件（10-cli §5）。
	mux.Handle("POST "+APIPrefix+"/apply", a.guard(a.apply))
	mux.Handle("GET "+APIPrefix+"/orphans", a.guard(a.listOrphans))
	mux.Handle("DELETE "+APIPrefix+"/orphans/{node}/{instance}", a.guard(a.purgeOrphan))
	mux.Handle("GET "+APIPrefix+"/nodes", a.guard(a.listNodes))
	mux.Handle("POST "+APIPrefix+"/nodes", a.guard(a.addNode))
	mux.Handle("DELETE "+APIPrefix+"/nodes/{name}", a.guard(a.removeNode))
	mux.Handle("POST "+APIPrefix+"/nodes/{name}/{action}", a.guard(a.nodeAction))
	mux.Handle("POST "+APIPrefix+"/nodes/tokens", a.guard(a.createJoinToken))
	mux.Handle("GET "+APIPrefix+"/nodes/tokens", a.guard(a.listJoinTokens))
	mux.Handle("POST "+APIPrefix+"/nodes/tokens/{id}/revoke", a.guard(a.revokeJoinToken))

	// **/join 刻意不在 guard 之下。**
	//
	// 它是整个系统里唯一不需要既有身份的入口——一台还没加入的机器手上
	// 只有一张 join token，没有 admin token 也没有证书。认证由 Service.Join
	// 里对 token 的校验完成，而不是这里。
	//
	// 放在 guard 之外**必须显式写出来**：一条悄悄绕过认证的路由是这类
	// 代码里最危险的东西，它得在路由表上一眼可见。
	mux.HandleFunc("POST "+APIPrefix+"/join", a.join)
	mux.Handle("GET "+APIPrefix+"/events", a.guard(a.listEvents))

	mux.Handle("GET "+APIPrefix+"/admin", a.guard(a.adminStatus))
	mux.Handle("POST "+APIPrefix+"/admin/password", a.guard(a.setAdminPassword))
	mux.Handle("POST "+APIPrefix+"/admin/reset", a.guard(a.resetAdmin))
	mux.Handle("POST "+APIPrefix+"/admin/backup", a.guard(a.createBackup))

	// **这两条刻意不在 guard 之下**，与 /join 同理：这台机器上此刻还不存在
	// 任何凭据，而初始化页正是要在那个时候被打开。
	//
	// bootstrap 是**一次性**的：设过之后永久 409。它带来的抢注窗口是
	// ADR-0037 明确接受的代价，缓解手段写在 bootstrapAdmin 的注释里。
	mux.HandleFunc("GET "+APIPrefix+"/auth/state", a.authState)
	mux.HandleFunc("POST "+APIPrefix+"/auth/bootstrap", a.bootstrapAdmin)
	mux.HandleFunc("GET "+APIPrefix+"/auth/challenge", a.challenge)
	mux.HandleFunc("POST "+APIPrefix+"/auth/login", a.login)
	mux.HandleFunc("POST "+APIPrefix+"/auth/logout", a.logout)

	// Web UI 挂在根上，**不在 guard 之下**。
	//
	// 静态资源本身不含任何数据——数据全部走 /api/v1，而那条路径是认证的。
	// 把登录页也挡在认证后面的话，用户就没有地方登录了。
	mux.Handle("/", webui.Handler())

	return withRequestID(a.withSecurityHeaders(mux))
}

// ── 安全响应头 ────────────────────────────────────────────────────────

// cspHeader 是内容安全策略。
//
// **按 Web UI 实际构建产物量身定的，不是抄一份通用模板**：
//
//	script-src 'self' 'wasm-unsafe-eval'
//	                            全部脚本走本机同源资源（Vite 构建产物），没有内联 <script>；
//	                            `'wasm-unsafe-eval'` 是登录页 PoW 求解（internal/authn 的
//	                            Argon2id 在浏览器里靠 WASM 算）需要的——没有它
//	                            `WebAssembly.compile()` 直接被 CSP 拦下，PoW 卡在 0%，
//	                            登录按钮永远点不开。这条不是纸上谈兵推出来的：第一版
//	                            CSP 没给这条，headless 浏览器实测登录页直接卡死，
//	                            控制台报的就是这一条 CSP 违规。**不给更宽的
//	                            `'unsafe-eval'`**：那会连 eval()/Function() 都放开，
//	                            攻击面比 WASM 编译这一件事大得多，而登录页只需要后者
//	style-src  'self' 'unsafe-inline'
//	                            Vue 的 :style 绑定、Element Plus 组件都靠内联 style 属性——
//	                            CSP 的 style-src 同时管 <style> 标签与内联 style 属性，
//	                            禁掉后者会让整个界面掉样式，不是好看不好看的问题
//	img-src    'self' data:    SliderCaptcha 的背景图与拼图块是服务端生成的 data: URI，
//	                            没有 data: 就是一张裂图，登录页直接不能用
//	connect-src 'self'         API 调用与 SSE（EventSource）都是同源
//	frame-ancestors 'none'     现行标准：管理界面不能被 iframe 嵌入
//	base-uri 'self', form-action 'self'
//	                            收紧到没有实际用途的默认值，不为潜在的注入点留口子
const cspHeader = "default-src 'self'; " +
	"script-src 'self' 'wasm-unsafe-eval'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"font-src 'self'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'"

// withSecurityHeaders 给每个响应加上一组基础安全头。
//
// **对全部响应生效，不分 API 还是 Web UI**：`internal/webui/webui.go`
// 只设置内容/缓存头，`httpapi.go` 也没设置过这些安全头——与其在两处
// 各接一遍、日后新增一个响应路径时忘记接，不如在两者共用的最外层
// 中间件上做一次，物理上不可能漏掉。
//
// **`X-Frame-Options` 与 CSP 的 `frame-ancestors` 两条都给**：前者是
// 老浏览器认的机制，后者是现行标准；两者语义重叠但没有互斥，同时发出
// 是防止历史包袱的标准做法，不是多余。
func (a *API) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", cspHeader)
		h.Set("X-Content-Type-Options", "nosniff")
		// same-origin：管理界面的地址（组件名、节点名都在 URL 里）不该被
		// 当成 Referer 泄漏给任何第三方——即便这个应用本就没有对外链接，
		// 显式声明也好过依赖浏览器默认值（各浏览器的默认值并不统一）。
		h.Set("Referrer-Policy", "same-origin")
		if a.EnableHSTS {
			// 一年 + includeSubDomains：HTTPS 模式下才有意义，
			// 见 API.EnableHSTS 的字段注释。
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// ── request id ──────────────────────────────────────────────────────

type requestIDKey struct{}

// RequestIDHeader 把请求 ID 回填到响应头，客户端/网关不用解析 JSON body
// 就能拿到——出问题时报障单里带上这个头的值，运维凭它去日志里精确定位。
const RequestIDHeader = "X-Request-Id"

// withRequestID 给每个进来的请求生成一个 ID，挂在 context 里、也回填到
// 响应头。500 不再把内部错误原样返回给客户端（见 writeErr）之后，这个
// ID 是客户端与服务端日志之间唯一的关联线索，没有它，一句「内部错误，
// 请找管理员」等于什么都没说。
//
// **不信任客户端传入的同名头**：这台机器是唯一的控制面，没有上游服务
// 需要跨进程传递 trace id；信任客户端自报的值只会让「这个 ID 对应哪次
// 真实请求」这件事失去意义。
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func newRequestID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败是环境级故障；退化成一个明显不是随机数的占位值
		// 好过让请求直接失败——日志关联能力降级，但服务不因此中断。
		return "unavailable"
	}
	return hex.EncodeToString(buf)
}

func requestIDOf(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey{}).(string)
	return id
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, actor string)

// guard 包一层认证。
func (a *API) guard(next handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Auth == nil {
			// 没配认证器就一律拒绝，而不是一律放行。
			// 「忘了配」在这里必须是拒绝服务，不能是敞开大门。
			a.writeErr(w, r, http.StatusUnauthorized, errors.New("server has no authentication configured"))
			return
		}
		// **两条认证路径在这一处收口**：Bearer token（CLI 与脚本）与
		// 会话 cookie（浏览器）。散开的话，「某个接口忘了检查会话」
		// 就是一个洞。
		actor, err := a.Auth.Authenticate(r)
		if err != nil {
			user, serr := sessionAuth{a}.Authenticate(r)
			if serr != nil {
				// 回 token 那条的错误：它更常见，也不泄漏「这里还有另一条路」
				a.writeErr(w, r, http.StatusUnauthorized, err)
				return
			}
			actor = user
		}
		next(w, r, actor)
	})
}

// ── 处理器 ──────────────────────────────────────────────────────────────

func (a *API) listComponents(w http.ResponseWriter, r *http.Request, _ string) {
	out, err := a.S.ListComponents(r.Context(), r.URL.Query().Get("site"))
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"components": out})
}

func (a *API) listNodes(w http.ResponseWriter, r *http.Request, _ string) {
	out, err := a.S.ListNodes(r.Context(), r.URL.Query().Get("site"))
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": out})
}

// AddNodeBody 是 POST /nodes 的请求体。
type AddNodeBody struct {
	Site    string `json:"site,omitempty"`
	Name    string `json:"name"`
	Address string `json:"address,omitempty"`
}

func (a *API) addNode(w http.ResponseWriter, r *http.Request, actor string) {
	var body AddNodeBody
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	out, err := a.S.AddNode(r.Context(), body.Site, body.Name, body.Address, actor)
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// CreateJoinTokenBody 是 POST /nodes/tokens 的请求体。
type CreateJoinTokenBody struct {
	Site string `json:"site,omitempty"`
	// Node 非空表示这张 token 只能签出该名字的证书。
	Node string `json:"node,omitempty"`
	// TTL 是有效期，如 "30m"。
	TTL  string `json:"ttl,omitempty"`
	Uses int    `json:"uses,omitempty"`
}

func (a *API) createJoinToken(w http.ResponseWriter, r *http.Request, actor string) {
	var body CreateJoinTokenBody
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	var ttl time.Duration
	if body.TTL != "" {
		d, err := time.ParseDuration(body.TTL)
		if err != nil {
			a.writeErr(w, r, http.StatusBadRequest, fmt.Errorf("ttl is not a duration: %w", err))
			return
		}
		ttl = d
	}
	v, err := a.S.CreateJoinToken(
		r.Context(), body.Site, body.Node, ttl, body.Uses, a.ConfDir, actor)
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (a *API) listJoinTokens(w http.ResponseWriter, r *http.Request, _ string) {
	out, err := a.S.ListJoinTokens(r.Context(), r.URL.Query().Get("site"))
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

func (a *API) revokeJoinToken(w http.ResponseWriter, r *http.Request, actor string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.writeErr(w, r, http.StatusBadRequest, errors.New("id is not a number"))
		return
	}
	if err := a.S.RevokeJoinToken(
		r.Context(), r.URL.Query().Get("site"), id, actor); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": id})
}

// JoinBody 是 POST /join 的请求体。
type JoinBody struct {
	Token string `json:"token"`
	Node  string `json:"node,omitempty"`
	// CSR 是节点本机生成的证书请求（PEM）。**私钥不过网。**
	CSR     string `json:"csr"`
	Address string `json:"address,omitempty"`
}

// JoinResponse 是签发结果。
type JoinResponse struct {
	Node string `json:"node"`
	Cert string `json:"cert"`
	CA   string `json:"ca"`
}

func (a *API) join(w http.ResponseWriter, r *http.Request) {
	var body JoinBody
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	res, err := a.S.Join(r.Context(), JoinRequest{
		Token: body.Token, Node: body.Node, CSR: []byte(body.CSR),
		Address: body.Address, ConfDir: a.ConfDir,
	})
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, JoinResponse{
		Node: res.Node, Cert: string(res.Cert), CA: string(res.CA),
	})
}

func (a *API) removeNode(w http.ResponseWriter, r *http.Request, actor string) {
	force := r.URL.Query().Get("force") == "true"
	err := a.S.RemoveNode(r.Context(), r.URL.Query().Get("site"),
		r.PathValue("name"), force, actor)
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": r.PathValue("name")})
}

// nodeAction 处理 revoke / unrevoke。
//
// 合成一个路由而不是两条：它们是同一个开关的两个方向，分开写迟早会
// 出现「revoke 记审计而 unrevoke 忘了记」这类不对称。
func (a *API) nodeAction(w http.ResponseWriter, r *http.Request, actor string) {
	name := r.PathValue("name")
	site := r.URL.Query().Get("site")
	var err error
	action := r.PathValue("action")
	switch action {
	case "revoke":
		err = a.S.SetNodeRevoked(r.Context(), site, name, true, actor)
	case "unrevoke":
		err = a.S.SetNodeRevoked(r.Context(), site, name, false, actor)
	case "cordon":
		err = a.S.SetNodeCordoned(r.Context(), site, name, true, actor)
	case "uncordon":
		err = a.S.SetNodeCordoned(r.Context(), site, name, false, actor)
	default:
		a.writeErr(w, r, http.StatusNotFound, fmt.Errorf(
			"unknown action %q (supported: revoke / unrevoke / cordon / uncordon)", action))
		return
	}
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": name, "action": action})
}

func (a *API) listEvents(w http.ResponseWriter, r *http.Request, _ string) {
	site, err := a.S.resolveSite(r.Context(), r.URL.Query().Get("site"))
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := a.S.Repos.Events().List(r.Context(), site.ID, limit)
	if err != nil {
		a.writeErr(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// DeployBody 是 POST /components 的请求体。
type DeployBody struct {
	Site        string              `json:"site,omitempty"`
	Pack        string              `json:"pack"`
	Component   string              `json:"component,omitempty"`
	Profile     string              `json:"profile,omitempty"`
	Nodes       []string            `json:"nodes,omitempty"`
	Roles       map[string][]string `json:"roles,omitempty"`
	Set         map[string]any      `json:"set,omitempty"`
	Require     map[string]string   `json:"require,omitempty"`
	Update      bool                `json:"update,omitempty"`
	AllowRemove bool                `json:"allowRemove,omitempty"`
	DryRun      bool                `json:"dryRun,omitempty"`
}

func (a *API) deploy(w http.ResponseWriter, r *http.Request, actor string) {
	var body DeployBody
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	if body.Pack == "" {
		a.writeErr(w, r, http.StatusBadRequest, errors.New("missing pack"))
		return
	}

	res, err := a.S.Deploy(r.Context(), DeployRequest{
		Site: body.Site, Pack: body.Pack, Component: body.Component,
		Profile: body.Profile, Nodes: body.Nodes, Roles: body.Roles,
		Set: body.Set, Require: body.Require,
		Update: body.Update, AllowRemove: body.AllowRemove,
		DryRun: body.DryRun, Actor: actor,
	})
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}

	code := http.StatusCreated
	if body.DryRun {
		code = http.StatusOK
	}
	writeJSON(w, code, map[string]any{
		"component": res.Component,
		"instances": instanceLines(res),
		"digests":   res.Digests,
		"warnings":  res.Warnings,
		"dryRun":    res.DryRun,
	})
}

func instanceLines(res *DeployResult) []string {
	out := make([]string, 0, len(res.Digests))
	for k := range res.Digests {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func (a *API) status(w http.ResponseWriter, r *http.Request, _ string) {
	out, err := a.S.Status(r.Context(),
		r.URL.Query().Get("site"), r.PathValue("name"))
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// componentForm 回一份参数表单。
//
// role / group 走查询串而不是路径：它们是**同一份表单的两个坐标**，
// 不是两种资源。做成 /roles/{role}/groups/{group} 会让「看角色级取值」
// 变成一个没有 group 段的特例 URL，而那个特例才是默认情形。
func (a *API) componentForm(w http.ResponseWriter, r *http.Request, _ string) {
	q := r.URL.Query()
	out, err := a.S.Form(r.Context(), q.Get("site"), r.PathValue("name"),
		q.Get("role"), q.Get("group"))
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// setParams 只改参数，不碰放置（23-web-ui §4.3 ①）。
//
// **PATCH 不是 PUT**：请求体里没提到的参数保持原值。用 PUT 的话，
// 一次「改一个参数」的请求必须带上全部参数，而漏掉的那些会被恢复默认
// ——两种请求体长得几乎一样，后果差得很远。
func (a *API) setParams(w http.ResponseWriter, r *http.Request, actor string) {
	var body struct {
		Set    map[string]any `json:"set"`
		Unset  []string       `json:"unset"`
		Role   string         `json:"role"`
		Group  string         `json:"group"`
		Node   string         `json:"node"`
		DryRun bool           `json:"dryRun"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	out, err := a.S.SetParams(r.Context(), SetParamsRequest{
		Site: r.URL.Query().Get("site"), Component: r.PathValue("name"),
		Set: body.Set, Unset: body.Unset,
		Role: body.Role, Group: body.Group, Node: body.Node,
		DryRun: body.DryRun, Actor: actor,
	})
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) listPacks(w http.ResponseWriter, r *http.Request, _ string) {
	out, err := a.S.ListPacks(r.Context(), r.URL.Query().Get("site"))
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) listGroups(w http.ResponseWriter, r *http.Request, _ string) {
	q := r.URL.Query()
	out, err := a.S.ListGroups(r.Context(), q.Get("site"), r.PathValue("name"), q.Get("role"))
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out})
}

// saveGroup 建组或改组。
//
// **PUT 不是 PATCH**：这里给的是组的**完整**样子（成员、参数、路径绑定）。
// 成员用 PATCH 语义会让「把某台机器移出组」无从表达——一个没提到的成员
// 到底是「不动它」还是「移走它」，请求体上看不出来。
func (a *API) saveGroup(w http.ResponseWriter, r *http.Request, actor string) {
	var body struct {
		Role    string              `json:"role"`
		Members []string            `json:"members"`
		Params  map[string]any      `json:"params"`
		Paths   map[string][]string `json:"paths"`
		DryRun  bool                `json:"dryRun"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	out, err := a.S.SaveGroup(r.Context(), GroupRequest{
		Site: r.URL.Query().Get("site"), Component: r.PathValue("name"),
		Role: body.Role, Name: r.PathValue("group"),
		Members: body.Members, Params: body.Params, Paths: body.Paths,
		DryRun: body.DryRun, Actor: actor,
	})
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) removeGroup(w http.ResponseWriter, r *http.Request, actor string) {
	q := r.URL.Query()
	out, err := a.S.RemoveGroup(r.Context(), GroupRequest{
		Site: q.Get("site"), Component: r.PathValue("name"),
		Role: q.Get("role"), Name: r.PathValue("group"),
		DryRun: q.Get("dryRun") == "true", Actor: actor,
	})
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// uploadPack 收一个 .mpack。
//
// **请求体直接是文件字节，不是 multipart。** multipart 要先把整个请求
// 读进内存或落一次临时文件才能拿到那个 part，而这里的文件动辄几百 MB。
// 直接流式收进临时目录，内存占用与文件大小无关。
//
// 代价：浏览器那边要用 fetch 发 Blob，不能用 <form enctype=multipart>。
// 那不是损失——这个界面本来就是 fetch 驱动的。
func (a *API) uploadPack(w http.ResponseWriter, r *http.Request, actor string) {
	// 上限是**最后一道**，不是唯一一道：ExtractMpack 里对单个条目也有限长。
	// 少了这里的话，一个不断写入的连接能把磁盘填满而永远走不到解包那步。
	const maxUpload = 4 << 30 // 4 GiB
	body := http.MaxBytesReader(w, r.Body, maxUpload)
	defer body.Close()

	out, err := a.S.UploadPack(r.Context(), body, a.PackDir, actor)
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (a *API) packForm(w http.ResponseWriter, r *http.Request, _ string) {
	q := r.URL.Query()
	out, err := a.S.PackForm(r.Context(), r.PathValue("name"),
		q.Get("version"), q.Get("profile"), q.Get("role"))
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) diff(w http.ResponseWriter, r *http.Request, _ string) {
	out, err := a.S.Diff(r.Context(),
		r.URL.Query().Get("site"), r.PathValue("name"))
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// AckDriftBody 是 POST …/ack-drift 的请求体。
type AckDriftBody struct {
	Site     string `json:"site,omitempty"`
	Role     string `json:"role,omitempty"`
	Node     string `json:"node,omitempty"`
	Resource string `json:"resource,omitempty"`
	Duration string `json:"duration"`
	Reason   string `json:"reason"`
}

func (a *API) ackDrift(w http.ResponseWriter, r *http.Request, actor string) {
	var body AckDriftBody
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	d, err := time.ParseDuration(body.Duration)
	if err != nil {
		a.writeErr(w, r, http.StatusBadRequest,
			fmt.Errorf("duration %q is invalid (e.g. 4h / 30m)", body.Duration))
		return
	}

	n, err := a.S.AckDrift(r.Context(), AckDriftRequest{
		Site: body.Site, Component: r.PathValue("name"),
		Role: body.Role, Node: body.Node, Resource: body.Resource,
		Duration: d, Reason: body.Reason, Actor: actor,
	})
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"suppressed": n})
}

// ── 辅助 ────────────────────────────────────────────────────────────────

// maxJSONBody 是普通 JSON 请求体的上限。
//
// 1 MiB 对这里的任何一类请求体（部署参数、配置组、用户）都绰绰有余——
// 真正的大体积负载（Pack 上传）走的是另一条路（uploadPack 自己的
// `http.MaxBytesReader`，4 GiB），不经过这个函数。少了这个上限，一个
// 未认证的调用方能用巨体积请求把 handler 的内存吃满。
const maxJSONBody = 1 << 20

// bodyReadTimeout 是读完请求体的时限。
//
// `ReadHeaderTimeout`（serve.go）只护住了请求头；正文可以用一个正常打开
// 的连接、以每次几个字节的速度慢慢喂，handler 会一直卡在 Decode 里等，
// 一个这样的连接就占掉一个 goroutine 且永不释放。
const bodyReadTimeout = 10 * time.Second

// decodeBody 解析请求体为 v，同时挡住三类资源与格式滥用：
//
//   - 体积：超过 maxJSONBody 时 Decode 返回 *http.MaxBytesError，
//     statusFor 把它映到 413。
//   - 读取时间：body 上设了独立的读取超时，与请求头的超时分开计算，
//     不会因为一直不封闭连接而占住 handler。
//   - Content-Type：必须是 application/json（允许带 charset），
//     不然把任意内容当 JSON 解是在帮攻击者省一次试探。
//   - 请求体只能有一个 JSON 值：`{...}{...}` 这种拼接体，第一个对象会
//     被正常解析、第二个被静默丢弃——调用方以为只发了一份，服务端却
//     不保证真的只处理了一份。
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt != "application/json" {
			return faults.Permanentf("parsing request body",
				"Content-Type must be application/json, got %q", ct)
		}
	}

	if rc := http.NewResponseController(w); rc != nil {
		if err := rc.SetReadDeadline(time.Now().Add(bodyReadTimeout)); err != nil {
			// 测试用的 httptest.ResponseRecorder 等不支持这个接口，
			// 是预期之内的——真实的 net/http 连接总是支持。
			_ = err
		}
	}

	// **错误一律标成 Permanent，除了体积超限**：那一种要被 statusFor
	// 识别成 413 而不是 400，靠的是 errors.As 认出被包裹的
	// *http.MaxBytesError——faults.Permanentf 只是在外面再包一层，
	// 不影响 errors.As 顺着链条找到它。
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	// 拼错的字段必须报错——静默忽略会让人以为设置生效了
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return faults.Wrap(faults.Permanent, "parsing request body", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return faults.Permanentf("parsing request body", "request body must contain exactly one JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// errCode 是响应体里的稳定机器可读错误码。
//
// **只由 HTTP 状态码决定，不看错误文案**——中文措辞可以随便改，改了也
// 不会牵动这个字段。客户端要做程序化分支（重试、提示用户）时应当读
// 这个字段，不该去匹配 `error` 里的中文。
func errCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_argument"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusRequestEntityTooLarge:
		return "too_large"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusInternalServerError:
		return "internal"
	default:
		return "error"
	}
}

// writeErr 把一个错误写成 JSON 响应。
//
// **500 不把 err.Error() 原样给客户端**：内部路径、SQL 语句、
// 配置细节这类东西可能就在这段文本里，客户端不该看见——服务端日志
// 留着完整 cause，客户端拿到的是 requestId，真出问题时运维凭它去日志
// 里精确定位，不用靠猜内部错误文案在说什么。
//
// 其余状态码（400/404/409/429...）的错误文案本来就是**写给用户看的
// 说明**，继续原样返回；它们不会包含内部实现细节，这是写这些错误信息
// 的人本该遵守、也一直在遵守的纪律，不是这里新加的约束。
func (a *API) writeErr(w http.ResponseWriter, r *http.Request, code int, err error) {
	id := requestIDOf(r)
	if code == http.StatusInternalServerError {
		a.S.log().Error("internal error",
			"requestId", id, "method", r.Method, "path", r.URL.Path, "err", err)
		writeJSON(w, code, map[string]string{
			"error":     "internal error, please give the requestId to an administrator to check the logs",
			"code":      errCode(code),
			"requestId": id,
		})
		return
	}
	writeJSON(w, code, map[string]string{
		"error": err.Error(), "code": errCode(code), "requestId": id,
	})
}

// statusFor 把领域错误映射到 HTTP 状态码。
//
// 只区分「找不到」「太大」「你写错了」「我出错了」四类。更细的分类
// 需要在每个错误上带类型标记，而那份代价换来的信息量，`error` 里的
// 中文已经给了。
func statusFor(err error) int {
	var mbe *http.MaxBytesError
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.As(err, &mbe):
		return http.StatusRequestEntityTooLarge
	case isUserError(err):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// isUserError 判断错误是不是用户输入造成的。
//
// **只认显式打过类型标记的**：此前这里还有一段按中文文案
// 子串匹配的兜底，代价是改一句错误提示的措辞就可能悄悄把 400 变成
// 500——文案是给人看的，不该参与控制流。子串列表已经删掉，落地时把
// 当时能匹配上那些关键词的全部错误构造点显式包了一层
// `faults.Permanentf`/`faults.Wrap(faults.Permanent, ...)`（`internal/mechd`
// 与 `internal/placement` 各处，见对应文件），行为在转换前后逐一核对过。
//
// 不用 `faults.ClassOf(err) == faults.Permanent` 兜底：那个函数对**未
// 分类**的错误也答 Permanent（这对调和循环是对的默认值），拿它当 HTTP
// 判据会让任何没打过标的内部错误都变成 400，掩盖真正的服务端故障。
func isUserError(err error) bool {
	var fe *faults.Error
	return errors.As(err, &fe) && fe.Class == faults.Permanent
}

func sortStrings(s []string) {
	for i := 0; i < len(s); i++ {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

// RunStateBody 是 POST …/stop 与 …/start 的请求体。
type RunStateBody struct {
	Site string `json:"site,omitempty"`
	Role string `json:"role,omitempty"`
	Node string `json:"node,omitempty"`
}

func (a *API) stop(w http.ResponseWriter, r *http.Request, actor string) {
	a.setRunState(w, r, actor, spec.RunStateStopped)
}

func (a *API) start(w http.ResponseWriter, r *http.Request, actor string) {
	a.setRunState(w, r, actor, spec.RunStateRunning)
}

func (a *API) setRunState(w http.ResponseWriter, r *http.Request, actor, state string) {
	var body RunStateBody
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	n, err := a.S.SetRunState(r.Context(), SetRunStateRequest{
		Site: body.Site, Component: r.PathValue("name"),
		Role: body.Role, Node: body.Node, State: state, Actor: actor,
	})
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"component": r.PathValue("name"), "runState": state, "instances": n,
	})
}

// RemoveBody 是 DELETE …/components/{name} 的请求体。
//
// **确认串走请求体，不走 query**：URL 会进网关日志与浏览器历史，而这是
// 唯一一个「打错就毁数据」的参数。
type RemoveBody struct {
	Site string `json:"site,omitempty"`
	// Confirm 必须等于 Component 名——二档确认（10-cli §4.3）。
	//
	// **服务端也要验一遍。** CLI 那道提示挡得住手滑，挡不住一条直接
	// 打过来的 HTTP 请求，而 UI 与任何脚本走的都是这条路。
	Confirm    string `json:"confirm"`
	KeepConfig bool   `json:"keepConfig,omitempty"`
	PurgeData  bool   `json:"purgeData,omitempty"`
	PurgeUser  bool   `json:"purgeUser,omitempty"`
	Force      bool   `json:"force,omitempty"`
	// IgnoreNotFound 让删一个不存在的组件静默成功。
	IgnoreNotFound bool `json:"ignoreNotFound,omitempty"`
	// DryRun 只算影响面。UI 靠它在弹确认框之前把后果显示出来。
	DryRun bool `json:"dryRun,omitempty"`
}

func (a *API) removeComponent(w http.ResponseWriter, r *http.Request, actor string) {
	var body RemoveBody
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	name := r.PathValue("name")

	// 干跑不需要确认串：它什么都不动，而它存在的意义正是**让人在确认
	// 之前看清后果**。要求先确认再预览是把顺序反过来了。
	if !body.DryRun && body.Confirm != name {
		a.writeErr(w, r, http.StatusBadRequest, fmt.Errorf(
			"removal requires a second confirmation: set confirm to the component name %q\n"+
				"  this is the most dangerous operation in the whole tool, typing the component name is the only safeguard", name))
		return
	}

	res, err := a.S.RemoveComponent(r.Context(), RemoveRequest{
		Site: body.Site, Component: name,
		KeepConfig: body.KeepConfig, PurgeData: body.PurgeData,
		PurgeUser: body.PurgeUser, Force: body.Force,
		IgnoreNotFound: body.IgnoreNotFound, DryRun: body.DryRun,
		Actor: actor,
	})
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ApplyBody 是 POST …/apply 的请求体。
type ApplyBody struct {
	Site string `json:"site,omitempty"`
	// Doc 是已解析的声明文件。
	//
	// **由客户端解析 YAML，服务端收结构化的东西**：`setFile` 里的路径是
	// 客户端的概念（那个文件多半在另一台机器上），因此文件本身必须在
	// 客户端读完。既然如此，YAML 也一起在那边解掉。
	Doc *ApplyDoc `json:"doc"`
	// Secrets 是 setFile 读出来的明文，键为 "<组件名>/<参数名>"。
	Secrets map[string]any `json:"secrets,omitempty"`
	DryRun  bool           `json:"dryRun,omitempty"`
}

func (a *API) apply(w http.ResponseWriter, r *http.Request, actor string) {
	var body ApplyBody
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	if body.Doc == nil {
		a.writeErr(w, r, http.StatusBadRequest, errors.New("request has no doc"))
		return
	}
	// **服务端也校验一遍。** 客户端解析过了，但 API 是公开的——一条
	// 手工拼出来的请求同样会走到这里。
	if err := body.Doc.Validate(); err != nil {
		a.writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	res, err := a.S.Apply(r.Context(), ApplyRequest{
		Site: body.Site, Doc: body.Doc, Secrets: body.Secrets,
		DryRun: body.DryRun, Actor: actor,
	})
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) listOrphans(w http.ResponseWriter, r *http.Request, _ string) {
	out, err := a.S.ListOrphans(r.Context(),
		r.URL.Query().Get("site"), r.URL.Query().Get("node"))
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"orphans": out})
}

// PurgeOrphanBody 是 DELETE …/orphans/{node}/{instance} 的请求体。
type PurgeOrphanBody struct {
	Site string `json:"site,omitempty"`
	// Confirm 必须等于实例键——二档确认。
	//
	// **服务端也验一遍**：CLI 那道提示挡得住手滑，挡不住一条直接打过来的
	// 请求，而这一条会删掉真实的数据目录。
	Confirm string `json:"confirm"`
}

func (a *API) purgeOrphan(w http.ResponseWriter, r *http.Request, actor string) {
	var body PurgeOrphanBody
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	inst := r.PathValue("instance")
	if body.Confirm != inst {
		a.writeErr(w, r, http.StatusBadRequest, fmt.Errorf(
			"purge requires a second confirmation: set confirm to the instance key %q\n"+
				"  it deletes the real data directory on that machine, and it is unrecoverable", inst))
		return
	}
	if err := a.S.PurgeOrphan(r.Context(), PurgeOrphanRequest{
		Site: body.Site, Node: r.PathValue("node"), Instance: inst, Actor: actor,
	}); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"node": r.PathValue("node"), "instance": inst, "purgeRequested": true,
	})
}

// RestartBody 是 POST …/restart 的请求体。
type RestartBody struct {
	Site string `json:"site,omitempty"`
	Role string `json:"role,omitempty"`
	Node string `json:"node,omitempty"`
	// TimeoutSeconds 覆盖等结果的上限；0 表示用默认值。
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

func (a *API) restart(w http.ResponseWriter, r *http.Request, actor string) {
	var body RestartBody
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	res, err := a.S.Restart(r.Context(), RestartRequest{
		Site: body.Site, Component: r.PathValue("name"),
		Role: body.Role, Node: body.Node,
		Timeout: time.Duration(body.TimeoutSeconds) * time.Second,
		Actor:   actor,
	})
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	// **部分失败仍然回 200**：结果本身就是逐实例的，而 HTTP 状态码
	// 表达不了「三台里两台成功」。调用方看 instances 里的 state。
	writeJSON(w, http.StatusOK, res)
}

// DriftPolicyBody 是 POST …/drift-policy 的请求体。
type DriftPolicyBody struct {
	Site string `json:"site,omitempty"`
	// Policy 是 report | ignore | 空（清除覆盖）。
	Policy string `json:"policy"`
}

func (a *API) setDriftPolicy(w http.ResponseWriter, r *http.Request, actor string) {
	var body DriftPolicyBody
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	err := a.S.SetDriftPolicy(r.Context(), SetDriftPolicyRequest{
		Site: body.Site, Component: r.PathValue("name"),
		Policy: body.Policy, Actor: actor,
	})
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"component": r.PathValue("name"), "driftPolicy": body.Policy,
	})
}

// UpgradeBody 是 POST …/upgrade 的请求体。
type UpgradeBody struct {
	Site string `json:"site,omitempty"`
	// Version 为空表示升到本地可用的最新一版。
	Version string `json:"version,omitempty"`
	Force   bool   `json:"force,omitempty"`
	DryRun  bool   `json:"dryRun,omitempty"`
}

// RollbackBody 是 POST …/rollback 的请求体。
type RollbackBody struct {
	Site string `json:"site,omitempty"`
	// Version 为空表示回到上一版。
	Version string `json:"version,omitempty"`
	DryRun  bool   `json:"dryRun,omitempty"`
}

func (a *API) upgrade(w http.ResponseWriter, r *http.Request, actor string) {
	var body UpgradeBody
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	res, err := a.S.Upgrade(r.Context(), UpgradeRequest{
		Site: body.Site, Component: r.PathValue("name"),
		Version: body.Version, Force: body.Force, DryRun: body.DryRun, Actor: actor,
	})
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, versionChangeResponse(res))
}

func (a *API) rollback(w http.ResponseWriter, r *http.Request, actor string) {
	var body RollbackBody
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	res, err := a.S.Rollback(r.Context(), RollbackRequest{
		Site: body.Site, Component: r.PathValue("name"),
		Version: body.Version, DryRun: body.DryRun, Actor: actor,
	})
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, versionChangeResponse(res))
}

// VersionChangeResponse 是 upgrade / rollback 的应答。
type VersionChangeResponse struct {
	Component string            `json:"component"`
	Instances []string          `json:"instances"`
	Digests   map[string]string `json:"digests"`
	Warnings  []string          `json:"warnings,omitempty"`
	DryRun    bool              `json:"dryRun,omitempty"`
}

func versionChangeResponse(res *DeployResult) VersionChangeResponse {
	out := VersionChangeResponse{
		Component: res.Component, Digests: res.Digests,
		Warnings: res.Warnings, DryRun: res.DryRun,
	}
	for k := range res.Digests {
		out.Instances = append(out.Instances, k)
	}
	sort.Strings(out.Instances)
	return out
}

func (a *API) rolloutPolicy(w http.ResponseWriter, r *http.Request, _ string) {
	v, err := a.S.RolloutPolicy(r.Context(),
		r.URL.Query().Get("site"), r.PathValue("name"))
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) setRolloutPolicy(w http.ResponseWriter, r *http.Request, actor string) {
	// **指针而不是 int**：0 是 canary 的合法值（关掉金丝雀），
	// 用 0 当「没传」会让人永远关不掉它。
	var body struct {
		Site           string `json:"site,omitempty"`
		MaxUnavailable *int   `json:"maxUnavailable,omitempty"`
		Canary         *int   `json:"canary,omitempty"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	v, err := a.S.SetRolloutPolicy(r.Context(), body.Site, r.PathValue("name"),
		body.MaxUnavailable, body.Canary, actor)
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) rolloutStatus(w http.ResponseWriter, r *http.Request, _ string) {
	v, err := a.S.RolloutStatus(r.Context(),
		r.URL.Query().Get("site"), r.PathValue("name"))
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *API) rolloutHistory(w http.ResponseWriter, r *http.Request, _ string) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := a.S.RolloutHistory(r.Context(),
		r.URL.Query().Get("site"), r.PathValue("name"), limit)
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rollouts": list})
}

// rolloutAction 处理 pause / resume / abort。
//
// 三个动作共用一个路由而不是三条：它们的入参与应答完全相同，
// 分开只会让新增一个动作要改四处。
func (a *API) rolloutAction(w http.ResponseWriter, r *http.Request, actor string) {
	var body struct {
		Site string `json:"site,omitempty"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	name := r.PathValue("name")

	var (
		v   *RolloutView
		err error
	)
	switch action := r.PathValue("action"); action {
	case "pause":
		v, err = a.S.SetRolloutPaused(r.Context(), body.Site, name, true, actor)
	case "resume":
		v, err = a.S.SetRolloutPaused(r.Context(), body.Site, name, false, actor)
	case "abort":
		v, err = a.S.AbortRollout(r.Context(), body.Site, name, actor)
	default:
		a.writeErr(w, r, http.StatusNotFound,
			fmt.Errorf("unknown action %q, available: pause | resume | abort", action))
		return
	}
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
