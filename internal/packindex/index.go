// Package packindex 索引本地可用的 Pack 集合。
//
// **绝不联网获取**（[ADR-0015](../../docs/adr/0015-offline-first-hermetic.md)）：
// 依赖只在本地集合内解析，缺失时列出缺什么，而不是去下载。
//
// 它服务两处：
//
//   - mechd 放置阶段的依赖解析（14-placement §4.3）
//   - `mechpack lint` 的跨 Pack 校验（规则 43）——那条规则此前无法实现，
//     正是因为缺一个跨 Pack 的索引
package packindex

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/mecharion/mecharion/internal/pack"
)

// Entry 是索引中的一个 Pack。
type Entry struct {
	Pack    *pack.Pack
	Dir     string
	Version pack.Version
}

// Name 返回 Pack 名。
func (e *Entry) Name() string { return e.Pack.Name }

// Index 按名字索引本地 Pack，同名多版本按版本**降序**排列。
type Index struct {
	byName map[string][]*Entry
	// problems 记录扫描时跳过的目录及原因，供诊断——
	// 静默跳过一个解析失败的 Pack，会让「为什么找不到依赖」无从查起。
	problems []Problem
	// dirs 是扫描过的目录，供 Reload 重扫。
	dirs []string
	mu   sync.RWMutex
}

// Problem 是一个被跳过的目录。
type Problem struct {
	Dir    string
	Reason string
}

// New 构造一个空索引。
func New() *Index { return &Index{byName: map[string][]*Entry{}} }

// Load 扫描若干目录，把其中的 Pack 收进索引。
//
// 每个目录下的**直接子目录**若含 pack.yaml 即视为一个 Pack；目录本身含
// pack.yaml 时也接受（单 Pack 目录）。不递归深挖——那会把 examples 里的
// 模板目录之类也扫进来。
func Load(dirs ...string) (*Index, error) {
	ix := New()
	for _, dir := range dirs {
		if err := ix.AddDir(dir); err != nil {
			return nil, err
		}
	}
	return ix, nil
}

// AddDir 把一个目录下的 Pack 加入索引。
func (ix *Index) AddDir(dir string) error {
	if dir == "" {
		return nil
	}
	ix.mu.Lock()
	if !slices.Contains(ix.dirs, dir) {
		ix.dirs = append(ix.dirs, dir)
	}
	ix.mu.Unlock()

	fi, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 目录不存在等同于「本地没有 Pack」，不是错误
		}
		return fmt.Errorf("packindex: 读取 %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("packindex: %s 不是目录", dir)
	}

	// 目录本身就是一个 Pack
	if _, err := os.Stat(filepath.Join(dir, pack.PackFile)); err == nil {
		ix.addPath(dir)
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("packindex: 列举 %s: %w", dir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(sub, pack.PackFile)); err != nil {
			continue
		}
		ix.addPath(sub)
	}
	return nil
}

func (ix *Index) addPath(dir string) {
	p, err := pack.Load(dir)
	if err != nil {
		ix.problems = append(ix.problems, Problem{Dir: dir, Reason: err.Error()})
		return
	}
	ix.Add(p, dir)
}

// Add 把一个已解析的 Pack 加入索引。
func (ix *Index) Add(p *pack.Pack, dir string) {
	if ix.byName == nil {
		ix.byName = map[string][]*Entry{}
	}
	e := &Entry{Pack: p, Dir: dir, Version: pack.ParseVersion(p.Version)}
	list := append(ix.byName[p.Name], e)
	// 版本降序：解析约束时取最高的满足项
	sort.SliceStable(list, func(i, j int) bool {
		return list[i].Version.Compare(list[j].Version) > 0
	})
	ix.byName[p.Name] = list
}

