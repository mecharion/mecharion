package ctlcmd

import (
	"strings"
	"testing"
)

// TestPathSegmentSpecialCharactersRoundTripThroughRealHTTP 是
// 端到端判据：一个带特殊字符的组件名,经过真实的 net/http 客户端
// 请求、真实的 mechd ServeMux 路由（`{name}` 通配段）、真实的服务端
// 处理,最终必须原样传到组件查询逻辑——而不是被 '/' 误判成多一段
// 路径导致 404、被 '?'/'&' 误判成查询串导致名字被截断。
//
// 用「组件不存在」这条错误路径来验证：既然组件确实不存在,服务端
// 一定会报错；错误文案里带的名字必须与命令行敲的**逐字节相同**,
// 这就证明了 seg() 转义 → HTTP 传输 → ServeMux 解出 PathValue 这条
// 链路没有丢字符、没有被提前截断在错误的路径段上。
func TestPathSegmentSpecialCharactersRoundTripThroughRealHTTP(t *testing.T) {
	w := newWired(t, "n1")

	cases := []string{
		"a/b",   // 未转义时会被当成两个路径段，路由到别的地方
		"a b",   // 空格
		"a?b",   // 未转义时会被当成查询串开始，名字被截断成 "a"
		"a&b=c", // 查询串分隔符混进名字
		"中文名字",  // 非 ASCII
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := w.run("status", name)
			if err == nil {
				t.Fatalf("组件 %q 本不存在，status 应当报错", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("错误信息应原样带上请求的组件名 %q，实际: %v", name, err)
			}
		})
	}
}
