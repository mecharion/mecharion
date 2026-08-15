package mechd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件验证：管理 UI 与 API 此前只设置内容/缓存头，没有任何
// anti-framing、nosniff、Referrer-Policy——管理界面可以被第三方页面
// iframe 嵌入做点击劫持。中间件对**全部**响应生效，不分 API 路径还是
// Web UI 静态资源，因为两者共用同一个最外层 handler（withSecurityHeaders
// 包在 mux 外面，见 httpapi.go）。

// TestSecurityHeadersOnAPIResponse 钉住 API 路径（用 /auth/state，
// 无需认证，最容易稳定触发）的响应带齐基础安全头。
func TestSecurityHeadersOnAPIResponse(t *testing.T) {
	f := newAuthFixture(t)
	req := httptest.NewRequest("GET", APIPrefix+"/auth/state", nil)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)

	assertSecurityHeaders(t, rec.Header())
}

// TestSecurityHeadersOnWebUIResponse 钉住 Web UI 路径（根路径，
// 不在 guard 之下）同样带齐——这条路径最容易在只测 API 时被漏掉，
// 而 iframe 点击劫持打的正是页面，不是 JSON 接口。
func TestSecurityHeadersOnWebUIResponse(t *testing.T) {
	f := newAuthFixture(t)
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)

	assertSecurityHeaders(t, rec.Header())
}

func assertSecurityHeaders(t *testing.T, h http.Header) {
	t.Helper()
	if h.Get("X-Frame-Options") != "DENY" {
		t.Errorf("X-Frame-Options = %q，期望 DENY", h.Get("X-Frame-Options"))
	}
	if h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q，期望 nosniff", h.Get("X-Content-Type-Options"))
	}
	if h.Get("Referrer-Policy") == "" {
		t.Error("缺少 Referrer-Policy")
	}
	csp := h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP 应当含 frame-ancestors 'none'，实际: %q", csp)
	}
}

// TestHSTSOnlyWhenEnabled 钉住 HSTS 只在 API.EnableHSTS 为 true
// （对应 HTTPS 模式）时才发出——HTTP（--insecure-http）模式下发这个头
// 没有意义，浏览器规范本就无视明文连接上收到的它。
func TestHSTSOnlyWhenEnabled(t *testing.T) {
	f := newAuthFixture(t)
	req := httptest.NewRequest("GET", APIPrefix+"/auth/state", nil)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if hsts := rec.Header().Get("Strict-Transport-Security"); hsts != "" {
		t.Errorf("EnableHSTS 未设置时不该发 HSTS，实际: %q", hsts)
	}

	f.api.EnableHSTS = true
	req2 := httptest.NewRequest("GET", APIPrefix+"/auth/state", nil)
	rec2 := httptest.NewRecorder()
	f.h.ServeHTTP(rec2, req2)
	hsts := rec2.Header().Get("Strict-Transport-Security")
	if !strings.Contains(hsts, "max-age=") || !strings.Contains(hsts, "includeSubDomains") {
		t.Errorf("EnableHSTS 打开时应当发出完整的 HSTS，实际: %q", hsts)
	}
}
