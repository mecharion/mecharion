//go:build unix

package resource

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// modeSupported 表示本平台实现 Unix 权限位。
const modeSupported = true

// fileOwner 读出文件的 uid/gid。
func fileOwner(fi fs.FileInfo) (uid, gid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

// chown 设置属主。uid/gid 为 -1 表示不改。
//
// 用 Lchown 而非 Chown：对软链要改的是链接自身的属主，跟随链接会
// 意外修改目标文件——那可能是软链之外的、不归本组件管的文件。
func chown(path string, uid, gid int) error {
	return os.Lchown(path, uid, gid)
}

// isNotDirErr 报告错误是否为 ENOTDIR。
func isNotDirErr(err error) bool { return errors.Is(err, syscall.ENOTDIR) }

// isNotEmptyErr 报告错误是否为「目录非空」。
func isNotEmptyErr(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}
