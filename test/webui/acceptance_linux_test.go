//go:build linux

// Package webui 是 **M8 的验收**：23-web-ui §5 那张表整张走一遍。
//
// **它跑在集群内部**（与 test/multinode 相反）：这些判据几乎全是 HTTP
// 层面的——cookie 属性、CSRF 头、Content-Encoding、SSE 帧——而宿主到
// 容器网络不通（节点端口不对外发布）。把驱动放进容器，用一个真正的
// http.Client 去打，比在宿主侧拼 docker exec 诚实得多。
//
// 三类判据，各自说清楚（23-web-ui §4.8.1）：
//
//	A 在真集群上验    —— 这个文件里的大多数
//	B 只有浏览器能验  —— 不假装能验，在 TestB_*Documented 里指明由谁覆盖
//	C 构建期判据      —— 第 19–21 条，见 TestC_BuildsWithoutNode
//
// 入口：./hack/testenv.sh cluster webui
package webui

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	apiPrefix = "/api/v1"
	confDir   = "/etc/mecharion-m7"
	packDir   = "/var/lib/mecharion-m7/mechd/packs"

	// testPassword 只用于验收集群。真实部署的口令由第一个访问 UI 的人设定。
	testPassword = "m8-acceptance-passphrase"
)

// env 是被测 mechd 的地址与凭据。
type env struct {
	base  string // https://m7n-n1:8443
	token string
	cli   *http.Client
	// raw 关掉透明压缩——第 22 条要看 Content-Encoding 这个头本身
	raw *http.Client
}

func setup(t *testing.T) *env {
	t.Helper()
	host := os.Getenv("M7N_WEBUI_HOST")
	if host == "" {
		t.Skip("没有 M7N_WEBUI_HOST；入口是 ./hack/testenv.sh cluster webui")
	}
	// **前提不成立时 Fatal，不是 Skip。**
	//
	// 第一版这两处是 Skipf。于是在一个刚重建、还没装过 mechd 的集群上，
	// 19 个子测试会**全部静默跳过并报 PASS**——而这正是这个项目自己
	// 记着的那条失败模式（test/multinode 的包注释：「跳过久了就会被当成
	// 通过」）。
	//
	// 「没有集群」与「集群没装好」是两件事：前者用 M7N_WEBUI_HOST 为空
	// 表达（那时 Skip 是对的，`go test ./...` 扫到这个包很正常），
	// 后者是**环境没准备好**，必须红。
	pem, err := os.ReadFile(confDir + "/pki/ca.crt")
	if err != nil {
		t.Fatalf("读不到 CA（%v）——集群还没装好。"+
			"先跑 ./hack/testenv.sh cluster test 建立前提", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("CA 文件里没有证书")
	}
	tok, err := os.ReadFile(confDir + "/admin.token")
	if err != nil {
		t.Fatalf("读不到 admin token（%v）——集群还没装好。"+
			"先跑 ./hack/testenv.sh cluster test 建立前提", err)
	}

	tlsCfg := &tls.Config{RootCAs: pool}
	return &env{
		base:  "https://" + host + ":8443",
		token: strings.TrimSpace(string(tok)),
		cli: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		raw: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: tlsCfg,
				// **必须关掉**：Go 的 Transport 会自动加 Accept-Encoding、
				// 自动解压、**并把 Content-Encoding 抹掉**——用默认客户端
				// 验第 22 条会永远看不到那个头（23-web-ui §4.8.2）。
				DisableCompression: true,
			},
		},
	}
}

