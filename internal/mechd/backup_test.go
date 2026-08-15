package mechd

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mecharion/mecharion/internal/faults"
)

// 这组测试针对一个此前存在的缺口：Store.Backup 早就实现了
// （VACUUM INTO，不用停服），但此前没有任何入口能触发它——运维手册
// 讲不出一条真的能执行的备份命令。

func TestBackupWritesAConsistentSnapshot(t *testing.T) {
	f := newFixture(t)
	dest := filepath.Join(t.TempDir(), "snap.db")

	got, err := f.svc.Backup(ctx(), "tester", dest)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != dest {
		t.Errorf("Path = %q, want %q", got.Path, dest)
	}
	info, statErr := os.Stat(dest)
	if statErr != nil {
		t.Fatalf("备份文件应当真的落盘: %v", statErr)
	}
	if got.Size != info.Size() {
		t.Errorf("Size = %d，实际文件是 %d", got.Size, info.Size())
	}
	if info.Size() == 0 {
		t.Error("备份文件不该是空的——mechd.db 至少有 schema 与已建的 site")
	}
}

// TestBackupDefaultDestUnderDataDirBackups 钉住不给 --out 时的落点。
func TestBackupDefaultDestUnderDataDirBackups(t *testing.T) {
	f := newFixture(t)
	got, err := f.svc.Backup(ctx(), "tester", "")
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(filepath.Dir(f.svc.Store.Path()), "backups")
	if filepath.Dir(got.Path) != wantDir {
		t.Errorf("默认落点目录 = %q, want %q", filepath.Dir(got.Path), wantDir)
	}
	if _, err := os.Stat(got.Path); err != nil {
		t.Errorf("默认落点也该真的写出文件: %v", err)
	}
}

// TestBackupRejectsExistingDest 钉住目标已存在时不覆盖，且这是一个
// **用户错误**（faults.Permanent），不是服务端故障——否则 HTTP 层会把
// 一个「你传的路径已经有文件」的输入错误报成 500。
func TestBackupRejectsExistingDest(t *testing.T) {
	f := newFixture(t)
	dest := filepath.Join(t.TempDir(), "exists.db")
	if err := os.WriteFile(dest, []byte("already here"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := f.svc.Backup(ctx(), "tester", dest)
	if err == nil {
		t.Fatal("目标已存在时应当报错，不该覆盖")
	}
	// **不用 faults.ClassOf**：未分类的错误也按 Permanent 处理（见该函数
	// 注释），这条判据不会区分「真的打过标」与「压根没打标」。这里要
	// 验证的是 statusFor/isUserError 真正依赖的东西——错误链上确实有一个
	// 具体的 *faults.Error，而不是恰好落进默认档。
	var fe *faults.Error
	if !errors.As(err, &fe) || fe.Class != faults.Permanent {
		t.Errorf("应当是一个显式打过 Permanent 标记的 *faults.Error，实际 %#v", err)
	}
	// 没有被覆盖
	body, readErr := os.ReadFile(dest)
	if readErr != nil || string(body) != "already here" {
		t.Error("原文件不该被动过")
	}
}

// TestBackupAudited 钉住备份留痕——它是一个会读取全部期望状态的
// 敏感操作，操作日志的四类动作里这条不该缺。
func TestBackupAudited(t *testing.T) {
	f := newFixture(t)
	dest := filepath.Join(t.TempDir(), "snap.db")
	if _, err := f.svc.Backup(ctx(), "tester", dest); err != nil {
		t.Fatal(err)
	}
	entries, err := f.svc.Repos.Events().ListAudit(ctx(), 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "backup-create" && e.Actor == "tester" && e.Target == dest {
			found = true
		}
	}
	if !found {
		t.Errorf("没找到备份的审计条目，实际: %+v", entries)
	}
}

// ── HTTP 层：只测请求体解析/错误映射/响应编码这层胶水，guard 本身的
// 认证/CSRF 行为已经在 authapi_test.go 里通用测过，不重复。

func TestCreateBackupHandler(t *testing.T) {
	f := newFixture(t)
	api := &API{S: f.svc}

	dest := filepath.Join(t.TempDir(), "snap.db")
	body, _ := json.Marshal(BackupBody{Dest: dest})
	req := httptest.NewRequest("POST", APIPrefix+"/admin/backup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.createBackup(rec, req, "tester")

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rec.Code, rec.Body)
	}
	var out BackupResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v", err)
	}
	if out.Path != dest {
		t.Errorf("Path = %q, want %q", out.Path, dest)
	}
}

// TestCreateBackupHandlerMapsExistingDestTo400 钉住 statusFor 真的把
// faults.Permanent 错误映成 400，不是意外走了默认的 500 分支。
func TestCreateBackupHandlerMapsExistingDestTo400(t *testing.T) {
	f := newFixture(t)
	api := &API{S: f.svc}

	dest := filepath.Join(t.TempDir(), "exists.db")
	if err := os.WriteFile(dest, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(BackupBody{Dest: dest})
	req := httptest.NewRequest("POST", APIPrefix+"/admin/backup", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	api.createBackup(rec, req, "tester")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("目标已存在应当映射成 400，实际 %d: %s", rec.Code, rec.Body)
	}
}
