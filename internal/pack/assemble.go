package pack

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// AssembleOptions 控制 assemble 的行为。
type AssembleOptions struct {
	// SourceRoot 是 sources 中相对路径的基准目录；空时取 Pack 目录。
	// 构建产物常在仓库的 dist/ 而非 Pack 目录内，因此需要它。
	SourceRoot string
	// Out 是输出目录；空时取 dist/<name>-<version>-<revision>。
	Out string
	// Force 允许覆盖已存在的输出目录。
	Force bool
	// SkipLint 跳过产物校验（仅用于排障，正常流程不应使用）。
	SkipLint bool
}

// AssembleResult 是一次组装的结果。
type AssembleResult struct {
	Out       string     `json:"out"       yaml:"out"`
	Name      string     `json:"name"      yaml:"name"`
	Version   string     `json:"version"   yaml:"version"`
	Revision  int        `json:"revision"  yaml:"revision"`
	Platforms []string   `json:"platforms" yaml:"platforms"`
	Blobs     []BlobStat `json:"blobs"     yaml:"blobs"`
	TotalSize int64      `json:"totalSize" yaml:"totalSize"`
	// Lint 是对产物的校验结果。
	Lint *Result `json:"-" yaml:"-"`
}

// BlobStat 是一个已组装 blob 的统计。
type BlobStat struct {
	Name     string `json:"name"     yaml:"name"`
	Platform string `json:"platform" yaml:"platform"`
	SHA256   string `json:"sha256"   yaml:"sha256"`
	Size     int64  `json:"size"     yaml:"size"`
	Filename string `json:"filename" yaml:"filename"`
	// Reused 表示该 blob 与另一平台/条目内容相同，已去重。
	Reused bool `json:"reused" yaml:"reused"`
}

// BlobFileName 返回 blob 在 blobs/ 目录中的文件名。
func BlobFileName(sha string) string { return "sha256-" + sha }

// Assemble 把源 Pack 目录组装成可发布的 Pack 目录。
//
// 它做四件事，且**只做这四件**：
//  1. 按 sources 计算每个载荷的 sha256 / size / filename
//  2. 未声明 platforms 时从各 blob 的平台键推导（不一致则报错）
//  3. 把 sources 段换成 blobs 段写进产物的 pack.yaml，其余内容原样保留
//  4. 拷贝 templates/ files/ hooks/ 与载荷本体
//
// 它**不构建你的软件**——构建由开发者自己的工具链完成（ADR-0015）。
func Assemble(dir string, opts AssembleOptions) (*AssembleResult, error) {
	p, err := Load(dir)
	if err != nil {
		return nil, err
	}

	srcRoot := opts.SourceRoot
	if srcRoot == "" {
		srcRoot = dir
	}

	sources, err := ParseSources(p.Doc)
	if err != nil {
		return nil, err
	}

	blobs, stats, err := hashSources(sources, srcRoot)
	if err != nil {
		return nil, err
	}
	// 源里已写死的 blobs 与算出来的合并；算出来的优先
	merged := map[string]Blob{}
	for bn, b := range p.Blobs {
		cp := Blob{}
		for k, v := range b {
			cp[k] = v
		}
		merged[bn] = cp
	}
	for bn, b := range blobs {
		if merged[bn] == nil {
			merged[bn] = Blob{}
		}
		for plat, e := range b {
			merged[bn][plat] = e
		}
	}

	platforms, err := resolvePlatforms(p.Platforms, merged)
	if err != nil {
		return nil, err
	}

	out := opts.Out
	if out == "" {
		out = filepath.Join("dist", fmt.Sprintf("%s-%s-%d", p.Name, p.Version, p.Revision))
	}
	if err := prepareOut(out, opts.Force); err != nil {
		return nil, err
	}

	// ① 写 pack.yaml：节点树外科手术，保留注释与字段顺序
	if err := writeManifest(p, out, merged, platforms); err != nil {
		return nil, err
	}

	// ② 拷贝逻辑目录
	for _, sub := range []string{DirTemplates, DirFiles, DirHooks} {
		if err := copyTree(filepath.Join(dir, sub), filepath.Join(out, sub)); err != nil {
			return nil, err
		}
	}

	// ③ 拷贝载荷，按内容寻址命名
	total, err := copyBlobs(sources, srcRoot, merged, dir, out, stats)
	if err != nil {
		return nil, err
	}

	res := &AssembleResult{
		Out: out, Name: p.Name, Version: p.Version, Revision: p.Revision,
		Platforms: platforms, Blobs: stats, TotalSize: total,
	}

	// ④ 校验产物——assemble 出一个过不了 lint 的 Pack 是没有意义的
	if !opts.SkipLint {
		outPack, err := Load(out)
		if err != nil {
			return res, fmt.Errorf("artifact could not be parsed: %w", err)
		}
		res.Lint = Lint(outPack, Options{Hermetic: true})
	}
	return res, nil
}

