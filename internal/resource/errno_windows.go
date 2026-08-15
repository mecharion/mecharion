//go:build windows

package resource

import "syscall"

// Windows 上 os.Remove 删非空目录返回的是 ERROR_DIR_NOT_EMPTY(145)，
// 而不是 syscall.ENOTEMPTY —— 后者在 Windows 上只是个占位常量，
// 永远不会被真正返回。只影响开发机上的单元测试。
var errNotEmpty error = syscall.ERROR_DIR_NOT_EMPTY
