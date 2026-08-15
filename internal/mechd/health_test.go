package mechd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 本文件验证：/healthz 与 /readyz 答的是两个不同的问题——
// 进程活着 vs. 真能处理请求——此前只有一个端点、且无条件答 ok，
// 两者从未被分开过。

// TestHealthzAlwaysOK 钉住 liveness 的语义：不查任何依赖，能走到这行
// 代码本身就是答案。
func TestHealthzAlwaysOK(t *testing.T) {
	f := newFixture(t)
	api := &API{S: f.svc}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", APIPrefix+"/healthz", nil)
	api.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz 应当恒为 200，实际 %d", rec.Code)
	}
}

// TestReadyzOKWhenDatabaseIsUp 是 readiness 的正常路径。
func TestReadyzOKWhenDatabaseIsUp(t *testing.T) {
	f := newFixture(t)
	api := &API{S: f.svc}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", APIPrefix+"/readyz", nil)
	api.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("数据库正常时 /readyz 应当 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// TestReadyzFailsWhenDatabaseIsDown 是这条端点存在的**唯一理由**：
// 进程活着（/healthz 仍会答 200）,但数据库连不上时,/readyz 必须如实
// 报不行,而不是跟 /healthz 一样永远 200——否则这条端点只是
// /healthz 的重复,没有多提供任何信息。
func TestReadyzFailsWhenDatabaseIsDown(t *testing.T) {
	f := newFixture(t)
	api := &API{S: f.svc}

	// 直接关掉读连接池，模拟"进程还在但数据库不可用"——不调用
	// Store.Close()（那会把测试自己的清理逻辑也搅进去），只关
	// PingContext 真正会打到的那个 *sql.DB。
	if err := f.svc.Store.Reader().Close(); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", APIPrefix+"/readyz", nil)
	api.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("数据库关闭时 /readyz 应当 503，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v", err)
	}
	if body["status"] != "not ready" {
		t.Errorf(`status 应为 "not ready"，实际 %q`, body["status"])
	}
}

// TestHealthzAndReadyzNeedNoAuth 钉住两者都是未认证入口——监控探针
// 不该被要求带 token，那会让"探针本身要不要缓存 token"变成一个
// 没有必要存在的问题。
func TestHealthzAndReadyzNeedNoAuth(t *testing.T) {
	f := newFixture(t)
	api := &API{S: f.svc, Auth: NewTokenAuth(TokenPrefix + "real-token")}

	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", APIPrefix+path, nil) // 不带 Authorization
		api.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s 不该要求认证，实际返回 401", path)
		}
	}
}
