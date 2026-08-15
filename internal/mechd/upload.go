package mechd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mecharion/mecharion/internal/faults"
	"github.com/mecharion/mecharion/internal/pack"
)

// UploadResult 是一次 Pack 上传的结果。
type UploadResult struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Revision int    `json:"revision"`
	// Digest 是上传文件本身的 sha256，进审计。
	Digest   string   `json:"digest"`
	Size     int64    `json:"size"`
	Warnings []string `json:"warnings,omitempty"`
	// Replaced 表示同名同版本的 Pack 已经存在，这次是覆盖。
	Replaced bool `json:"replaced,omitempty"`
}

// UploadPack 收一个 .mpack 进本地 Pack 集合。
//
// **这是这个系统里唯一一处新增的供应链入口**（23-web-ui §2.7）：
// 其余写操作都只是既有 API 的另一个入口，而这里让一份新的可执行内容
// 从浏览器进到 mechd。
//
// 顺序是硬要求：
//
//	① 收进**临时目录**，不是 Pack 集合
//	② 解包，逐条查路径（绝对路径 / .. / 链接一律拒）
//	③ 跑一遍完整 lint
//	④ 全过了才原子移进 Pack 集合
//
// 第 ① 条不能省。解到 Pack 集合里再查，那一瞬间放置阶段就可能看见它——
// 而「半个 Pack」这个状态一旦存在，就会有人在它存在的那几毫秒里部署。
func (s *Service) UploadPack(
	ctx context.Context, r io.Reader, packDir, actor string,
) (*UploadResult, error) {
	if packDir == "" {
		return nil, faults.Permanentf("uploading Pack", "no Pack collection directory configured")
	}
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		return nil, err
	}

	// 临时目录与 Pack 集合**同一个文件系统**：最后那一步要能原子改名。
	// 放 os.TempDir() 的话跨文件系统改名会退化成拷贝，而拷贝到一半被
	// 中断就又造出了「半个 Pack」。
	staging, err := os.MkdirTemp(packDir, ".upload-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)

	// ── ① 落到临时目录，同时算整份文件的摘要 ──
	sum := sha256.New()
	blob := filepath.Join(staging, "upload"+pack.MpackExt)
	f, err := os.Create(blob)
	if err != nil {
		return nil, err
	}
	size, err := io.Copy(f, io.TeeReader(r, sum))
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("receiving upload: %w", err)
	}
	if size == 0 {
		return nil, faults.Permanentf("uploading Pack", "file is empty")
	}
	digest := hex.EncodeToString(sum.Sum(nil))

	// ── ② 解包。路径校验在 ExtractMpack 里，对每一条都重判一遍 ──
	extracted := filepath.Join(staging, "tree")
	if err := os.MkdirAll(extracted, 0o755); err != nil {
		return nil, err
	}
	in, err := os.Open(blob)
	if err != nil {
		return nil, err
	}
	err = pack.ExtractMpack(in, extracted)
	in.Close()
	if err != nil {
		return nil, faults.Permanentf("uploading Pack", "%v", err)
	}

	// ── ③ lint。与 mechpack lint 同一份实现 ──
	p, err := pack.Load(extracted)
	if err != nil {
		return nil, faults.Permanentf("uploading Pack", "reading pack.yaml: %v", err)
	}
	// **hermetic 开着**：离线约束是这个项目的底线，而上传是外来内容
	// 进来的唯一入口——正是最该查它的地方（ADR-0015）。
	//
	// Resolver 给本地 Pack 集合：一个依赖别的 Pack 的包，要能核对那个
	// 依赖在不在本地。不给的话相关检查整个跳过（见 pack.Options 注释）。
	res := pack.Lint(p, pack.Options{Hermetic: true, Resolver: s.Packs})
	if len(res.Errors()) > 0 {
		return nil, faults.Permanentf("uploading Pack",
			"%s failed validation:\n%s", p.Name, lintText(res))
	}

	out := &UploadResult{
		Name: p.Name, Version: p.Version, Revision: p.Revision,
		Digest: digest, Size: size,
	}
	for _, f := range res.Warnings() {
		out.Warnings = append(out.Warnings, f.Message)
	}

	// ── ④ 原子移进 Pack 集合 ──
	//
	// 目录名与 mechpack assemble 的产出一致，好让「本地这份是哪来的」
	// 一眼看得出来。
	final := filepath.Join(packDir,
		fmt.Sprintf("%s-%s-%d", p.Name, p.Version, p.Revision))
	if _, err := os.Stat(final); err == nil {
		out.Replaced = true
		// **先挪走再放新的**：直接往上盖会在两者之间留一个混合状态，
		// 而那时的目录里一半是旧版一半是新版
		old := final + ".replacing"
		os.RemoveAll(old)
		if err := os.Rename(final, old); err != nil {
			return nil, err
		}
		defer os.RemoveAll(old)
	}
	if err := os.Rename(extracted, final); err != nil {
		return nil, err
	}

	// ── ⑤ 载荷入库（thick → thin，03-pack §2 的 `mechpack push`）──
	//
	// **少了这一步，上传出来的包能部署却永远不收敛。**
	//
	// thick pack 里带着 `blobs/sha256-<hex>`，而节点是按 sha256 向
	// mechd 的载荷库要的（Backend.OpenBlob 读 <BlobDir>/sha256/<xx>/<hex>）。
	// 两处的布局不同，不搬过去的话 agent 拿不到载荷——症状是「装上了、
	// 一直不收敛」，而 deploy 那一步是成功的，没有任何地方报错。
	//
	// 这个缺陷是拿真集群跑出来的：单元测试里那个最小 Pack 没有载荷。
	if n, err := s.importBlobs(final); err != nil {
		return nil, fmt.Errorf("importing blobs: %w", err)
	} else if n > 0 {
		s.log().Info("blobs imported", "pack", p.Name, "blobs", n)
	}

	// 让它立刻可用，不必重启 mechd
	if err := s.Packs.Reload(); err != nil {
		s.log().Warn("failed to rescan Pack collection", "err", err)
	}

	s.audit(ctx, actor, "pack.upload", p.Name, p, "ok")
	if site, err := s.resolveSite(ctx, ""); err == nil {
		s.event(ctx, site.ID, "pack.uploaded", p.Name, map[string]any{
			"version": p.Version, "revision": p.Revision,
			"digest": digest, "size": size,
		})
	}
	return out, nil
}

