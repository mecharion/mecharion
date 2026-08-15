package resource

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mecharion/mecharion/internal/spec"
)

// TypeArchive 是 `archive` 资源的类型名。
const TypeArchive = "archive"

// archiveMarker 是解压完成标记文件的名字。
//
// 用标记文件而非「dest 非空」来判定幂等，是为了能检测出
// **blob 变了但路径没变** 的情况——那时目录非空，但内容是旧版本的。
const archiveMarker = ".mecharion-archive"

type archiveArgs struct {
	Blob string `json:"blob"`
	Dest string `json:"dest"`
	// Strip 是剥离的路径层数（tarball 通常带一层版本目录）。
	Strip int `json:"strip,omitempty"`
	// Exclude 是要跳过的 glob，匹配剥离后的路径。
	Exclude []string `json:"exclude,omitempty"`
}

// markerFile 记录一次解压的结果。
type markerFile struct {
	Blob      string `json:"blob"`
	SHA256    string `json:"sha256"`
	Strip     int    `json:"strip"`
	Entries   int    `json:"entries"`
	Extracted string `json:"extractedAt"`
}

// Archive 把一个归档载荷解开到目标目录。
type Archive struct {
	base
	env  *Env
	args archiveArgs
}

func newArchive(env *Env, r spec.Resource) (Resource, error) {
	var a archiveArgs
	if err := decodeArgs(r, &a); err != nil {
		return nil, err
	}
	if a.Blob == "" {
		return nil, badArg(r, "缺少 blob")
	}
	if err := requireAbs(r, "dest", a.Dest); err != nil {
		return nil, err
	}
	if a.Strip < 0 {
		return nil, badArg(r, "strip 不能为负")
	}
	for _, p := range a.Exclude {
		if _, err := path.Match(p, "x"); err != nil {
			return nil, badArg(r, fmt.Sprintf("exclude 中的 %q 不是合法的 glob: %v", p, err))
		}
	}
	return &Archive{base: base{id: r.ID, typ: r.Type}, env: env, args: a}, nil
}

func (a *Archive) markerPath() string { return filepath.Join(a.args.Dest, archiveMarker) }

// Read 读标记文件。
func (a *Archive) Read(context.Context) (Observed, error) {
	b, err := os.ReadFile(a.markerPath())
	switch {
	case isNotExist(err):
		return Observed{State: StateAbsent}, nil
	case err != nil:
		return unknown("读取 %s: %v", a.markerPath(), err), nil
	}

	var m markerFile
	if err := json.Unmarshal(b, &m); err != nil {
		// 标记文件坏了 → 当作没解压过，重新解一次。这比 Unknown 好：
		// Unknown 会让资源被跳过，而重解一次是安全且能自愈的。
		return Observed{State: StateAbsent}, nil
	}
	return present(map[string]any{
		"sha256":  m.SHA256,
		"blob":    m.Blob,
		"strip":   strconv.Itoa(m.Strip),
		"entries": strconv.Itoa(m.Entries),
	}), nil
}

// Diff 比较已解开的载荷摘要与 strip 层数。
func (a *Archive) Diff(o Observed) []Change {
	var b diffBuilder
	switch o.State {
	case StateUnknown:
		return nil
	case StateAbsent:
		b.absent()
		return b.changes
	}

	ref, err := a.env.Blob(a.args.Blob)
	if err == nil {
		b.scalar("blob", shortSum(ref.SHA256), shortSum(o.Field("sha256")))
	}
	b.scalar("strip", strconv.Itoa(a.args.Strip), o.Field("strip"))
	return b.changes
}

