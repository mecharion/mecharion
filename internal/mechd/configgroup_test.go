package mechd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/spec"
	"github.com/mecharion/mecharion/internal/store"
)

// 第 7 步的判据。
//
// ADR-0021 在 2026-08-02 就定了具名 ConfigGroup，表、解析链、渲染路径、
// 多盘绑定的求值代码全都在——缺的只有创建入口。这些测试守的是那个入口
// 补上之后，**ADR 的原始动机场景真的能配出来**。

func saveGroup(t *testing.T, f *fixture, req GroupRequest) *SetParamsResult {
	t.Helper()
	req.Site, req.Component = DefaultSite, "paramkit"
	if req.Actor == "" {
		req.Actor = "test"
	}
	if req.Role == "" {
		req.Role = "main"
	}
	out, err := f.svc.SaveGroup(ctx(), req)
	if err != nil {
		t.Fatalf("建组: %v", err)
	}
	return out
}

// TestGroupOverrideReachesTheRightMachines 是这一整步的核心判据。
//
// 建一个只含 n1 的组并在组上改一个参数：**n1 变，n2 不变**。
// 只断言「组建出来了」是不够的——第 5 步之前那个组选择器也「存在」，
// 它只是永远选不到东西。
func TestGroupOverrideReachesTheRightMachines(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil) // n1、n2 各一个 main 实例

	out := saveGroup(t, f, GroupRequest{
		Name: "just-n1", Members: []string{"n1"},
		Params: map[string]any{"p_int": 42},
	})

	var nodes []string
	for _, c := range out.Changed {
		nodes = append(nodes, c.Node)
	}
	if len(nodes) != 1 || nodes[0] != "n1" {
		t.Fatalf("组覆盖应当只影响 n1，实际影响 %v", nodes)
	}

	// 从组的视角看：值是 42，来源是「本组覆盖」
	fs := fieldsOf(formOf(t, f, "paramkit", "main", "just-n1"))
	if fmt.Sprint(fs["p_int"].Value) != "42" || fs["p_int"].Source != "group" {
		t.Errorf("组内应当看到 42/group，得到 %v/%s",
			fs["p_int"].Value, fs["p_int"].Source)
	}
	// 从角色的视角看：还是默认值
	fs = fieldsOf(formOf(t, f, "paramkit", "main", ""))
	if fs["p_int"].Source == "group" {
		t.Error("角色级视图不该显示某个组的覆盖")
	}
}

// TestOverlappingMembersAreRefusedAtWrite 守的是「一个实例只属于一个组」。
//
// 读取时按优先级挑一个是另一种守法，但那意味着「这台机器到底用哪个组的
// 配置」取决于一条没人记得的规则，症状是「我明明改了 A 组，那台机器没变」。
func TestOverlappingMembersAreRefusedAtWrite(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	saveGroup(t, f, GroupRequest{Name: "a", Members: []string{"n1"}})

	_, err := f.svc.SaveGroup(ctx(), GroupRequest{
		Site: DefaultSite, Component: "paramkit", Role: "main",
		Name: "b", Members: []string{"n1"}, Actor: "test",
	})
	if err == nil {
		t.Fatal("同一台机器进两个组应当被拒绝")
	}
	if !strings.Contains(err.Error(), "a") {
		t.Errorf("错误里要说清它已经在哪个组，得到: %v", err)
	}
}

// TestMemberMustActuallyRunTheRole 守的是成员的有效性。
//
// 把一台没有跑这个角色的机器放进组里，那条覆盖永远不会生效——
// 而用户会以为自己配好了。
func TestMemberMustActuallyRunTheRole(t *testing.T) {
	f := formFixture(t)
	if _, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "paramkit", Roles: map[string][]string{"main": {"n1"}}, Actor: "test",
	}); err != nil {
		t.Fatal(err)
	}

	// n2 在册，但上面没有 paramkit 的实例
	_, err := f.svc.SaveGroup(ctx(), GroupRequest{
		Site: DefaultSite, Component: "paramkit", Role: "main",
		Name: "g", Members: []string{"n2"}, Actor: "test",
	})
	if err == nil {
		t.Fatal("成员机器上没有该角色的实例时应当被拒绝")
	}
}

