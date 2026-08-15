package pack

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// `.mpack` —— 离线交付的单文件形态（spec §3）。
//
//	<name>-<version>-<revision>.mpack     # tar + zstd
//
// 归档内是一个组装好的 Pack 目录。**thick pack** 的 `blobs/` 是完整的，
// **thin pack** 的为空（blob 由 mechd 的载荷库按 sha256 解析）。
//
// 规范对归档有四条可复现要求，它们不是洁癖，各自挡一件事：
//
//	条目按路径排序        同一份内容打两次要得到同一个字节流，否则**摘要对不上**
//	时间戳 / uid/gid 归零  同上；顺带不把打包机器的用户信息带进交付物
//	不含符号链接          解包时软链可以指向归档之外——最经典的一类解包逃逸
//	不含绝对路径与 ..     同上，且更直接
//
// 后两条在**解包侧也要再查一遍**：打包时校验只能保证「我们打的包是干净
// 的」，而上传上来的包是别人打的（23-web-ui §2.7）。

// MpackExt 是文件后缀。
const MpackExt = ".mpack"

// mpackMaxEntry 是单个归档条目的大小上限。
//
// 解压炸弹的第一道：一个几 KB 的 zstd 流可以解出上百 GB。这里的上限
// 按「一个 JDK 或数据库发行包」的量级取，宁可让一个超大的合法包被拒
// （那时会有明确的错误），也不让磁盘被一个上传请求填满。
const mpackMaxEntry = 8 << 30 // 8 GiB

// MpackName 返回一个 Pack 的标准文件名。
func MpackName(p *Pack) string {
	return fmt.Sprintf("%s-%s-%d%s", p.Name, p.Version, p.Revision, MpackExt)
}

// WriteMpack 把一个已组装的 Pack 目录打成 .mpack。
//
// **可复现**：同样的目录内容打两次得到同样的字节流。这是摘要能当身份用
// 的前提——而摘要是这个系统里一切信任的锚。
func WriteMpack(dir string, w io.Writer) error {
	files, err := collectFiles(dir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("%s contains no files to package", dir)
	}

	zw, err := zstd.NewWriter(w)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(zw)

	for _, rel := range files {
		full := filepath.Join(dir, rel)
		info, err := os.Lstat(full)
		if err != nil {
			return err
		}
		// 符号链接在**打包时**就拒掉，不是解包时才发现
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink, .mpack does not accept symlinks", rel)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", rel)
		}

		// 时间戳与 uid/gid 全部归零；权限只保留「可执行与否」这一位。
		//
		// 保留完整 mode 会让同一份内容在 umask 不同的机器上打出不同的包，
		// 而那正是可复现要挡的东西。
		mode := int64(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: rel, Mode: mode, Size: info.Size(),
			Typeflag: tar.TypeReg, Format: tar.FormatPAX,
			// **显式写 epoch 0**，不靠 time.Time{} 被 tar 层截断成 0。
			// 零值是公元 1 年，不同 tar 格式对它的处理不一样——依赖那个
			// 截断行为，可复现就成了实现细节的副作用。
			ModTime: time.Unix(0, 0),
		}); err != nil {
			return err
		}
		f, err := os.Open(full)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		f.Close()
		if err != nil {
			return err
		}
	}

	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// collectFiles 列出目录里全部文件的相对路径，**按路径排序**。
func collectFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		// tar 里一律用正斜杠：Windows 上打的包要能在 Linux 上解开
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// ExtractMpack 把一个 .mpack 解到 dir。
//
// **它假定输入是不可信的。** 上传是这个系统里唯一一处新增的供应链入口
// （23-web-ui §2.7），因此这里对每一条路径都要重新判一遍——打包侧的
// 校验保证不了别人打的包。
func ExtractMpack(r io.Reader, dir string) error {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return fmt.Errorf("not a valid zstd stream: %w", err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			// **先判「断了」，再判「不是这个格式」。** 两者的下一步动作
			// 完全不同：前者重传一次，后者换个文件。
			//
			// 顺序反过来的话，一个传到一半的合法包会被报成「这不像一个
			// .mpack」——用户会去检查文件对不对，而它本来就是对的。
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				return fmt.Errorf(
					"archive is incomplete (broke off after reading %d entries) -- the file was likely truncated, "+
						"please upload it again: %w", n, err)
			}
			// zstd.NewReader 不会立刻失败（它是惰性的），于是「传了个
			// tar.gz 上来」的症状是一句 "magic number mismatch"——
			// 那句话指向 tar 层，而问题在外面那层。
			if n == 0 {
				return fmt.Errorf(
					"this doesn't look like a .mpack (tar + zstd): %w\n"+
						"  → generate it with mechpack bundle, or check the file wasn't compressed twice", err)
			}
			return fmt.Errorf("reading archive: %w", err)
		}
		if err := checkEntry(h); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeDir {
			continue // 目录由文件路径隐含创建
		}

		target := filepath.Join(dir, filepath.FromSlash(h.Name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if h.Mode&0o111 != 0 {
			mode = 0o755
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		// **限长拷贝**：h.Size 是归档自己声称的，不能信它来预分配，
		// 但可以用它之外的一个硬上限挡住解压炸弹
		_, err = io.Copy(f, io.LimitReader(tr, mpackMaxEntry+1))
		f.Close()
		if err != nil {
			// **条目内容读到一半断了，与条目头读不出来是同一件事**：
			// 文件被截断。大一点的包总是在这条路径上断，而不是在
			// tr.Next() 上——第一版只包了后者，于是真集群上传一个截断的
			// 4.6 MB 包，回的还是一句裸的 "unexpected EOF"。
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				return fmt.Errorf(
					"archive is incomplete (broke off while reading %q) -- the file was likely truncated, "+
						"please upload it again: %w", h.Name, err)
			}
			return fmt.Errorf("extracting %q: %w", h.Name, err)
		}
		n++
	}
	if n == 0 {
		return fmt.Errorf("archive contains no files")
	}
	return nil
}

// checkEntry 判一条归档条目能不能收。
//
// 四条拒绝理由分开说，因为**现场最先要知道的是「哪一条」**：
// 「包有问题」说不出该去看什么。
func checkEntry(h *tar.Header) error {
	switch h.Typeflag {
	case tar.TypeReg, tar.TypeDir:
	case tar.TypeSymlink, tar.TypeLink:
		return fmt.Errorf(
			"archive entry %q is a %s -- .mpack does not accept them: "+
				"links can point outside the archive, the classic extraction escape",
			h.Name, linkKind(h.Typeflag))
	default:
		return fmt.Errorf("archive entry %q has type %d, which is not accepted (only regular files and directories)",
			h.Name, h.Typeflag)
	}

	name := path.Clean(filepath.ToSlash(h.Name))
	switch {
	case path.IsAbs(name) || strings.HasPrefix(h.Name, "/"):
		return fmt.Errorf("archive entry %q is an absolute path", h.Name)
	case name == ".." || strings.HasPrefix(name, "../"):
		return fmt.Errorf("archive entry %q escapes the archive root (contains ..)", h.Name)
	// Windows 上打的包可能带盘符；它在 Linux 上会被当成普通文件名，
	// 但反过来解到 Windows 上就是一次逃逸
	case len(name) > 1 && name[1] == ':':
		return fmt.Errorf("archive entry %q has a drive letter", h.Name)
	}
	if h.Size > mpackMaxEntry {
		return fmt.Errorf("archive entry %q claims %d bytes, exceeding the limit", h.Name, h.Size)
	}
	return nil
}

func linkKind(t byte) string {
	if t == tar.TypeSymlink {
		return "symlink"
	}
	return "hard link"
}
