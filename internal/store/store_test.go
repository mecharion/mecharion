package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), Options{
		Path: filepath.Join(t.TempDir(), "mechd.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenRunsMigrations(t *testing.T) {
	s := openTest(t)

	v, err := s.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v < 1 {
		t.Errorf("迁移版本 = %d，期望 ≥ 1", v)
	}

	// 四组表都要在
	for _, table := range []string{
		"sites", "nodes", "components", "config_groups", "role_instances",
		"pack_bindings", "secrets", "vault_keys",
		"instance_status", "node_facts", "drift_reports", "suppressions",
		"rollouts", "rollout_batches", "events", "audit",
	} {
		var n int
		err := s.Reader().QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`,
			table).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("表 %s 不存在", table)
		}
	}
}

// TestMigrationsAreReversible 钉住 Down 能跑通。
//
// 不可回滚的迁移在升级出问题时是灾难：唯一的退路变成「从备份恢复」。
func TestMigrationsAreReversible(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	// 回滚**整条链**而不只是最后一次：这条测试要证的是每个迁移的
	// Down 都写对了，而那种错误要到真的需要回滚时才暴露
	if err := s.DownTo(ctx, 0); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	var n int
	if err := s.Reader().QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='sites'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("回滚后 sites 表应当消失")
	}

	// 再 Up 一次要能回到原样
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("重新迁移失败: %v", err)
	}
	if err := s.Reader().QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='sites'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("重新迁移后 sites 表应当回来")
	}
}

// TestOpenIsIdempotent 钉住重复打开不重复迁移。
func TestOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mechd.db")

	for i := 0; i < 3; i++ {
		s, err := Open(ctx, Options{Path: path})
		if err != nil {
			t.Fatalf("第 %d 次打开: %v", i+1, err)
		}
		if _, err := s.Version(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestWALEnabled 钉住 WAL 与外键约束真的开着。
//
// pragma 写在连接串里而不是 Open 之后手工 Exec，是为了让**连接池新建的
// 每条连接**都带上；这个用例正是验证那一点。
func TestWALEnabled(t *testing.T) {
	s := openTest(t)

	var mode string
	if err := s.Reader().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode = %q，期望 wal", mode)
	}

	var fk int
	if err := s.Reader().QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Error("外键约束必须开启——否则删 Site 会留下悬空的 Component")
	}
}

// TestForeignKeysEnforced 钉住外键真的在拦。
func TestForeignKeysEnforced(t *testing.T) {
	s := openTest(t)
	_, err := s.Writer().Exec(
		`INSERT INTO nodes (site_id, name, address, created_at) VALUES (999, 'n1', '10.0.0.1', ?)`,
		FormatTime(time.Now()))
	if err == nil {
		t.Fatal("引用不存在的 site 应当被拒绝")
	}
}

// TestOrdinalUniquePerRole 钉住 ADR-0028 的存储层保证。
//
// ordinal 在角色内唯一，且**允许空洞**——移除实例后序号不回收，
// 因为回收会让新实例拿到刚被释放的编号，而集群里其它成员的元数据
// 可能还引用着旧成员。
func TestOrdinalUniquePerRole(t *testing.T) {
	s := openTest(t)
	now := FormatTime(time.Now())

	r, err := s.Writer().Exec(
		`INSERT INTO sites (name, kind, created_at) VALUES ('s1','standalone',?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	siteID, _ := r.LastInsertId()

	r, err = s.Writer().Exec(
		`INSERT INTO components (site_id,name,pack_name,pack_version,created_at,updated_at)
		 VALUES (?,'zk','zookeeper','3.9.1',?,?)`, siteID, now, now)
	if err != nil {
		t.Fatal(err)
	}
	compID, _ := r.LastInsertId()

	nodeID := func(name string) int64 {
		res, err := s.Writer().Exec(
			`INSERT INTO nodes (site_id,name,address,created_at) VALUES (?,?,?,?)`,
			siteID, name, "10.0.0."+name, now)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	n1, n2 := nodeID("1"), nodeID("2")

	ins := func(node int64, ord int) error {
		_, err := s.Writer().Exec(
			`INSERT INTO role_instances (component_id,role,node_id,ordinal,created_at)
			 VALUES (?,'server',?,?,?)`, compID, node, ord, now)
		return err
	}

	if err := ins(n1, 0); err != nil {
		t.Fatal(err)
	}
	// 同一角色内序号重复必须被拒
	if err := ins(n2, 0); err == nil {
		t.Error("同角色内 ordinal 重复应当被拒绝")
	}
	// 空洞是允许的：0 之后直接给 5
	if err := ins(n2, 5); err != nil {
		t.Errorf("序号空洞应当被允许（移除实例后不回收）: %v", err)
	}
}

func TestBackup(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	dest := filepath.Join(t.TempDir(), "backup.db")
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() == 0 {
		t.Error("备份文件为空")
	}

	// 备份出来的库应当能直接打开且版本一致
	b, err := Open(ctx, Options{Path: dest})
	if err != nil {
		t.Fatalf("备份文件应当是一个可用的库: %v", err)
	}
	defer b.Close()

	// 已存在时拒绝覆盖——备份是不可逆的写，不该静默覆盖
	if err := s.Backup(ctx, dest); err == nil {
		t.Error("目标已存在时应当拒绝")
	}
}

func TestTimeRoundTrip(t *testing.T) {
	want := time.Date(2026, 8, 4, 10, 30, 0, 123456789, time.UTC)
	got, err := ParseTime(FormatTime(want))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Errorf("往返后 %v != %v", got, want)
	}

	// 字典序必须等于时间序，否则 ORDER BY 与范围查询都不成立
	early := FormatTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	late := FormatTime(time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC))
	if !(early < late) {
		t.Errorf("字典序应当等于时间序: %q vs %q", early, late)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(context.Background(), Options{}); err == nil {
		t.Error("未指定路径应当报错")
	}
}
