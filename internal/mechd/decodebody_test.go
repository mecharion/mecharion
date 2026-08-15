package mechd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDecodeBody 系列验证：未认证入口此前对请求体没有任何大小
// 或读取时间上的边界，只有 ReadHeaderTimeout 护住了请求头。用 /auth/login
// 当靶子——它本来就不需要凭据，最贴近「未认证调用方能打多远」这个问题。

func rawLogin(t *testing.T, f *authFixture, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", APIPrefix+"/auth/login", bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// TestDecodeBodyRejectsOversizedBody 钉住体积上限：超过 maxJSONBody 时
// 返回 413，而不是把整个体读进内存再报错。
func TestDecodeBodyRejectsOversizedBody(t *testing.T) {
	f := newAuthFixture(t)

	huge := LoginBody{User: AdminUser, Password: strings.Repeat("a", maxJSONBody+1)}
	body, err := json.Marshal(huge)
	if err != nil {
		t.Fatal(err)
	}

	rec := rawLogin(t, f, body, "application/json")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d，期望 %d\n响应: %s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}
}

// TestDecodeBodyRejectsWrongContentType 钉住 Content-Type 校验：
// 把任意内容当 JSON 解是在帮攻击者省一次试探。
func TestDecodeBodyRejectsWrongContentType(t *testing.T) {
	f := newAuthFixture(t)
	body, _ := json.Marshal(LoginBody{User: AdminUser, Password: goodPW, Challenge: f.stub.answer})

	rec := rawLogin(t, f, body, "text/plain")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400\n响应: %s", rec.Code, rec.Body.String())
	}
}

// TestDecodeBodyAllowsMissingContentType 确认没带 Content-Type 头（不是
// 带错）时不受影响——很多现有测试与手写脚本本来就不设它。
func TestDecodeBodyAllowsMissingContentType(t *testing.T) {
	f := newAuthFixture(t)
	body, _ := json.Marshal(LoginBody{User: AdminUser, Password: goodPW, Challenge: f.stub.answer})

	rec := rawLogin(t, f, body, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200\n响应: %s", rec.Code, rec.Body.String())
	}
}

// TestDecodeBodyRejectsTrailingJSONValue 钉住「请求体只能有一个 JSON
// 值」：`{...}{...}` 这种拼接体，标准库的 Decode 只解析第一个、静默丢弃
// 第二个——调用方以为只发了一份，服务端却不保证真的只处理了这一份。
func TestDecodeBodyRejectsTrailingJSONValue(t *testing.T) {
	f := newAuthFixture(t)
	first, _ := json.Marshal(LoginBody{User: AdminUser, Password: goodPW, Challenge: f.stub.answer})
	second := []byte(`{"user":"someone-else"}`)
	body := append(append([]byte{}, first...), second...)

	rec := rawLogin(t, f, body, "application/json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（多余的第二个 JSON 值应当被拒）\n响应: %s",
			rec.Code, rec.Body.String())
	}
}

// TestDecodeBodyAcceptsWellFormedRequest 是对照组：确认前面几条校验
// 没有误伤正常请求。
func TestDecodeBodyAcceptsWellFormedRequest(t *testing.T) {
	f := newAuthFixture(t)
	body, _ := json.Marshal(LoginBody{User: AdminUser, Password: goodPW, Challenge: f.stub.answer})

	rec := rawLogin(t, f, body, "application/json; charset=utf-8")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200\n响应: %s", rec.Code, rec.Body.String())
	}
}
