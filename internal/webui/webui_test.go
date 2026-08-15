package webui

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// 本文件钉住 **M8 第 1 步**：UI 的装载方式。
//
// 这一步没有界面可验，要验的是它**怎么装进 mechd**：
//
//	没有 Node 的机器构建得过 → 但那个二进制要说清自己没带界面
//	有 Node 构建过之后        → 静态资源以 gzip 直出
//
// 第一条尤其要紧：`//go:embed all:dist` 里的 `all:` 少一个字，
// 一次干净 clone 就会编译失败，而报错完全看不出与前端有关。

// TestEmptyDistStillCompiles 是这一步最重要的一条。
//
// 它其实是**编译期**的断言——这个包能被编译进测试二进制，本身就说明
// `//go:embed all:dist` 在只有 `.gitkeep` 的目录上也成立。运行到这里就是过了。
//
// 少了 `all:` 前缀的话，embed 会忽略点号开头的文件，于是空目录被判成
// 「no matching files」，整个包编译不过——那时这条测试连跑都跑不起来。
func TestEmptyDistStillCompiles(t *testing.T) {
	if _, err := assets(); err != nil {
		t.Fatalf("dist 子树取不出来: %v", err)
	}
}

// TestNotBuiltSaysSoInsteadOf404 钉住「没构建」时要**吵**。
//
// 404 看起来像路由坏了或者版本不对，而真实原因是这台机器上的 mechd 是在
// 没有 Node 的环境里构建的——那是一句话能说清、也一句话能解决的事。
func TestNotBuiltSaysSoInsteadOf404(t *testing.T) {
	rec := httptest.NewRecorder()
	notBuilt(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("状态码应当是 200（mechd 本身是好的，只是没带界面），实际 %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"尚未构建", "make webui", "不受影响"} {
		if !strings.Contains(body, want) {
			t.Errorf("说明页里缺 %q:\n%s", want, body)
		}
	}
}

// newTestHandler 造一个带假产物的 handler。
func newTestHandler(files map[string]string) *handler {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return &handler{fsys: fsys}
}

func gz(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestServesPrecompressed 钉住预压缩的资源直接送出，不在运行期重压。
func TestServesPrecompressed(t *testing.T) {
	h := newTestHandler(map[string]string{
		"assets/app.js.gz": gz(t, "console.log(1)"),
	})

	req := httptest.NewRequest("GET", "/assets/app.js", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("应当以 gzip 直出，实际 Content-Encoding=%q", got)
	}
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("按编码变化的响应要带 Vary，实际 %q", got)
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("送出的不是合法 gzip: %v", err)
	}
	body, _ := io.ReadAll(zr)
	if string(body) != "console.log(1)" {
		t.Errorf("解出来的内容不对: %q", body)
	}
}

// TestDecompressesForClientsWithoutGzip 钉住不接受 gzip 的客户端也拿得到东西。
//
// 现实中几乎不会走到这条路，但少了它，一个 `Accept-Encoding: identity` 的
// 客户端会拿到一坨二进制——而那种故障排查起来极其费劲。
func TestDecompressesForClientsWithoutGzip(t *testing.T) {
	h := newTestHandler(map[string]string{
		"assets/app.js.gz": gz(t, "console.log(1)"),
	})

	req := httptest.NewRequest("GET", "/assets/app.js", nil)
	req.Header.Set("Accept-Encoding", "identity")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("不接受 gzip 时不该带 Content-Encoding，实际 %q", got)
	}
	if got := rec.Body.String(); got != "console.log(1)" {
		t.Errorf("应当就地解压，实际 %q", got)
	}
}

// TestSPAFallback 钉住前端路由回退，**但只对看起来像页面的请求**。
//
// 一个找不到的 .js 回退成 HTML 的话，浏览器只会报一句莫名其妙的语法错误，
// 而真实原因是资源 404——那是最难查的一类现场。
func TestSPAFallback(t *testing.T) {
	h := newTestHandler(map[string]string{
		"index.html": "<!doctype html>ok",
	})

	t.Run("前端路由回退到 index.html", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/components/web", nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
			t.Errorf("应当回退到 index.html，实际 %d %q", rec.Code, rec.Body.String())
		}
	})

	t.Run("找不到的资源老实 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/assets/missing.js", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("带扩展名的资源找不到就该 404，实际 %d", rec.Code)
		}
	})
}

// TestCacheHeaders 钉住 index.html 不被缓存住。
//
// 带指纹的资源可以长缓存，而 index.html 是入口：缓存住之后，
// 用户升级完 mechd 打开的还是旧界面，而且刷新也不管用。
func TestCacheHeaders(t *testing.T) {
	h := newTestHandler(map[string]string{
		"index.html":            "<!doctype html>",
		"assets/app-abc123.css": "body{}",
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("index.html 不能被缓存住，实际 %q", got)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/assets/app-abc123.css", nil))
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("带指纹的资源应当长缓存，实际 %q", got)
	}
}

// TestNoPathEscape 钉住路径穿越进不来。
func TestNoPathEscape(t *testing.T) {
	h := newTestHandler(map[string]string{"index.html": "ok"})
	for _, p := range []string{"/../go.mod", "/assets/../../go.mod"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if strings.Contains(rec.Body.String(), "module ") {
			t.Errorf("%s 读到了仓库里的文件", p)
		}
	}
}