// hashSources 计算每个来源文件的摘要。
func hashSources(sources SourceSet, root string) (map[string]Blob, []BlobStat, error) {
	blobs := map[string]Blob{}
	var stats []BlobStat
	seen := map[string]bool{}

	for _, bn := range sortedKeys(sources) {
		platforms := sources[bn]
		blobs[bn] = Blob{}
		for _, plat := range sortedKeys(platforms) {
			s := platforms[plat]
			abs := s.Path
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(root, filepath.FromSlash(s.Path))
			}
			sum, size, err := hashFile(abs)
			if err != nil {
				return nil, nil, fmt.Errorf("sources.%s.%s: %w", bn, plat, err)
			}
			name := s.Filename
			if name == "" {
				name = filepath.Base(abs)
			}
			blobs[bn][plat] = BlobEntry{
				SHA256: sum, Size: size, Filename: name,
				SourceURL: s.SourceURL, MediaType: s.MediaType,
			}
			stats = append(stats, BlobStat{
				Name: bn, Platform: plat, SHA256: sum, Size: size,
				Filename: name, Reused: seen[sum],
			})
			seen[sum] = true
		}
	}
	return blobs, stats, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("reading payload: %w", err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if st.IsDir() {
		return "", 0, fmt.Errorf("%s is a directory, payload must be a file", path)
	}

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// resolvePlatforms 推导 platforms（规范 §6.1）。
//
// 推导只发生在打包期——作者就在现场，是给出错误的最佳时机。发布产物中
// platforms 永远显式存在，§19 的交叉校验因此始终有效。
func resolvePlatforms(declared []string, blobs map[string]Blob) ([]string, error) {
	if len(declared) > 0 {
		return declared, nil
	}
	if len(blobs) == 0 {
		return nil, fmt.Errorf("platforms is not declared and there are no blobs to infer it from\n" +
			"  a host-config-only Pack (no payload) must declare platforms explicitly")
	}

	union := map[string]bool{}
	var names []string
	for _, bn := range sortedKeys(blobs) {
		names = append(names, bn)
		for plat := range blobs[bn] {
			union[plat] = true
		}
	}

	// 各 blob 的平台键集合必须一致，否则拒绝推导
	for _, bn := range names {
		var missing []string
		for _, plat := range sortedKeys(union) {
			if _, ok := blobs[bn][plat]; !ok {
				missing = append(missing, plat)
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf(
				"blob %q is missing platform %s (declared by other blobs)\n"+
					"  add the blob for that platform, or declare platforms explicitly to narrow support",
				bn, strings.Join(missing, ", "))
		}
	}

	out := sortedKeys(union)
	sort.Strings(out)
	return out, nil
}

func prepareOut(out string, force bool) error {
	if st, err := os.Stat(out); err == nil {
		if !st.IsDir() {
			return fmt.Errorf("output path %s already exists and is not a directory", out)
		}
		entries, err := os.ReadDir(out)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			if !force {
				return fmt.Errorf("output directory %s is not empty; use --force to overwrite", out)
			}
			if err := os.RemoveAll(out); err != nil {
				return err
			}
		}
	}
	return os.MkdirAll(out, 0o755)
}

// writeManifest 生成产物的 pack.yaml。
func writeManifest(p *Pack, out string, blobs map[string]Blob, platforms []string) error {
	doc := p.Doc
	if doc == nil || doc.Kind != yaml.MappingNode {
		return fmt.Errorf("pack.yaml top level must be a mapping")
	}

	// sources → blobs，占用原位置以保持字段顺序
	at := removeKey(doc, SourcesKey)
	if len(blobs) > 0 {
		setKeyAt(doc, "blobs", blobsNode(blobs), at)
	}

	// platforms 缺失时补上，放在 name/version/revision 之后
	if len(p.Platforms) == 0 && len(platforms) > 0 {
		pos := mapIndex(doc, "revision")
		if pos < 0 {
			pos = mapIndex(doc, "version")
		}
		if pos >= 0 {
			pos += 2
		}
		setKeyAt(doc, "platforms", platformsNode(platforms), pos)
	}

	// revision 未写时显式落盘，避免产物依赖「默认值」的实现细节
	if mapIndex(doc, "revision") < 0 {
		pos := mapIndex(doc, "version")
		if pos >= 0 {
			pos += 2
		}
		setKeyAt(doc, "revision", intScalar(int64(p.Revision)), pos)
	}

	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("serializing pack.yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(out, PackFile), []byte(buf.String()), 0o644)
}

// copyBlobs 把载荷拷进产物的 blobs/ 目录，按 sha256 命名。
// 内容相同的载荷天然只存一份——这正是内容寻址的去重收益。
func copyBlobs(sources SourceSet, root string, blobs map[string]Blob,
	srcDir, out string, stats []BlobStat) (int64, error) {

	dst := filepath.Join(out, DirBlobs)
	var total int64
	written := map[string]bool{}

	put := func(from, sum string, size int64) error {
		if written[sum] {
			return nil
		}
		written[sum] = true
		total += size
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		return copyFile(from, filepath.Join(dst, BlobFileName(sum)))
	}

	// 来自 sources 的载荷
	for _, bn := range sortedKeys(sources) {
		for _, plat := range sortedKeys(sources[bn]) {
			s := sources[bn][plat]
			abs := s.Path
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(root, filepath.FromSlash(s.Path))
			}
			e := blobs[bn][plat]
			if err := put(abs, e.SHA256, e.Size); err != nil {
				return 0, err
			}
		}
	}

	// 源目录里已有的 blobs/（作者直接放好的载荷）
	existing, err := os.ReadDir(filepath.Join(srcDir, DirBlobs))
	if err == nil {
		for _, e := range existing {
			if e.IsDir() {
				continue
			}
			from := filepath.Join(srcDir, DirBlobs, e.Name())
			if written[strings.TrimPrefix(e.Name(), "sha256-")] {
				continue
			}
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return 0, err
			}
			if err := copyFile(from, filepath.Join(dst, e.Name())); err != nil {
				return 0, err
			}
		}
	}

	return total, nil
}

func copyTree(src, dst string) error {
	st, err := os.Stat(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return nil
	}
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	st, err := in.Stat()
	if err != nil {
		return err
	}
	// 只保留可执行位——归档要求 uid/gid 与时间戳归零，模式也归一化
	mode := os.FileMode(0o644)
	if st.Mode()&0o111 != 0 {
		mode = 0o755
	}

	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, in); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
