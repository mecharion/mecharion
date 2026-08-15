// Package pathutil 提供文件路径的边界检查。
//
// internal/ident 在入口拦住了运维手填的标识符，但有些文件名的构成部分
// 不来自运维（例如 Role 名来自 Pack 定义，只在 `mechpack lint` 时被校验，
// 而 lint 是可选步骤）。这里是最终落到磁盘路径之前的第二道防线：
// 不管名字来自哪里、经过了多少层拼接，结果必须仍然落在预期目录内。
package pathutil

import (
	"fmt"
	"path/filepath"
	"strings"
)

// EnsureWithin 校验 candidate 归一化后仍落在 root 之内（含 root 本身）。
func EnsureWithin(root, candidate string) error {
	rootAbs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return fmt.Errorf("pathutil: 归一化根目录 %q: %w", root, err)
	}
	candAbs, err := filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return fmt.Errorf("pathutil: 归一化路径 %q: %w", candidate, err)
	}
	if candAbs == rootAbs {
		return nil
	}
	if !strings.HasPrefix(candAbs, rootAbs+string(filepath.Separator)) {
		return fmt.Errorf("pathutil: %q 越出边界 %q", candidate, root)
	}
	return nil
}
