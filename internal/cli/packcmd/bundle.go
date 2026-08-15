package packcmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/mecharion/mecharion/internal/pack"
	"github.com/spf13/cobra"
)

// NewBundleCmd 构造 `mechpack bundle`（spec §3 · 03-pack §2）。
//
// 它把一个**已组装**的 Pack 目录打成单文件 `.mpack`——离线交付的载体。
// 组装由 `mechpack assemble` 做（算 sha256、把 sources 换成 blobs）；
// 这里只负责归档，不重新算任何东西。
//
// 两步分开是因为它们的失败方式不同：assemble 失败是「你的产物不对」，
// bundle 失败是「打包环境不对」。混成一步会让错误信息说不清该去看哪边。
func NewBundleCmd(output *string) *cobra.Command {
	var out string

	cmd := &cobra.Command{
		Use:   "bundle [dir]",
		Short: "Pack an assembled Pack directory into a single .mpack file",
		Long: `Pack an assembled Pack directory into a ` + "`.mpack`" + ` (tar + zstd).

**This file is the vehicle for offline edge delivery**: copy it to the
target machine, or upload it from the Web UI.

The archive is **reproducible** — packing the same content twice produces
the same byte stream: entries sorted by path, timestamps and uid/gid
zeroed, only the executable bit kept. That's not fastidiousness: the
digest is the anchor of every trust relationship in this system, and that
requires a stable byte stream.

The archive contains no symlinks, absolute paths, or ` + "`..`" + ` — those can
point outside the archive on extraction, the classic extraction-escape
pattern.`,
		Example: `  mechpack assemble . && mechpack bundle dist/nginx-1.27.0-1
  mechpack bundle dist/nginx-1.27.0-1 --out /media/usb/`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}

			// 先把它读出来：文件名要用 name-version-revision，
			// 而那三样在 pack.yaml 里
			p, err := pack.Load(dir)
			if err != nil {
				return fmt.Errorf("reading %s: %w", dir, err)
			}

			target := out
			if target == "" || isDir(target) {
				target = filepath.Join(target, pack.MpackName(p))
			}

			// **先写临时文件再改名。** 中断留下的半个 .mpack 与一个完整的
			// 在文件系统上长得一模一样，而它会被当成交付物拷到 U 盘上。
			tmp := target + ".part"
			f, err := os.Create(tmp)
			if err != nil {
				return err
			}
			if err := pack.WriteMpack(dir, f); err != nil {
				f.Close()
				os.Remove(tmp)
				return err
			}
			if err := f.Close(); err != nil {
				os.Remove(tmp)
				return err
			}
			if err := os.Rename(tmp, target); err != nil {
				os.Remove(tmp)
				return err
			}

			st, err := os.Stat(target)
			if err != nil {
				return err
			}
			if *output == OutputJSON || *output == OutputYAML {
				return encode(c.OutOrStdout(), *output, map[string]any{
					"file": target, "size": st.Size(),
					"pack": p.Name, "version": p.Version, "revision": p.Revision,
				})
			}
			fmt.Fprintf(c.OutOrStdout(), "Wrote %s (%s)\n",
				target, humanSize(st.Size()))
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "Output file or directory, defaults to the current directory")
	return cmd
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
