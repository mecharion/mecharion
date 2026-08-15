package resource

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/mecharion/mecharion/internal/spec"
)

// TypeSymlink 是 `symlink` 资源的类型名。
const TypeSymlink = "symlink"

type symlinkArgs struct {
	Path   string `json:"path"`
	Target string `json:"target"`
	// Force 为 true 时允许覆盖已存在的非软链路径。
	Force bool `json:"force,omitempty"`
}

// Symlink 保证一条软链存在且指向声明的目标。
type Symlink struct {
	base
	env  *Env
	args symlinkArgs
}

func newSymlink(env *Env, r spec.Resource) (Resource, error) {
	var a symlinkArgs
	if err := decodeArgs(r, &a); err != nil {
		return nil, err
	}
	if err := requireAbs(r, "path", a.Path); err != nil {
		return nil, err
	}
	if a.Target == "" {
		return nil, badArg(r, "缺少 target")
	}
	return &Symlink{base: base{id: r.ID, typ: r.Type}, env: env, args: a}, nil
}

// Read 读出链接自身，不跟随。
func (s *Symlink) Read(context.Context) (Observed, error) {
	fi, err := os.Lstat(s.args.Path)
	switch {
	case isNotExist(err):
		return Observed{State: StateAbsent}, nil
	case err != nil:
		return unknown("lstat %s: %v", s.args.Path, err), nil
	}

	if fi.Mode()&fs.ModeSymlink == 0 {
		// 路径被一个真实文件/目录占着。Diff 要能报出来，因此是 Present
		// 而非 error——加 force 就能收敛，用户有明确的下一步动作。
		return present(map[string]any{
			"kind":   fileKind(fi),
			"target": "",
		}), nil
	}

	target, err := os.Readlink(s.args.Path)
	if err != nil {
		return unknown("readlink %s: %v", s.args.Path, err), nil
	}
	return present(map[string]any{"kind": "软链", "target": target}), nil
}

// Diff 比较指向的目标。
func (s *Symlink) Diff(o Observed) []Change {
	var b diffBuilder
	switch o.State {
	case StateUnknown:
		return nil
	case StateAbsent:
		b.absent()
		return b.changes
	}
	b.scalar("kind", "软链", o.Field("kind"))
	b.scalar("target", s.args.Target, o.Field("target"))
	return b.changes
}

// Apply 建立或改指软链。
//
// 「建临时名 + rename」让改指成为原子操作——先 unlink 再 symlink 会有
// 一个链接不存在的窗口，而 current 软链正是在这个窗口里被 systemd 读到
// 就会启动失败。这也是 generation 原子切换的基础（ADR-0008）。
func (s *Symlink) Apply(context.Context) error {
	// 已经指对了就不动——重建虽然结果相同，但中间的 rename 会让
	// 正在读这条链的进程有一瞬间看到旧值，白白引入一个竞态窗口。
	if t, err := os.Readlink(s.args.Path); err == nil && t == s.args.Target {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.args.Path), 0o755); err != nil {
		return Transient("创建父目录", err)
	}

	if fi, err := os.Lstat(s.args.Path); err == nil && fi.Mode()&fs.ModeSymlink == 0 {
		if !s.args.Force {
			return Permanentf("创建软链",
				"%s 已存在且不是软链（是 %s）；如确需覆盖请在 Pack 中声明 force: true",
				s.args.Path, fileKind(fi))
		}
		if err := os.RemoveAll(s.args.Path); err != nil {
			return Transient("移除占位路径", err)
		}
	}

	tmp := filepath.Join(filepath.Dir(s.args.Path), "."+filepath.Base(s.args.Path)+".tmp")
	_ = os.Remove(tmp) // 上次中断可能留下
	if err := os.Symlink(s.args.Target, tmp); err != nil {
		return Transient("创建软链", err)
	}
	if err := os.Rename(tmp, s.args.Path); err != nil {
		// Windows 的 MoveFileEx 不接受用 MOVEFILE_REPLACE_EXISTING 覆盖一个
		// 已存在的目录型软链（reparse point 带 FILE_ATTRIBUTE_DIRECTORY），
		// 会报 Access is denied——mechlet 唯一的生产平台是 Linux（见
		// symlink_test.go 的 requireSymlinkSupport），rename 原子性在
		// Windows 上本就不成立，这里退化成非原子的「先删再建」。
		if runtime.GOOS == "windows" {
			if rmErr := os.Remove(s.args.Path); rmErr == nil {
				err = os.Rename(tmp, s.args.Path)
			}
		}
		if err != nil {
			_ = os.Remove(tmp)
			return Transient("切换软链", err)
		}
	}
	return nil
}

// Remove 删除软链本身，不碰目标。
func (s *Symlink) Remove(context.Context) error {
	fi, err := os.Lstat(s.args.Path)
	if isNotExist(err) {
		return nil
	}
	if err != nil {
		return Transient("删除软链", err)
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		// 我们没建过它，就不该删它
		return nil
	}
	if err := os.Remove(s.args.Path); err != nil && !isNotExist(err) {
		return Transient("删除软链", err)
	}
	return nil
}
