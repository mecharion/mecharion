package mechd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

// BackupBody 是 `POST /admin/backup` 的请求体。
type BackupBody struct {
	// Dest 是备份文件的落盘路径（服务端本地文件系统，不是客户端）。
	// 留空时用 Service.Backup 的默认路径。
	Dest string `json:"dest,omitempty"`
}

// BackupResult 是一次备份的结果。
type BackupResult struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// Backup 对 mechd 自己的 SQLite 数据库做一次一致性快照（VACUUM INTO，
// 不用停服），落在 mechd 所在的这台机器上——与 `mechd ca export`/`issue`
// 一样，这不是把文件传给客户端下载的接口。
//
// **只备份数据库，不是全部期望状态**：主密钥（`/etc/mecharion/secret.key`）
// 与 PKI（`/etc/mecharion/pki/`）必须分开单独备份，见
// docs/design/07-persistence.md §1.7——这里不代劳，因为「打包在一起」
// 正好抵消了信封加密把主密钥与密文分开存放这件事本身的意义。
func (s *Service) Backup(ctx context.Context, actor, dest string) (BackupResult, error) {
	if dest == "" {
		dest = filepath.Join(filepath.Dir(s.Store.Path()), "backups",
			"mechd-"+s.now().Format("20060102-150405")+".db")
	}
	if err := s.Store.Backup(ctx, dest); err != nil {
		return BackupResult{}, err
	}
	info, err := os.Stat(dest)
	if err != nil {
		return BackupResult{}, fmt.Errorf("mechd: backup was written but its file info could not be read: %w", err)
	}
	s.audit(ctx, actor, "backup-create", dest, nil, "ok")
	return BackupResult{Path: dest, Size: info.Size()}, nil
}

func (a *API) createBackup(w http.ResponseWriter, r *http.Request, actor string) {
	var body BackupBody
	if err := decodeBody(w, r, &body); err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	v, err := a.S.Backup(r.Context(), actor, body.Dest)
	if err != nil {
		a.writeErr(w, r, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}
