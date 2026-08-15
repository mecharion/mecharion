package pki

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mecharion/mecharion/internal/ident"
)

// TestIssueNodeRejectsInvalidName 验证：IssueNode 把 node 直接拼进
// `nodes/<node>.crt` / `.key` 文件名，而它有三个使用者（mechd 启动、
// `mechd ca` 手工签发、Join RPC）——与其要求三处各自记得先校验，这个
// 共享原语必须自己拒绝非法名字，且在**写出任何文件之前**拒绝。
func TestIssueNodeRejectsInvalidName(t *testing.T) {
	dir := t.TempDir()

	_, err := IssueNode(dir, "../evil", 0)
	if err == nil {
		t.Fatal("非法节点名应当被拒绝，实际签发成功")
	}
	if !errors.Is(err, ident.ErrInvalid) {
		t.Errorf("错误 %v 未包裹 ident.ErrInvalid", err)
	}

	// 更要紧的断言：CA 之外**没有任何**节点证书文件被写出来。
	nodesDir := filepath.Join(dir, "nodes")
	if entries, statErr := os.ReadDir(nodesDir); statErr == nil && len(entries) != 0 {
		t.Fatalf("nodes 目录本应不存在或为空，实际: %v", entries)
	}
}