// Apply 解开归档并写入标记文件。
//
// 幂等性来自「标记文件最后写」：解到一半崩溃时标记文件不存在，下次
// Read 报 Absent 从头再解一遍。归档解压本身是覆盖式的，重解无副作用。
func (a *Archive) Apply(ctx context.Context) error {
	src, ref, err := a.env.BlobPath(a.args.Blob)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(a.args.Dest, 0o755); err != nil {
		return Transient("创建目标目录", err)
	}

	f, err := os.Open(src)
	if err != nil {
		return Transient("打开载荷", err)
	}
	defer f.Close()

	format, err := detectFormat(f)
	if err != nil {
		return err
	}

	var n int
	switch format {
	case formatZip:
		fi, serr := f.Stat()
		if serr != nil {
			return Transient("读取载荷大小", serr)
		}
		n, err = a.extractZip(ctx, f, fi.Size())
	default:
		var r io.Reader = f
		if format == formatTarGz {
			zr, gerr := gzip.NewReader(f)
			if gerr != nil {
				return Permanentf("解压载荷", "%s 不是合法的 gzip: %v", ref.Filename, gerr)
			}
			defer zr.Close()
			r = zr
		}
		n, err = a.extractTar(ctx, r)
	}
	if err != nil {
		return err
	}

	m, err := json.Marshal(markerFile{
		Blob: a.args.Blob, SHA256: ref.SHA256, Strip: a.args.Strip,
		Entries: n, Extracted: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return Permanent("写解压标记", err)
	}
	return writeAtomic(ctx, a.env, a.markerPath(), bytes.NewReader(append(m, '\n')),
		ownership{Mode: "0644"})
}

// Remove 是 no-op。
//
// archive 的目标通常是 generation 目录，而 generation 是不可变的、
// 由调和器按保留策略整体回收（ADR-0008）。让 archive 自己去删一堆
// 解出来的文件，既多余又有误删的风险。
func (a *Archive) Remove(context.Context) error { return nil }

// ── 解压 ────────────────────────────────────────────────────────────────

type archiveFormat int

const (
	formatTar archiveFormat = iota
	formatTarGz
	formatZip
)

// detectFormat 用魔数判定格式，读完把偏移复位。
//
// 按魔数而非文件名后缀：blob 的 filename 是 Pack 作者填的，不该让一个
// 写错的后缀导致解压失败。
func detectFormat(f *os.File) (archiveFormat, error) {
	var head [4]byte
	n, err := io.ReadFull(f, head[:])
	if err != nil && n < 2 {
		return 0, Permanentf("识别载荷格式", "载荷太小，无法识别格式")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, Transient("识别载荷格式", err)
	}
	switch {
	case head[0] == 0x1f && head[1] == 0x8b:
		return formatTarGz, nil
	case string(head[:4]) == "PK\x03\x04":
		return formatZip, nil
	default:
		return formatTar, nil
	}
}

func (a *Archive) extractTar(ctx context.Context, r io.Reader) (int, error) {
	tr := tar.NewReader(r)
	n := 0
	for {
		if err := ctx.Err(); err != nil {
			return n, Transient("解压", err)
		}
		h, err := tr.Next()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, Permanentf("解压", "读取归档条目: %v", err)
		}

		name, ok := a.rewrite(h.Name)
		if !ok {
			continue
		}
		dest, err := a.safeJoin(name)
		if err != nil {
			return n, err
		}

		mode := fs.FileMode(h.Mode).Perm()
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, dirModeFor(mode)); err != nil {
				return n, Transient("创建目录", err)
			}
		case tar.TypeReg:
			if err := a.writeEntry(dest, tr, mode); err != nil {
				return n, err
			}
		case tar.TypeSymlink:
			if err := a.writeSymlink(dest, h.Linkname); err != nil {
				return n, err
			}
		case tar.TypeLink:
			target, err := a.safeJoin(mustRewrite(a, h.Linkname))
			if err != nil {
				return n, err
			}
			_ = os.Remove(dest)
			if err := os.Link(target, dest); err != nil {
				return n, Transient("创建硬链", err)
			}
		default:
			// 设备文件、FIFO 等一律跳过——组件载荷里出现它们要么是
			// 打包失误，要么就该走 package 资源，不该由这里静默创建。
			continue
		}
		n++
	}
}

func (a *Archive) extractZip(ctx context.Context, r io.ReaderAt, size int64) (int, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return 0, Permanentf("解压", "不是合法的 zip: %v", err)
	}
	n := 0
	for _, e := range zr.File {
		if err := ctx.Err(); err != nil {
			return n, Transient("解压", err)
		}
		name, ok := a.rewrite(e.Name)
		if !ok {
			continue
		}
		dest, err := a.safeJoin(name)
		if err != nil {
			return n, err
		}

		mode := e.Mode().Perm()
		switch {
		case e.FileInfo().IsDir():
			if err := os.MkdirAll(dest, dirModeFor(mode)); err != nil {
				return n, Transient("创建目录", err)
			}
		case e.Mode()&fs.ModeSymlink != 0:
			rc, err := e.Open()
			if err != nil {
				return n, Transient("读取归档条目", err)
			}
			target, err := io.ReadAll(io.LimitReader(rc, 4096))
			rc.Close()
			if err != nil {
				return n, Transient("读取软链目标", err)
			}
			if err := a.writeSymlink(dest, string(target)); err != nil {
				return n, err
			}
		default:
			rc, err := e.Open()
			if err != nil {
				return n, Transient("读取归档条目", err)
			}
			err = a.writeEntry(dest, rc, mode)
			rc.Close()
			if err != nil {
				return n, err
			}
		}
		n++
	}
	return n, nil
}

