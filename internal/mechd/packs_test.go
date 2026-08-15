package mechd

import "testing"

// TestPacksListShowsWhatIsDeployed 守的是部署页最常见的那个问题：
// 「这个我是不是已经装过了」。
//
// 少了这一列，用户会用同一个 Pack 再部署一份、用同一个默认组件名，
// 然后撞上「组件已存在」——而那条错误出现在整个流程的最后一步。
func TestPacksListShowsWhatIsDeployed(t *testing.T) {
	f := formFixture(t)

	before, err := f.svc.ListPacks(ctx(), DefaultSite)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range before.Packs {
		if len(p.Deployed) != 0 {
			t.Fatalf("还没部署时 %s 不该有 deployed，得到 %v", p.Name, p.Deployed)
		}
	}

	deployKit(t, f, nil)

	after, err := f.svc.ListPacks(ctx(), DefaultSite)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, p := range after.Packs {
		if p.Name != "paramkit" {
			continue
		}
		found = true
		if len(p.Deployed) != 1 || p.Deployed[0] != "paramkit" {
			t.Errorf("paramkit 应当报出已部署的组件名，得到 %v", p.Deployed)
		}
		if len(p.Versions) == 0 {
			t.Error("版本列表不能是空的——部署页要靠它选版本")
		}
		if len(p.Roles) == 0 {
			t.Error("角色列表不能是空的")
		}
	}
	if !found {
		t.Fatal("列表里没有 paramkit")
	}
}

// TestUnnamedRoleAppearsInPackList 是第 5 步那个缺陷的同类。
//
// `roleNames` 当时直接读了 `r.Name`，省略角色名的 Pack 于是给出一个空
// 字符串。这里是另一处会犯同样错误的地方——两处都要走 EffectiveName。
func TestUnnamedRoleAppearsInPackList(t *testing.T) {
	f := formFixture(t)
	out, err := f.svc.ListPacks(ctx(), DefaultSite)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range out.Packs {
		if p.Name != "noname" {
			continue
		}
		if len(p.Roles) != 1 || p.Roles[0] != "default" {
			t.Errorf("省略了 name 的角色应当报成 default，得到 %v", p.Roles)
		}
		return
	}
	t.Fatal("列表里没有 noname")
}
