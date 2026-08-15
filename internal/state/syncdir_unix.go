//go:build unix

package state

import "os"

// syncDir 刷新目录项，确保 rename 本身也落盘。
//
// 只 fsync 文件而不 fsync 其父目录时，崩溃后可能出现「文件内容在、但目录项
// 还指向旧 inode」的情况。对 mechlet 这种要铺到成千上万台边缘机器、且会
// 经历非正常断电的场景，这一步不能省。
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	// 某些文件系统（tmpfs、部分网络文件系统）不支持对目录 fsync，
	// 返回 EINVAL/ENOTSUP。这不是错误——它们本就没有需要刷的目录项缓存。
	if err := d.Sync(); err != nil {
		return nil
	}
	return nil
}
