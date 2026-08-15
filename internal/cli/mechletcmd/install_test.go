package mechletcmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/ident"
)

func TestResolveNodeName(t *testing.T) {
	okHostname := func(h string) func() (string, error) {
		return func() (string, error) { return h, nil }
	}
	failHostname := func() (string, error) {
		return "", fmt.Errorf("boom")
	}

	cases := []struct {
		name     string
		explicit string
		hostname func() (string, error)
		want     string
		wantErr  bool
	}{
		{"显式名字原样使用", "my-node-01", okHostname("SHOULD-NOT-BE-USED"), "my-node-01", false},
		{"显式名字不做归一化-哪怕自己不合法", "MyNode", okHostname("x"), "", true},
		{"主机名转小写", "", okHostname("DESKTOP-ABC123"), "desktop-abc123", false},
		{"FQDN主机名保留点号", "", okHostname("Node1.Example.COM"), "node1.example.com", false},
		{"主机名带下划线-转小写后仍不合法则报错", "", okHostname("my_host"), "", true},
		{"os.Hostname失败即返回错误", "", failHostname, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveNodeName(tc.explicit, tc.hostname)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveNodeName(%q) = %q, 期望报错", tc.explicit, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveNodeName(%q) 报错: %v", tc.explicit, err)
			}
			if got != tc.want {
				t.Errorf("resolveNodeName(%q) = %q, 期望 %q", tc.explicit, got, tc.want)
			}
		})
	}
}

// TestResolveNodeName_InvalidErrorIsIdentifiable 确认失败原因可判定为
// 标识符非法（而不是只有一句人读的中文），下游（如 CLI 的退出码分类）
// 才能据此区分「输入不合法」与其它种类的失败。
func TestResolveNodeName_InvalidErrorIsIdentifiable(t *testing.T) {
	_, err := resolveNodeName("My_Bad_Name", func() (string, error) { return "", nil })
	if !errors.Is(err, ident.ErrInvalid) {
		t.Fatalf("错误 %v 未包裹 ident.ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "--node") {
		t.Errorf("错误信息应当提示如何补救（--node），实际: %v", err)
	}
}
