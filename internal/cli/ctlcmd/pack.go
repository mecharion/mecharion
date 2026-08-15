package ctlcmd

import (
	"fmt"
	"os"

	rootcli "github.com/mecharion/mecharion/internal/cli"
	"github.com/mecharion/mecharion/internal/mechd"
	"github.com/spf13/cobra"
)

// NewPackCmd 构造 `mechctl pack`（10-cli §10）。
//
// **只有 upload**：`list`/`show`/`pull` 这几个查询动词 10-cli.md 里定过，
// 但还没做——先补的是这一个，因为它挡住了从命令行走完一次真实部署：
// `mechctl component deploy` 能找到本地
// pack-dir 扫到的 Pack 定义，但那只是元数据，真正的 blob 内容要经
// `POST /packs` 才会被服务端的 UploadPack 解包、lint、入库——在这之前，
// 命令行没有任何路径能把一个真实的 .mpack 送到 mechd。Web UI 的上传
// 按钮走的是同一个接口（`webui/src/lib/api.ts` 的 `upload`）。
func NewPackCmd(flags *ClientFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pack",
		Short: "Manage the Pack collection on mechd",
	}
	cmd.AddCommand(newPackUploadCmd(flags))
	return cmd
}

func newPackUploadCmd(f *ClientFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "upload <file.mpack>",
		Short: "Upload a .mpack to mechd's Pack collection",
		Long: `Upload a .mpack (a thick pack, containing every blob) to mechd.

The server will: unpack into a temp directory → check every path (rejecting
absolute paths / .. / links) → run the full lint including hermetic checks
→ only once everything passes, atomically move it into the Pack collection
→ store the blobs keyed by sha256 (that's exactly what mechlet fetches by
during reconciliation, not the thick form sitting next to pack.yaml). If
any step fails, no partial state is left behind.

Uploading the same name and version again **overwrites** it rather than
erroring — for repeatedly re-uploading during local iteration.`,
		Example: "  mechctl pack upload dist/myapp-0.1.0-1.mpack",
		Args:    cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			path := args[0]
			file, err := os.Open(path)
			if err != nil {
				return validationErr(fmt.Errorf("open %s: %w", path, err))
			}
			defer file.Close()

			var out mechd.UploadResult
			if err := cli.Upload(mechd.APIPrefix+"/packs", file, &out); err != nil {
				return err
			}

			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(c.OutOrStdout(), out)
			case rootcli.OutputYAML:
				return writeYAML(c.OutOrStdout(), out)
			}
			w := c.OutOrStdout()
			verb := "Uploaded"
			if out.Replaced {
				verb = "Overwrote"
			}
			fmt.Fprintf(w, "%s %s %s (revision %d)\n", verb, out.Name, out.Version, out.Revision)
			for _, warn := range out.Warnings {
				fmt.Fprintf(w, "  warning: %s\n", warn)
			}
			return nil
		},
	}
	return cmd
}