// rewrite 应用 strip 与 exclude，返回归一化后的相对路径。
// ok 为 false 表示该条目应当跳过。
func (a *Archive) rewrite(name string) (string, bool) {
	name = strings.TrimPrefix(path.Clean(strings.ReplaceAll(name, `\`, "/")), "./")
	if name == "" || name == "." {
		return "", false
	}
	if a.args.Strip > 0 {
		parts := strings.Split(name, "/")
		if len(parts) <= a.args.Strip {
			// 被剥掉的那几层目录本身，跳过
			return "", false
		}
		name = strings.Join(parts[a.args.Strip:], "/")
	}
	if a.excluded(name) {
		return "", false
	}
	return name, true
}

// mustRewrite 供硬链目标使用——目标一定在归档内，不该被 exclude 掉。
func mustRewrite(a *Archive, name string) string {
	n, _ := a.rewrite(name)
	return n
}

// excluded 报告路径是否被 exclude 命中。目录被排除时其下全部内容一并排除。
func (a *Archive) excluded(name string) bool {
	for _, pat := range a.args.Exclude {
		for p := name; p != "." && p != "/" && p != ""; p = path.Dir(p) {
			if ok, _ := path.Match(pat, p); ok {
				return true
			}
		}
	}
	return false
}

// safeJoin 把归档内的相对路径接到 dest 下，并拒绝一切逃逸。
//
// 归档来自 Pack 作者，而 Pack 是可以由第三方提供的——一个含
// `../../etc/cron.d/x` 条目的 tarball 能直接拿到 root 执行权。
// 系统不做 Pack 来源校验（ADR-0040），这里是唯一的防线，不能依赖
// 别处再挡一层。
func (a *Archive) safeJoin(name string) (string, error) {
	rel := filepath.FromSlash(name)
	if !filepath.IsLocal(rel) {
		return "", Permanentf("解压",
			"归档中的条目 %q 会写到目标目录之外，已拒绝", name)
	}
	return filepath.Join(a.args.Dest, rel), nil
}

func (a *Archive) writeEntry(dest string, r io.Reader, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return Transient("创建目录", err)
	}
	// 覆盖已存在的旧文件：中断后重试时目录里可能有半个文件。
	// 直接 O_TRUNC 而非 rename，是因为归档整体的原子性由标记文件保证。
	if mode == 0 {
		mode = 0o644
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		// 已存在且权限不足以写入（比如上一版解出的 0444 文件）
		if os.IsPermission(err) {
			_ = os.Remove(dest)
			f, err = os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
		}
		if err != nil {
			return Transient("写入文件", err)
		}
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return Transient("写入文件", err)
	}
	if err := f.Close(); err != nil {
		return Transient("写入文件", err)
	}
	// O_CREATE 的 mode 受 umask 影响，显式设一次
	if err := os.Chmod(dest, mode); err != nil {
		return Transient("设置权限", err)
	}
	return nil
}

// writeSymlink 建立归档内的软链，并确保它指不出目标目录。
func (a *Archive) writeSymlink(dest, target string) error {
	if filepath.IsAbs(target) || strings.HasPrefix(target, "/") {
		return Permanentf("解压",
			"归档中的软链 %s 指向绝对路径 %s，已拒绝", dest, target)
	}
	// 相对目标要落在 dest 之内
	resolved := path.Join(path.Dir(filepath.ToSlash(dest)), filepath.ToSlash(target))
	root := filepath.ToSlash(a.args.Dest)
	if resolved != root && !strings.HasPrefix(resolved, root+"/") {
		return Permanentf("解压",
			"归档中的软链 %s → %s 指向目标目录之外，已拒绝", dest, target)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return Transient("创建目录", err)
	}
	_ = os.Remove(dest)
	if err := os.Symlink(target, dest); err != nil {
		return Transient("创建软链", err)
	}
	return nil
}
