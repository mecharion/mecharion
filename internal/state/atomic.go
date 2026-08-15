package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// writeJSON 原子写入一个 JSON 文件。
//
// 「写临时文件 + rename」利用 POSIX rename 的原子性：任何时刻崩溃都不会
// 留下半个文件——要么是旧内容，要么是新内容。这是 mechlet 不用数据库
// 却仍能保证状态一致的基础（ADR-0013）。
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 %s: %w", filepath.Base(path), err)
	}
	b = append(b, '\n')
	return writeFileAtomic(path, b, 0o600)
}

// readJSON 读取 JSON 文件。文件不存在时返回 (false, nil)。
func readJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("读取 %s: %w", path, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("解析 %s: %w", path, err)
	}
	return true, nil
}

// writeFileAtomic 把内容原子地写到 path。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("创建临时文件: %w", err)
	}
	tmpName := tmp.Name()
	// 失败路径上清理临时文件；成功时 rename 已让它消失，Remove 返回的
	// NotExist 无害。
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时文件: %w", err)
	}
	// 先 Sync 再 rename：崩溃时 rename 已生效但内容还在页缓存里，
	// 会留下一个空文件——这正是原子写要避免的。
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("刷盘: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("替换 %s: %w", path, err)
	}
	return syncDir(dir)
}
