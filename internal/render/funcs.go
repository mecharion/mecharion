package render

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"github.com/mecharion/mecharion/internal/pack"
	"gopkg.in/yaml.v3"
)

// FuncMap 是渲染时可用的受限函数集（spec §9.1）。
//
// 名字与数量必须与 pack.FuncNames 完全一致——lint 用那份名单解析模板，
// 渲染用这份实现求值。两边不一致的后果是**过了 lint 却渲染不出来**，
// 而那要到部署时才暴露。TestFuncMapMatchesLintNames 钉住这条。
//
// 刻意**不提供** env / exec / 文件读取 / 网络访问：任何能绕过 hermetic
// 约束的函数都不给（ADR-0015）。这不是保守，是让「离线可部署」这条承诺
// 在模板层也成立——一个 `{{ env "..." }}` 就能让整个 Pack 依赖部署机的环境。
func FuncMap() template.FuncMap {
	fm := template.FuncMap{
		"default": tplDefault,
		"quote":   func(v any) string { return quoteWith(v, `"`) },
		"squote":  func(v any) string { return quoteWith(v, `'`) },
		"upper":   func(s string) string { return strings.ToUpper(s) },
		"lower":   func(s string) string { return strings.ToLower(s) },
		"trim":    strings.TrimSpace,
		"replace": func(old, new, s string) string { return strings.ReplaceAll(s, old, new) },
		"join":    tplJoin,
		"split":   func(sep, s string) []string { return strings.Split(s, sep) },
		"indent":  func(n int, s string) string { return indent(n, s) },
		"nindent": func(n int, s string) string { return "\n" + indent(n, s) },
		"b64enc":  func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) },
		"b64dec":  tplB64dec,
		"toYaml":  tplToYaml,
		"toJson":  tplToJSON,
		"add":     func(a, b any) (int64, error) { return arith("add", a, b) },
		"sub":     func(a, b any) (int64, error) { return arith("sub", a, b) },
		"mul":     func(a, b any) (int64, error) { return arith("mul", a, b) },
		"div":     func(a, b any) (int64, error) { return arith("div", a, b) },
		"min":     func(a, b any) (int64, error) { return arith("min", a, b) },
		"max":     func(a, b any) (int64, error) { return arith("max", a, b) },
	}
	return fm
}

// tplDefault 在值为「空」时返回兜底值。
//
// 参数顺序是 (兜底, 值)，因为惯用写法是管道：`{{ .Params.x | default "y" }}`。
func tplDefault(dflt, given any) any {
	if isEmpty(given) {
		return dflt
	}
	return given
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case int:
		return x == 0
	case int64:
		return x == 0
	case float64:
		return x == 0
	case []any:
		return len(x) == 0
	case []string:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

func quoteWith(v any, q string) string {
	s := toStr(v)
	// 引号内的同名引号必须转义，否则生成的是语法错误的配置而不是错误的值——
	// 后者尚能被应用发现，前者常常表现为难以定位的启动失败
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, q, `\`+q)
	return q + s + q
}

// tplJoin 用分隔符连接列表。列表既可能是 []string（多值路径），
// 也可能是 []any（来自 YAML 的 list 参数）。
func tplJoin(sep string, list any) (string, error) {
	items, err := toStrings(list)
	if err != nil {
		return "", fmt.Errorf("join: %w", err)
	}
	return strings.Join(items, sep), nil
}

func toStrings(list any) ([]string, error) {
	switch x := list.(type) {
	case []string:
		return x, nil
	case []any:
		out := make([]string, len(x))
		for i, it := range x {
			out[i] = toStr(it)
		}
		return out, nil
	case nil:
		return nil, nil
	}
	return nil, fmt.Errorf("expected a list, got %T", list)
}

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func indent(n int, s string) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		// 不给空行加尾随空格：YAML 与多数 linter 会挑出来，
		// 而它对渲染结果没有任何意义
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}

func tplB64dec(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", fmt.Errorf("b64dec: %w", err)
	}
	return string(b), nil
}

func tplToYaml(v any) (string, error) {
	b, err := yaml.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("toYaml: %w", err)
	}
	return strings.TrimRight(string(b), "\n"), nil
}

func tplToJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("toJson: %w", err)
	}
	return string(b), nil
}

// ── 算术 ────────────────────────────────────────────────────────────────

// arith 是六个算术函数的共同实现。
//
// 操作数可以是 size 字面量：`{{ min (div .Node.Facts.Memory.Total 2) "31GB" }}`
// 是 spec §7.4 给出的范例写法。因此字符串先按 size 解析成字节数，
// 再参与运算——**返回值一律是字节数**。
//
// 「算出来的字节数」怎么变回 `8GB` 这种人读的形式，由参数层按声明的
// `type: size` 归一化（见 params.go 的 normalize）。放在这里做不到：
// 函数不知道自己的结果要喂给哪个参数。
func arith(op string, a, b any) (int64, error) {
	x, err := toBytes(a)
	if err != nil {
		return 0, fmt.Errorf("%s: first operand: %w", op, err)
	}
	y, err := toBytes(b)
	if err != nil {
		return 0, fmt.Errorf("%s: second operand: %w", op, err)
	}
	switch op {
	case "add":
		return x + y, nil
	case "sub":
		return x - y, nil
	case "mul":
		return x * y, nil
	case "div":
		if y == 0 {
			return 0, fmt.Errorf("div: division by zero")
		}
		return x / y, nil
	case "min":
		return min(x, y), nil
	case "max":
		return max(x, y), nil
	}
	return 0, fmt.Errorf("unknown operation %q", op)
}

// toBytes 把一个操作数化为整数。字符串按 size 解析，因此
// `"31GB"` 与 `31000000000` 等价。
func toBytes(v any) (int64, error) {
	switch x := v.(type) {
	case int:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case int64:
		return x, nil
	case uint64:
		return int64(x), nil
	case float64:
		return int64(x), nil
	case string:
		n, err := pack.ParseSize(x)
		if err != nil {
			return 0, fmt.Errorf("%q is neither a number nor a size", x)
		}
		return n, nil
	case SizeValue:
		// `{{ div .Params.heap 2 }}` —— 富类型直接参与运算，
		// 不必先写 `.Bytes`
		return x.Bytes, nil
	case DurationValue:
		return x.Nanoseconds, nil
	case nil:
		return 0, fmt.Errorf("value missing")
	}
	return 0, fmt.Errorf("expected a number or size, got %T", v)
}
