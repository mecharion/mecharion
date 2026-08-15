package pack

import (
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ParamType 是参数类型。规范 §7.2 —— 这是**完整且封闭**的类型集，
// 不存在自定义或外部 schema 逃生舱（ADR-0007）。
type ParamType string

// 12 种参数类型。
const (
	TypeString   ParamType = "string"
	TypeInt      ParamType = "int"
	TypeFloat    ParamType = "float"
	TypeBool     ParamType = "bool"
	TypeEnum     ParamType = "enum"
	TypePath     ParamType = "path"
	TypePort     ParamType = "port"
	TypeDuration ParamType = "duration"
	TypeSize     ParamType = "size"
	TypeCIDR     ParamType = "cidr"
	TypeSecret   ParamType = "secret"
)

// listPrefix 是 list<T> 的前缀。
const listPrefix = "list<"

// ScalarTypes 是可作为 list<T> 元素的标量类型。
var ScalarTypes = []ParamType{
	TypeString, TypeInt, TypeFloat, TypeBool, TypeEnum,
	TypePath, TypePort, TypeDuration, TypeSize, TypeCIDR, TypeSecret,
}

// Param 是一个参数声明。
type Param struct {
	Type        ParamType `yaml:"type"`
	Default     any       `yaml:"default"`
	Required    bool      `yaml:"required"`
	Description string    `yaml:"description"`

	Min     any    `yaml:"min"`
	Max     any    `yaml:"max"`
	Pattern string `yaml:"pattern"`
	Values  []any  `yaml:"values"`

	Unit     string `yaml:"unit"`
	Group    string `yaml:"group"`
	Advanced bool   `yaml:"advanced"`

	Sensitive       bool `yaml:"sensitive"`
	Immutable       bool `yaml:"immutable"`
	RestartRequired bool `yaml:"restartRequired"`
	ReloadRequired  bool `yaml:"reloadRequired"`

	From        string `yaml:"from"`
	DefaultFrom string `yaml:"defaultFrom"`

	// Generate 让引擎生成初始值，仅 secret 适用（规范 §7.6）。
	Generate *Generate `yaml:"generate"`
}

// Generate 描述引擎如何生成一个密码。
//
// 它的存在是为了让**无人值守**成立：运维不必在提供方与消费方各填一次
// 同一个密码——那既是错误的来源，轮换时更是灾难。
type Generate struct {
	Length  int    `yaml:"length"`
	Charset string `yaml:"charset"`
}

// 生成用字符集。
const (
	// CharsetAlnum 是默认值：只含字母数字。
	//
	// 默认排除符号，是因为口令常要穿过 shell、连接串、YAML 与各家应用
	// 自己的解析器，**符号是转义 bug 的主要来源**。
	CharsetAlnum = "alnum"
	// CharsetAlnumSymbol 在字母数字之外加入符号，熵更高。
	CharsetAlnumSymbol = "alnumSymbol"
	// CharsetHex 是十六进制，适合需要纯 ASCII 且无歧义的场景。
	CharsetHex = "hex"
)

// Charsets 是全部合法字符集名，顺序即文档中的呈现顺序。
var Charsets = []string{CharsetAlnum, CharsetAlnumSymbol, CharsetHex}

// MinGenerateLength 是生成长度的下限。
//
// 8 位是一条底线而非建议值——低于它的口令在离线爆破面前没有意义，
// 而 Pack 作者写一个更小的数字几乎总是笔误。
const MinGenerateLength = 8

// DefaultGenerateLength 是未声明 length 时的取值。
const DefaultGenerateLength = 32

// EffectiveLength 返回生效的生成长度。
func (g Generate) EffectiveLength() int {
	if g.Length <= 0 {
		return DefaultGenerateLength
	}
	return g.Length
}

// EffectiveCharset 返回生效的字符集。
func (g Generate) EffectiveCharset() string {
	if g.Charset == "" {
		return CharsetAlnum
	}
	return g.Charset
}

// IsList 报告该类型是否为 list<T>。
func (t ParamType) IsList() bool {
	return strings.HasPrefix(string(t), listPrefix) && strings.HasSuffix(string(t), ">")
}

// ElemType 返回 list<T> 的元素类型；非 list 时返回自身。
func (t ParamType) ElemType() ParamType {
	if !t.IsList() {
		return t
	}
	return ParamType(strings.TrimSuffix(strings.TrimPrefix(string(t), listPrefix), ">"))
}

// Valid 报告该类型是否合法。
func (t ParamType) Valid() bool {
	e := t
	if t.IsList() {
		e = t.ElemType()
		if e.IsList() {
			return false // 不支持嵌套 list
		}
	}
	for _, s := range ScalarTypes {
		if e == s {
			return true
		}
	}
	return false
}

// IsSensitive 报告该参数是否应脱敏；secret 类型隐含 sensitive。
func (p Param) IsSensitive() bool {
	return p.Sensitive || p.Type == TypeSecret || p.Type.ElemType() == TypeSecret
}

// ── 值校验 ──────────────────────────────────────────────────────────────

// ValidateValue 按类型校验一个具体取值。
func (p Param) ValidateValue(v any) error {
	if p.Type.IsList() {
		items, ok := toSlice(v)
		if !ok {
			return fmt.Errorf("expected a list, got %T", v)
		}
		elem := Param{Type: p.Type.ElemType(), Values: p.Values, Pattern: p.Pattern, Min: p.Min, Max: p.Max}
		for i, it := range items {
			if err := elem.ValidateValue(it); err != nil {
				return fmt.Errorf("item %d: %w", i, err)
			}
		}
		return nil
	}

	switch p.Type {
	case TypeString, TypeSecret:
		s, ok := toString(v)
		if !ok {
			return fmt.Errorf("expected a string, got %T", v)
		}
		if p.Pattern != "" {
			re, err := regexp.Compile(p.Pattern)
			if err != nil {
				return fmt.Errorf("invalid pattern: %w", err)
			}
			if !re.MatchString(s) {
				return fmt.Errorf("%q does not match pattern %q", s, p.Pattern)
			}
		}
		return nil

	case TypeBool:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("expected a boolean, got %T", v)
		}
		return nil

	case TypeEnum:
		s, _ := toString(v)
		for _, allowed := range p.Values {
			if as, _ := toString(allowed); as == s {
				return nil
			}
		}
		return fmt.Errorf("%q is not one of values %v", s, p.Values)

	case TypeInt:
		n, err := toInt(v)
		if err != nil {
			return err
		}
		return checkRange(float64(n), p.Min, p.Max)

	case TypeFloat:
		f, err := toFloat(v)
		if err != nil {
			return err
		}
		return checkRange(f, p.Min, p.Max)

	case TypePort:
		n, err := toInt(v)
		if err != nil {
			return err
		}
		if n < 1 || n > 65535 {
			return fmt.Errorf("port %d is out of range 1-65535", n)
		}
		return checkRange(float64(n), p.Min, p.Max)

	case TypePath:
		s, ok := toString(v)
		if !ok {
			return fmt.Errorf("expected a string path, got %T", v)
		}
		// 含模板表达式时无法在此判定，交由模板校验
		if strings.Contains(s, "{{") {
			return nil
		}
		if !path.IsAbs(s) {
			return fmt.Errorf("%q must be an absolute path", s)
		}
		return nil

	case TypeDuration:
		s, ok := toString(v)
		if !ok {
			return fmt.Errorf("expected a duration string (e.g. 30s / 5m / 1h), got %T", v)
		}
		if strings.Contains(s, "{{") {
			return nil
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("%q is not a valid duration (e.g. 30s / 5m / 1h)", s)
		}
		return checkRange(float64(d), p.Min, p.Max)

	case TypeSize:
		s, ok := toString(v)
		if !ok {
			return fmt.Errorf("expected a size string (e.g. 4GB / 512MB / 1Gi), got %T", v)
		}
		if strings.Contains(s, "{{") {
			return nil
		}
		b, err := ParseSize(s)
		if err != nil {
			return err
		}
		return checkRange(float64(b), p.Min, p.Max)

	case TypeCIDR:
		s, ok := toString(v)
		if !ok {
			return fmt.Errorf("expected a CIDR string, got %T", v)
		}
		if strings.Contains(s, "{{") {
			return nil
		}
		if _, err := netip.ParsePrefix(s); err != nil {
			return fmt.Errorf("%q is not a valid CIDR: %v", s, err)
		}
		return nil
	}
	return fmt.Errorf("unknown type %q", p.Type)
}

