//go:build generate

// 这个文件只在 `go generate` 时被看到（build tag `generate`），
// 因此它不参与普通构建，也不会把 gzip 之类的依赖带进 mechd。
package webui

// 构建前端并把产物拷进 dist/。
//
// 分两步而不是让 Vite 直接输出到 dist/：**压缩这一步用 Go 做**，
// 不往前端依赖树里再塞一个插件。前端的依赖越少，五年后重写它时越轻。
//
//go:generate go run github.com/mecharion/mecharion/internal/tools/webuibuild