// lintText 把校验结果排成人能读的样子。
func lintText(res *pack.Result) string {
	var b []byte
	for _, f := range res.Errors() {
		b = append(b, "  "...)
		if f.Rule != "" {
			b = append(b, "["+f.Rule+"] "...)
		}
		b = append(b, f.Path...)
		b = append(b, ": "...)
		b = append(b, f.Message...)
		if f.Hint != "" {
			b = append(b, "\n    hint: "+f.Hint...)
		}
		b = append(b, '\n')
	}
	return string(b)
}

// importBlobs 把一个 thick pack 里的载荷搬进内容寻址的载荷库。
//
// 名字里的 sha256 **要重新算一遍核对**：文件名是归档自己声称的，而这是
// 供应链入口。名不副实的载荷进了库，之后每一次按摘要取都会拿到错的东西
// ——而那时已经查不到是上传这一步引入的。
func (s *Service) importBlobs(packDir string) (int, error) {
	entries, err := os.ReadDir(filepath.Join(packDir, "blobs"))
	if os.IsNotExist(err) {
		return 0, nil // thin pack：载荷本来就不在包里
	}
	if err != nil {
		return 0, err
	}

	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		want, ok := strings.CutPrefix(e.Name(), "sha256-")
		if !ok {
			continue // 不是内容寻址命名的，跳过
		}
		src := filepath.Join(packDir, "blobs", e.Name())

		got, err := fileSum(src)
		if err != nil {
			return n, err
		}
		if got != want {
			return n, faults.Permanentf("uploading Pack",
				"blob %s's actual digest is %s -- a mislabeled blob cannot be imported", e.Name(), got)
		}

		dst := filepath.Join(s.BlobDir, "sha256", want[:2], want)
		if _, err := os.Stat(dst); err == nil {
			continue // 已经有了：内容寻址天然去重
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return n, err
		}
		// 先写 .part 再改名：半个载荷与一个完整的在文件系统上长得一样，
		// 而按摘要取的时候不会再校验一遍
		if err := copyFile(src, dst+".part"); err != nil {
			return n, err
		}
		if err := os.Rename(dst+".part", dst); err != nil {
			os.Remove(dst + ".part")
			return n, err
		}
		n++
	}
	return n, nil
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
