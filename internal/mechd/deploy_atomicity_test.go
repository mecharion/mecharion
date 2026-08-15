package mechd

import (
	"context"
	"fmt"
	"testing"

	"github.com/mecharion/mecharion/internal/store"
)

// TestDeploy 的原子性系列验证：Component、Instance、Facts、
// Bindings 的写入与渲染此前是分步提交，任一步失败都会留下半份状态。
// 现在这一整块包进一个事务——本文件在几个不同的失败点上验证「数据库
// 快照必须与调用前完全一致」。

// failingRepos 包一层 store.Repos，只在指定的一次调用上返回错误，
// 其余全部原样委托给真实实现。**测试侧的故障注入**，不是生产代码：
// Deploy 本身不知道、也不需要知道调用方是不是在测它的回滚。
type failingRepos struct {
	store.Repos
	// failComponentCreate 等各自控制在对应方法的第几次调用上失败
	// （1 起）；0 表示不注入。
	failComponentCreate  int
	failInstanceEnsure   int
	failInstanceSetFacts int

	componentCreateCalls int
	instanceEnsureCalls  int
	setFactsCalls        int
}

func (r *failingRepos) Components() store.ComponentRepo {
	return &failingComponentRepo{ComponentRepo: r.Repos.Components(), r: r}
}

func (r *failingRepos) Instances() store.InstanceRepo {
	return &failingInstanceRepo{InstanceRepo: r.Repos.Instances(), r: r}
}

type failingComponentRepo struct {
	store.ComponentRepo
	r *failingRepos
}

func (c *failingComponentRepo) Create(ctx context.Context, in store.Component) (store.Component, error) {
	c.r.componentCreateCalls++
	if c.r.failComponentCreate != 0 && c.r.componentCreateCalls == c.r.failComponentCreate {
		return store.Component{}, fmt.Errorf("注入故障：Components().Create 第 %d 次调用", c.r.componentCreateCalls)
	}
	return c.ComponentRepo.Create(ctx, in)
}

type failingInstanceRepo struct {
	store.InstanceRepo
	r *failingRepos
}

func (i *failingInstanceRepo) Ensure(
	ctx context.Context, componentID int64, role string, nodeID int64, cfgGroupID *int64,
) (store.RoleInstance, error) {
	i.r.instanceEnsureCalls++
	if i.r.failInstanceEnsure != 0 && i.r.instanceEnsureCalls == i.r.failInstanceEnsure {
		return store.RoleInstance{}, fmt.Errorf("注入故障：Instances().Ensure 第 %d 次调用", i.r.instanceEnsureCalls)
	}
	return i.InstanceRepo.Ensure(ctx, componentID, role, nodeID, cfgGroupID)
}

func (i *failingInstanceRepo) SetFacts(ctx context.Context, id int64, facts map[string]any) error {
	i.r.setFactsCalls++
	if i.r.failInstanceSetFacts != 0 && i.r.setFactsCalls == i.r.failInstanceSetFacts {
		return fmt.Errorf("注入故障：Instances().SetFacts 第 %d 次调用", i.r.setFactsCalls)
	}
	return i.InstanceRepo.SetFacts(ctx, id, facts)
}

// TestDeployRollsBackOnMissingDependency 是最自然的一种失败：渲染阶段
// 解析依赖绑定时发现依赖没有可用的 Component（java-webapp 需要 jdk11
// 与 postgresql，这里故意一个都不部署）。这正是 `bindOne` 此前的
// 行为——它会在渲染管线内部直接落库固化绑定，与 Component/
// Instance 的写入完全脱节。
func TestDeployRollsBackOnMissingDependency(t *testing.T) {
	f := newFixture(t, "n1")

	_, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "java-webapp", Component: "webapp",
		Roles: map[string][]string{"default": {"n1"}},
	})
	if err == nil {
		t.Fatal("缺依赖的部署应当失败")
	}

	comps, lerr := f.svc.Repos.Components().List(ctx(), f.site.ID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(comps) != 0 {
		t.Fatalf("渲染失败后不该有任何 Component 落库，实际 %d 个: %+v", len(comps), comps)
	}
}