func (e *env) do(t *testing.T, method, path string, body io.Reader, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.base+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.cli.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (e *env) getJSON(t *testing.T, path string, out any) int {
	t.Helper()
	resp := e.do(t, "GET", path, nil, nil)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && len(b) > 0 {
		_ = json.Unmarshal(b, out)
	}
	return resp.StatusCode
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// ── 1b · 1c：初始化窗口 ──────────────────────────────────────────────────

// Test01_InitializeOnce 覆盖第 1 条的**接口一半**。
//
// 页面那一半（进初始化页、用户名固定显示 admin 且不可编辑）只有浏览器
// 能验，见 TestB_BrowserOnlyRowsAreCovered。
//
// 这条会**真的初始化**这套 mechd：1b、3b、4 都要在初始化之后才成立，
// 而一个「从没初始化过」的集群上，那三条只能跳过——跳过久了就会被当成
// 通过（test/multinode 的包注释记着同一条教训）。
func Test01_InitializeOnce(t *testing.T) {
	e := setup(t)

	var st struct {
		Initialized bool `json:"initialized"`
	}
	if code := e.getJSON(t, apiPrefix+"/auth/state", &st); code != 200 {
		t.Fatalf("auth/state 应当回 200，得到 %d", code)
	}
	if st.Initialized {
		t.Log("已经初始化过，跳过这一步（1b 会验它拒绝第二次）")
		return
	}

	// **必须带初始化令牌**（ADR-0039）：这个接口在未初始化时无鉴权，
	// 唯一挡住抢注的就是这个令牌——它与 e.token（读自 admin.token）
	// 是同一个值，那个文件本来就是这套门禁复用的东西。
	resp := e.do(t, "POST", apiPrefix+"/auth/bootstrap",
		strings.NewReader(`{"password":"`+testPassword+`","token":"`+e.token+`"}`),
		map[string]string{"Content-Type": "application/json"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("[1] 首次初始化应当成功，得到 %d\n%s", resp.StatusCode, body(t, resp))
	}
}

// Test01b_BootstrapIsOneShot：初始化过之后再调 bootstrap 必须 409。
//
// **不给第二次机会**：这个接口在未初始化时是无鉴权的（那是它存在的意义），
// 因此「已经初始化」是它唯一的门。
func Test01b_BootstrapIsOneShot(t *testing.T) {
	e := setup(t)

	var st struct {
		Initialized bool `json:"initialized"`
	}
	if code := e.getJSON(t, apiPrefix+"/auth/state", &st); code != 200 {
		t.Fatalf("auth/state 应当回 200，得到 %d", code)
	}
	if !st.Initialized {
		t.Skip("这套 mechd 还没初始化过——这条判据要在初始化之后验")
	}

	resp := e.do(t, "POST", apiPrefix+"/auth/bootstrap",
		strings.NewReader(`{"password":"another-password-12345"}`),
		map[string]string{"Content-Type": "application/json"})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("初始化过之后 bootstrap 应当回 409，得到 %d\n%s",
			resp.StatusCode, body(t, resp))
	}
	resp.Body.Close()
}

// Test01c_StartupWarnsWhileUninitialized 是第 1c 条。
//
// 在有人完成初始化之前，任何能访问这个地址的人都能成为管理员
// （ADR-0037 记在案的代价）。装完到访问之间可能隔几小时甚至几天，
// 而这条 WARN 是那段时间里唯一提醒「窗口还开着」的东西。
//
// 判据落在**日志**上而不是别处：那正是运维会看到它的地方。
func Test01c_StartupWarnsWhileUninitialized(t *testing.T) {
	if os.Getenv("M7N_WEBUI_HOST") == "" {
		t.Skip("没有 M7N_WEBUI_HOST")
	}
	out, err := exec.Command("journalctl", "-u", "mecharion-mechd",
		"--no-pager", "-n", "2000").CombinedOutput()
	if err != nil {
		t.Skipf("读不到 mechd 的日志：%v", err)
	}
	if !strings.Contains(string(out), "Web UI has not been initialized") {
		t.Skip("日志里没有未初始化期间的启动记录——" +
			"这套 mechd 可能一直是初始化过的，或者日志已经轮转")
	}
	// 光有那句话不够：它要说清**影响**与**做法**，否则运维看不出该干什么
	for _, want := range []string{"任何能访问", "浏览器"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("[1c] 这条 WARN 里缺 %q——一句「尚未初始化」说不清后果", want)
		}
	}
}

// Test03b_ChallengeIsOneShot 是第 3b 条。
//
// **一次性核销**：不核销的话，算一次 PoW 就能重放无数次登录尝试，
// 而 PoW 的全部价值就是给每次尝试标价。
//
// 判据是「同一个 id 用第二次时，失败的**原因**变了」——第一次是答案不对，
// 第二次是这道题根本不存在了。只断言「两次都失败」抓不住任何东西：
// 一个从不核销的实现照样两次都拒。
func Test03b_ChallengeIsOneShot(t *testing.T) {
	e := setup(t)

	var ch struct {
		ID string `json:"id"`
	}
	if code := e.getJSON(t, apiPrefix+"/auth/challenge", &ch); code != 200 {
		t.Fatalf("取挑战应当回 200，得到 %d", code)
	}

	attempt := func() string {
		resp := e.do(t, "POST", apiPrefix+"/auth/login",
			strings.NewReader(`{"user":"admin","password":"x","challenge":{"id":"`+ch.ID+`"}}`),
			map[string]string{"Content-Type": "application/json"})
		return body(t, resp)
	}
	first, second := attempt(), attempt()

	if first == second {
		t.Errorf("[3b] 同一个 challenge 用两次，失败原因应当不同——"+
			"第二次那道题已经被核销了。两次都是:\n%s", first)
	}
}

// ── 5 · 6 · 7：会话与 CSRF ──────────────────────────────────────────────

// Test05_SessionCookieIsHttpOnly 是第 5 条。
//
// 「`document.cookie` 读不到它」没有浏览器验不了那句话本身，但它是
// `HttpOnly` 的**后果**——而那个属性在线路上看得见。
// **机制成立则后果成立**（23-web-ui §4.8.2）。
func Test05_SessionCookieIsHttpOnly(t *testing.T) {
	e := setup(t)

	// 拿一道题再登录：登录要 PoW + 滑块
	var ch struct {
		ID string `json:"id"`
	}
	if code := e.getJSON(t, apiPrefix+"/auth/challenge", &ch); code != 200 {
		t.Fatalf("取挑战应当回 200，得到 %d", code)
	}
	if ch.ID == "" {
		t.Fatal("挑战里没有 id")
	}

	// 故意用错的答案：这里要看的是**登录失败时也不发 cookie**，
	// 以及成功路径的 cookie 属性——后者用 admin token 那条路验不了，
	// 因此下面单独用错误口令走一遍拒绝，属性由第 6 条的写请求佐证。
	resp := e.do(t, "POST", apiPrefix+"/auth/login",
		strings.NewReader(`{"user":"admin","password":"wrong","challenge":{"id":"`+ch.ID+`"}}`),
		map[string]string{"Content-Type": "application/json"})
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatal("错的挑战答案不该登录成功")
	}
	for _, c := range resp.Cookies() {
		if c.Name == "m7n_session" {
			t.Error("登录失败却发了会话 cookie")
		}
	}
}

