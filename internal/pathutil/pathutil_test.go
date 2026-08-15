package pathutil

import (
	"path/filepath"
	"testing"
)

func TestEnsureWithin(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	cases := []struct {
		name      string
		candidate string
		ok        bool
	}{
		{"root自身", root, true},
		{"直接子项", filepath.Join(root, "pg-main__primary.json"), true},
		{"嵌套子目录", filepath.Join(root, "a", "b", "c.json"), true},
		{"父目录遍历逃逸", filepath.Join(root, "..", "etc", "passwd"), false},
		{"父目录遍历落回内部", filepath.Join(root, "a", "..", "b.json"), true},
		{"绝对路径逃逸", filepath.Join(filepath.Dir(root), "other", "x.json"), false},
		{"同前缀但非子目录", root + "-evil", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := EnsureWithin(root, tc.candidate)
			if tc.ok && err != nil {
				t.Fatalf("EnsureWithin(%q) = %v, 期望通过", tc.candidate, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("EnsureWithin(%q) = nil, 期望被拒绝", tc.candidate)
			}
		})
	}
}
