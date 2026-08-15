package ctlcmd

import (
	"fmt"

	rootcli "github.com/mecharion/mecharion/internal/cli"
	"github.com/mecharion/mecharion/internal/mechd"
	"github.com/spf13/cobra"
)

// NewBackupCmd 构造 `mechctl backup`。
//
// **只有 create**：这条命令存在的理由是 mechd 已经实现了一致性快照
// （`Store.Backup`，VACUUM INTO，不用停服）却没有任何入口能触发它——
// 这条命令补上这个缺口，与 `pack upload` 补上此前完全没有的上传路径
// 是同一种模式。落盘路径是 mechd **所在这台机器**的本地路径，
// 不是把备份文件传回客户端下载——与 `mechd ca export`/`issue` 同一个
// 模型。
func NewBackupCmd(flags *ClientFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up mechd's database",
	}
	cmd.AddCommand(newBackupCreateCmd(flags))
	return cmd
}

func newBackupCreateCmd(f *ClientFlags) *cobra.Command {
	var dest string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Take a consistent snapshot of mechd's database",
		Long: `Take a consistent snapshot (VACUUM INTO) of mechd's SQLite database, without
stopping the service.

Only the database itself is backed up. The master key
(/etc/mecharion/secret.key) and PKI (/etc/mecharion/pki/) must be backed up
separately — keeping them together defeats the point of envelope
encryption. See docs/design/07-persistence.md §1.7 for the complete backup
checklist.

When --out is empty, it lands in the backups/ subdirectory under mechd's
data directory — that's just a convenient default, not an actual backup
strategy: a backup that means anything has to land on a different disk or a
different machine, since a "backup" on the same disk is lost right along
with the original when that disk fails.`,
		Example: "  mechctl backup create --out /mnt/backup/mechd-$(date +%Y%m%d).db",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cli, err := f.client()
			if err != nil {
				return validationErr(err)
			}
			var out mechd.BackupResult
			body := mechd.BackupBody{Dest: dest}
			if err := cli.Do("POST", mechd.APIPrefix+"/admin/backup", body, &out); err != nil {
				return err
			}

			switch f.output() {
			case rootcli.OutputJSON:
				return writeJSON(c.OutOrStdout(), out)
			case rootcli.OutputYAML:
				return writeYAML(c.OutOrStdout(), out)
			}
			fmt.Fprintf(c.OutOrStdout(), "Backed up to %s (%d bytes)\n", out.Path, out.Size)
			return nil
		},
	}
	cmd.Flags().StringVar(&dest, "out", "", "Where to write the backup file (a path on the machine mechd runs on), defaults to backups/ under the data directory")
	return cmd
}
