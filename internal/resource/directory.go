package resource

import (
	"context"
	"fmt"
	"io/fs"
	"os"

	"github.com/mecharion/mecharion/internal/spec"
)

// TypeDirectory 是 `directory` 资源的类型名。
const TypeDirectory = "directory"

type dirArgs struct {
	Path string `json:"path"`
	ownership
	// Recursive 为 true 时对整棵子树收敛属主与权限。
	Recursive bool `json:"recursive,omitempty"`
}

// Directory 保证一个目录存在，且属主与权限符合声明。
type Directory struct {
	base
	env  *Env
	args dirArgs
}

func newDirectory(env *Env, r spec.Resource) (Resource, error) {
	var a dirArgs
	if err := decodeArgs(r, &a); err != nil {
		return nil, err
	}
	if err := requireAbs(r, "path", a.Path); err != nil {
		return nil, err
	}
	if err := a.validate(); err != nil {
		return nil, err
	}
	return &Directory{base: base{id: r.ID, typ: r.Type}, env: env, args: a}, nil
}

// Read 探测目录。
func (d *Directory) Read(ctx context.Context) (Observed, error) {
	fi, err := os.Stat(d.args.Path)
	switch {
	case os.IsNotExist(err):
		// Stat 跟随软链。链本身在、目标不在 → 悬空软链，MkdirAll 会以
		// EEXIST 失败，这里提前给出能看懂的错误。
		if li, lerr := os.Lstat(d.args.Path); lerr == nil && li.Mode()&fs.ModeSymlink != 0 {
			target, _ := os.Readlink(d.args.Path)
			return Observed{}, Permanentf("读取目录",
				"%s 是指向 %s 的软链，而目标不存在", d.args.Path, target)
		}
		return Observed{State: StateAbsent}, nil
	case isNotDirErr(err):
		return Observed{}, Permanentf("读取目录",
			"%s 的某一级父路径不是目录", d.args.Path)
	case err != nil:
		return unknown("stat %s: %v", d.args.Path, err), nil
	}

	if !fi.IsDir() {
		// 「这里应该是目录但躺着一个文件」是配置问题，不是漂移。
		// 引擎删掉它腾地方是破坏性的，必须由人来决定。
		return Observed{}, Permanentf("读取目录",
			"%s 已存在但不是目录（是 %s）", d.args.Path, fileKind(fi))
	}

	fields := map[string]any{}
	d.args.readInto(ctx, d.env, fields, fi)
	return present(fields), nil
}

// Diff 比较 mode / owner / group。
func (d *Directory) Diff(o Observed) []Change {
	var b diffBuilder
	switch o.State {
	case StateUnknown:
		return nil
	case StateAbsent:
		b.absent()
		return b.changes
	}
	d.args.diffInto(&b, o)
	return b.changes
}

// Apply 创建目录并收敛属主与权限。
//
// 幂等性来自 MkdirAll（已存在即返回 nil）与 chmod/chown（重复设置同一值
// 无副作用）。
func (d *Directory) Apply(ctx context.Context) error {
	if err := os.MkdirAll(d.args.Path, 0o755); err != nil {
		return Transient("创建目录", err)
	}
	if d.args.Recursive {
		return d.args.applyRecursive(ctx, d.env, d.args.Path)
	}
	return d.args.apply(ctx, d.env, d.args.Path)
}

// Remove **仅在目录为空时**删除。
//
// 非空目录可能装着用户数据——组件的 data 目录就是最典型的例子。
// 清空它必须是用户显式发起的动作（`component remove --purge-data`），
// 不能由一次 remove 顺手做掉。
func (d *Directory) Remove(context.Context) error {
	err := os.Remove(d.args.Path)
	switch {
	case err == nil, os.IsNotExist(err), isNotEmptyErr(err):
		return nil
	default:
		return Transient("删除目录", err)
	}
}

func fileKind(fi fs.FileInfo) string {
	switch m := fi.Mode(); {
	case m&fs.ModeSymlink != 0:
		return "软链"
	case m&fs.ModeDevice != 0:
		return "设备文件"
	case m&fs.ModeNamedPipe != 0:
		return "命名管道"
	case m&fs.ModeSocket != 0:
		return "套接字"
	case m.IsDir():
		return "目录"
	default:
		return fmt.Sprintf("普通文件，%d 字节", fi.Size())
	}
}
