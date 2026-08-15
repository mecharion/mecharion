package store

import (
	"context"
	"errors"
	"testing"
)

type repoFixture struct {
	t     *testing.T
	s     *Store
	r     Repos
	site  Site
	nodes map[string]Node
}

func newRepoFixture(t *testing.T) *repoFixture {
	t.Helper()
	s := openTest(t)
	f := &repoFixture{t: t, s: s, r: s.Repos(), nodes: map[string]Node{}}

	site, err := f.r.Sites().Create(context.Background(), Site{
		Name: "s1", Kind: "standalone", Labels: map[string]string{"env": "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.site = site
	return f
}

func (f *repoFixture) node(name string) Node {
	f.t.Helper()
	if n, ok := f.nodes[name]; ok {
		return n
	}
	n, err := f.r.Nodes().Upsert(context.Background(), Node{
		SiteID: f.site.ID, Name: name, Address: "10.0.0." + name,
		Labels: map[string]string{"rack": "r1"},
		Roots:  map[string]string{"opt": "/opt/mecharion"},
		Volumes: []Volume{
			{Name: "data1", Path: "/data1", Class: "bulk"},
		},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.nodes[name] = n
	return n
}

func (f *repoFixture) component(name string) Component {
	f.t.Helper()
	c, err := f.r.Components().Create(context.Background(), Component{
		SiteID: f.site.ID, Name: name,
		Pack:   PackRef{Name: "zookeeper", Version: "3.9.1", Revision: 1},
		Params: map[string]any{"client_port": float64(2181)},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func bg() context.Context { return context.Background() }

// TestSiteRoundTripThroughRepo 钉住 JSON 列与时间的往返转换。
//
// 领域类型与 sqlcgen 的行结构刻意分开：后者字段全是 string/int64，
// 直接端到业务层会让每个调用点各自解 JSON、各自 parse 时间。
func TestSiteRoundTripThroughRepo(t *testing.T) {
	f := newRepoFixture(t)

	got, err := f.r.Sites().GetByName(bg(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Labels["env"] != "test" {
		t.Errorf("labels 未往返: %v", got.Labels)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at 未解析成 time.Time")
	}

	list, err := f.r.Sites().List(bg())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("List = %d 条", len(list))
	}
}

func TestNodeJSONColumns(t *testing.T) {
	f := newRepoFixture(t)
	f.node("1")

	got, err := f.r.Nodes().GetByName(bg(), f.site.ID, "1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Roots["opt"] != "/opt/mecharion" {
		t.Errorf("roots 未往返: %v", got.Roots)
	}
	if len(got.Volumes) != 1 || got.Volumes[0].Class != "bulk" {
		t.Errorf("volumes 未往返: %v", got.Volumes)
	}
	if got.Status != "unknown" {
		t.Errorf("status 缺省 = %q，期望 unknown", got.Status)
	}

	// Upsert 幂等：同名节点更新而非重复插入
	if _, err := f.r.Nodes().Upsert(bg(), Node{
		SiteID: f.site.ID, Name: "1", Address: "10.0.0.99",
	}); err != nil {
		t.Fatal(err)
	}
	list, _ := f.r.Nodes().List(bg(), f.site.ID)
	if len(list) != 1 {
		t.Fatalf("Upsert 应当更新而非新增，实际 %d 条", len(list))
	}
	if list[0].Address != "10.0.0.99" {
		t.Errorf("地址未更新: %s", list[0].Address)
	}
}

func TestNotFoundIsSentinel(t *testing.T) {
	f := newRepoFixture(t)

	_, err := f.r.Sites().GetByName(bg(), "不存在")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("应当是 ErrNotFound，实际 %v", err)
	}
	_, err = f.r.Nodes().GetByName(bg(), f.site.ID, "不存在")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("应当是 ErrNotFound，实际 %v", err)
	}
	_, err = f.r.Components().GetByName(bg(), f.site.ID, "不存在")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("应当是 ErrNotFound，实际 %v", err)
	}
}

// ── ADR-0028：ordinal 的分配 ────────────────────────────────────────────

// TestEnsureKeepsExistingOrdinal 是本层最重要的一条。
//
// 扩容时已有实例的序号**必须原样保留**。按名字重排会让 ZooKeeper 的
// myid、Kafka 的 node.id 同时改变，集群当场损坏。
func TestEnsureKeepsExistingOrdinal(t *testing.T) {
	f := newRepoFixture(t)
	c := f.component("zk")
	inst := f.r.Instances()

	// 初始三节点：n2, n3, n4
	first := map[string]int{}
	for _, n := range []string{"2", "3", "4"} {
		ri, err := inst.Ensure(bg(), c.ID, "server", f.node(n).ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		first[n] = ri.Ordinal
	}
	if first["2"] != 0 || first["3"] != 1 || first["4"] != 2 {
		t.Fatalf("初始序号 = %v，期望 2→0 3→1 4→2", first)
	}

	// 扩容加入 n1 —— **名字排在最前**，正是会触发重排 bug 的情形
	ri, err := inst.Ensure(bg(), c.ID, "server", f.node("1").ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ri.Ordinal != 3 {
		t.Errorf("新实例序号 = %d，期望 3（取最大+1）", ri.Ordinal)
	}

	// 已有三个必须纹丝不动
	for _, n := range []string{"2", "3", "4"} {
		got, err := inst.Ensure(bg(), c.ID, "server", f.node(n).ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.Ordinal != first[n] {
			t.Errorf("节点 %s 的序号从 %d 变成了 %d —— 集群身份会错乱",
				n, first[n], got.Ordinal)
		}
	}
}

// TestEnsureIsIdempotent 钉住重复调用不产生新实例。
func TestEnsureIsIdempotent(t *testing.T) {
	f := newRepoFixture(t)
	c := f.component("zk")
	inst := f.r.Instances()
	node := f.node("1")

	var firstID int64
	for i := 0; i < 3; i++ {
		ri, err := inst.Ensure(bg(), c.ID, "server", node.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstID = ri.ID
		} else if ri.ID != firstID {
			t.Errorf("第 %d 次拿到了不同的实例", i+1)
		}
	}
	list, _ := inst.List(bg(), c.ID)
	if len(list) != 1 {
		t.Errorf("应当只有 1 个实例，实际 %d", len(list))
	}
}

// TestOrdinalNotReclaimedAfterDelete 钉住序号不回收。
//
// 回收会让新实例拿到刚被释放的编号，而集群里其它成员的元数据可能还
// 引用着旧成员——这正是要避免的「身份复用」。
func TestOrdinalNotReclaimedAfterDelete(t *testing.T) {
	f := newRepoFixture(t)
	c := f.component("zk")
	inst := f.r.Instances()

	var ids []int64
	for _, n := range []string{"1", "2", "3"} {
		ri, err := inst.Ensure(bg(), c.ID, "server", f.node(n).ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ri.ID)
	}

	// 删掉中间那个（ordinal=1）
	if err := inst.Delete(bg(), ids[1]); err != nil {
		t.Fatal(err)
	}

	// 新实例应当拿 3，而不是复用空出来的 1
	ri, err := inst.Ensure(bg(), c.ID, "server", f.node("4").ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ri.Ordinal != 3 {
		t.Errorf("新实例序号 = %d，期望 3——空洞不该被回收", ri.Ordinal)
	}

	list, _ := inst.ListByRole(bg(), c.ID, "server")
	var ords []int
	for _, x := range list {
		ords = append(ords, x.Ordinal)
	}
	if len(ords) != 3 || ords[0] != 0 || ords[1] != 2 || ords[2] != 3 {
		t.Errorf("序号序列 = %v，期望 [0 2 3]（1 是空洞）", ords)
	}
}

// TestOrdinalsAreIndependentPerRole 钉住序号在角色内唯一，不是全组件唯一。
func TestOrdinalsAreIndependentPerRole(t *testing.T) {
	f := newRepoFixture(t)
	c := f.component("hdfs")
	inst := f.r.Instances()

	nn, err := inst.Ensure(bg(), c.ID, "namenode", f.node("1").ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	dn, err := inst.Ensure(bg(), c.ID, "datanode", f.node("2").ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if nn.Ordinal != 0 || dn.Ordinal != 0 {
		t.Errorf("每个角色各有一条从 0 起的独立序列，实际 nn=%d dn=%d",
			nn.Ordinal, dn.Ordinal)
	}
}

// ── 绑定 ────────────────────────────────────────────────────────────────

// TestBindingDeleteProtection 钉住引用计数。
func TestBindingDeleteProtection(t *testing.T) {
	f := newRepoFixture(t)
	zk := f.component("zk")
	kafka := f.component("kafka")

	if _, err := f.r.Bindings().Create(bg(), PackBinding{
		ComponentID: kafka.ID, RequireName: "zookeeper", BoundComponentID: zk.ID,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := f.r.Bindings().CountDependents(bg(), zk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("依赖者计数 = %d，期望 1", n)
	}

	// 有依赖者时删除应当被外键拦住
	if err := f.r.Components().Delete(bg(), zk.ID); err == nil {
		t.Error("被依赖的 Component 不该能直接删掉")
	}
	// 依赖者本身可以删，删完引用计数归零
	if err := f.r.Components().Delete(bg(), kafka.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.r.Bindings().CountDependents(bg(), zk.ID); n != 0 {
		t.Errorf("删掉依赖者后计数应当归零，实际 %d", n)
	}
}

func TestComponentUpdate(t *testing.T) {
	f := newRepoFixture(t)
	c := f.component("zk")

	c.Pack.Version = "3.9.2"
	c.Params = map[string]any{"client_port": float64(2182)}
	got, err := f.r.Components().Update(bg(), c)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pack.Version != "3.9.2" {
		t.Errorf("版本 = %s", got.Pack.Version)
	}
	if got.Params["client_port"] != float64(2182) {
		t.Errorf("参数 = %v", got.Params)
	}
	if !got.UpdatedAt.After(got.CreatedAt) && !got.UpdatedAt.Equal(got.CreatedAt) {
		t.Error("updated_at 应当被刷新")
	}
}

func TestConfigGroupUpsert(t *testing.T) {
	f := newRepoFixture(t)
	c := f.component("hdfs")

	g, err := f.r.ConfigGroups().Upsert(bg(), ConfigGroup{
		ComponentID: c.ID, Role: "datanode", Name: "node-n7",
		Members: []string{"n7"},
		Params:  map[string]any{"max_xcievers": float64(8192)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Members) != 1 || g.Members[0] != "n7" {
		t.Errorf("members 未往返: %v", g.Members)
	}

	// 同名再写是更新
	if _, err := f.r.ConfigGroups().Upsert(bg(), ConfigGroup{
		ComponentID: c.ID, Role: "datanode", Name: "node-n7",
		Members: []string{"n7", "n8"},
	}); err != nil {
		t.Fatal(err)
	}
	list, _ := f.r.ConfigGroups().List(bg(), c.ID)
	if len(list) != 1 || len(list[0].Members) != 2 {
		t.Errorf("Upsert 应当更新，实际 %v", list)
	}
}
