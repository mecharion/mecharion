package mechd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/spec"
)

// 本文件验证：500 不泄漏内部错误、有稳定的 code 与 requestId、
// 状态分类不再依赖中文文案子串。

// reqWithID 构造一个挂着固定 requestId 的请求，绕开 withRequestID
// 中间件的随机性，方便断言拿到的正是塞进去的那个值。
func reqWithID(id string) *http.Request {
	r := httptest.NewRequest("GET", "/whatever", nil)
	return r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
}

// TestWriteErrHidesInternalDetailOn500 断言：500 的响应体不能包含
// err.Error() 里的任何内容——那可能是 SQL、内部路径、配置细节。
// 客户端只该拿到一个通用文案 + requestId，真正的 cause 留在服务端
// 日志里。
func TestWriteErrHidesInternalDetailOn500(t *testing.T) {
	f := newFixture(t)
	api := &API{S: f.svc}

	sensitive := "store: 查询 Node: open /etc/mecharion/secret.db: permission denied"
	rec := httptest.NewRecorder()
	req := reqWithID("req-abc123")

	api.writeErr(rec, req, http.StatusInternalServerError, errors.New(sensitive))

	if strings.Contains(rec.Body.String(), sensitive) {
		t.Fatalf("500 响应体泄漏了内部错误文本: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret.db") {
		t.Fatalf("500 响应体里出现了内部路径片段: %s", rec.Body.String())
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v\n%s", err, rec.Body.String())
	}
	if body["code"] != "internal" {
		t.Errorf(`code = %q，期望 "internal"`, body["code"])
	}
	if body["requestId"] != "req-abc123" {
		t.Errorf("requestId = %q，期望原样带回请求里挂的那个值", body["requestId"])
	}
	if body["error"] == "" {
		t.Error("error 字段不该是空的——客户端至少要知道出错了")
	}
}

// TestWriteErrKeepsUserFacingTextOn400 是对照组：非 500 的错误文案本来
// 就是写给用户看的说明，必须原样送达，不能被 500 的隐藏逻辑误伤。
func TestWriteErrKeepsUserFacingTextOn400(t *testing.T) {
	f := newFixture(t)
	api := &API{S: f.svc}

	rec := httptest.NewRecorder()
	req := reqWithID("req-def456")
	userErr := faults.Permanentf("", "节点 %q 不在册", "n1")

	api.writeErr(rec, req, http.StatusBadRequest, userErr)

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v", err)
	}
	if body["error"] != userErr.Error() {
		t.Errorf("error = %q，期望原样带回 %q", body["error"], userErr.Error())
	}
	if body["code"] != "invalid_argument" {
		t.Errorf(`code = %q，期望 "invalid_argument"`, body["code"])
	}
	if body["requestId"] != "req-def456" {
		t.Errorf("requestId = %q", body["requestId"])
	}
}

// TestStatusForIgnoresWording 验证：改变中文消息不改变 HTTP status。
// 用两组对照证明分类真的不再看文案：
//
//   - 一个带着旧子串表兜底关键词的**未打类型标记**的错误，此前会被误判成
//     400，现在必须落到 500（因为它本质上就是一个没人分类过的错误）；
//   - 一个打了 faults.Permanent 标记、但文案已经改得面目全非（甚至换成
//     英文）的错误，必须仍然是 400——分类看的是类型，不是那句话怎么说。
func TestStatusForIgnoresWording(t *testing.T) {
	// 旧的子串兜底列表：放置校验失败、不在册、已存在、必须、不合法、
	// 没有声明、会移除、没有匹配、不存在、有多个候选。
	oldKeywords := []string{
		"放置校验失败: foo", "节点 x 不在册", "组件 y 已存在",
		"这里必须给出理由", "参数不合法", "Pack 没有声明这个",
		"部署会移除 3 个实例", "没有匹配的实例", "站点 z 不存在",
		"依赖 dep 有多个候选",
	}
	for _, msg := range oldKeywords {
		t.Run("未打标记/"+msg, func(t *testing.T) {
			// 故意不经过 faults.Permanentf——模拟一个「文案恰好撞上了旧
			// 关键词，但从没被显式分类过」的错误，这正是旧机制会误判
			// 的那种输入。
			err := errors.New(msg)
			if got := statusFor(err); got != http.StatusInternalServerError {
				t.Errorf("statusFor(%q) = %d，期望 500（未打类型标记的错误不该因为文案巧合变成 400）",
					msg, got)
			}
		})
	}

	rewordings := []string{
		fmt.Sprintf("节点 %q 不在册", "n1"),
		fmt.Sprintf("node %q not registered", "n1"),
		"完全不相关的另一句话，跟旧关键词毫无关系",
	}
	for _, msg := range rewordings {
		t.Run("已打标记/"+msg, func(t *testing.T) {
			err := faults.Permanentf("", "%s", msg)
			if got := statusFor(err); got != http.StatusBadRequest {
				t.Errorf("statusFor(faults.Permanentf(%q)) = %d，期望 400"+
					"（打了类型标记的错误，状态不该随文案怎么改而变）", msg, got)
			}
		})
	}
}

