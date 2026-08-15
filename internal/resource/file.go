package resource

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"

	"github.com/mecharion/mecharion/internal/spec"
)

// `file` 与 `template` 的类型名。
//
// 在 mechlet 这一侧两者是同一件事：mechd 已经把模板渲染成了 content
// （12-spec-and-state §1.4「Resources —— 完全渲染」）。保留两个类型名
// 是为了让 status / diff 的输出仍能说清这份文件是静态的还是渲染出来的。
const (
	TypeFile     = "file"
	TypeTemplate = "template"
)

// textDiffLimit 是「读全文供行级 diff」的大小上限。
//
// 一个 200MB 的载荷不该被读进内存做字符串比较，因此内容比对一律用
// sha256；只有在摘要不同、且两侧都小于此阈值时才取全文。
const textDiffLimit = 1 << 20

type fileArgs struct {
	// file 用 path，template 用 dest。两者语义相同，构造时归一。
	Path string `json:"path,omitempty"`
	Dest string `json:"dest,omitempty"`

	// 内容的三个互斥来源（规范 §14.2）。
	Content *string `json:"content,omitempty"`
	Source  string  `json:"source,omitempty"`
	Blob    string  `json:"blob,omitempty"`

	// Src 只用于给出更好的错误信息——它出现在已解析规格里就是个 bug。
	Src string `json:"src,omitempty"`

	ownership
}

// File 保证一个文件存在且内容、属主、权限符合声明。
type File struct {
	base
	env  *Env
	args fileArgs
	path string

	// 期望内容的摘要按需计算一次。Diff 不接收 ctx 也不该做 IO，
	// 因此把这份计算放在这里，Read 与 Diff 共用。
	once     sync.Once
	wantSum  string
	wantSize int64
	wantErr  error

	// wantText/gotText 只在摘要不同且内容够小时填充，供 CLI 做行级 diff。
	wantText string
}

func newFile(env *Env, r spec.Resource) (Resource, error) {
	var a fileArgs
	if err := decodeArgs(r, &a); err != nil {
		return nil, err
	}

	path := a.Path
	switch {
	case a.Path != "" && a.Dest != "":
		return nil, badArg(r, "path 与 dest 只能有一个")
	case a.Dest != "":
		path = a.Dest
	}
	if err := requireAbs(r, "path", path); err != nil {
		return nil, err
	}

	if a.Src != "" {
		return nil, badArg(r, "已解析规格中不应出现 src——"+
			"template 的渲染由 mechd 完成，下发的应当是 content")
	}
	n := 0
	for _, set := range []bool{a.Content != nil, a.Source != "", a.Blob != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		return nil, badArg(r, fmt.Sprintf(
			"content / source / blob 三者必须恰好声明一个，实际声明了 %d 个", n))
	}
	if err := a.validate(); err != nil {
		return nil, err
	}

	return &File{base: base{id: r.ID, typ: r.Type}, env: env, args: a, path: path}, nil
}

// desired 打开期望内容。调用方负责关闭。
func (f *File) desired() (io.ReadCloser, error) {
	switch {
	case f.args.Content != nil:
		return io.NopCloser(bytes.NewReader([]byte(*f.args.Content))), nil
	case f.args.Source != "":
		p, err := f.env.SourcePath(f.args.Source)
		if err != nil {
			return nil, err
		}
		fh, err := os.Open(p)
		if err != nil {
			return nil, Permanentf("读取 source", "打开 %s: %v", p, err)
		}
		return fh, nil
	default:
		p, _, err := f.env.BlobPath(f.args.Blob)
		if err != nil {
			return nil, err
		}
		fh, err := os.Open(p)
		if err != nil {
			return nil, Transient("读取 blob", err)
		}
		return fh, nil
	}
}

