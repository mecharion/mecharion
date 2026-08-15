//go:build !unix

package state

// syncDir 在非 Unix 平台上是空操作。
//
// Windows 不支持对目录句柄做 fsync，且 rename 的语义也不同。mechlet 只在
// Linux 上运行（见 docs/design/25-roadmap.md 的平台矩阵），此处存在仅为
// 让 `go build ./...` 与单元测试在开发机上照常通过。
func syncDir(string) error { return nil }
