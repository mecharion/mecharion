package ctlcmd

import (
	"net/url"
	"strings"
)

// seg 转义一个会被拼进 URL path 的用户可控标识符（组件名/节点名/
// 配置组名/角色名等）。
//
// **纵深防御，不是唯一防线**：这些标识符已经由 internal/ident 在
// 写入 store 时校验过字符集，落库之后的名字不可能带
// '/'、空格或其它特殊字符。但这里拼路径的时机往往**早于**那次校验
// ——`config group create <name>` 之类命令把用户刚敲的 name 直接
// 拼进 PUT 路径，服务端还没来得及说"这个名字不合法"。不转义的话，
// 一个带 '/' 的名字会把请求错路由到另一个 handler（比如 404 或者
// 命中不相关的路径段），而不是拿到服务端本该给的、干净的
// invalid_identifier 400——转义之后，最坏情况也只是原样把这个
// （最终会被判定非法的）字符串送到服务端，由服务端给出准确的错误。
func seg(s string) string { return url.PathEscape(s) }

// query 把非空的键值对编码成 "?a=b&c=d" 形式的查询串；没有非空值时
// 返回空串。用 url.Values 而不是手写 "?"+sep+"k=v" 拼接——后者对
// 值里本身含 '&'/'='/空格的情况（角色名、site 名都是用户可控字符串）
// 没有任何转义，而且判断"这是第一个参数还是要用 &"的分支很容易漏改。
func query(kv map[string]string) string {
	v := url.Values{}
	for k, val := range kv {
		if val != "" {
			v.Set(k, val)
		}
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// appendQuery 把 extra 合并进 path 已有的查询串（如果有的话），
// 而不是假设 path 一定不带 '?' 就直接拼接——`Client.Do` 给请求加
// `site` 参数时正是这种情况：调用方传进来的 path 可能已经带了
// 别的查询参数。
func appendQuery(path string, extra url.Values) string {
	if len(extra) == 0 {
		return path
	}
	base, existing, _ := strings.Cut(path, "?")
	v, _ := url.ParseQuery(existing)
	for k := range extra {
		v.Set(k, extra.Get(k))
	}
	return base + "?" + v.Encode()
}
