package ctlcmd

import (
	"net/url"
	"testing"
)

// TestSegEscapesPathSpecialCharacters 钉住 seg() 对会打乱路径分段的
// 字符做了转义：一个带 '/' 的标识符转义前
// 会被当成两个路径段，转义后必须原样能解出来。
func TestSegEscapesPathSpecialCharacters(t *testing.T) {
	cases := []string{
		"a/b",    // 会被误判成两个路径段
		"a b",    // 空格
		"a?b=c",  // 会被误判成查询串开始
		"a#frag", // 会被误判成 fragment
		"a&b",    // 查询串分隔符,出现在 path 里本身不特殊,但一并覆盖
		"中文名字",   // 非 ASCII
		"a%2Fb",  // 已经带了一次转义的输入,不能被二次转义搞坏语义
		"",
	}
	for _, name := range cases {
		esc := seg(name)
		back, err := url.PathUnescape(esc)
		if err != nil {
			t.Errorf("seg(%q) = %q，不是合法的转义: %v", name, esc, err)
			continue
		}
		if back != name {
			t.Errorf("seg(%q) 转义后解不回原值，实际解出 %q（转义结果 %q）", name, back, esc)
		}
		// 转义结果不应包含裸的路径分隔符——否则拼进 path 后还是会被
		// 误判成多个路径段,转义等于没做。
		for _, r := range esc {
			if r == '/' {
				t.Errorf("seg(%q) = %q 里仍然带着裸的 '/'", name, esc)
			}
		}
	}
}

// TestQueryEncodesSpecialCharactersAndOmitsEmpty 钉住 query() 的两条性质：
// 值里的特殊字符被正确编码，空值不出现在结果里。
func TestQueryEncodesSpecialCharactersAndOmitsEmpty(t *testing.T) {
	q := query(map[string]string{"role": "a&b=c", "site": "", "node": "n1"})
	v, err := url.ParseQuery(q[1:]) // 去掉前导 '?'
	if err != nil {
		t.Fatalf("query() 产出的不是合法查询串 %q: %v", q, err)
	}
	if got := v.Get("role"); got != "a&b=c" {
		t.Errorf("role 解出 %q，期望 %q", got, "a&b=c")
	}
	if v.Has("site") {
		t.Errorf("空值不该出现在查询串里，实际 %q", q)
	}
	if got := v.Get("node"); got != "n1" {
		t.Errorf("node 解出 %q，期望 n1", got)
	}

	if got := query(map[string]string{"a": ""}); got != "" {
		t.Errorf("全部值为空时应返回空串，实际 %q", got)
	}
	if got := query(nil); got != "" {
		t.Errorf("nil 应返回空串，实际 %q", got)
	}
}

// TestAppendQueryMergesWithoutClobberingExisting 钉住 appendQuery 与
// 调用方自己拼好的查询串合并，而不是假设 path 一定不带 '?'。
func TestAppendQueryMergesWithoutClobberingExisting(t *testing.T) {
	got := appendQuery("/api/v1/orphans?node=n1", url.Values{"site": {"prod"}})
	v, err := url.ParseQuery(got[len("/api/v1/orphans")+1:])
	if err != nil {
		t.Fatalf("appendQuery 产出的不是合法路径+查询串 %q: %v", got, err)
	}
	if v.Get("node") != "n1" {
		t.Errorf("合并后原有的 node 参数丢了，实际 %q", got)
	}
	if v.Get("site") != "prod" {
		t.Errorf("新加的 site 参数没生效，实际 %q", got)
	}

	// 没有 extra 时原样返回，不画蛇添足加个空 '?'
	if got := appendQuery("/api/v1/nodes", nil); got != "/api/v1/nodes" {
		t.Errorf("extra 为空时不该改动 path，实际 %q", got)
	}

	// 值本身带特殊字符也要能正确合并、正确编码
	got = appendQuery("/api/v1/x", url.Values{"site": {"a&b"}})
	v, err = url.ParseQuery(got[len("/api/v1/x")+1:])
	if err != nil {
		t.Fatalf("特殊字符值合并后不是合法查询串 %q: %v", got, err)
	}
	if v.Get("site") != "a&b" {
		t.Errorf("特殊字符值没有正确编码，实际 %q", got)
	}
}