// Names 返回索引中全部 Pack 名，按字典序。
func (ix *Index) Names() []string {
	out := make([]string, 0, len(ix.byName))
	for k := range ix.byName {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Problems 返回扫描时跳过的目录。
func (ix *Index) Problems() []Problem { return ix.problems }

// Versions 返回某个 Pack 的全部版本，降序。
func (ix *Index) Versions(name string) []*Entry { return ix.byName[name] }

// Resolve 返回满足约束的**最高**版本。
//
// 取最高而非最低：依赖声明的是下限（`>=11.0.20`），用户装了更新的版本
// 通常就是想用它。
func (ix *Index) Resolve(name, constraint string) (*Entry, error) {
	ix.mu.RLock()
	list := ix.byName[name]
	ix.mu.RUnlock()
	if len(list) == 0 {
		return nil, &NotFoundError{Name: name, Constraint: constraint, Known: ix.Names()}
	}
	c, err := pack.ParseConstraint(constraint)
	if err != nil {
		return nil, err
	}
	for _, e := range list {
		if c.Matches(e.Version) {
			return e, nil
		}
	}
	return nil, &VersionError{Name: name, Constraint: constraint, Available: versionsOf(list)}
}

func versionsOf(list []*Entry) []string {
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.Pack.Version)
	}
	return out
}

// NotFoundError 表示本地根本没有这个 Pack。
type NotFoundError struct {
	Name       string
	Constraint string
	Known      []string
}

func (e *NotFoundError) Error() string {
	msg := fmt.Sprintf("本地没有 Pack %q（要求 %s）", e.Name, orAny(e.Constraint))
	if len(e.Known) > 0 {
		msg += "\n  本地已有: " + strings.Join(e.Known, ", ")
	}
	// 绝不联网获取是刻意的（ADR-0015）——提示用户自己把 Pack 放进来
	return msg + "\n  → 先部署或导入该 Pack；引擎不会联网获取"
}

// VersionError 表示有这个 Pack，但没有满足约束的版本。
type VersionError struct {
	Name       string
	Constraint string
	Available  []string
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("Pack %q 没有满足 %s 的版本\n  本地已有: %s",
		e.Name, orAny(e.Constraint), strings.Join(e.Available, ", "))
}

func orAny(c string) string {
	if strings.TrimSpace(c) == "" {
		return "*"
	}
	return c
}

// ── lint 的依赖解析器 ───────────────────────────────────────────────────

// Exports 实现 pack.DepResolver：返回被依赖 Pack 的导出名。
//
// ok 为 false 表示**无法核对**（本地没有这个 Pack）——此时 lint 只告警，
// 不报错。依赖方可能来自别处、单独发布，缺席不代表 Pack 写错了。
func (ix *Index) Exports(name, constraint string) (exports []string, ok bool) {
	e, err := ix.Resolve(name, constraint)
	if err != nil {
		return nil, false
	}
	out := make([]string, 0, len(e.Pack.Exports))
	for k := range e.Pack.Exports {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, true
}

var _ pack.DepResolver = (*Index)(nil)

// Reload 重扫已知目录。
//
// **必要而不是优化。** 索引原本只在 mechd 启动时扫一次，于是「把新版本的
// Pack 放进 pack-dir 再 upgrade」——升级唯一的正常用法——必须先重启 mechd
// 才生效。那不是可以让用户忍受的东西。
//
// 重扫的代价是解析几十份 YAML。它只发生在**人触发的操作**上
// （deploy / upgrade / rollback），不在节点订阅那条高频路径上。
func (ix *Index) Reload() error {
	ix.mu.RLock()
	dirs := append([]string(nil), ix.dirs...)
	ix.mu.RUnlock()

	fresh := New()
	for _, d := range dirs {
		if err := fresh.AddDir(d); err != nil && !os.IsNotExist(err) {
			// 重扫失败时**保留旧索引**：一次读目录出错不该让已经能用的
			// Pack 集合突然消失，那会把一个小故障放大成全站不可部署。
			return fmt.Errorf("packindex: 重扫失败，沿用上一次的索引: %w", err)
		}
	}

	ix.mu.Lock()
	ix.byName, ix.problems = fresh.byName, fresh.problems
	ix.mu.Unlock()
	return nil
}
