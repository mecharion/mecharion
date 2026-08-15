package vault

import (
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/pack"
)

// TestGenerateOnlyOnce 是 §7.6 的核心保证。
//
// 每轮调和都重新生成会让密码每 60 秒换一次，服务永远连不上——
// 固化不是优化，是正确性。
func TestGenerateOnlyOnce(t *testing.T) {
	f := newFixture(t)
	v := f.open()
	g := pack.Generate{Length: 32}

	first, ver, created, err := v.Generate(ctx(), f.compID, "pw", g)
	if err != nil {
		t.Fatal(err)
	}
	if !created || ver != 1 {
		t.Errorf("首次应当生成，实际 created=%v ver=%d", created, ver)
	}

	for i := 0; i < 5; i++ {
		got, ver, created, err := v.Generate(ctx(), f.compID, "pw", g)
		if err != nil {
			t.Fatal(err)
		}
		if created {
			t.Fatalf("第 %d 次不该重新生成", i+2)
		}
		if got != first || ver != 1 {
			t.Fatalf("第 %d 次拿到了不同的值/版本: %q v%d", i+2, got, ver)
		}
	}
}

// TestGenerateDoesNotOverrideUserValue 钉住运维给了值就不生成。
func TestGenerateDoesNotOverrideUserValue(t *testing.T) {
	f := newFixture(t)
	v := f.open()

	if _, err := v.Put(ctx(), f.compID, "pw", "运维手填的"); err != nil {
		t.Fatal(err)
	}
	got, _, created, err := v.Generate(ctx(), f.compID, "pw", pack.Generate{Length: 32})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("已有值时不该生成")
	}
	if got != "运维手填的" {
		t.Errorf("拿到 %q，期望保留用户的值", got)
	}
}

// TestDefaultCharsetExcludesSymbols 钉住默认字符集不含符号。
//
// 口令要穿过 shell、连接串、EnvironmentFile 与各家应用自己的解析器——
// 符号是转义 bug 的主要来源（spec §7.6）。
func TestDefaultCharsetExcludesSymbols(t *testing.T) {
	f := newFixture(t)
	v := f.open()

	// 多生成几次，降低「恰好没抽到符号」的可能
	for i := 0; i < 20; i++ {
		val, _, err := v.Rotate(ctx(), f.compID, "pw", pack.Generate{Length: 64})
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(val, charsSymbol) {
			t.Fatalf("默认字符集不该含符号，实际 %q", val)
		}
		for _, c := range val {
			if !strings.ContainsRune(charsAlnum, c) {
				t.Fatalf("出现了字母数字之外的字符 %q: %q", c, val)
			}
		}
	}
}

func TestCharsets(t *testing.T) {
	f := newFixture(t)
	v := f.open()

	cases := []struct {
		charset string
		allowed string
	}{
		{pack.CharsetAlnum, charsAlnum},
		{pack.CharsetAlnumSymbol, charsAlnum + charsSymbol},
		{pack.CharsetHex, charsHex},
	}
	for _, tc := range cases {
		val, _, err := v.Rotate(ctx(), f.compID, "pw",
			pack.Generate{Length: 64, Charset: tc.charset})
		if err != nil {
			t.Fatalf("charset=%s: %v", tc.charset, err)
		}
		for _, c := range val {
			if !strings.ContainsRune(tc.allowed, c) {
				t.Errorf("charset=%s 出现了越界字符 %q", tc.charset, c)
			}
		}
	}

	if _, _, err := v.Rotate(ctx(), f.compID, "pw",
		pack.Generate{Length: 32, Charset: "base64"}); err == nil {
		t.Error("未知字符集应当报错")
	}
}

func TestGenerateRespectsLength(t *testing.T) {
	f := newFixture(t)
	v := f.open()

	for _, n := range []int{8, 16, 32, 64, 128} {
		val, _, err := v.Rotate(ctx(), f.compID, "pw", pack.Generate{Length: n})
		if err != nil {
			t.Fatal(err)
		}
		if len(val) != n {
			t.Errorf("length=%d 生成了 %d 位", n, len(val))
		}
	}

	// 缺省长度
	val, _, err := v.Rotate(ctx(), f.compID, "pw", pack.Generate{})
	if err != nil {
		t.Fatal(err)
	}
	if len(val) != pack.DefaultGenerateLength {
		t.Errorf("缺省长度 = %d，期望 %d", len(val), pack.DefaultGenerateLength)
	}

	// 低于下限应当被拒——低于这条线的口令在离线爆破面前没有意义
	if _, _, err := v.Rotate(ctx(), f.compID, "pw", pack.Generate{Length: 4}); err == nil {
		t.Error("长度低于下限应当报错")
	}
}

// TestGenerateIsUnbiased 钉住字符分布没有明显偏置。
//
// 用 `rand.Int63() % len(charset)` 这种写法，在字符集长度不是 2 的幂时会让
// 靠前的字符出现得更频繁——62 个字符的 alnum 正是这种情况。这种弱化**不会
// 有任何症状**，只能靠统计发现，因此值得一个用例守着。
func TestGenerateIsUnbiased(t *testing.T) {
	const n = 60000
	counts := map[rune]int{}
	for i := 0; i < n/60; i++ {
		s, err := randomString(60, pack.CharsetAlnum)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range s {
			counts[c]++
		}
	}

	expect := float64(n) / float64(len(charsAlnum))
	for _, c := range charsAlnum {
		got := float64(counts[c])
		// 宽松阈值：只抓「靠前字符系统性偏多」这类量级的偏置，
		// 不做严格的统计检验，免得偶发抖动让 CI 变红
		if got < expect*0.7 || got > expect*1.3 {
			t.Errorf("字符 %q 出现 %.0f 次，期望约 %.0f —— 分布明显不均",
				c, got, expect)
		}
	}
}

// TestGeneratedValuesDiffer 钉住两次生成不会撞。
func TestGeneratedValuesDiffer(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		s, err := randomString(32, pack.CharsetAlnum)
		if err != nil {
			t.Fatal(err)
		}
		if seen[s] {
			t.Fatalf("生成了重复的值: %q", s)
		}
		seen[s] = true
	}
}
