// Package webui 把构建好的 Web UI 打进 mechd。
//
// 产物**不进版本库**（[ADR-0036]）：`dist/` 由 `go generate` 现场构建，
// `.gitignore` 里只留一个 `.gitkeep`。因此一次干净的 clone 里这个目录是空的，
// 而 `go build ./...` **仍然必须编译得过**——没有 Node 的人也要能构建 mechd，
// 只是构建出来的那个没有界面。
//
// [ADR-0036]: ../../docs/adr/0036-webui-vue-and-generated-dist.md
package webui

import (
	"embed"
	"io/fs"
)

// **`all:` 前缀不能省。**
//
// 没有它，embed 会忽略点号开头的文件，于是只含 `.gitkeep` 的空目录会被判成
// 「no matching files」——**一次干净 clone 直接编译失败**，而那个报错完全
// 看不出与前端有关。加上 `all:` 之后空目录也收得下。
//
//go:embed all:dist
var embedded embed.FS

// assets 返回 dist 子树。
func assets() (fs.FS, error) { return fs.Sub(embedded, "dist") }

// Built 报告 UI 是否真的构建过。
//
// 判据是 `index.html` 在不在，而不是「目录里有没有东西」——只有 `.gitkeep`
// 的目录同样非空，但那不是一个能打开的界面。
func Built() bool {
	sub, err := assets()
	if err != nil {
		return false
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return false
	}
	return true
}
