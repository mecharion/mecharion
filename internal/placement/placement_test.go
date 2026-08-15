package placement

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/store"
)

// loadPack 由一段 pack.yaml 造出 *pack.Pack。
func loadPack(t *testing.T, body string) *pack.Pack {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, pack.PackFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := pack.Load(dir)
	if err != nil {
		t.Fatalf("解析 Pack: %v", err)
	}
	return p
}

// nodes 造一批节点，id 从 1 递增。
func nodes(names ...string) []store.Node {
	out := make([]store.Node, 0, len(names))
	for i, n := range names {
		out = append(out, store.Node{
			ID: int64(i + 1), Name: n, Address: "10.0.0." + n,
			Labels: map[string]string{},
		})
	}
	return out
}

// withRack 给节点打上 rack label。
func withRack(ns []store.Node, racks ...string) []store.Node {
	for i := range ns {
		if i < len(racks) {
			ns[i].Labels["rack"] = racks[i]
		}
	}
	return ns
}

func nodeMap(ns []store.Node) map[int64]store.Node {
	m := map[int64]store.Node{}
	for _, n := range ns {
		m[n.ID] = n
	}
	return m
}

const zkPack = `
schema: pack/v1
name: zookeeper
version: "3.9.1"
platforms: [linux/amd64]
roles:
  - name: server
    cardinality: "1-N"
    quorum: true
    resources: []
placement:
  - antiAffinity: [server]
    scope: node
    reason: "同机多实例不提高可用性"
`

// ── ordinal 差异 ────────────────────────────────────────────────────────

// TestExistingOrdinalsSurviveScaleOut 是本包最重要的一条。
//
// 扩容加入一个**名字排在最前**的节点，已有实例的序号必须纹丝不动。
// 按名字重排会让 ZooKeeper 的 myid、Kafka 的 node.id 同时改变，
// 集群当场损坏（ADR-0028）。
func TestExistingOrdinalsSurviveScaleOut(t *testing.T) {
	p := loadPack(t, zkPack)
	ns := nodes("n2", "n3", "n4", "n1") // n1 的 ID 最大，名字最小

	existing := []store.RoleInstance{
		{ID: 1, Role: "server", NodeID: 1, Ordinal: 0}, // n2
		{ID: 2, Role: "server", NodeID: 2, Ordinal: 1}, // n3
		{ID: 3, Role: "server", NodeID: 3, Ordinal: 2}, // n4
	}

	plan, err := Compute(Input{
		Component: "zk", Pack: p,
		Nodes:    map[string][]store.Node{"server": ns},
		Existing: existing, NodeByID: nodeMap(ns),
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Keep) != 3 || len(plan.Add) != 1 || len(plan.Remove) != 0 {
		t.Fatalf("差异 = keep %d, add %d, remove %d",
			len(plan.Keep), len(plan.Add), len(plan.Remove))
	}
	want := map[string]int{"n2": 0, "n3": 1, "n4": 2}
	for _, a := range plan.Keep {
		if a.Ordinal != want[a.Node.Name] {
			t.Errorf("节点 %s 的序号变成了 %d，期望 %d —— 集群身份会错乱",
				a.Node.Name, a.Ordinal, want[a.Node.Name])
		}
	}
	if plan.Add[0].Node.Name != "n1" {
		t.Errorf("新增的应当是 n1，实际 %s", plan.Add[0].Node.Name)
	}
	if plan.Add[0].Ordinal != -1 {
		t.Errorf("新增实例的序号应当留给 store 分配（-1），实际 %d", plan.Add[0].Ordinal)
	}
}

// TestScaleInProducesRemoveSet 钉住缩容被识别为移除。
func TestScaleInProducesRemoveSet(t *testing.T) {
	p := loadPack(t, zkPack)
	ns := nodes("n1", "n2", "n3")

	existing := []store.RoleInstance{
		{ID: 1, Role: "server", NodeID: 1, Ordinal: 0},
		{ID: 2, Role: "server", NodeID: 2, Ordinal: 1},
		{ID: 3, Role: "server", NodeID: 3, Ordinal: 2},
	}
	plan, err := Compute(Input{
		Component: "zk", Pack: p,
		Nodes:    map[string][]store.Node{"server": ns[:2]},
		Existing: existing, NodeByID: nodeMap(ns),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Remove) != 1 || plan.Remove[0].Ordinal != 2 {
		t.Errorf("应当移除 ordinal=2 的实例，实际 %+v", plan.Remove)
	}
	if len(plan.Keep) != 2 {
		t.Errorf("应当保留 2 个，实际 %d", len(plan.Keep))
	}
}

// ── cardinality ─────────────────────────────────────────────────────────

func TestCardinalityViolations(t *testing.T) {
	p := loadPack(t, `
schema: pack/v1
name: pg
version: "16.4"
platforms: [linux/amd64]
roles:
  - name: primary
    cardinality: "1"
    resources: []
  - name: replica
    cardinality: "0-N"
    resources: []
`)
	ns := nodes("n1", "n2")

	// primary 给了 2 个 —— 超上限
	_, err := Compute(Input{
		Component: "pg-main", Pack: p,
		Nodes: map[string][]store.Node{"primary": ns},
	})
	if err == nil {
		t.Fatal("cardinality \"1\" 给了 2 个节点应当被拒绝")
	}
	for _, want := range []string{"primary", `"1"`, "n1, n2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应含 %q，实际:\n%v", want, err)
		}
	}

	// primary 一个都没给 —— 低于下限
	_, err = Compute(Input{
		Component: "pg-main", Pack: p,
		Nodes: map[string][]store.Node{"replica": ns},
	})
	if err == nil {
		t.Fatal("必选角色未指定节点应当被拒绝")
	}

	// replica 可以为 0
	plan, err := Compute(Input{
		Component: "pg-main", Pack: p,
		Nodes: map[string][]store.Node{"primary": ns[:1]},
	})
	if err != nil {
		t.Fatalf("cardinality 0-N 的角色可以不部署: %v", err)
	}
	if plan.Total() != 1 {
		t.Errorf("总数 = %d", plan.Total())
	}
}

