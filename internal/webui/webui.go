package webui

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// Handler 返回 Web UI 的静态资源处理器。
//
// 三件事：
//
//	① 没构建过时给一页**说明**，不是 404
//	② 预压缩过的资源直接以 Content-Encoding: gzip 送出
//	③ 前端路由（SPA）回退到 index.html
func Handler() http.Handler {
	sub, err := assets()
	if err != nil || !Built() {
		return http.HandlerFunc(notBuilt)
	}
	return &handler{fsys: sub}
}

// notBuilt 是「UI 没构建」时的说明页。
//
// **不能返回 404。** 404 看起来像路由坏了或者版本不对，而真实原因是这台机器
// 上的 mechd 是在没有 Node 的环境里构建的——那是一句话就能说清、也一句话就能
// 解决的事，没必要让人去翻源码。
func notBuilt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 200 而不是 5xx：mechd 本身是好的，API 全部可用，只是没带界面
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, notBuiltPage)
}

const notBuiltPage = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<title>Mecharion — Web UI 尚未构建</title>
<style>
 body{font:16px/1.7 system-ui,sans-serif;max-width:44rem;margin:4rem auto;padding:0 1.5rem;color:#1f2328}
 code{background:#f0f2f4;padding:.15em .4em;border-radius:4px}
 pre{background:#f0f2f4;padding:1rem;border-radius:6px;overflow-x:auto}
 .hint{color:#59636e}
</style></head><body>
<h1>Web UI 尚未构建</h1>
<p>这个 mechd 是在<strong>没有 Node 的环境</strong>里构建的，因此没有带上界面。
   <strong>API 与 CLI 的全部功能不受影响。</strong></p>
<p>在有 Node 的机器上执行：</p>
<pre>make webui   # 构建前端并拷进 internal/webui/dist
make build   # 重新构建 mechd</pre>
<p class="hint">UI 产物不进版本库（ADR-0036），因此一次干净的 clone 里它本来就是空的。</p>
</body></html>
`

type handler struct{ fsys fs.FS }

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}

	if h.serve(w, r, name) {
		return
	}

	// **前端路由回退到 index.html**：`/components/web` 这种地址是 Vue Router
	// 的路径，磁盘上没有对应文件。但只对「看起来像页面」的请求回退——
	// 一个找不到的 .js 回退成 HTML，浏览器只会报一句莫名其妙的语法错误。
	if path.Ext(name) == "" {
		if h.serve(w, r, "index.html") {
			return
		}
	}
	http.NotFound(w, r)
}

// serve 送出一个资源；找不到时返回 false。
//
// **优先送预压缩的那份**：构建时已经压好 `.gz`，运行期不必每次重压。
// 客户端不接受 gzip 时就地解压——现实中几乎不会走到，但少了它，
// 一个 `Accept-Encoding: identity` 的客户端会拿到一坨二进制。
func (h *handler) serve(w http.ResponseWriter, r *http.Request, name string) bool {
	if raw, err := fs.ReadFile(h.fsys, name+".gz"); err == nil {
		setType(w, name)
		if acceptsGzip(r) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Vary", "Accept-Encoding")
			_, _ = w.Write(raw)
			return true
		}
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return false
		}
		defer zr.Close()
		_, _ = io.Copy(w, zr)
		return true
	}

	raw, err := fs.ReadFile(h.fsys, name)
	if err != nil {
		return false
	}
	setType(w, name)
	_, _ = w.Write(raw)
	return true
}

func setType(w http.ResponseWriter, name string) {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// 带指纹的资源可以长缓存；index.html 不行——它是入口，缓存住就更新不了
	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
}

func acceptsGzip(r *http.Request) bool {
	for _, v := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if strings.EqualFold(strings.TrimSpace(strings.SplitN(v, ";", 2)[0]), "gzip") {
			return true
		}
	}
	return false
}