// TestDriftPolicyTighteningRejectionStaysUserError 钉住一次真实撞到过的
// 回归：`internal/spec.CheckDriftOverride`（"覆盖只能放松不能收紧"）
// 不在 internal/mechd/internal/placement 里，第一轮转换扫描时漏掉了，
// 容器化验收套件的 TestDriftPolicyOverrideRejectsTightening 当场撞到——
// 响应从「说清楚是收紧被拒」变成了「内部错误，请把 requestId 提供给
// 管理员」。这条测试把它固定在单元测试层，不必每次都跑一遍容器才能
// 发现同类回归。
func TestDriftPolicyTighteningRejectionStaysUserError(t *testing.T) {
	err := spec.CheckDriftOverride(spec.DriftReconcile)
	if err == nil {
		t.Fatal("收紧应当被拒绝")
	}
	if got := statusFor(err); got != http.StatusBadRequest {
		t.Fatalf("statusFor(CheckDriftOverride(reconcile)) = %d，期望 400\n错误: %v", got, err)
	}
}

// TestListComponentsUnknownSiteReturns400 是一次端到端回归：证明
// resolve.go 里「站点不存在」这条错误真的显式打了类型标记，而不是
// 全套单元测试之外一条从没被走过的死代码。走真实的 HTTP 路由
// （withRequestID → guard → listComponents → resolveSite → statusFor），
// 不是直接调用某个内部函数。
func TestListComponentsUnknownSiteReturns400(t *testing.T) {
	f := newAuthFixture(t)

	req := httptest.NewRequest("GET", APIPrefix+"/components?site=no-such-site", nil)
	req.Header.Set("Authorization", "Bearer m7n_test-token")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（站点不存在是调用方的问题，不是服务端故障）\n响应: %s",
			rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v", err)
	}
	if !strings.Contains(body["error"], "does not exist") {
		t.Errorf("error = %q，期望说清楚是站点不存在", body["error"])
	}
}

// TestWriteErrRequestIDMatchesResponseHeader 用一次真实的 HTTP 往返
// （经过 withRequestID 中间件）确认响应头与响应体里的 requestId 是
// 同一个值——两处任一对不上，客户端凭 body 里的 requestId 去搜日志、
// 凭 header 去关联网关记录，会各自指向两个不同的请求。
func TestWriteErrRequestIDMatchesResponseHeader(t *testing.T) {
	f := newAuthFixture(t)

	// 用一个必然 400 的坏请求触发 writeErr——具体是哪个接口不重要，
	// 重要的是它真的走了 withRequestID → guard → writeErr 这条完整链路。
	req := httptest.NewRequest("POST", APIPrefix+"/auth/login",
		strings.NewReader(`not json`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)

	headerID := rec.Header().Get(RequestIDHeader)
	if headerID == "" {
		t.Fatal("响应头里没有 " + RequestIDHeader)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v\n%s", err, rec.Body.String())
	}
	if body["requestId"] != headerID {
		t.Errorf("响应体 requestId = %q，响应头 %s = %q，两者应当一致",
			body["requestId"], RequestIDHeader, headerID)
	}
}
