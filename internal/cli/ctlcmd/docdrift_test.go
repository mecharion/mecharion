package ctlcmd

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/docs/design"
	"github.com/mecharion/mecharion/internal/cli"

	"github.com/spf13/cobra"
)

// 这一条是 M9 存在的理由本身（验收表第 15 条）。
//
// M8 收尾审查逐条核对「文档承诺的」与「代码里有的」，发现 10-cli 里有
// 十几个动词只存在于文档。那次核对是**人工的**，而人工核对只会做一次
// ——下一次漂移会在没人看的时候悄悄发生。
//
// 因此把它变成一条测试：10-cli 命令表里每一条 `mechctl <名词> <动词>`，
// 要么真的存在，要么**在文档里被显式标注未实现**。两者都不是，就报错。

// docVerb 是文档里出现的一条命令。
type docVerb struct {
	line     string
	noun     string
	verb     string
	markedNo bool // 那一行标了「未实现」
}

// **排除 flag**：`mechctl --local component status` 这类示例行里，
// 第一个词是选项而不是名词。不排除的话它会被当成一个叫 `--local`
// 的名词报成漂移——一条假警报会让整条守卫失去可信度。
var verbLine = regexp.MustCompile(`^mechctl\s+([a-z][a-z-]*)\s+([a-z][a-z-]*)`)

// parseDocVerbs 从 10-cli 的代码块里抽出所有 `mechctl <名词> <动词>`。
func parseDocVerbs(t *testing.T) []docVerb {
	t.Helper()

	var out []docVerb
	inBlock := false
	var blockMarkedNo bool
	for _, line := range strings.Split(design.CLIDoc, "\n") {
		if strings.HasPrefix(line, "```") {
			inBlock = !inBlock
			blockMarkedNo = false
			continue
		}
		if !inBlock {
			continue
		}
		// 块内的注释行可以给整块打上「未实现」的标记
		if strings.Contains(line, "尚未实现") && strings.HasPrefix(strings.TrimSpace(line), "#") {
			blockMarkedNo = true
			continue
		}
		m := verbLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		out = append(out, docVerb{
			line: strings.TrimSpace(line), noun: m[1], verb: m[2],
			markedNo: blockMarkedNo || strings.Contains(line, "未实现"),
		})
	}
	if len(out) == 0 {
		t.Fatal("从 10-cli 里一条命令都没解析出来——正则或文档结构变了")
	}
	return out
}

// topLevel 是「后面跟参数而不是子命令」的顶层命令。
var topLevel = map[string]bool{
	"apply": true, "version": true, "completion": true, "deploy": true,
}

// alwaysReal 是由根命令而非本包提供的（version / completion 是 cobra 与
// 根命令装的，deploy 是 component deploy 的别名）。
var alwaysReal = map[string]bool{
	"version": true, "completion": true, "deploy": true,
}

// realVerbs 走一遍 cobra 命令树，返回 "名词 动词" 的集合。
func realVerbs(t *testing.T) map[string]bool {
	t.Helper()
	flags := &ClientFlags{Global: &cli.GlobalFlags{}}
	root := &cobra.Command{Use: "mechctl"}
	root.AddCommand(
		NewApplyCmd(flags), NewBackupCmd(flags), NewComponentCmd(flags), NewConfigCmd(flags),
		NewNodeCmd(flags), NewOrphansCmd(flags), NewPackCmd(flags),
		NewRolloutCmd(flags), NewUserCmd(flags),
	)

	out := map[string]bool{}
	for _, noun := range root.Commands() {
		n := noun.Name()
		out[n] = true // 顶层动词（apply）也算
		for _, verb := range noun.Commands() {
			out[n+" "+verb.Name()] = true
			// 再下一层（node facts show 之类）
			for _, sub := range verb.Commands() {
				out[n+" "+verb.Name()+" "+sub.Name()] = true
			}
		}
	}
	return out
}

// TestDocumentedVerbsExistOrAreMarked 是验收表第 15 条。
//
// **不要求文档只写已实现的**：设计与实现本来就有时间差，而把没做的设计
// 从文档里删掉，会让那些设计连同理由一起消失（M8 审查的结论）。要求的是
// **说实话**——没做的必须标出来。
func TestDocumentedVerbsExistOrAreMarked(t *testing.T) {
	real := realVerbs(t)

	var lying []string
	for _, d := range parseDocVerbs(t) {
		// **不能退回到「名词存在就算数」。**
		//
		// 第一版写的是 `real[key] || real[d.noun]`，于是任何已存在名词
		// 下的任意动词都算通过——`mechctl component teleport` 也能过。
		// 那条守卫只抓得到整个名词缺失（`pack`），而真正常见的漂移恰恰是
		// 「名词还在、某个动词没做」。变异测试当场抓到了这一点。
		exists := real[d.noun+" "+d.verb]
		if topLevel[d.noun] {
			// 顶层命令（apply -f、version、completion、deploy 别名）
			// 后面跟的是参数不是子命令，只看名词那一层。
			exists = real[d.noun] || alwaysReal[d.noun]
		}
		if !exists && !d.markedNo {
			lying = append(lying, d.line)
		}
	}
	sort.Strings(lying)

	if len(lying) > 0 {
		t.Errorf("10-cli 里这些命令既不存在、也没标「未实现」——"+
			"读文档的人会以为它们能用:\n  %s\n\n"+
			"  要么实现它，要么在那一行（或那个代码块的注释里）写上「未实现」。\n"+
			"  **不要直接删掉**：那会让设计连同理由一起消失。",
			strings.Join(lying, "\n  "))
	}
}

// TestMarkedVerbsAreReallyMissing 是上面那条的反面。
//
// 一个**已经做出来却还标着「未实现」**的动词同样是谎话，而且更隐蔽：
// 没有人会去用一个文档说不存在的东西。
func TestMarkedVerbsAreReallyMissing(t *testing.T) {
	real := realVerbs(t)

	var stale []string
	for _, d := range parseDocVerbs(t) {
		if d.markedNo && real[d.noun+" "+d.verb] {
			stale = append(stale, d.line)
		}
	}
	sort.Strings(stale)

	if len(stale) > 0 {
		t.Errorf("这些命令已经实现了，文档却还标着「未实现」——"+
			"没有人会去用一个文档说不存在的东西:\n  %s",
			strings.Join(stale, "\n  "))
	}
}