// wantDigest 返回期望内容的 sha256 与大小，只计算一次。
func (f *File) wantDigest() (string, int64, error) {
	f.once.Do(func() {
		// blob 的摘要在规格里就有，不必再算一遍——它可能有几百 MB。
		if f.args.Blob != "" {
			if b, err := f.env.Blob(f.args.Blob); err == nil && b.SHA256 != "" {
				f.wantSum, f.wantSize = b.SHA256, b.Size
				return
			}
		}
		if f.args.Content != nil {
			c := []byte(*f.args.Content)
			f.wantSum, f.wantSize = hashBytes(c), int64(len(c))
			return
		}
		rc, err := f.desired()
		if err != nil {
			f.wantErr = err
			return
		}
		defer rc.Close()
		if fh, ok := rc.(*os.File); ok {
			f.wantSum, f.wantSize, err = hashFile(fh.Name())
			if err != nil {
				f.wantErr = Transient("计算期望内容摘要", err)
			}
			return
		}
		b, err := io.ReadAll(rc)
		if err != nil {
			f.wantErr = Transient("读取期望内容", err)
			return
		}
		f.wantSum, f.wantSize = hashBytes(b), int64(len(b))
	})
	return f.wantSum, f.wantSize, f.wantErr
}

// Read 探测文件。
func (f *File) Read(ctx context.Context) (Observed, error) {
	wantSum, wantSize, err := f.wantDigest()
	if err != nil {
		return Observed{}, err
	}

	fi, err := os.Lstat(f.path)
	switch {
	case isNotExist(err):
		return Observed{State: StateAbsent}, nil
	case err != nil:
		return unknown("lstat %s: %v", f.path, err), nil
	}
	if fi.IsDir() {
		return Observed{}, Permanentf("读取文件",
			"%s 已存在但是一个目录", f.path)
	}

	gotSum, gotSize, err := hashFileCached(f.env, f.path)
	if err != nil {
		// 读得到 stat 却读不出内容——权限、IO 错误、NFS 挂死。
		// 这属于「本该能读但这次读不到」，是 Unknown 而非 Absent。
		return unknown("读取 %s 的内容: %v", f.path, err), nil
	}

	fields := map[string]any{
		"sha256": gotSum,
		"size":   strconv.FormatInt(gotSize, 10),
	}
	f.args.readInto(ctx, f.env, fields, fi)

	// 仅在确有差异、且两侧都够小的时候才取全文
	if gotSum != wantSum && gotSize <= textDiffLimit && wantSize <= textDiffLimit {
		if b, err := os.ReadFile(f.path); err == nil {
			fields["content"] = string(b)
		}
		if rc, err := f.desired(); err == nil {
			b, err := io.ReadAll(io.LimitReader(rc, textDiffLimit))
			rc.Close()
			if err == nil {
				f.wantText = string(b)
			}
		}
	}
	return present(fields), nil
}

// Diff 比较内容与属主、权限。
func (f *File) Diff(o Observed) []Change {
	var b diffBuilder
	switch o.State {
	case StateUnknown:
		return nil
	case StateAbsent:
		b.absent()
		return b.changes
	}

	wantSum, _, err := f.wantDigest()
	if err == nil && wantSum != o.Field("sha256") {
		want, got := f.wantText, o.Field("content")
		if want == "" && got == "" {
			// 超过阈值，只报摘要——CLI 会照原样展示这一行
			want = "sha256:" + shortSum(wantSum)
			got = "sha256:" + shortSum(o.Field("sha256"))
			b.scalar("content", want, got)
		} else {
			b.text("content", want, got)
		}
	}
	f.args.diffInto(&b, o)
	return b.changes
}

// Apply 写入文件。
//
// 内容已经一致时**不重写**，只收敛 mode 与属主。重写虽然结果相同，但会
// 改动 mtime——那会惊动 inotify 类的自动重载，也会让「这份配置上次是什么
// 时候变的」这个排障问题失去答案。比对一次摘要比整份重写便宜得多。
func (f *File) Apply(ctx context.Context) error {
	wantSum, _, err := f.wantDigest()
	if err != nil {
		return err
	}
	if gotSum, _, herr := hashFileCached(f.env, f.path); herr == nil && gotSum == wantSum {
		return f.args.apply(ctx, f.env, f.path)
	}

	rc, err := f.desired()
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := writeAtomic(ctx, f.env, f.path, rc, f.args.ownership); err != nil {
		return err
	}
	// 刚写完，内容是什么我们最清楚——顺手登记，省掉下一轮的一次全量哈希
	recordDigest(f.env, f.path, wantSum)
	return nil
}

// Remove 删除文件。
func (f *File) Remove(context.Context) error {
	if err := os.Remove(f.path); err != nil && !isNotExist(err) {
		return Transient("删除文件", err)
	}
	return nil
}