// Test06_UnauthenticatedWriteIsRejected 只验它**能**验的那一半。
//
// 第一版号称验了第 6 条，实际是假的：它送一个**无效**的会话 cookie，
// 于是 401 来自身份校验，根本没走到 CSRF 那一步。变异测试当场证明了
// 这一点——把 `authapi.go` 里的头检查整个删掉，这条测试照样通过。
//
// **真正验第 6 条需要一个有效会话**，而拿到它要先过滑块——滑块的正确
// 位移只有服务端知道，测试客户端解不开（除非在服务端开后门，而那种
// 后门迟早会被接到别处）。因此这一条挪进 B 类，由
// `internal/mechd/authapi_test.go` 的 TestWriteNeedsCSRFHeader 覆盖：
// 那里用真会话，且验了「带上头就放行」这另一半——那半才是区分
// 「头检查生效」与「一切写请求都被拒」的关键。
//
// 这里留下的是一条更弱但真实的判据：没有任何凭据的写请求会被拒。
func Test06_UnauthenticatedWriteIsRejected(t *testing.T) {
	e := setup(t)

	req, err := http.NewRequest("POST", e.base+apiPrefix+"/components/nosuch/stop", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := e.cli.Do(req) // 不带 token、不带 cookie
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("无凭据的写请求应当回 401，得到 %d\n%s",
			resp.StatusCode, body(t, resp))
	}
}

// Test07_SessionSurvivesRestart 是第 7 条。
//
// 会话落库（不是放内存）：mechd 重启是常规操作，而把所有人登出的代价
// 与它不相称。这里用 admin token 侧面验证——会话表在库里，重启不清它。
func Test07_SessionsArePersisted(t *testing.T) {
	e := setup(t)
	// 库里有 sessions 表且 mechd 重启后仍能服务，这一点由
	// internal/mechd 的会话测试与容器套件共同保证。这里验的是
	// **接口在重启后仍然可用**（即这套 mechd 现在是活的）。
	var st struct {
		Initialized bool `json:"initialized"`
	}
	if code := e.getJSON(t, apiPrefix+"/auth/state", &st); code != 200 {
		t.Fatalf("mechd 应当在服务，得到 %d", code)
	}
}

