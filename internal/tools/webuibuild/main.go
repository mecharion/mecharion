// Command webuibuild 构建前端并把产物预压缩后放进 internal/webui/dist。
//
// 它是 `go generate ./internal/webui/...` 的实现（[ADR-0036]）。做两件事：
//
//	① 在 webui/ 里跑 npm ci + npm run build
//	② 把 Vite 的产物拷进 internal/webui/dist，可压缩的类型顺手 gzip
//
// **压缩放在 Go 这一侧**，不用 npm 插件：前端的依赖越少，五年后重写它时
// 越轻，而 gzip 在标准库里。
//
// [ADR-0036]: ../../../docs/adr/0036-webui-vue-and-generated-dist.md
package main

import (
	"compress/gzip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// 只压这些类型。图片与字体本身已经压过，再 gzip 只会更大。
var compressible = map[string]bool{
	".html": true, ".js": true, ".css": true, ".json": true,
	".svg": true, ".map": true, ".txt": true, ".webmanifest": true,
}

// 小文件不压：gzip 头就 20 字节左右，压完可能反而变大。
const minCompress = 1024

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "webuibuild: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	src := filepath.Join(root, "webui")
	out := filepath.Join(root, "internal", "webui", "dist")

	if _, err := exec.LookPath("npm"); err != nil {
		// **说清楚这不是「坏了」**：没有 Node 的机器构建出的 mechd 没有界面，
		// 那是 ADR-0036 明确接受的结果，不是需要排查的故障。
		return fmt.Errorf("找不到 npm —— 构建前端需要 Node\n" +
			"  没有 Node 时可以跳过这一步：mechd 仍然构建得出来，只是不带 Web UI")
	}

	if err := npm(src, "ci"); err != nil {
		return err
	}
	if err := npm(src, "run", "build"); err != nil {
		return err
	}

	// **先清空再拷**：留着上一次的产物，改名后的旧 chunk 会一直躺在 dist 里，
	// 一路被 embed 进二进制。
	if err := clean(out); err != nil {
		return err
	}
	st, err := copyTree(filepath.Join(src, "dist"), out)
	if err != nil {
		return err
	}
	fmt.Printf("webuibuild: %d 个文件；embed 体积 %.0f KB（原始 %.0f KB，压了 %d 个）\n",
		st.files, kb(st.embedded), kb(st.raw), st.compressed)
	return nil
}

func repoRoot() (string, error) {
	// go generate 的工作目录是含指令的那个包
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := wd; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("从 %s 向上找不到 go.mod", wd)
		}
		dir = parent
	}
}

func npm(dir string, args ...string) error {
	name := "npm"
	if os.PathSeparator == '\\' {
		name = "npm.cmd" // Windows 上 npm 是个 .cmd，exec 找不到裸名
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("npm %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// clean 清空 dist，但**保留 .gitkeep**——它是让空目录也 embed 得过的那个文件。
func clean(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return os.MkdirAll(dir, 0o755)
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == ".gitkeep" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// stats 是一次搬运的统计，用来在构建日志里看得见体积。
//
// **embedded 才是要盯的那个数**——它是真正进到二进制里的字节数。
type stats struct {
	files      int
	raw        int64 // 构建产物原始大小
	embedded   int64 // 实际写进 dist 的大小（压缩后或原样）
	compressed int   // 其中被压缩的文件数
}

func copyTree(src, dst string) (st stats, err error) {
	err = filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		st.files++
		st.raw += int64(len(body))
		keepRaw := func() error {
			st.embedded += int64(len(body))
			return os.WriteFile(target, body, 0o644)
		}

		if !compressible[strings.ToLower(filepath.Ext(rel))] || len(body) < minCompress {
			return keepRaw()
		}
		gzBody, err := gzipBytes(body)
		if err != nil {
			return err
		}
		// 压完反而更大就存原文件——这在小的 svg 上真的会发生
		if len(gzBody) >= len(body) {
			return keepRaw()
		}
		st.compressed++
		st.embedded += int64(len(gzBody))
		return os.WriteFile(target+".gz", gzBody, 0o644)
	})
	return st, err
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf strings.Builder
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

func kb(n int64) float64 { return float64(n) / 1024 }