func TestUnknownRoleRejected(t *testing.T) {
	p := loadPack(t, zkPack)
	_, err := Compute(Input{
		Component: "zk", Pack: p,
		Nodes: map[string][]store.Node{"ghost": nodes("n1")},
	})
	if err == nil {
		t.Fatal("指向不存在的角色应当被拒绝")
	}
	if !strings.Contains(err.Error(), "defined roles") {
		t.Errorf("错误信息应列出可用角色，实际:\n%v", err)
	}
}

// ── 放置约束 ────────────────────────────────────────────────────────────

// TestDuplicateNodeRejected 钉住同角色的节点列表不得重复。
//
// 不拦的话后果是**静默的不一致**：cardinality 按 2 算过了关，提交时
// Ensure 却因为已存在而只建出 1 个——计划说两个、实际一个，且无人报错。
func TestDuplicateNodeRejected(t *testing.T) {
	p := loadPack(t, zkPack)
	dup := []store.Node{
		{ID: 1, Name: "n1", Labels: map[string]string{}},
		{ID: 1, Name: "n1", Labels: map[string]string{}},
	}
	_, err := Compute(Input{
		Component: "zk", Pack: p,
		Nodes: map[string][]store.Node{"server": dup},
	})
	if err == nil {
		t.Fatal("同一节点在一个角色里出现两次应当被拒绝")
	}
	for _, want := range []string{"n1", "appears more than once", "one instance per machine"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应含 %q，实际:\n%v", want, err)
		}
	}
}

// TestAntiAffinitySingleRoleIsAboutScope 说明单角色反亲和的真实用途。
//
// `scope: node` 下它是**结构上恒真**的——(role, node) 在存储层就唯一，
// 重复的节点列表又被上一条拦掉了，同角色两个实例根本落不到同一台机器上。
// 它真正有意义的是 rack / zone 这类 scope。
func TestAntiAffinitySingleRoleIsAboutScope(t *testing.T) {
	p := loadPack(t, `
schema: pack/v1
name: zookeeper
version: "3.9.1"
platforms: [linux/amd64]
roles:
  - name: server
    cardinality: "1-N"
    quorum: true
    resources: []
placement:
  - antiAffinity: [server]
    scope: rack
    reason: "同机架故障会一次带走多数派"
`)
	_, err := Compute(Input{
		Component: "zk", Pack: p,
		Nodes: map[string][]store.Node{
			"server": withRack(nodes("n1", "n2", "n3"), "r1", "r1", "r2"),
		},
	})
	if err == nil {
		t.Fatal("同机架的两个 server 应当被拒绝")
	}
	for _, want := range []string{"antiAffinity[server]", "scope=rack", "← conflict", "rack=r1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应含 %q，实际:\n%v", want, err)
		}
	}
}