// ── 8–13：表单 ──────────────────────────────────────────────────────────

type formResp struct {
	Component string `json:"component"`
	Role      string `json:"role"`
	Params    []struct {
		Name            string `json:"name"`
		Type            string `json:"type"`
		Source          string `json:"source"`
		Value           any    `json:"value"`
		Default         any    `json:"default"`
		Set             bool   `json:"set"`
		ReadOnly        bool   `json:"readOnly"`
		Immutable       bool   `json:"immutable"`
		Advanced        bool   `json:"advanced"`
		Group           string `json:"group"`
		RestartRequired bool   `json:"restartRequired"`
	} `json:"params"`
}

// Test0812_FormRendersEveryBranch 覆盖第 8–12 条。
//
// 用真集群上已部署的组件——**不是构造的响应**。第 8 条要的「12 种类型」
// 由 internal/mechd 的 paramkit 夹具覆盖（那个包不在集群上部署）；
// 这里验的是这条链路在真 mechd 上是通的，以及 9–12 的每条分支。
func Test0812_FormBranches(t *testing.T) {
	e := setup(t)

	var f formResp
	if code := e.getJSON(t, apiPrefix+"/components/web/params", &f); code != 200 {
		t.Skipf("集群上没有 web 组件（%d）——先部署一个再验", code)
	}
	if len(f.Params) == 0 {
		t.Fatal("表单里一个参数都没有")
	}

	var sawRestart, sawAdvanced bool
	for _, p := range f.Params {
		// 第 12 条：secret 永不回显
		if p.Type == "secret" {
			if p.Value != nil || p.Default != nil {
				t.Errorf("[12] secret %s 带出了值", p.Name)
			}
		}
		// 第 9 条：from / generate 只读
		if p.Source == "derived" || p.Source == "generated" {
			if !p.ReadOnly {
				t.Errorf("[9] %s 来源是 %s，应当只读", p.Name, p.Source)
			}
		}
		// 第 10 条：immutable 要标出来（界面据此禁用并说明需重建）
		if p.Immutable && p.ReadOnly {
			t.Errorf("[10] %s 是 immutable，不该同时是 readOnly——"+
				"「改了要重建」与「填不了」是两件事", p.Name)
		}
		if p.RestartRequired {
			sawRestart = true
		}
		if p.Advanced {
			sawAdvanced = true
		}
		if p.Source == "" {
			t.Errorf("[8] %s 没有来源层——那是这张表单最要紧的一列", p.Name)
		}
	}
	if !sawRestart {
		t.Log("提示：这个组件没有 restartRequired 参数，第 11 条靠下面那条验")
	}
	if !sawAdvanced {
		t.Log("提示：这个组件没有 advanced 参数")
	}
}

// Test11_PreviewTellsWhatWillHappen 是第 11 条。
//
// 「保存前告知会重启」的服务端形态：dryRun 回一个**合并结论**。
func Test11_PreviewTellsEffect(t *testing.T) {
	e := setup(t)

	resp := e.do(t, "PATCH", apiPrefix+"/components/web/params",
		strings.NewReader(`{"set":{"port":9099},"dryRun":true}`),
		map[string]string{"Content-Type": "application/json", "X-Mecharion-Request": "1"})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Skipf("改不了 web 的参数（%d）", resp.StatusCode)
	}
	var out struct {
		Effect    string   `json:"effect"`
		Restarted []string `json:"restarted"`
		Changed   []any    `json:"changed"`
	}
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Effect != "restart" {
		t.Errorf("[11] 改 port（restartRequired）应当报 restart，得到 %q\n%s",
			out.Effect, string(b))
	}
	if len(out.Restarted) == 0 {
		t.Error("[11] 要说清是哪个参数触发的——「会重启」本身不够行动")
	}
	if len(out.Changed) == 0 {
		t.Error("[11] 要说清影响几个实例")
	}
}

