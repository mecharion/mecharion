//go:build linux

package multinode

import (
	"context"
	"strings"
	"testing"
	"time"
)

// M9 第 6 步的验收：一份声明文件能把集群带到它描述的状态。
//
// 单元测试验的是编排（幂等、不删、跳过正在移除的）。这里验的是**整条
// 命令**：文件真的被读进去、组件真的被部署到真机上、第二次真的什么也
// 不做——而后者只有在有真实状态的情况下才有意义。

const applyDoc = `
components:
  - name: web
    pack: go-webapp
    roles:
      default: [NODES]
`

func TestApplyBringsTheClusterToTheDeclaredState(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	n1, n2, n3 := c.node(1), c.node(2), c.node(3)
	site := setupThreeNodeSite(ctx, t, c, n1, n2, n3)
	srv := []string{"--server", "https://" + n1 + ":" + mechdPort,
		"--token", site.token, "--ca-file", caPath}

	doc := strings.Replace(applyDoc, "NODES", n1+", "+n2, 1)
	path := "/tmp/m7n-apply.yaml"
	if out, err := c.sh(ctx, n1,
		"cat > "+path+" <<'EOF'\n"+doc+"EOF"); err != nil {
		t.Fatalf("写声明文件: %v\n%s", err, out)
	}

	// ── ① 干跑什么都不该改 ──
	dry := c.mustRun(ctx, t, n1,
		append([]string{"mechctl", "apply", "-f", path, "--dry-run"}, srv...)...)
	if !strings.Contains(dry, "dry run") {
		t.Errorf("--dry-run 要说明它没改东西:\n%s", dry)
	}
	if list := componentList(ctx, t, c, n1, site.token); strings.Contains(list, "web") {
		t.Fatalf("干跑把组件真的建出来了:\n%s", list)
	}

	// ── ② 真的应用 ──
	out := c.mustRun(ctx, t, n1,
		append([]string{"mechctl", "apply", "-f", path}, srv...)...)
	if !strings.Contains(out, "web") {
		t.Fatalf("apply 输出里应当有 web:\n%s", out)
	}
	if !waitUntil(ctx, 4*time.Minute, func() bool {
		return componentConverged(ctx, t, c, n1, site.token)
	}) {
		t.Fatalf("没有收敛:\n%s", statusDump(ctx, t, c, n1, site.token))
	}
	// 机器上真的有那个服务
	for _, n := range []string{n1, n2} {
		if !unitLoadedOn(ctx, t, c, n) {
			t.Errorf("%s 上没有 unit——apply 没有真的部署出来", n)
		}
	}
	// **只在声明的两台上**：第三台不该被牵连。
	//
	// 判据取自控制面的实例清单，不是 n3 的机器状态——夹具重建站点时
	// 只擦控制面的库，机器上前几条测试留下的 unit 还在那儿。拿机器状态
	// 当判据会把「上一条测试的残留」报成「apply 部署错了地方」，
	// 而那两件事的排查方向完全相反。
	st := componentStatus(ctx, t, c, n1, site.token)
	for _, in := range st.Instances {
		if in.Node == n3 {
			t.Errorf("%s 不在声明里，却出现在实例清单里", n3)
		}
	}
	if len(st.Instances) != 2 {
		t.Errorf("应当只有 2 个实例，实际 %d", len(st.Instances))
	}

	// ── ③ 幂等：第二次什么也不做 ──
	//
	// 不带 --update 的 deploy 会撞上「组件已存在，重复 deploy 默认拒绝」。
	// 那条拒绝对 deploy 是对的，对 apply 是致命的——一份声明文件本来就
	// 该反复应用。
	again, err := c.run(ctx, n1,
		append([]string{"mechctl", "apply", "-f", path}, srv...)...)
	if err != nil {
		t.Fatalf("同一份文件第二次 apply 必须成功: %v\n%s", err, again)
	}
	if strings.Contains(again, "already exists") {
		t.Errorf("第二次撞上了「已存在」——apply 没有走 --update:\n%s", again)
	}

	// ── ④ 不删：文件里没提的组件原封不动 ──
	//
	// 这是验收表第 14 条，也是这条命令的安全底线：一份写漏了的文件
	// 不该删掉线上组件。
	empty := "/tmp/m7n-apply-empty.yaml"
	if out, err := c.sh(ctx, n1, "cat > "+empty+" <<'EOF'\n"+
		"components:\n  - name: other\n    pack: go-webapp\n    roles:\n      default: ["+n3+"]\nEOF"); err != nil {
		t.Fatalf("写第二份文件: %v\n%s", err, out)
	}
	out = c.mustRun(ctx, t, n1,
		append([]string{"mechctl", "apply", "-f", empty}, srv...)...)

	if !strings.Contains(out, "left untouched") || !strings.Contains(out, "web") {
		t.Errorf("要把「集群里有、文件里没有」的组件列出来提醒:\n%s", out)
	}
	if list := componentList(ctx, t, c, n1, site.token); !strings.Contains(list, "web") {
		t.Fatalf("文件里没提的组件被删掉了——声明式不等于「文件里没有就删」:\n%s", list)
	}
	if !unitLoadedOn(ctx, t, c, n1) {
		t.Error("机器上的服务也被动了")
	}
}

// TestApplyRejectsATypo 守这个格式最要紧的一条。
//
// 一个被静默忽略的字段是这类文件最贵的失败方式：文件看起来说了一件事，
// 系统做的是另一件，而两者都不报错。
func TestApplyRejectsATypo(t *testing.T) {
	c := requireCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	n1, n2, n3 := c.node(1), c.node(2), c.node(3)
	site := setupThreeNodeSite(ctx, t, c, n1, n2, n3)
	srv := []string{"--server", "https://" + n1 + ":" + mechdPort,
		"--token", site.token, "--ca-file", caPath}

	bad := "/tmp/m7n-apply-typo.yaml"
	if out, err := c.sh(ctx, n1, "cat > "+bad+" <<'EOF'\n"+
		"components:\n  - name: web\n    pack: go-webapp\n    rolez:\n      default: ["+n1+"]\nEOF"); err != nil {
		t.Fatalf("写文件: %v\n%s", err, out)
	}

	out, err := c.run(ctx, n1,
		append([]string{"mechctl", "apply", "-f", bad}, srv...)...)
	if err == nil {
		t.Fatalf("拼错的字段必须报错，不能静默忽略:\n%s", out)
	}
	if !strings.Contains(out, "rolez") {
		t.Errorf("错误里要指出是哪个字段:\n%s", out)
	}
}
