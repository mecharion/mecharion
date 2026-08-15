//go:build !unix

package resource

import (
	"errors"
	"io/fs"
	"syscall"
)

// 本文件让 `go build ./...` 与单元测试在 Windows 开发机上照常通过。
// mechlet 只在 Linux 上运行（docs/design/25-roadmap.md 的平台矩阵）。

// modeSupported 为 false：Windows 的 ACL 模型与 Unix 权限位不对应，
// chmod 只能改动只读属性，读回来的永远是 0666 / 0777。
//
// 因此这些平台上**不比对也不强制 mode**——否则每个资源都会在每轮
// 调和里报一次永远收敛不了的 mode 漂移。
const modeSupported = false

// fileOwner 在非 Unix 平台上读不到 uid/gid。
func fileOwner(fs.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }

// chown 在非 Unix 平台上是空操作——强行把 uid/gid 映射到 ACL
// 只会得到一个既不正确也不可预期的结果。
func chown(string, int, int) error { return nil }

func isNotDirErr(err error) bool { return errors.Is(err, syscall.ENOTDIR) }

func isNotEmptyErr(err error) bool { return errors.Is(err, errNotEmpty) }