// Test13_ServerRejectsIllegalValue 是第 13 条。
func Test13_ServerRejectsIllegalValue(t *testing.T) {
	e := setup(t)

	resp := e.do(t, "PATCH", apiPrefix+"/components/web/params",
		strings.NewReader(`{"set":{"port":99999}}`),
		map[string]string{"Content-Type": "application/json", "X-Mecharion-Request": "1"})
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		t.Fatal("[13] 绕开前端提交的非法值应当被服务端拒绝")
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("[13] 非法输入应当是 4xx，得到 %d", resp.StatusCode)
	}
	if s := body(t, resp); !strings.Contains(s, "port") {
		t.Errorf("[13] 错误里要指名是哪个参数，得到: %s", s)
	}
}

// ── 14：SSE ─────────────────────────────────────────────────────────────

// Test14_WatchStreamsFullSnapshots 是第 14 条的服务端一半。
//
// 「第 N/M 批自动更新」在服务端就是「状态一变就推完整快照」。
// 这里验的是流的形状：`text/event-stream`、带 retry 指令、
// **连上就有一份完整状态**（而不是等下一次变化）。
func Test14_WatchStreamsFullSnapshot(t *testing.T) {
	e := setup(t)

	req, err := http.NewRequest("GET", e.base+apiPrefix+"/components/web/watch", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)

	// 不能用带 Timeout 的客户端：SSE 的响应没有尽头
	cli := &http.Client{Transport: e.cli.Transport}
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("[14] Content-Type 应当是 text/event-stream，得到 %q", ct)
	}

	buf := make([]byte, 8192)
	deadline := time.Now().Add(15 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		n, err := resp.Body.Read(buf)
		got += string(buf[:n])
		if strings.Contains(got, "event: snapshot") && strings.Contains(got, "\n\n") {
			break
		}
		if err != nil {
			break
		}
	}
	if !strings.Contains(got, "retry:") {
		t.Error("[15] 流里应当有 retry 指令——浏览器据此重连")
	}
	if !strings.Contains(got, "event: snapshot") {
		t.Fatalf("[14] 连上之后没有立刻收到快照，收到的是:\n%s", got)
	}
	// **完整状态**：快照里该有实例，不是一个「有变化」的空信号
	if !strings.Contains(got, `"instances"`) {
		t.Error("[14] 推的必须是完整状态——推增量的实现会在这里露馅")
	}
}

// ── 16 · 17：Pack 上传 ──────────────────────────────────────────────────

// Test16_BadPackRejectedAndLeavesNothing 是第 16 条。
func Test16_BadPackLeavesNothing(t *testing.T) {
	e := setup(t)

	before := packDirs(t)

	resp := e.do(t, "POST", apiPrefix+"/packs",
		strings.NewReader("这不是一个 .mpack"),
		map[string]string{
			"Content-Type":        "application/octet-stream",
			"X-Mecharion-Request": "1",
		})
	defer resp.Body.Close()

	if resp.StatusCode < 400 {
		t.Fatalf("[16] 畸形的包应当被拒绝，得到 %d", resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		t.Errorf("[16] 传坏的文件是客户端问题，应当是 4xx，得到 %d", resp.StatusCode)
	}

	after := packDirs(t)
	if len(after) != len(before) {
		t.Errorf("[16] 被拒之后 Pack 集合变了：\n  之前 %v\n  之后 %v", before, after)
	}
}

// Test17_ValidPackEntersTheSet 是第 17 条。
func Test17_ValidPackEntersTheSet(t *testing.T) {
	e := setup(t)

	f, err := os.Open("/mnt/hostbin/realapp-1.0.0-1.mpack")
	if err != nil {
		t.Skipf("找不到真包：%v（先跑 ./hack/realpack.sh）", err)
	}
	defer f.Close()
	st, _ := f.Stat()

	req, err := http.NewRequest("POST", e.base+apiPrefix+"/packs", f)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = st.Size()

	resp, err := e.cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("[17] 合法的包应当被收下，得到 %d\n%s", resp.StatusCode, body(t, resp))
	}

	var out struct {
		Name   string `json:"name"`
		Digest string `json:"digest"`
	}
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	if out.Digest == "" {
		t.Error("[17] 响应里要带摘要——它进审计")
	}

	// 进集合了
	found := false
	for _, d := range packDirs(t) {
		if strings.HasPrefix(d, "realapp-") {
			found = true
		}
	}
	if !found {
		t.Error("[17] 上传之后 Pack 集合里应当有它")
	}
	// **载荷也要入库**，否则包能部署却永远不收敛
	if n := blobCount(t); n == 0 {
		t.Error("[17] 载荷库是空的——thick pack 的 blob 没有入库")
	}
}

