package mechd

import (
	"context"
	"sort"
)

// PackView 是本地 Pack 集合里的一个 Pack。
//
// 按**名字**聚合而不是按 (名字, 版本) 平铺：用户先选「装什么」，
// 再选「哪一版」。平铺会让一个有 8 个历史版本的 Pack 在列表里占 8 行，
// 把「本地一共有哪些组件可装」这个问题埋掉。
type PackView struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Keywords    []string `json:"keywords,omitempty"`
	// Versions 降序，第一个是默认选中的那个。
	Versions []string `json:"versions"`
	// Roles / Profiles 取**最高版本**的声明，只供列表页预览。
	//
	// 真正要用的那一版由用户选定后再查——不同版本的角色集可以不同，
	// 拿最高版本的角色去部署一个旧版本是错的。
	Roles    []string `json:"roles,omitempty"`
	Profiles []string `json:"profiles,omitempty"`
	// Deployed 是已经用这个 Pack 部署出来的 Component 名。
	//
	// 它回答的是列表页上最常见的一个问题：「这个我是不是已经装过了」。
	Deployed []string `json:"deployed,omitempty"`
}

// PackProblem 是扫描时被跳过的目录。
//
// **必须回给用户**：静默跳过一个解析失败的 Pack，会让「我明明把包放进去了，
// 列表里却没有」变成一次无从查起的排查。
type PackProblem struct {
	Dir    string `json:"dir"`
	Reason string `json:"reason"`
}

// PacksView 是 Pack 列表。
type PacksView struct {
	Packs    []PackView    `json:"packs"`
	Problems []PackProblem `json:"problems,omitempty"`
}

// ListPacks 列出本地可用的 Pack。
func (s *Service) ListPacks(ctx context.Context, siteName string) (*PacksView, error) {
	// 重扫一次：新放进 pack-dir 的 Pack 应当立刻出现在列表里，
	// 不该要求重启 mechd。与 Deploy 里那次重扫同一条理由。
	if err := s.Packs.Reload(); err != nil {
		s.log().Warn("failed to rescan Pack collection, keeping the previous index", "err", err)
	}

	deployed := map[string][]string{}
	if site, err := s.resolveSite(ctx, siteName); err == nil {
		comps, err := s.Repos.Components().List(ctx, site.ID)
		if err != nil {
			return nil, err
		}
		for _, c := range comps {
			deployed[c.Pack.Name] = append(deployed[c.Pack.Name], c.Name)
		}
	}

	out := &PacksView{}
	for _, name := range s.Packs.Names() {
		entries := s.Packs.Versions(name)
		if len(entries) == 0 {
			continue
		}
		v := PackView{
			Name:        name,
			Description: entries[0].Pack.Description,
			Keywords:    entries[0].Pack.Keywords,
			Deployed:    deployed[name],
		}
		for _, e := range entries {
			v.Versions = append(v.Versions, e.Pack.Version)
		}
		for _, r := range entries[0].Pack.Roles {
			v.Roles = append(v.Roles, r.EffectiveName())
		}
		for _, pf := range entries[0].Pack.Profiles {
			v.Profiles = append(v.Profiles, pf.Name)
		}
		sort.Strings(v.Deployed)
		out.Packs = append(out.Packs, v)
	}

	for _, p := range s.Packs.Problems() {
		out.Problems = append(out.Problems, PackProblem{Dir: p.Dir, Reason: p.Reason})
	}
	return out, nil
}
