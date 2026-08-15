//go:build linux

package multinode

import (
	"context"
	"strings"
	"testing"
	"time"
)

// M9 第 4 步的整条链路验收。
//
// remove 之后数据目录**故意留着**。这里验的是那之后的两件事：
//
//	找得到  orphans list 列出节点、实例、路径
//	清得掉  orphans purge 之后目录真的没了
//
// 少了任何一半，「默认保留」就等于在每台机器上悄悄堆垃圾——10-cli §4.3
// 明写「保留而不提供发现机制等于把问题推给未来」。

func TestOrphansFindAndPurgeTheLeftovers(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	n1, n2, n3 := c.node(1), c.node(2), c.node(3)
	site := setupThreeNodeSite(ctx, t, c, n1, n2, n3)
	tok := site.token
	srv := []string{"--server", "https://" + n1 + ":" + mechdPort,
		"--token", tok, "--ca-file", caPath}

	// ── ① 装起来，往数据目录里放个记号 ──
	c.mustRun(ctx, t, n1, append([]string{"mechctl", "component", "deploy",
		"go-webapp", "-c", "web", "--nodes", n1 + "," + n2}, srv...)...)
	if !waitUntil(ctx, 4*time.Minute, func() bool {
		return componentConverged(ctx, t, c, n1, tok)
	}) {
		t.Fatalf("没有收敛:\n%s", statusDump(ctx, t, c, n1, tok))
	}
	for _, n := range []string{n1, n2} {
		if out, err := c.sh(ctx, n,
			"mkdir -p "+webDataDir+" && echo payload > "+webDataDir+"/keepme"); err != nil {
			t.Fatalf("[%s] 放记号: %v\n%s", n, err, out)
		}
	}

	// ── ② remove（默认保留数据）──
	c.mustRunStdin(ctx, t, n1, "web\n",
		append([]string{"mechctl", "component", "remove", "web"}, srv...)...)
	if !waitUntil(ctx, 5*time.Minute, func() bool {
		return componentGone(ctx, t, c, n1, tok)
	}) {
		t.Fatalf("记录没消失:\n%s", componentList(ctx, t, c, n1, tok))
	}

	// ── ③ 找得到 ──
	//
	// 组件记录一删，那些实例就不在下发里了，节点侧的 refreshOrphans 会把
	// 留下的收据报上来。**中心侧不需要另行登记**——这一步验的正是那一条。
	// **等到两条都齐，而不是等到第一条。** 两台机器各按自己的上报节奏报
	// 上来；等到「有一条」就断言「有两条」，抓到的永远是先到的那台，
	// 而失败信息会让人以为另一台坏了——那是一次纯粹由测试制造的误导。
	var list string
	if !waitUntil(ctx, 3*time.Minute, func() bool {
		list = c.mustRun(ctx, t, n1,
			append([]string{"mechctl", "orphans", "list"}, srv...)...)
		return strings.Count(list, "web__default") == 2 &&
			strings.Contains(list, webDataDir)
	}) {
		t.Fatalf("两台机器各该有一条带路径的孤儿，实际:\n%s", list)
	}

	// ── ④ 清得掉 ──
	out := c.mustRunStdin(ctx, t, n1, "web__default\n",
		append([]string{"mechctl", "orphans", "purge", n1, "web__default"}, srv...)...)
	if !strings.Contains(out, webDataDir) {
		t.Errorf("purge 之前要把将要删掉的目录列出来:\n%s", out)
	}

	if !waitUntil(ctx, 4*time.Minute, func() bool {
		_, err := c.sh(ctx, n1, "test -d "+webDataDir)
		return err != nil // 目录没了
	}) {
		t.Fatalf("%s 上的数据目录没被清掉", n1)
	}

	// ── ⑤ 只清了那一台 ──
	//
	// 同一个实例键在两台机器上都有。purge 只给了 n1，n2 的数据必须原封不动
	// ——否则一次「清掉这台的残留」会顺手抹掉另一台的。
	got, err := c.sh(ctx, n2, "cat "+webDataDir+"/keepme")
	if err != nil || strings.TrimSpace(got) != "payload" {
		t.Errorf("[%s] 没被 purge 的那台数据必须原封不动，读到 %q (%v)", n2, got, err)
	}

	// ── ⑥ 清掉的那条从列表里消失 ──
	//
	// 自限：节点清完之后本地收据就没了，孤儿不再上报，那一行跟着消失。
	if !waitUntil(ctx, 3*time.Minute, func() bool {
		l := c.mustRun(ctx, t, n1,
			append([]string{"mechctl", "orphans", "list", "--node", n1}, srv...)...)
		return !strings.Contains(l, "web__default")
	}) {
		t.Errorf("清干净之后 %s 的那条孤儿应当从列表里消失", n1)
	}
}

// TestOrphansPurgeRefusesUnknown：不存在的孤儿要干净地拒绝，并给出下一步。
func TestOrphansPurgeRefusesUnknown(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	n1, n2, n3 := c.node(1), c.node(2), c.node(3)
	site := setupThreeNodeSite(ctx, t, c, n1, n2, n3)
	srv := []string{"--server", "https://" + n1 + ":" + mechdPort,
		"--token", site.token, "--ca-file", caPath}

	out, err := c.runStdin(ctx, n1, "",
		append([]string{"mechctl", "orphans", "purge", n1, "nope__default"}, srv...)...)
	if err == nil {
		t.Fatalf("不存在的孤儿应当报错:\n%s", out)
	}
	if !strings.Contains(out, "orphans list") {
		t.Errorf("要给出下一步:\n%s", out)
	}
}