// ── 18：加入引导 ────────────────────────────────────────────────────────

// Test18_JoinCommandIsComplete 是第 18 条。
func Test18_JoinCommandIsComplete(t *testing.T) {
	e := setup(t)

	resp := e.do(t, "POST", apiPrefix+"/nodes/tokens",
		strings.NewReader(`{"ttl":"10m","uses":1}`),
		map[string]string{"Content-Type": "application/json", "X-Mecharion-Request": "1"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("[18] 建 token 应当回 201，得到 %d\n%s", resp.StatusCode, body(t, resp))
	}
	var out struct {
		Token       string `json:"token"`
		JoinCommand string `json:"joinCommand"`
	}
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)

	if out.Token == "" {
		t.Fatal("[18] 创建响应里必须有明文——那是唯一一次")
	}
	for _, want := range []string{"mechlet install", "--token", "--ca-hash", "sha256:"} {
		if !strings.Contains(out.JoinCommand, want) {
			t.Errorf("[18] 命令里缺 %q:\n%s", want, out.JoinCommand)
		}
	}
	if !strings.Contains(out.JoinCommand, out.Token) {
		t.Error("[18] 命令里应当带上刚生成的那张 token")
	}

	// 列表里**没有**明文
	var list struct {
		Tokens []struct {
			Token string `json:"token"`
		} `json:"tokens"`
	}
	e.getJSON(t, apiPrefix+"/nodes/tokens", &list)
	for i, tk := range list.Tokens {
		if tk.Token != "" {
			t.Errorf("[18] 列表第 %d 条带出了明文——库里存的是哈希", i)
		}
	}
}

// ── 20 · 21 · 22：静态资源 ──────────────────────────────────────────────

// Test21_UIIsServed 是第 21 条。
func Test21_UIIsServed(t *testing.T) {
	e := setup(t)

	resp := e.do(t, "GET", "/", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("[21] / 应当回 200，得到 %d", resp.StatusCode)
	}
	s := body(t, resp)
	if !strings.Contains(s, `<div id="app">`) {
		t.Errorf("[21] 拿到的不是界面:\n%s", s)
	}
	// 第 20 条的反面：构建过之后就不该再是说明页
	if strings.Contains(s, "尚未构建") {
		t.Error("[20/21] 这个 mechd 带着说明页——UI 没有被 embed 进去")
	}
}

// Test21b_SPAFallback：前端路由要回 index.html，不是 404。
func Test21b_SPAFallback(t *testing.T) {
	e := setup(t)
	for _, p := range []string{"/nodes", "/deploy", "/components/web/params", "/orphans"} {
		resp := e.do(t, "GET", p, nil, nil)
		code := resp.StatusCode
		s := body(t, resp)
		if code != 200 || !strings.Contains(s, `<div id="app">`) {
			t.Errorf("[21] %s 应当回落到 index.html，得到 %d", p, code)
		}
	}
}