// TestRemovingAGroupIsARealChange 是 §4.4.4。
//
// 删组之后成员回落到角色级取值——那不是「清理」，是一次真实的配置变更：
// 配置文件会变、digest 会变、声明了 restartRequired 的参数会重启服务。
// 把它做成无提示的删除是最危险的形态。
func TestRemovingAGroupIsARealChange(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	saveGroup(t, f, GroupRequest{
		Name: "g", Members: []string{"n1"},
		Params: map[string]any{"p_restart": 777},
	})

	out, err := f.svc.RemoveGroup(ctx(), GroupRequest{
		Site: DefaultSite, Component: "paramkit", Role: "main",
		Name: "g", DryRun: true, Actor: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Changed) == 0 {
		t.Fatal("删组会让成员的规格变回去，Changed 不该是空的")
	}
	if out.Effect != EffectRestart {
		t.Errorf("组里覆盖过 restartRequired 的参数，删组应当报 %s，得到 %s",
			EffectRestart, out.Effect)
	}
}

// TestSetParamsOnNodeAutoCreatesGroup 是 ADR-0021 消除 UX 摩擦的那一条。
//
// 用户敲的是「改这一台的参数」，模型里却不存在无名的 per-node 覆盖。
// 于是系统替他建一个只含那台机器的组——**并且必须告诉他**。
func TestSetParamsOnNodeAutoCreatesGroup(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	out, err := f.svc.SetParams(ctx(), SetParamsRequest{
		Site: DefaultSite, Component: "paramkit", Role: "main", Node: "n1",
		Set: map[string]any{"p_int": 7}, Actor: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.CreatedGroup != "node-n1" {
		t.Errorf("应当报出自动建的组名 node-n1，得到 %q —— "+
			"静默创建会让 config group list 里凭空冒出一堆 node-* 组",
			out.CreatedGroup)
	}

	groups, err := f.svc.ListGroups(ctx(), DefaultSite, "paramkit", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Name != "node-n1" {
		t.Fatalf("应当有一个 node-n1 组，得到 %+v", groups)
	}
	if len(groups[0].Members) != 1 || groups[0].Members[0] != "n1" {
		t.Errorf("组成员应当只有 n1，得到 %v", groups[0].Members)
	}

	// 只有 n1 受影响
	if len(out.Changed) != 1 || out.Changed[0].Node != "n1" {
		t.Errorf("--node 改配置应当只影响那一台，实际 %+v", out.Changed)
	}
}

// TestSecondEditOnSameNodeReusesTheGroup 守的是自动建组不会越建越多。
func TestSecondEditOnSameNodeReusesTheGroup(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	for _, v := range []int{7, 9} {
		if _, err := f.svc.SetParams(ctx(), SetParamsRequest{
			Site: DefaultSite, Component: "paramkit", Role: "main", Node: "n1",
			Set: map[string]any{"p_int": v}, Actor: "test",
		}); err != nil {
			t.Fatal(err)
		}
	}
	groups, err := f.svc.ListGroups(ctx(), DefaultSite, "paramkit", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("第二次改同一台机器应当复用那个组，实际有 %d 个组", len(groups))
	}
	if got := groups[0].Params["p_int"]; got != float64(9) && got != 9 {
		t.Errorf("组里应当是第二次的值 9，得到 %v", got)
	}
}

// TestPathBindingRequiresTheVolumeToExist 是 §4.4.6。
//
// 留到渲染时才报的话，症状是「组建好了，下一次调和整个组失败」——
// 而那时用户已经离开了建组的上下文。
func TestPathBindingRequiresTheVolumeToExist(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	_, err := f.svc.SaveGroup(ctx(), GroupRequest{
		Site: DefaultSite, Component: "paramkit", Role: "main",
		Name: "disks", Members: []string{"n1"},
		Paths: map[string][]string{"conf": {"nosuchvol"}},
		Actor: "test",
	})
	if err == nil {
		t.Fatal("绑定一个节点上不存在的卷应当在**建组时**就被拒绝")
	}
	if !strings.Contains(err.Error(), "nosuchvol") {
		t.Errorf("错误里要指名是哪个卷，得到: %v", err)
	}
}

// TestMultiDiskBindingReachesThePaths 是 ADR-0021 的**原始动机场景**。
//
// 20 台 4 盘、5 台 12 盘的 DataNode——那 5 台的 dataDirs 要落在更多块盘上。
// spec §8.6 给了写法，render/paths.go 从 M2 起就能求值它，而 mechd 侧
// `pathBindingsFor` 一直是个 `return nil` 的桩，于是这个场景从来没能真的
// 配出来。这条测试是它第一次成立。
func TestMultiDiskBindingReachesThePaths(t *testing.T) {
	f := formFixture(t)

	// 给 n1 两块盘
	n1, err := f.svc.Repos.Nodes().GetByName(ctx(), f.site.ID, "n1")
	if err != nil {
		t.Fatal(err)
	}
	n1.Volumes = []store.Volume{
		{Name: "data1", Path: "/data1", Class: "bulk"},
		{Name: "data2", Path: "/data2", Class: "bulk"},
	}
	if _, err := f.svc.Repos.Nodes().Upsert(ctx(), n1); err != nil {
		t.Fatal(err)
	}
	deployKit(t, f, nil)

	saveGroup(t, f, GroupRequest{
		Name: "two-disk", Members: []string{"n1"},
		Paths: map[string][]string{"conf": {"data1", "data2"}},
	})

	// 规格里那条路径真的变成两个值了吗
	sp := specOf(t, f, "paramkit", "main", "n1")
	pv, ok := sp.Paths["conf"]
	if !ok {
		t.Fatal("规格里没有 conf 这条路径")
	}
	if len(pv.Values) != 2 {
		t.Fatalf("绑定了两块盘，conf 应当有 2 个取值，得到 %v", pv.Values)
	}
	if !strings.HasPrefix(pv.Values[0], "/data1") || !strings.HasPrefix(pv.Values[1], "/data2") {
		t.Errorf("路径应当落在 /data1 与 /data2 上，得到 %v", pv.Values)
	}

	// 组外的机器**自己的**路径不受影响——这才是「只影响成员」的判据
	other := specOf(t, f, "paramkit", "main", "n2")
	if got := other.Paths["conf"].Values; len(got) != 1 {
		t.Errorf("n2 不在组里，它自己的 conf 不该变成多盘，得到 %v", got)
	}
}

// TestPathBindingRipplesThroughTopology 钉住一个**反直觉但正确**的事实。
//
// 给 n1 绑两块盘之后，n2 的 digest 也变了——第一版测试断言「只有 n1 变」，
// 当场变红。查下去发现原因在 `topology`：**每个实例的规格里都带着同角色
// 全部实例的路径**，因为模板要能 `range .Topology.Roles.main` 去引用同伴
// （HDFS 的 namenode 列表、Kafka 的 broker 端点都靠它）。
//
// 因此这条要写清楚：**「只改这一台」在路径上不成立**。改一台机器的多盘
// 绑定会让同角色其余实例重新物化。参数覆盖没有这个问题（它不进拓扑），
// 两者的影响面不同，界面与文档都不该把它们说成一回事。
//
// 有了这条测试，将来谁想「精简一下 topology，把 paths 去掉」会立刻看到
// 自己在破坏什么。
func TestPathBindingRipplesThroughTopology(t *testing.T) {
	f := formFixture(t)
	n1, err := f.svc.Repos.Nodes().GetByName(ctx(), f.site.ID, "n1")
	if err != nil {
		t.Fatal(err)
	}
	n1.Volumes = []store.Volume{
		{Name: "data1", Path: "/data1"}, {Name: "data2", Path: "/data2"},
	}
	if _, err := f.svc.Repos.Nodes().Upsert(ctx(), n1); err != nil {
		t.Fatal(err)
	}
	deployKit(t, f, nil)

	// 路径绑定：波及同角色的其它实例
	out := saveGroup(t, f, GroupRequest{
		Name: "disks", Members: []string{"n1"},
		Paths: map[string][]string{"conf": {"data1", "data2"}},
	})
	if len(out.Changed) != 2 {
		t.Errorf("路径绑定会经拓扑波及同角色全部实例，应当有 2 个变化，得到 %d\n"+
			"  若这里变成 1，多半是有人把 paths 从 topology 里拿掉了——"+
			"那会让 range .Topology.Roles 的模板拿不到同伴的路径", len(out.Changed))
	}

	// 参数覆盖：不进拓扑，只影响成员
	out2 := saveGroup(t, f, GroupRequest{
		Name: "params-only", Members: []string{"n2"},
		Params: map[string]any{"p_int": 33},
	})
	if len(out2.Changed) != 1 || out2.Changed[0].Node != "n2" {
		t.Errorf("参数覆盖不进拓扑，应当只影响 n2，得到 %+v", out2.Changed)
	}
}

// TestGroupDoesNotDisturbOrdinals 守的是 ADR-0028。
//
// ordinal 是实例的身份（ZooKeeper 的 myid、Kafka 的 node.id）。跟着分组走
// 会让一次「把两台机器归到一个组」变成一次集群身份错乱——而
// 「顺手按组重排一下让它们连号」是一个看起来很合理的改动。
func TestGroupDoesNotDisturbOrdinals(t *testing.T) {
	f := formFixture(t)
	deployKit(t, f, nil)

	comp, err := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "paramkit")
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.svc.Repos.Instances().List(ctx(), comp.ID)
	if err != nil {
		t.Fatal(err)
	}
	was := map[int64]int{}
	for _, i := range before {
		was[i.NodeID] = i.Ordinal
	}

	saveGroup(t, f, GroupRequest{Name: "g", Members: []string{"n2"},
		Params: map[string]any{"p_int": 5}})

	after, err := f.svc.Repos.Instances().List(ctx(), comp.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range after {
		if was[i.NodeID] != i.Ordinal {
			t.Errorf("建组改变了 ordinal（node %d: %d → %d）——"+
				"它是实例的身份，不该跟着分组走",
				i.NodeID, was[i.NodeID], i.Ordinal)
		}
	}
}

// specOf 渲染一遍并取某个实例的规格。
//
// 直接走内部的 renderComponent：测试与被测代码同包，没必要为了取一份
// 规格在服务层开一个导出方法——那种方法一旦存在，迟早会被接到别处。
func specOf(t *testing.T, f *fixture, comp, role, node string) *spec.ResolvedSpec {
	t.Helper()
	site, c, err := f.svc.componentOf(ctx(), DefaultSite, comp)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := f.svc.Packs.Resolve(c.Pack.Name, "="+c.Pack.Version)
	if err != nil {
		t.Fatal(err)
	}
	insts, byID, err := f.svc.existingInstances(ctx(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := f.svc.freezeFacts(ctx(), c.ID, assignmentsOf(insts, byID), insts, false)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.renderComponent(ctx(), site, c, entry.Pack, inputs, false)
	if err != nil {
		t.Fatalf("渲染 %s: %v", comp, err)
	}
	sp := res.Spec(role, node)
	if sp == nil {
		t.Fatalf("没有 %s@%s 的规格", role, node)
	}
	return sp
}
