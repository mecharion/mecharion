package resource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// ownership 是三个可选的属主/权限字段，被多种资源类型共用。
//
// **空串表示「Pack 未声明」**，未声明的字段既不参与 Diff 也不在 Apply 中
// 被强制——否则每个没写 owner 的资源都会报一次漂移。
type ownership struct {
	Mode  string `json:"mode,omitempty"`
	Owner string `json:"owner,omitempty"`
	Group string `json:"group,omitempty"`
}

// parseMode 把 "0750" 解析为 fs.FileMode。空串返回 (0, false, nil)。
func (o ownership) parseMode() (fs.FileMode, bool, error) {
	if o.Mode == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseUint(o.Mode, 8, 32)
	if err != nil {
		return 0, false, Permanentf("解析 mode", "mode %q 不是合法的八进制权限位", o.Mode)
	}
	return fs.FileMode(n) & fs.ModePerm, true, nil
}

// validate 在构造资源时检查 mode 的写法。
func (o ownership) validate() error {
	_, _, err := o.parseMode()
	return err
}

// readInto 把实际的 mode/owner/group 填进观测字段。
//
// 属主反查只在 Pack 真的声明了 owner/group 时才做——否则每读一个文件
// 都要 fork 一次 getent，而绝大多数资源根本不关心属主。
func (o ownership) readInto(ctx context.Context, env *Env, fields map[string]any, fi fs.FileInfo) {
	if modeSupported {
		fields["mode"] = fmt.Sprintf("%04o", fi.Mode().Perm())
	}
	uid, gid, ok := fileOwner(fi)
	if !ok {
		return
	}
	fields["uid"] = strconv.Itoa(uid)
	fields["gid"] = strconv.Itoa(gid)
	if o.Owner != "" {
		fields["owner"] = env.NameForID(ctx, "passwd", uid)
	}
	if o.Group != "" {
		fields["group"] = env.NameForID(ctx, "group", gid)
	}
}

// diffInto 比较三个字段。
//
// owner/group 按**名字**比对，而观测侧记录的是「uid 反查出的名字」。
// 反查不到时记录数字，此时声明了名字必然报差异——那正是想要的：
// 属主的 uid 在系统里已经没有对应的用户了，值得让人知道。
func (o ownership) diffInto(d *diffBuilder, obs Observed) {
	if o.Mode != "" && modeSupported {
		// 归一化后比较："750" 与 "0750" 是同一件事
		if m, ok, err := o.parseMode(); err == nil && ok {
			d.scalar("mode", fmt.Sprintf("%04o", m), obs.Field("mode"))
		}
	}
	d.scalar("owner", o.Owner, obs.Field("owner"))
	d.scalar("group", o.Group, obs.Field("group"))
}

// ApplyOwnership 收敛一个已存在路径的 mode 与属主。
//
// 供调和器的阶段① 使用：paths 声明的目录由引擎自动创建（spec §8.3），
// 那条路径不经过任何 Resource，但属主与权限的处理必须和 directory 资源
// 完全一致——否则同一份声明在两个地方会得到两种结果。
func ApplyOwnership(ctx context.Context, env *Env, path, mode, owner, group string) error {
	return ownership{Mode: mode, Owner: owner, Group: group}.apply(ctx, env, path)
}

// apply 收敛一个已存在路径的 mode 与属主。
func (o ownership) apply(ctx context.Context, env *Env, path string) error {
	if m, ok, err := o.parseMode(); err != nil {
		return err
	} else if ok {
		if err := os.Chmod(path, m); err != nil {
			return Transient("设置权限", err)
		}
	}
	return o.applyOwner(ctx, env, path)
}

// applyOwner 只收敛属主，不动 mode（symlink 用得上）。
func (o ownership) applyOwner(ctx context.Context, env *Env, path string) error {
	if o.Owner == "" && o.Group == "" {
		return nil
	}
	uid, gid := -1, -1
	if o.Owner != "" {
		id, err := env.LookupUID(ctx, o.Owner)
		if err != nil {
			return err
		}
		uid = id
	}
	if o.Group != "" {
		id, err := env.LookupGID(ctx, o.Group)
		if err != nil {
			return err
		}
		gid = id
	}
	if err := chown(path, uid, gid); err != nil {
		return Transient("设置属主", err)
	}
	return nil
}

// applyRecursive 对整棵子树收敛属主与权限。
//
// 目录与文件的 mode 不能一概而论——0640 的目录进不去。因此递归时
// 只对目录补上对应的执行位。
func (o ownership) applyRecursive(ctx context.Context, env *Env, root string) error {
	m, hasMode, err := o.parseMode()
	if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return Transient("遍历目录", err)
		}
		if hasMode {
			want := m
			if de.IsDir() {
				want = dirModeFor(m)
			}
			if err := os.Chmod(p, want); err != nil {
				return Transient("设置权限", err)
			}
		}
		return o.applyOwner(ctx, env, p)
	})
}

// dirModeFor 由文件权限推出目录权限：有读权限的位补上执行位。
func dirModeFor(m fs.FileMode) fs.FileMode {
	for _, shift := range []uint{6, 3, 0} {
		if m&(0o4<<shift) != 0 {
			m |= 0o1 << shift
		}
	}
	return m
}

// ── 内容 ────────────────────────────────────────────────────────────────

// hashFile 计算文件的 sha256。
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// hashBytes 计算内存内容的 sha256。
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeAtomic 把 r 的内容原子地写到 path。
//
// 顺序是「写临时文件 → Sync → chmod/chown → rename」。**属主与权限必须在
// rename 之前设好**，否则文件会有一小段时间以 root:root 0600 或者更糟的
// 0644 存在——配置文件里可能有密码。
func writeAtomic(ctx context.Context, env *Env, path string, r io.Reader, o ownership) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Transient("创建父目录", err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return Transient("创建临时文件", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 成功路径上 rename 已让它消失，这里的 NotExist 无害

	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return Transient("写入临时文件", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return Transient("刷盘", err)
	}
	if err := tmp.Close(); err != nil {
		return Transient("关闭临时文件", err)
	}

	// CreateTemp 建出来是 0600；未声明 mode 时给 0644，而不是留着 0600
	// ——大多数配置文件需要被服务进程读取。
	m, ok, err := o.parseMode()
	if err != nil {
		return err
	}
	if !ok {
		m = 0o644
	}
	if err := os.Chmod(tmpName, m); err != nil {
		return Transient("设置权限", err)
	}
	if err := o.applyOwner(ctx, env, tmpName); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return Transient("替换目标文件", err)
	}
	return nil
}

// isNotExist 把「路径不存在」与「父目录不是目录」都算作不存在。
func isNotExist(err error) bool {
	return os.IsNotExist(err) || isNotDirErr(err)
}