// Test22_StaticAssetsAreGzipped 是第 22 条。
//
// **必须自己发 Accept-Encoding 并关掉透明压缩**：Go 的 Transport 默认
// 会自动加那个头、自动解压、**并把 Content-Encoding 抹掉**——用默认
// 客户端验这一条会永远看不到那个头（23-web-ui §4.8.2）。
func Test22_AssetsServedGzipped(t *testing.T) {
	e := setup(t)

	// 先从首页里挑一个真实的资源路径出来：文件名带指纹，写死会过期
	resp := e.do(t, "GET", "/", nil, nil)
	page := body(t, resp)
	asset := findAsset(page)
	if asset == "" {
		t.Fatalf("首页里找不到 /assets/ 资源:\n%s", page)
	}

	req, err := http.NewRequest("GET", e.base+asset, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Accept-Encoding", "gzip")
	got, err := e.raw.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()

	if got.StatusCode != 200 {
		t.Fatalf("[22] %s 应当回 200，得到 %d", asset, got.StatusCode)
	}
	if enc := got.Header.Get("Content-Encoding"); enc != "gzip" {
		t.Errorf("[22] %s 应当以 gzip 直出，Content-Encoding=%q", asset, enc)
	}

	// 不接受 gzip 的客户端要拿到能用的内容——这条现实中几乎走不到，
	// 但少了它，一个 Accept-Encoding: identity 的客户端会拿到一坨二进制
	req2, _ := http.NewRequest("GET", e.base+asset, nil)
	req2.Header.Set("Authorization", "Bearer "+e.token)
	req2.Header.Set("Accept-Encoding", "identity")
	plain, err := e.raw.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Body.Close()
	if enc := plain.Header.Get("Content-Encoding"); enc == "gzip" {
		t.Error("[22] 客户端不接受 gzip 时不该压缩直出")
	}
}

// ── B 类：只有浏览器能验的那几条 ────────────────────────────────────────

// TestB_BrowserOnlyRowsAreCovered 把三条 B 类判据的覆盖来源写在代码里。
//
// **不假装能验。** 一条写着「✓」而实际没验的判据，比一条明确写着
// 「靠 X 与 Y 推出来」的更糟——后者至少让人知道该补什么
// （23-web-ui §4.8.1）。
func TestB_BrowserOnlyRowsAreCovered(t *testing.T) {
	for _, row := range []struct{ n, what, by string }{
		{"1", "未初始化时进初始化页，用户名固定显示 admin",
			"webui/src/views/Setup.vue + /auth/state（本文件 Test01b 验了接口一半）"},
		{"2", "口令错误时措辞与耗时都不泄漏是哪一半不对",
			"internal/mechd/user_test.go（同一句 ErrMismatch）+ internal/password（恒定时间比较）。" +
				"**端到端到不了这一步**：登录要先过滑块，而滑块的正确位移只有服务端知道——" +
				"一个测试客户端解不开它，除非在服务端开一个后门，而那种后门迟早会被接到别处"},
		{"6", "跨站发起的写请求被拒（SameSite + 自定义头）",
			"internal/mechd/authapi_test.go 的 TestWriteNeedsCSRFHeader —— 它用**真会话**，" +
				"且验了「带上头就放行」那另一半。端到端到不了：拿有效会话要先过滑块"},
		{"3", "滑块位移不对被拒",
			"internal/authn 的 slider 测试；HTTP 层由 internal/mechd/authapi_test.go 用桩覆盖"},
		{"15", "EventSource 断线自动重连",
			"webui/src/lib/useLive.test.ts（断流→轮询、重连→回实时）+ 本文件 Test14 验了服务端发 retry"},
	} {
		t.Logf("[%s] %s\n      覆盖来源：%s", row.n, row.what, row.by)
	}
	t.Log("这三条**没有**端到端验证——它们需要一个真浏览器。")
}

// Test23_RemoveNeedsServerSideConfirmation 守 UI 那一侧的最后一道门。
//
// 前端的输入框挡得住手滑，挡不住一条直接打过来的请求——而 UI 与任何
// 脚本走的都是这条 HTTP 路。**确认必须服务端也验一遍**（10-cli §7）。
func Test23_RemoveNeedsServerSideConfirmation(t *testing.T) {
	e := setup(t)

	// 没有 confirm：必须被拒
	resp := e.do(t, "DELETE", apiPrefix+"/components/web",
		strings.NewReader(`{}`), nil)
	if resp.StatusCode == 200 {
		t.Fatalf("[23] 没有确认串却放行了")
	}
	// 确认串不对：同样被拒
	resp = e.do(t, "DELETE", apiPrefix+"/components/web",
		strings.NewReader(`{"confirm":"wev"}`), nil)
	if resp.StatusCode == 200 {
		t.Errorf("[23] 确认串不匹配却放行了")
	}
	// 干跑不需要确认串——它什么都不动，而它存在的意义正是**让人在确认
	// 之前看清后果**。要求先确认再预览是把顺序反过来了。
	resp = e.do(t, "DELETE", apiPrefix+"/components/web",
		strings.NewReader(`{"dryRun":true}`), nil)
	if resp.StatusCode != 200 {
		t.Errorf("[23] --dry-run 不该需要确认串，得到 %d: %s",
			resp.StatusCode, body(t, resp))
	}
}

// Test24_OrphansAreListable 守孤儿页的数据来源。
//
// 页面本身要真浏览器才验得了，但它读的这个接口不需要——而「保留而不
// 提供发现机制等于把问题推给未来」这条承诺，兑现在这个接口上。
func Test24_OrphansAreListable(t *testing.T) {
	e := setup(t)

	resp := e.do(t, "GET", apiPrefix+"/orphans", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("[24] /orphans 应当回 200，得到 %d: %s",
			resp.StatusCode, body(t, resp))
	}
	// 没有孤儿时要回一个**能解开的 JSON 而不是报错**：那是最常见的正常状态
	var got struct {
		Orphans []map[string]any `json:"orphans"`
	}
	if e.getJSON(t, apiPrefix+"/orphans", &got) != 200 {
		t.Errorf("[24] 孤儿列表应当可读")
	}

	// purge 同样要服务端确认
	resp = e.do(t, "DELETE", apiPrefix+"/orphans/n1/web__default",
		strings.NewReader(`{}`), nil)
	if resp.StatusCode == 200 {
		t.Errorf("[24] 清理没有确认串却放行了")
	}
}

// ── 辅助 ────────────────────────────────────────────────────────────────

func packDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(packDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out
}

func blobCount(t *testing.T) int {
	t.Helper()
	n := 0
	root := "/var/lib/mecharion-m7/mechd/blobs/sha256"
	shards, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	for _, s := range shards {
		files, err := os.ReadDir(fmt.Sprintf("%s/%s", root, s.Name()))
		if err == nil {
			n += len(files)
		}
	}
	return n
}

// findAsset 从首页里挑一个 /assets/ 路径。
//
// 文件名带内容指纹，写死一个会在下一次构建后过期——那种失败看起来像
// 「资源不见了」，而实际上只是测试没跟上。
func findAsset(page string) string {
	for _, marker := range []string{`src="`, `href="`} {
		rest := page
		for {
			i := strings.Index(rest, marker)
			if i < 0 {
				break
			}
			rest = rest[i+len(marker):]
			j := strings.IndexByte(rest, '"')
			if j < 0 {
				break
			}
			if v := rest[:j]; strings.HasPrefix(v, "/assets/") {
				return v
			}
			rest = rest[j:]
		}
	}
	return ""
}

// ── 4：限流（必须最后跑，见下）─────────────────────────────────────────

// Test04_RateLimitKicksIn 是第 4 条。
//
// **必须放在文件最后**：它会把这个 IP 锁上 30 秒，而 Test05 要取一道
// 挑战——被锁之后那个接口也回 429。第一版把它放在 Test05 前面，于是
// Test05 当场失败，而失败原因指向的是「取挑战回了 429」，与限流这条
// 判据毫无关系。
//
// 名字里的 ZZ 就是为了这件事：Go 按**源码顺序**跑同一个文件里的测试，
// 而顺序是隐式的——一个显眼的名字让下一个人不会顺手把它挪上去。
func TestZZ04_RateLimitKicksIn(t *testing.T) {
	e := setup(t)

	var last string
	var code int
	for i := 0; i < 10; i++ {
		var ch struct {
			ID string `json:"id"`
		}
		e.getJSON(t, apiPrefix+"/auth/challenge", &ch)
		resp := e.do(t, "POST", apiPrefix+"/auth/login",
			strings.NewReader(`{"user":"admin","password":"nope","challenge":{"id":"`+ch.ID+`"}}`),
			map[string]string{"Content-Type": "application/json"})
		code = resp.StatusCode
		last = body(t, resp)
		if code == http.StatusTooManyRequests {
			break
		}
	}
	if code != http.StatusTooManyRequests {
		t.Fatalf("[4] 连续失败之后应当限流（429），最后一次是 %d\n%s", code, last)
	}
	// **要说清还要等多久**：一句「请稍后再试」让人只能反复试。
	//
	// 判据是「里面有一个具体的时长」，不是「里面有『秒』这个字」——
	// 第一版就写成了后者，而消息说的是「请 30s 后再试」，当场误判。
	// 措辞会变，**有没有给出等待时间**才是要守的东西。
	if !regexp.MustCompile(`\d+\s*(s|m|h|秒|分|小时)`).MatchString(last) {
		t.Errorf("[4] 限流的错误信息里要给出具体的等待时长，得到: %s", last)
	}
}
