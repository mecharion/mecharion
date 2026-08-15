package pack

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEverySpecRuleIsImplemented 交叉核对规范 §19 的规则表与代码里真正
// 发得出的规则编号。
//
// 起因是一个真事故：**R46（含口令的文件不得对其他人可读）在规范里写了，
// 代码里一行都没有。** 示例全过、测试全绿，而那条安全检查根本不存在——
// 这类缺口靠人工比对表格是发现不了的。
//
// 反向也查：代码里发得出、规范里没写的编号同样是不一致，说明规范漏更新了。
func TestEverySpecRuleIsImplemented(t *testing.T) {
	spec := specRuleIDs(t)
	impl := implementedRuleIDs(t)

	if len(spec) == 0 {
		t.Fatal("没从规范里解析出任何规则编号——解析逻辑坏了")
	}

	var missing []string
	for _, id := range spec {
		if !impl[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		t.Errorf("规范声明了这些规则，但代码里发不出来：%s\n"+
			"  一条承诺了却不存在的检查，比没有这条规则更糟——"+
			"用户以为它在守着。", strings.Join(missing, " "))
	}

	specSet := map[string]bool{}
	for _, id := range spec {
		specSet[id] = true
	}
	var undocumented []string
	for id := range impl {
		if !specSet[id] {
			undocumented = append(undocumented, id)
		}
	}
	sort.Strings(undocumented)
	if len(undocumented) > 0 {
		t.Errorf("代码里发得出但规范 §19 没写：%s\n"+
			"  用户读规范时不知道有这条检查，撞上了会觉得是工具的 bug。",
			strings.Join(undocumented, " "))
	}
}

// pendingMark 标记规范中「已声明但尚未实现」的规则。
const pendingMark = "⏳"

var (
	// 规则表的行形如 `| 26d | 说明… |`
	specRuleRe = regexp.MustCompile(`^\|\s*(\d+[a-z]?)\s*\|`)
	// 代码里形如 l.err("R46", …) / l.warn("R21", …)
	implRuleRe = regexp.MustCompile(`l\.(?:err|warn)\(\s*"(R\d+[a-z]?)"`)
)

// specRuleIDs 从 docs/spec/pack-v1.md 的 §19 中解析规则编号，返回 "RNN" 形式。
func specRuleIDs(t *testing.T) []string {
	t.Helper()
	path := filepath.Join("..", "..", "docs", "spec", "pack-v1.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("读不到规范 %s: %v", path, err)
	}

	lines := strings.Split(string(body), "\n")
	var out []string
	inSection, inTable := false, false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "## 19."):
			inSection = true
			continue
		case inSection && strings.HasPrefix(line, "## "):
			inSection = false
		case inSection && strings.HasPrefix(line, "### "):
			// 规则表都在小节里；小节之前那张「为什么还没做」的说明表
			// 不是规则声明，不能被当成规则
			inTable = true
		}
		if !inSection || !inTable {
			continue
		}
		m := specRuleRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// ⏳ 标记的规则规范里已声明「尚未实现」，用户读规范时就知道它没在守着。
		// 单一事实来源在规范，测试跟着它走，两边不会各说各话。
		if strings.Contains(line, pendingMark) {
			continue
		}
		out = append(out, "R"+normalizeRuleID(m[1]))
	}
	sort.Strings(out)
	return out
}

// implementedRuleIDs 扫描本包源码，收集所有能被发出的规则编号。
func implementedRuleIDs(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range implRuleRe.FindAllStringSubmatch(string(body), -1) {
			out["R"+normalizeRuleID(strings.TrimPrefix(m[1], "R"))] = true
		}
	}
	return out
}

// normalizeRuleID 把 "6" 与 "06" 归一，规范表里两种写法都出现过。
func normalizeRuleID(s string) string {
	num := s
	suffix := ""
	if n := len(s); n > 0 && s[n-1] >= 'a' && s[n-1] <= 'z' {
		num, suffix = s[:n-1], s[n-1:]
	}
	for len(num) > 1 && num[0] == '0' {
		num = num[1:]
	}
	return num + suffix
}

// TestSpecRuleIDsAreUnique 钉住规则编号不重号。
//
// 这不是假想的洁癖：**规范里真的一度有两条 51**——设计文档为一条尚未实现
// 的规则预定了这个号，而另一条规则实现时也拿了它。上面那条覆盖测试用的是
// 集合，重号两边都在集合里，因此它一声不吭。
//
// 重号的代价落在用户身上：`lint` 报 R51，而规范里两条 R51 说的是完全不同
// 的两件事，照着哪条改都可能是错的。
func TestSpecRuleIDsAreUnique(t *testing.T) {
	seen := map[string]int{}
	for _, id := range specRuleIDs(t) {
		seen[id]++
	}
	var dup []string
	for id, n := range seen {
		if n > 1 {
			dup = append(dup, id)
		}
	}
	sort.Strings(dup)
	if len(dup) > 0 {
		t.Errorf("规范 §19 里这些编号出现了不止一次：%s\n"+
			"  用户按编号查规范时会看到两条不同的说明，不知道该照哪条改。",
			strings.Join(dup, " "))
	}
}