func checkRange(v float64, min, max any) error {
	if min != nil {
		if m, err := toFloat(min); err == nil && v < m {
			return fmt.Errorf("value %v is less than min %v", v, min)
		}
	}
	if max != nil {
		if m, err := toFloat(max); err == nil && v > m {
			return fmt.Errorf("value %v is greater than max %v", v, max)
		}
	}
	return nil
}

// ── size 解析 ───────────────────────────────────────────────────────────

var sizeRe = regexp.MustCompile(`^\s*([0-9]+(?:\.[0-9]+)?)\s*([A-Za-z]*)\s*$`)

var sizeUnits = map[string]int64{
	"":    1,
	"B":   1,
	"KB":  1000,
	"MB":  1000 * 1000,
	"GB":  1000 * 1000 * 1000,
	"TB":  1000 * 1000 * 1000 * 1000,
	"PB":  1000 * 1000 * 1000 * 1000 * 1000,
	"KI":  1 << 10,
	"MI":  1 << 20,
	"GI":  1 << 30,
	"TI":  1 << 40,
	"PI":  1 << 50,
	"KIB": 1 << 10,
	"MIB": 1 << 20,
	"GIB": 1 << 30,
	"TIB": 1 << 40,
	"PIB": 1 << 50,
}

// ParseSize 解析 size 字面量，返回字节数。
// 支持十进制（KB/MB/GB/TB/PB）与二进制（Ki/Mi/Gi/Ti/Pi）两套单位。
func ParseSize(s string) (int64, error) {
	m := sizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%q is not a valid size (e.g. 4GB / 512MB / 1Gi)", s)
	}
	mult, ok := sizeUnits[strings.ToUpper(m[2])]
	if !ok {
		return 0, fmt.Errorf("%q has unrecognized unit %q (use B/KB/MB/GB/TB/PB or Ki/Mi/Gi/Ti/Pi)", s, m[2])
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("the number in %q could not be parsed: %w", s, err)
	}
	return int64(f * float64(mult)), nil
}

// ── 类型转换助手 ────────────────────────────────────────────────────────

func toString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), true
	case bool:
		return strconv.FormatBool(x), true
	default:
		return "", false
	}
}

func toInt(v any) (int64, error) {
	switch x := v.(type) {
	case int:
		return int64(x), nil
	case int64:
		return x, nil
	case float64:
		if x != float64(int64(x)) {
			return 0, fmt.Errorf("expected an integer, got %v", x)
		}
		return int64(x), nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not an integer", x)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("expected an integer, got %T", v)
	}
}

func toFloat(v any) (float64, error) {
	switch x := v.(type) {
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case float64:
		return x, nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a number", x)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("expected a number, got %T", v)
	}
}

func toSlice(v any) ([]any, bool) {
	if s, ok := v.([]any); ok {
		return s, true
	}
	return nil, false
}
