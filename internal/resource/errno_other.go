//go:build !unix && !windows

package resource

import "syscall"

// js/wasm、plan9 等既非 Unix 也非 Windows 的平台。它们不是 mechlet 的
// 目标平台，这里只保证能编译。
var errNotEmpty error = syscall.ENOTEMPTY
