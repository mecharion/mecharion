package ident

import (
	"errors"
	"strings"
	"testing"
)

// TestValidate_Label 覆盖 Component/Site/ConfigGroup 共用的单段 label 规则。
func TestValidate_Label(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"合法-简单", "postgresql", true},
		{"合法-带连字符与数字", "pg-main-01", true},
		{"合法-单字符", "a", true},
		{"合法-63字符上限", strings.Repeat("a", 63), true},
		{"空白", "", false},
		{"空格", "pg main", false},
		{"父目录遍历", "..", false},
		{"父目录遍历-嵌入", "pg-..-main", false},
		{"正斜杠", "pg/main", false},
		{"反斜杠", "pg\\main", false},
		{"绝对路径-unix", "/etc/passwd", false},
		{"绝对路径-windows", `C:\Windows`, false},
		{"URL保留字符-问号", "pg?main", false},
		{"URL保留字符-井号", "pg#main", false},
		{"URL保留字符-at", "pg@main", false},
		{"URL保留字符-冒号", "pg:main", false},
		{"URL保留字符-百分号", "pg%2e%2e", false},
		{"Unicode混淆-西里尔a", "pg\u0430main", false}, // U+0430 CYRILLIC SMALL LETTER A
		{"Unicode混淆-全角字符", "ｐｇ", false},
		{"超长-64字符", strings.Repeat("a", 64), false},
		{"超长-远超上限", strings.Repeat("a", 10000), false},
		{"大写字母", "PgMain", false},
		{"下划线", "pg_main", false},
		{"点号", "pg.main", false},
		{"首字符连字符", "-pgmain", false},
		{"尾字符连字符", "pgmain-", false},
		{"仅连字符", "-", false},
	}
	for _, kind := range []Kind{Component, Site, ConfigGroup} {
		for _, tc := range cases {
			t.Run(kind.label()+"/"+tc.name, func(t *testing.T) {
				err := Validate(kind, tc.in)
				if tc.ok && err != nil {
					t.Fatalf("Validate(%q) = %v, 期望通过", tc.in, err)
				}
				if !tc.ok && err == nil {
					t.Fatalf("Validate(%q) = nil, 期望被拒绝", tc.in)
				}
				if !tc.ok {
					if !errors.Is(err, ErrInvalid) {
						t.Fatalf("错误 %v 未包裹 ErrInvalid", err)
					}
					if !strings.HasPrefix(err.Error(), "invalid_identifier") {
						t.Fatalf("错误信息 %q 未以稳定前缀 invalid_identifier 开头", err.Error())
					}
				}
			})
		}
	}
}

// TestValidate_Node 覆盖 Node 专用的 subdomain 规则（允许点号分隔的多段）。
func TestValidate_Node(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"合法-简单主机名", "node-1", true},
		{"合法-FQDN", "node-1.example.com", true},
		{"合法-单标签253字符内", strings.Repeat("a", 63) + "." + strings.Repeat("b", 63), true},
		{"空白", "", false},
		{"父目录遍历", "..", false},
		{"父目录遍历-段间", "node1/../node2", false},
		{"父目录遍历-伪装成两段", "node1..node2", false}, // 中间段为空
		{"正斜杠", "node/1", false},
		{"反斜杠", `node\1`, false},
		{"绝对路径-unix", "/etc/passwd", false},
		{"绝对路径-windows", `C:\Windows`, false},
		{"URL保留字符", "node#1", false},
		{"Unicode混淆", "\u0430node", false},
		{"超长-254字符", strings.Repeat("a", 254), false},
		{"空段-前导点", ".node", false},
		{"空段-尾随点", "node.", false},
		{"空段-连续点", "node..sub", false},
		{"大写字母", "Node-1", false},
		{"下划线", "node_1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(Node, tc.in)
			if tc.ok && err != nil {
				t.Fatalf("Validate(%q) = %v, 期望通过", tc.in, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("Validate(%q) = nil, 期望被拒绝", tc.in)
			}
			if !tc.ok && !errors.Is(err, ErrInvalid) {
				t.Fatalf("错误 %v 未包裹 ErrInvalid", err)
			}
		})
	}
}

// TestFail_TruncatesOverlongEcho 确认超长输入不会被整段塞进错误信息。
func TestFail_TruncatesOverlongEcho(t *testing.T) {
	huge := strings.Repeat("a", 10000)
	err := Validate(Component, huge)
	if err == nil {
		t.Fatal("期望被拒绝")
	}
	if len(err.Error()) > 300 {
		t.Fatalf("错误信息长度 %d，超长输入未被截断", len(err.Error()))
	}
}
