package pack

import (
	"fmt"
	"strconv"
	"strings"
)

// Version 是一个上游版本号。
//
// **刻意宽松地解析**：上游版本号五花八门（`16.4`、`3.9.1`、`11.0.24`、
// `252.22-1~deb12u1`、`3.6.0-rc1`），强求严格 semver 会让一大批真实软件
// 无法表达。这里只取各段的前导数字，其余当作预发布后缀。
type Version struct {
	parts []int
	// Suffix 是第一个非数字段之后的部分（`-rc1`、`~deb12u1`）。
	Suffix string
	// Raw 是原始字符串，错误信息里原样回显。
	Raw string
}

// ParseVersion 解析一个版本号。**从不失败**——认不出的部分归入 Suffix。
func ParseVersion(s string) Version {
	v := Version{Raw: s}
	body := strings.TrimSpace(s)

	// 先切出预发布/发行版后缀
	if i := strings.IndexAny(body, "-+~"); i >= 0 {
		v.Suffix = body[i:]
		body = body[:i]
	}

	for _, seg := range strings.Split(body, ".") {
		n, rest := leadingInt(seg)
		v.parts = append(v.parts, n)
		if rest != "" {
			// `1.2b3` 这类：剩余部分并入后缀，不再往下解析
			v.Suffix = rest + v.Suffix
			break
		}
	}
	return v
}

func leadingInt(s string) (int, string) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, s
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil {
		return 0, s
	}
	return n, s[i:]
}

// Part 返回第 i 段，越界时为 0。
//
// 缺省为 0 让 `16` 与 `16.0` 相等——上游常常两种写法混用，
// 把它们当成不同版本会导致约束莫名其妙地不匹配。
func (v Version) Part(i int) int {
	if i < len(v.parts) {
		return v.parts[i]
	}
	return 0
}

func (v Version) String() string { return v.Raw }

// Compare 按段逐一比较；相等时**有后缀的更小**（预发布先于正式版）。
func (v Version) Compare(o Version) int {
	n := max(len(v.parts), len(o.parts))
	for i := 0; i < n; i++ {
		switch {
		case v.Part(i) < o.Part(i):
			return -1
		case v.Part(i) > o.Part(i):
			return 1
		}
	}
	switch {
	case v.Suffix == o.Suffix:
		return 0
	case v.Suffix == "":
		return 1 // 正式版 > 预发布
	case o.Suffix == "":
		return -1
	case v.Suffix < o.Suffix:
		return -1
	default:
		return 1
	}
}

// ── 约束 ────────────────────────────────────────────────────────────────

// Constraint 是一个版本范围表达式。
//
// 支持的写法（逗号分隔表示同时满足）：
//
//   - 任意版本
//     >=16  >16      不小于 / 大于
//     <=16  <16      不大于 / 小于
//     =16   16       精确匹配已声明的各段（`16` 匹配 16.x，`16.4` 只匹配 16.4.x）
//     ~16.4          同一次版本内（>=16.4.0, <16.5.0）
//     ^16.4          同一大版本内（>=16.4.0, <17.0.0）
//     >=14, <16      合取
//
// **不支持 `||` 析取**：真实的 requires 里没见过需要它的场景，而它会让
// 「为什么这个版本没被选中」变得难以解释。需要时再加。
type Constraint struct {
	terms []term
	raw   string
}

type term struct {
	op string
	v  Version
	// segs 是约束里显式写出的段数，用于 `=` 与 `~` / `^` 的边界判定。
	segs int
}

// AnyVersion 是匹配一切的约束。
func AnyVersion() Constraint { return Constraint{raw: "*"} }

// ParseConstraint 解析约束表达式。空串等同于 `*`。
func ParseConstraint(s string) (Constraint, error) {
	c := Constraint{raw: s}
	body := strings.TrimSpace(s)
	if body == "" || body == "*" {
		return c, nil
	}

	for _, part := range strings.Split(body, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		t, err := parseTerm(part)
		if err != nil {
			return Constraint{}, fmt.Errorf("version constraint %q: %w", s, err)
		}
		c.terms = append(c.terms, t)
	}
	if len(c.terms) == 0 {
		return Constraint{}, fmt.Errorf("version constraint %q is empty", s)
	}
	return c, nil
}

func parseTerm(s string) (term, error) {
	// 先拦 `||`——它可能出现在任何位置（`>=1 || <3`），放在比较符之后判断
	// 会让带前缀的写法整个漏掉。
	if strings.Contains(s, "|") {
		return term{}, fmt.Errorf("|| disjunction is not supported -- it makes \"why wasn't this version selected\" " +
			"hard to explain; use commas to express conjunction instead")
	}

	for _, op := range []string{">=", "<=", "!=", ">", "<", "=", "~", "^"} {
		if rest, ok := strings.CutPrefix(s, op); ok {
			rest = strings.TrimSpace(rest)
			if rest == "" {
				return term{}, fmt.Errorf("%q is not followed by a version number", op)
			}
			if op == "!=" {
				return term{}, fmt.Errorf("!= is not supported -- it expresses \"exclude this version\", " +
					"but a dependency should declare what it needs, not what it doesn't want")
			}
			return term{op: op, v: ParseVersion(rest), segs: countSegs(rest)}, nil
		}
	}
	// 裸版本号等同于 `=`
	return term{op: "=", v: ParseVersion(s), segs: countSegs(s)}, nil
}

func countSegs(s string) int {
	if i := strings.IndexAny(s, "-+~"); i >= 0 {
		s = s[:i]
	}
	return len(strings.Split(strings.TrimSpace(s), "."))
}

// String 返回原始表达式。
func (c Constraint) String() string {
	if c.raw == "" {
		return "*"
	}
	return c.raw
}

// Matches 报告版本是否满足全部约束项。
func (c Constraint) Matches(v Version) bool {
	for _, t := range c.terms {
		if !t.matches(v) {
			return false
		}
	}
	return true
}

func (t term) matches(v Version) bool {
	cmp := v.Compare(t.v)
	switch t.op {
	case ">=":
		return cmp >= 0
	case ">":
		return cmp > 0
	case "<=":
		return cmp <= 0
	case "<":
		return cmp < 0
	case "=":
		// 只比对约束里显式写出的段：`16` 匹配 16.4，`16.4` 不匹配 16.5
		for i := 0; i < t.segs; i++ {
			if v.Part(i) != t.v.Part(i) {
				return false
			}
		}
		return true
	case "~":
		// 锁住主版本与次版本，最多两段（与 npm / cargo 的 ~ 一致）：
		//   ~16.4   → >=16.4,   <16.5    锁 0,1
		//   ~16.4.2 → >=16.4.2, <16.5    锁 0,1
		//   ~16     → >=16,     <17      锁 0
		lock := min(t.segs, 2)
		for i := 0; i < lock; i++ {
			if v.Part(i) != t.v.Part(i) {
				return false
			}
		}
		return cmp >= 0
	case "^":
		// 锁住大版本：^16.4 → >=16.4, <17
		return v.Part(0) == t.v.Part(0) && cmp >= 0
	default:
		return false
	}
}
