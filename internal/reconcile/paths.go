package reconcile

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/resource"
	"github.com/mecharion/mecharion/internal/spec"
)

// distSuffix 是载荷自带配置目录被改名后的后缀。
//
// 保留每个 generation 的默认值基线，才能回答「新版本新增/改名了哪些配置项」
// （04-paths-and-storage §3）。Debian 的 .dpkg-dist、RPM 的 .rpmnew 都是
// 同一问题的补丁式解法——一开始就留位置，成本几乎为零。
const distSuffix = ".dist"

// pathNames 返回 paths 的名字，按字典序。
//
// map 遍历顺序随机，而目录创建顺序影响的是错误信息的稳定性：
// 同一个故障每次报不同的路径，会让人以为是两个问题。
func pathNames(m map[string]spec.PathValue) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// createPaths 是调和的阶段①：创建 paths 声明的全部目录。
//
// 必须早于资源阶段——模板要往这些目录里渲染。这也是 Pack **不需要**
// 为这些路径再写 directory 资源的原因（spec §8.3）。
//
// `kind: multi` 时逐个创建：HDFS DataNode 的每块盘都要建。
func createPaths(ctx context.Context, env *resource.Env, s *spec.ResolvedSpec) error {
	owned := directoryResourcePaths(s)

	for _, name := range pathNames(s.Paths) {
		p := s.Paths[name]
		for i, dir := range p.Values {
			if dir == "" {
				return faults.Permanentf("创建路径",
					"paths.%s[%d] 为空", name, i)
			}
			if !filepath.IsAbs(dir) {
				return faults.Permanentf("创建路径",
					"paths.%s[%d] 必须是绝对路径，实际 %q", name, i, dir)
			}
			if err := makeDir(ctx, env, dir, p, owned[dir]); err != nil {
				return faults.Permanentf("创建路径", "paths.%s: %v", name, err)
			}
		}
	}
	return nil
}

// directoryResourcePaths 返回被 `directory` 资源显式接管的路径。
//
// **同一个路径既在 paths 里、又有一条 directory 资源，是合法且常见的写法**：
// paths 决定它在哪，directory 资源给它更具体的属主与权限
// （go-webapp 的 data 目录就是这样：paths 默认 0755，资源要 0750 + webapp）。
//
// 两处都强制的话，引擎每轮都会和自己打架：
//
//	阶段①  按 paths 的 mode chmod 回 0755
//	阶段②  directory 资源看到 0755 ≠ 0750 → 报「漂移」
//	        driftPolicy 默认 report → 不改
//	下一轮 重复
//
// 后果有两条，都严重：**Pack 声明的 0750 从未真正生效**（安全声明失效），
// 以及**一条永远存在的假漂移**——那会把运维训练成无视漂移告警。
//
// 因此：**更具体的那个赢**。paths 仍然负责把目录建出来（模板要往里写，
// 顺序上必须早于资源阶段），但属主与权限交给资源。
func directoryResourcePaths(s *spec.ResolvedSpec) map[string]bool {
	out := map[string]bool{}
	for _, r := range s.Resources {
		if r.Type != resource.TypeDirectory {
			continue
		}
		var a struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(r.Args, &a) == nil && a.Path != "" {
			out[a.Path] = true
		}
	}
	return out
}

// makeDir 建一个目录并收敛属主与权限。
//
// byResource 为 true 时只建目录，属主与权限留给那条 directory 资源。
func makeDir(
	ctx context.Context, env *resource.Env, dir string, p spec.PathValue, byResource bool,
) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if byResource {
		return nil
	}
	return resource.ApplyOwnership(ctx, env, dir, p.Mode, p.Owner, p.Group)
}

// linkPaths 把声明了 linkInto 的路径接进 generation 目录。
//
// 它**不能**和阶段① 合并：`distDir` 的改名要在载荷解开之后才做得了，
// 而载荷是资源阶段解的。顺序（04-paths-and-storage §3）：
//
//	① 解开 blob 到 generation 目录        ← 资源阶段
//	② 把载荷自带的 config/ 改名 config.dist/
//	③ 渲染权威配置到 /etc/…                ← 资源阶段
//	④ 建软链 <generation>/config -> /etc/…
//
// 于是应用看到的是它期望的 $HOME/config，而配置真身在 /etc 下跨版本存活。
func linkPaths(s *spec.ResolvedSpec, genDir string) error {
	for _, name := range pathNames(s.Paths) {
		p := s.Paths[name]
		if p.LinkInto == "" {
			continue
		}
		if p.Kind == "multi" {
			return faults.Permanentf("接入路径",
				"paths.%s: kind=multi 不能声明 linkInto——一条软链指不了多块盘", name)
		}
		target := p.First()
		if target == "" {
			return faults.Permanentf("接入路径", "paths.%s 没有值", name)
		}
		if err := linkOne(p.LinkInto, target, p.DistDir, genDir); err != nil {
			return faults.Permanentf("接入路径", "paths.%s: %v", name, err)
		}
	}
	return nil
}

// linkOne 处理一条 linkInto。
func linkOne(link, target, distDir, genDir string) error {
	if !filepath.IsAbs(link) {
		return faults.Permanentf("", "linkInto 必须是绝对路径，实际 %q", link)
	}
	// linkInto 必须落在 generation 之内——它的用途就是把外部路径接进
	// generation。指到别处要么是笔误，要么会破坏「generation 只读」不变式。
	if genDir != "" && !within(genDir, link) {
		return faults.Permanentf("",
			"linkInto %q 不在 generation 目录 %q 之内", link, genDir)
	}

	fi, err := os.Lstat(link)
	switch {
	case err == nil && fi.Mode()&os.ModeSymlink != 0:
		if cur, rerr := os.Readlink(link); rerr == nil && cur == target {
			return nil // 已经接好了
		}
		if rerr := os.Remove(link); rerr != nil {
			return rerr
		}

	case err == nil && fi.IsDir():
		// 载荷自带的同名目录。改名保留为基线，而不是删掉——那是这个
		// 版本的默认配置，`config diff --from --to` 要靠它。
		if distDir == "" {
			return faults.Permanentf("",
				"%s 已存在且是目录，但没有声明 distDir\n"+
					"  载荷自带该目录时必须声明 distDir，引擎才知道把它改名保留",
				link)
		}
		dist := filepath.Join(filepath.Dir(link), filepath.Base(link)+distSuffix)
		if _, derr := os.Lstat(dist); derr == nil {
			// 上一次物化到一半被打断，.dist 已经存在
			if rerr := os.RemoveAll(link); rerr != nil {
				return rerr
			}
		} else if rerr := os.Rename(link, dist); rerr != nil {
			return rerr
		}

	case err == nil:
		return faults.Permanentf("",
			"%s 已存在且既不是软链也不是目录（是普通文件？），无法接入", link)

	case !os.IsNotExist(err):
		return err
	}

	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	return os.Symlink(target, link)
}

// within 报告 p 是否位于 root 之内。
func within(root, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	if err != nil {
		return false
	}
	return filepath.IsLocal(rel) || rel == "."
}

// homeOf 取出组件的安装根。没有声明 home 的（纯主机配置 Pack）返回空串。
func homeOf(s *spec.ResolvedSpec) string {
	return s.Paths["home"].First()
}

// pathSnapshot 把 paths 转成状态文件里固化用的形态。
func pathSnapshot(s *spec.ResolvedSpec) map[string][]string {
	out := make(map[string][]string, len(s.Paths))
	for name, p := range s.Paths {
		out[name] = append([]string(nil), p.Values...)
	}
	return out
}
