package pack

import "testing"

// TestParseVersionIsLenient 钉住「宽松解析」。
//
// 上游版本号五花八门。强求严格 semver 会让一大批真实软件无法表达——
// PostgreSQL 的 `16.4` 只有两段，Debian 的 `252.22-1~deb12u1` 带发行版后缀。
func TestParseVersionIsLenient(t *testing.T) {
	cases := []struct {
		in     string
		parts  []int
		suffix string
	}{
		{"16.4", []int{16, 4}, ""},
		{"3.9.1", []int{3, 9, 1}, ""},
		{"11.0.24", []int{11, 0, 24}, ""},
		{"252.22-1~deb12u1", []int{252, 22}, "-1~deb12u1"},
		{"3.6.0-rc1", []int{3, 6, 0}, "-rc1"},
		{"1.2.3+build5", []int{1, 2, 3}, "+build5"},
		{"16", []int{16}, ""},
	}
	for _, tc := range cases {
		v := ParseVersion(tc.in)
		for i, want := range tc.parts {
			if v.Part(i) != want {
				t.Errorf("%q 第 %d 段 = %d，期望 %d", tc.in, i, v.Part(i), want)
			}
		}
		if v.Suffix != tc.suffix {
			t.Errorf("%q 后缀 = %q，期望 %q", tc.in, v.Suffix, tc.suffix)
		}
	}
}

// TestVersionCompare 钉住比较语义。
func TestVersionCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"16.4", "16.5", -1},
		{"16.5", "16.4", 1},
		{"16.4", "16.4", 0},
		// 缺省段视为 0：上游常常两种写法混用
		{"16", "16.0", 0},
		{"16.0.0", "16", 0},
		{"3.10", "3.9", 1}, // 数值比较，不是字典序
		// 预发布小于正式版
		{"3.6.0-rc1", "3.6.0", -1},
		{"3.6.0", "3.6.0-rc1", 1},
		{"1.0.0-alpha", "1.0.0-beta", -1},
	}
	for _, tc := range cases {
		got := ParseVersion(tc.a).Compare(ParseVersion(tc.b))
		if got != tc.want {
			t.Errorf("Compare(%q, %q) = %d，期望 %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestConstraintMatching(t *testing.T) {
	cases := []struct {
		constraint string
		version    string
		want       bool
	}{
		{"", "16.4", true},
		{"*", "anything", true},

		{">=14", "16.4", true},
		{">=14", "13.9", false},
		{">=16.4", "16.4", true},
		{">16.4", "16.4", false},
		{"<16", "15.9", true},
		{"<16", "16.0", false},
		// `<=16` 等价于 `<=16.0.0`，因此 16.4 不满足。见下方专门的用例。
		{"<=16", "16.4", false},
		{"<=16", "16.0", true},
		{"<=16", "15.9", true},

		// `=` 只比对写出来的段
		{"16", "16.4", true},
		{"16", "17.0", false},
		{"16.4", "16.4.2", true},
		{"16.4", "16.5", false},

		// ~ 锁主版本与次版本，最多两段（与 npm / cargo 一致）
		{"~16.4", "16.4.9", true},
		{"~16.4", "16.5.0", false},
		{"~16.4", "16.3.9", false},
		{"~16.4.2", "16.4.9", true},
		{"~16.4.2", "16.4.1", false},
		{"~16.4.2", "16.5.0", false},
		{"~16", "16.9", true},
		{"~16", "17.0", false},

		// ^ 锁大版本
		{"^16.4", "16.9", true},
		{"^16.4", "17.0", false},
		{"^16.4", "16.3", false},

		// 合取
		{">=14, <16", "15.2", true},
		{">=14, <16", "16.0", false},
		{">=14, <16", "13.9", false},
	}
	for _, tc := range cases {
		c, err := ParseConstraint(tc.constraint)
		if err != nil {
			t.Errorf("解析 %q: %v", tc.constraint, err)
			continue
		}
		if got := c.Matches(ParseVersion(tc.version)); got != tc.want {
			t.Errorf("%q 匹配 %q = %v，期望 %v",
				tc.constraint, tc.version, got, tc.want)
		}
	}
}

// TestComparisonIsPerSegment 钉住一个容易误解的地方。
//
// `<=16` 等价于 `<=16.0.0`，**不是**「16.x 都行」——比较是逐段的，
// 16.4 大于 16.0。这与 npm / cargo 的语义一致。
//
// 想表达「16.x 都行」应当写 `~16` 或 `<17`。这条容易踩，因此单列一个用例
// 加上规范里的一段说明。
func TestComparisonIsPerSegment(t *testing.T) {
	le16, _ := ParseConstraint("<=16")
	if le16.Matches(ParseVersion("16.4")) {
		t.Error("<=16 不该匹配 16.4——它等价于 <=16.0.0")
	}

	// 想要「16.x 都行」的两种正确写法
	for _, c := range []string{"~16", "<17"} {
		cc, err := ParseConstraint(c)
		if err != nil {
			t.Fatal(err)
		}
		if !cc.Matches(ParseVersion("16.4")) {
			t.Errorf("%s 应当匹配 16.4", c)
		}
	}
}

// TestConstraintRejectsUnsupported 钉住两条刻意不支持的写法。
func TestConstraintRejectsUnsupported(t *testing.T) {
	for _, c := range []string{">=1 || <3", "!=16.4", ">="} {
		if _, err := ParseConstraint(c); err == nil {
			t.Errorf("%q 应当被拒绝", c)
		}
	}
}

// TestUpgradePolicyPattern 钉住示例包用的写法能通过。
func TestUpgradePolicyPattern(t *testing.T) {
	// postgresql 声明 ~16：15.x → 16.x 这类跨界升级要被拒
	c, err := ParseConstraint("~16")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Matches(ParseVersion("16.4")) {
		t.Error("~16 应当匹配 16.4")
	}
	if c.Matches(ParseVersion("15.9")) {
		t.Error("~16 不该匹配 15.9——pg_upgrade 是另一回事")
	}
	if c.Matches(ParseVersion("17.0")) {
		t.Error("~16 不该匹配 17.0")
	}
}