const hdfsPack = `
schema: pack/v1
name: hdfs
version: "3.3.6"
platforms: [linux/amd64]
roles:
  - name: namenode
    cardinality: "1-2"
    resources: []
  - name: secondarynamenode
    cardinality: "0-1"
    resources: []
  - name: datanode
    cardinality: "1-N"
    resources: []
placement:
  - antiAffinity: [namenode, secondarynamenode]
    scope: node
    reason: "SNN 与 NN 同节点时无法承担元数据恢复职责"
`

// TestAntiAffinityMultiRole 钉住多角色互斥，并检查错误信息的格式。
func TestAntiAffinityMultiRole(t *testing.T) {
	p := loadPack(t, hdfsPack)
	n1 := nodes("n1")

	_, err := Compute(Input{
		Component: "hdfs-prod", Pack: p,
		Nodes: map[string][]store.Node{
			"namenode":          n1,
			"secondarynamenode": n1, // 同一台机器
			"datanode":          nodes("n2"),
		},
	})
	if err == nil {
		t.Fatal("NN 与 SNN 同机应当被拒绝")
	}
	// spec §12 规定的输出格式：约束、逐实例落点、← conflict、reason
	for _, want := range []string{
		"placement validation failed: hdfs-prod",
		"antiAffinity[namenode, secondarynamenode]",
		"namenode", "secondarynamenode",
		"← conflict",
		"reason: SNN 与 NN 同节点时无法承担元数据恢复职责",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应含 %q，实际:\n%v", want, err)
		}
	}
}

// TestAntiAffinityAcrossRacks 钉住 scope 为 label key 时按 label 分组。
func TestAntiAffinityAcrossRacks(t *testing.T) {
	p := loadPack(t, `
schema: pack/v1
name: hdfs
version: "3.3.6"
platforms: [linux/amd64]
roles:
  - name: journalnode
    cardinality: "1-N"
    resources: []
placement:
  - antiAffinity: [journalnode]
    scope: rack
    reason: "同机架故障会一次带走多数派"
`)
	// 两台在同一机架
	sameRack := withRack(nodes("n1", "n2"), "r1", "r1")
	_, err := Compute(Input{
		Component: "hdfs", Pack: p,
		Nodes: map[string][]store.Node{"journalnode": sameRack},
	})
	if err == nil {
		t.Fatal("同机架应当被拒绝")
	}
	if !strings.Contains(err.Error(), "rack=r1") {
		t.Errorf("错误信息应指出同处哪个 scope，实际:\n%v", err)
	}

	// 分开就通过
	diffRack := withRack(nodes("n1", "n2"), "r1", "r2")
	if _, err := Compute(Input{
		Component: "hdfs", Pack: p,
		Nodes: map[string][]store.Node{"journalnode": diffRack},
	}); err != nil {
		t.Errorf("不同机架应当通过: %v", err)
	}
}

// TestMissingLabelIsNotAPass 钉住「无法验证不等于通过」。
func TestMissingLabelIsNotAPass(t *testing.T) {
	p := loadPack(t, `
schema: pack/v1
name: x
version: "1.0"
platforms: [linux/amd64]
roles:
  - name: a
    cardinality: "1-N"
    resources: []
placement:
  - antiAffinity: [a]
    scope: rack
    enforcement: required
    reason: "跨机架分布"
`)
	// 节点没有 rack label
	_, err := Compute(Input{
		Component: "x", Pack: p,
		Nodes: map[string][]store.Node{"a": nodes("n1", "n2")},
	})
	if err == nil {
		t.Fatal("required 约束遇到缺 label 的节点必须报错——无法验证不等于通过")
	}
	if !strings.Contains(err.Error(), "missing label") {
		t.Errorf("错误信息 = %v", err)
	}
}