// TestDeployRollsBackWhenInstanceEnsureFails 钉住：Component 已经在
// 事务里写完，但紧接着的 Instance ensure 失败——**Component 那一半
// 不该单独留下**。这是此前「分步提交」最典型的坏结果：库里有一个不带
// 任何实例的 Component，`status` 会显示一个从未真正放置过的组件。
func TestDeployRollsBackWhenInstanceEnsureFails(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3")
	deployJDK(t, f, "n1", "n2", "n3")

	failing := &failingRepos{Repos: f.svc.Repos, failInstanceEnsure: 1}
	f.svc.Repos = failing

	_, err := f.svc.Deploy(ctx(), DeployRequest{
		Pack: "zookeeper", Component: "zk-main", Profile: "ensemble",
		Roles: map[string][]string{"server": {"n1", "n2", "n3"}},
	})
	if err == nil {
		t.Fatal("注入的故障应当让部署失败")
	}

	f.svc.Repos = failing.Repos // 换回真实 Repos 再去查库
	comps, lerr := f.svc.Repos.Components().List(ctx(), f.site.ID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	for _, c := range comps {
		if c.Name == "zk-main" {
			t.Fatalf("Instance ensure 失败后不该留下 zk-main 这个 Component: %+v", c)
		}
	}
}

// TestDeployUpdateRollsBackLeavingOriginalStateIntact 测更危险的那种：
// 不是「全新部署失败」，而是**对一个已经在正常运行的组件做 --update
// 扩容，中途失败**。用扩容（3→4 台）而不是原地改参数，是因为原地
// 改参数时 topology 没变，事实早就冻结过，Instance 相关的写入压根不会
// 被调用到——测不出「Component 已经改、Instance 还没改」这类真正的
// 半吊子状态。扩容会同时触发 Component 更新（新参数）与 Instance
// ensure/SetFacts（新实例），失败之后要求**原有三个实例逐一不变**，
// 第四个不存在，Component 的参数也没被改成这次失败 update 里的新值——
// 不是「数量对得上」这种弱断言。
func TestDeployUpdateRollsBackLeavingOriginalStateIntact(t *testing.T) {
	f := newFixture(t, "n1", "n2", "n3", "n4")
	deployZK(t, f, "n1", "n2", "n3")

	comp, err := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.svc.Repos.Instances().List(ctx(), comp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 3 {
		t.Fatalf("前置条件不成立：期望 3 个实例，实际 %d", len(before))
	}

	// 第 4 个节点是唯一的新实例，它的事实快照是这次 update 里
	// 第一次（也是唯一一次）SetFacts 调用——卡在这里最贴近
	// 「放置、ensure 都已经成功，事务大半已经跑完才炸」的场景。
	failing := &failingRepos{Repos: f.svc.Repos, failInstanceSetFacts: 1}
	f.svc.Repos = failing

	_, err = f.svc.Deploy(ctx(), DeployRequest{
		Pack: "zookeeper", Component: "zk-main", Profile: "ensemble", Update: true,
		Roles: map[string][]string{"server": {"n1", "n2", "n3", "n4"}},
		Set:   map[string]any{"client_port": float64(2182)},
	})
	if err == nil {
		t.Fatal("注入的故障应当让这次 update 失败")
	}

	f.svc.Repos = failing.Repos
	after, err := f.svc.Repos.Instances().List(ctx(), comp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("实例数从 %d 变成了 %d——第四台不该被留下", len(before), len(after))
	}
	byID := map[int64]store.RoleInstance{}
	for _, ri := range before {
		byID[ri.ID] = ri
	}
	for _, got := range after {
		want, ok := byID[got.ID]
		if !ok {
			t.Fatalf("失败的 update 之后出现了新实例 id=%d", got.ID)
		}
		if got.Ordinal != want.Ordinal {
			t.Errorf("实例 %d 的 ordinal 从 %d 变成了 %d", got.ID, want.Ordinal, got.Ordinal)
		}
	}

	gotComp, err := f.svc.Repos.Components().GetByName(ctx(), f.site.ID, "zk-main")
	if err != nil {
		t.Fatal(err)
	}
	if gotComp.Params["client_port"] == float64(2182) {
		t.Error("Component 的参数不该被改成失败那次 update 里的新值")
	}
}