// TestPreferredOnlyWarns 钉住 preferred 只告警不拒绝。
func TestPreferredOnlyWarns(t *testing.T) {
	p := loadPack(t, `
schema: pack/v1
name: x
version: "1.0"
platforms: [linux/amd64]
roles:
  - name: a
    cardinality: "1-N"
    resources: []
placement:
  - antiAffinity: [a]
    scope: rack
    enforcement: preferred
    reason: "尽量跨机架"
`)
	plan, err := Compute(Input{
		Component: "x", Pack: p,
		Nodes: map[string][]store.Node{
			"a": withRack(nodes("n1", "n2"), "r1", "r1"),
		},
	})
	if err != nil {
		t.Fatalf("preferred 不该拒绝: %v", err)
	}
	if len(plan.Warnings) == 0 {
		t.Error("preferred 未满足时应当告警")
	}
}

// TestAffinityRequiresSameScope 钉住亲和约束。
func TestAffinityRequiresSameScope(t *testing.T) {
	p := loadPack(t, `
schema: pack/v1
name: x
version: "1.0"
platforms: [linux/amd64]
roles:
  - name: datanode
    cardinality: "1-N"
    resources: []
  - name: nodemanager
    cardinality: "1-N"
    resources: []
placement:
  - affinity: [datanode, nodemanager]
    scope: node
    reason: "计算贴近数据"
`)
	ns := nodes("n1", "n2")

	// 分开放 —— 违反
	_, err := Compute(Input{
		Component: "x", Pack: p,
		Nodes: map[string][]store.Node{
			"datanode":    ns[:1],
			"nodemanager": ns[1:],
		},
	})
	if err == nil {
		t.Fatal("affinity 要求同处，分开放应当被拒绝")
	}
	if !strings.Contains(err.Error(), "计算贴近数据") {
		t.Errorf("应当带上 reason，实际:\n%v", err)
	}

	// 同机 —— 通过
	if _, err := Compute(Input{
		Component: "x", Pack: p,
		Nodes: map[string][]store.Node{
			"datanode":    ns[:1],
			"nodemanager": ns[:1],
		},
	}); err != nil {
		t.Errorf("同机应当通过: %v", err)
	}
}

// TestConstraintCheckedAgainstFinalState 钉住校验对象是「放置后的全部实例」。
//
// 只看新增会漏掉「新加的 SNN 与已有的 NN 撞在一起」。
func TestConstraintCheckedAgainstFinalState(t *testing.T) {
	p := loadPack(t, hdfsPack)
	n1 := nodes("n1")

	// NN 已经在 n1 上，这次新增 SNN 也放 n1
	_, err := Compute(Input{
		Component: "hdfs", Pack: p,
		Nodes: map[string][]store.Node{
			"namenode":          n1,
			"secondarynamenode": n1,
			"datanode":          nodes("n2"),
		},
		Existing: []store.RoleInstance{
			{ID: 1, Role: "namenode", NodeID: 1, Ordinal: 0},
		},
		NodeByID: nodeMap(n1),
	})
	if err == nil {
		t.Fatal("新增实例与已有实例冲突时必须被发现")
	}
}

// ── quorum ──────────────────────────────────────────────────────────────

// TestQuorumMaxUnavailable 钉住 (N-1)/2。
//
// 3 节点 ZK 并发下线 2 个会直接失去多数派。
func TestQuorumMaxUnavailable(t *testing.T) {
	p := loadPack(t, zkPack)
	cases := []struct {
		n    int
		want int
	}{{1, 0}, {3, 1}, {5, 2}, {7, 3}}

	for _, tc := range cases {
		names := make([]string, tc.n)
		for i := range names {
			names[i] = string(rune('a' + i))
		}
		plan, err := Compute(Input{
			Component: "zk", Pack: p,
			Nodes: map[string][]store.Node{"server": nodes(names...)},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := plan.MaxUnavailable["server"]; got != tc.want {
			t.Errorf("%d 个实例的 maxUnavailable = %d，期望 %d", tc.n, got, tc.want)
		}
	}
}

// TestQuorumEvenCountWarns 钉住偶数实例告警但不拒绝。
func TestQuorumEvenCountWarns(t *testing.T) {
	p := loadPack(t, zkPack)
	plan, err := Compute(Input{
		Component: "zk", Pack: p,
		Nodes: map[string][]store.Node{"server": nodes("n1", "n2", "n3", "n4")},
	})
	if err != nil {
		t.Fatalf("偶数实例只告警不拒绝: %v", err)
	}
	found := false
	for _, w := range plan.Warnings {
		if strings.Contains(w, "even instance count") {
			found = true
		}
	}
	if !found {
		t.Errorf("应当告警偶数实例，实际 %v", plan.Warnings)
	}
}

// ── profile ─────────────────────────────────────────────────────────────

// TestProfileOverridesCardinality 钉住形态能改 cardinality。
func TestProfileOverridesCardinality(t *testing.T) {
	p := loadPack(t, `
schema: pack/v1
name: hdfs
version: "3.3.6"
platforms: [linux/amd64]
roles:
  - name: namenode
    cardinality: "1"
    resources: []
profiles:
  - name: ha
    roles:
      namenode: { cardinality: "2" }
`)
	ns := nodes("n1", "n2")

	// 默认形态只允许 1 个
	if _, err := Compute(Input{
		Component: "hdfs", Pack: p,
		Nodes: map[string][]store.Node{"namenode": ns},
	}); err == nil {
		t.Error("默认形态下 2 个 NN 应当被拒绝")
	}

	// ha 形态允许 2 个
	if _, err := Compute(Input{
		Component: "hdfs", Pack: p, Profile: "ha",
		Nodes: map[string][]store.Node{"namenode": ns},
	}); err != nil {
		t.Errorf("ha 形态下 2 个 NN 应当通过: %v", err)
	}
}

// TestDisabledRoleRejectsNodes 钉住形态下已关闭的角色不能指定节点。
func TestDisabledRoleRejectsNodes(t *testing.T) {
	p := loadPack(t, `
schema: pack/v1
name: hdfs
version: "3.3.6"
platforms: [linux/amd64]
roles:
  - name: namenode
    cardinality: "1"
    resources: []
  - name: secondarynamenode
    cardinality: "0-1"
    resources: []
profiles:
  - name: ha
    roles:
      secondarynamenode: { enabled: false }
`)
	_, err := Compute(Input{
		Component: "hdfs", Pack: p, Profile: "ha",
		Nodes: map[string][]store.Node{
			"namenode":          nodes("n1"),
			"secondarynamenode": nodes("n2"),
		},
	})
	if err == nil {
		t.Fatal("为已关闭的角色指定节点应当被拒绝")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("错误信息 = %v", err)
	}
}

// ── 依赖 scope ──────────────────────────────────────────────────────────

func TestCheckDependenciesNodeScope(t *testing.T) {
	plan := &Plan{Add: []Assignment{
		{Role: "default", Node: store.Node{ID: 1, Name: "n1"}},
		{Role: "default", Node: store.Node{ID: 2, Name: "n2"}},
	}}

	err := CheckDependencies("java-webapp", plan, []DepPresence{{
		Name: "jdk11", Scope: pack.ScopeNode,
		NodesWithIt: map[int64]bool{1: true}, // n2 上没有
	}})
	if err == nil {
		t.Fatal("scope:node 依赖缺失应当被拒绝")
	}
	for _, want := range []string{"jdk11", "n2", "deploy jdk11 on these nodes first"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应含 %q，实际:\n%v", want, err)
		}
	}

	// 都装了就通过
	if err := CheckDependencies("java-webapp", plan, []DepPresence{{
		Name: "jdk11", Scope: pack.ScopeNode,
		NodesWithIt: map[int64]bool{1: true, 2: true},
	}}); err != nil {
		t.Errorf("依赖齐备应当通过: %v", err)
	}
}

func TestCheckDependenciesSiteScope(t *testing.T) {
	plan := &Plan{Add: []Assignment{
		{Role: "default", Node: store.Node{ID: 1, Name: "n1"}},
	}}

	err := CheckDependencies("kafka", plan, []DepPresence{{
		Name: "zookeeper", Scope: pack.ScopeSite, PresentInSite: false,
	}})
	if err == nil {
		t.Fatal("scope:site 依赖不存在应当被拒绝")
	}
	if !strings.Contains(err.Error(), "--require") {
		t.Errorf("应当提示可用 --require 指定，实际:\n%v", err)
	}

	// site 内存在即可，不要求同节点
	if err := CheckDependencies("kafka", plan, []DepPresence{{
		Name: "zookeeper", Scope: pack.ScopeSite, PresentInSite: true,
	}}); err != nil {
		t.Errorf("site 内存在即应通过: %v", err)
	}
}
