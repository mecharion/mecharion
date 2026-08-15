// Package store 是 mechd 的持久化层。
//
// 设计见 docs/design/07-persistence.md。三条工程约定决定了本包的形状：
//
//   - **WAL 模式**：允许 1 写 + N 读并发
//   - **写收敛到单连接**：SQLite 是单写者，彻底避免 SQLITE_BUSY
//   - **迁移嵌进二进制**：离线环境下不能依赖外部迁移工具
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // 纯 Go 驱动，CGO_ENABLED=0 的前提

	"github.com/mecharion/mecharion/internal/faults"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// DriverName 是 modernc.org/sqlite 注册的驱动名。
const DriverName = "sqlite"

// Options 是打开一个 Store 的参数。
type Options struct {
	// Path 是数据库文件路径。":memory:" 走内存库（仅测试）。
	Path string
	// BusyTimeout 是遇到锁时的等待上限。
	BusyTimeout time.Duration
	// Now 可替换，供测试固定时间。
	Now func() time.Time
}

func (o Options) busyTimeout() time.Duration {
	if o.BusyTimeout <= 0 {
		return 5 * time.Second
	}
	return o.BusyTimeout
}

// Store 持有读写两组连接。
//
// **写与读用不同的连接池**：写池限制为 1，这是 SQLite 单写者模型的直接体现。
// 不这么做的话，两个并发写会撞上 SQLITE_BUSY，而重试逻辑会散落到每个调用点。
type Store struct {
	write *sql.DB
	read  *sql.DB
	path  string
	now   func() time.Time
}

// Open 打开（必要时创建）数据库并跑完迁移。
func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("store: no database path specified")
	}
	if opts.Path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(opts.Path), 0o700); err != nil {
			return nil, fmt.Errorf("store: creating data directory: %w", err)
		}
	}

	write, err := open(opts, true)
	if err != nil {
		return nil, err
	}
	read, err := open(opts, false)
	if err != nil {
		write.Close()
		return nil, err
	}

	s := &Store{write: write, read: read, path: opts.Path, now: opts.Now}
	if s.now == nil {
		s.now = time.Now
	}
	if err := s.migrate(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func open(opts Options, writer bool) (*sql.DB, error) {
	// 连接串里的 pragma 由驱动在每条连接上执行——写在这里而不是 Open 之后
	// 手工 Exec，是为了保证**连接池新建的每条连接**都带上它们。
	dsn := opts.Path + fmt.Sprintf(
		"?_pragma=busy_timeout(%d)"+
			"&_pragma=journal_mode(WAL)"+
			"&_pragma=synchronous(NORMAL)"+
			"&_pragma=foreign_keys(ON)",
		opts.busyTimeout().Milliseconds())

	db, err := sql.Open(DriverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: opening database: %w", err)
	}
	if writer {
		// ★ 单写者。这一行就是「写收敛到单连接」的全部实现。
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(8)
	}
	db.SetConnMaxLifetime(0)
	return db, nil
}

// migrate 跑完全部未应用的迁移。
func (s *Store) migrate(ctx context.Context) error {
	goose.SetBaseFS(migrationFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("store: setting migration dialect: %w", err)
	}
	if err := goose.UpContext(ctx, s.write, "migrations"); err != nil {
		return fmt.Errorf("store: migration failed: %w", err)
	}
	return nil
}

// Version 返回当前的迁移版本。
func (s *Store) Version(ctx context.Context) (int64, error) {
	goose.SetBaseFS(migrationFS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return 0, err
	}
	return goose.GetDBVersionContext(ctx, s.write)
}

// Down 回滚一个迁移版本。仅供测试与灾难恢复。
func (s *Store) Down(ctx context.Context) error {
	goose.SetBaseFS(migrationFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	return goose.DownContext(ctx, s.write, "migrations")
}

// DownTo 回滚到指定版本；0 表示全部回滚。
//
// 与 Down 分开是因为两者的用途不同：Down 撤销**一次**升级（升级出问题时
// 的退路），DownTo(0) 验证的是**整条迁移链**都可逆。后者只有测试会用，
// 但它拦住的是「某个迁移写了 Down 却写错了」——那种错误要到真的需要回滚
// 时才暴露，而那时人已经在处理另一个故障了。
func (s *Store) DownTo(ctx context.Context, version int64) error {
	goose.SetBaseFS(migrationFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	return goose.DownToContext(ctx, s.write, "migrations", version)
}

// Close 关闭两个连接池。
func (s *Store) Close() error {
	var first error
	for _, db := range []*sql.DB{s.write, s.read} {
		if db == nil {
			continue
		}
		if err := db.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Path 返回数据库文件路径。
func (s *Store) Path() string { return s.path }

// Backup 用 VACUUM INTO 拿一份一致性快照，**不用停服**。
//
// 注意备份数据库**不等于备份了全部期望状态**：主密钥在
// /etc/mecharion/secret.key，且必须与它分开存放——放一起就抵消了信封加密
// （docs/design/07-persistence.md §1.7）。
func (s *Store) Backup(ctx context.Context, dest string) error {
	// **打类型标记**：目标已存在是调用方传错了路径，不是
	// mechd 自己出了故障——不打标的话 statusFor 默认给 500，把一个
	// 显而易见的用户输入错误说成「服务端错误」。
	if _, err := os.Stat(dest); err == nil {
		return faults.Permanentf("backup", "destination %s already exists", dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	// VACUUM INTO 不接受参数占位符，只能拼串；dest 来自运维输入，
	// 因此转义单引号防止意外截断语句。
	if _, err := s.write.ExecContext(ctx,
		"VACUUM INTO '"+escapeSQLString(dest)+"'"); err != nil {
		return fmt.Errorf("store: backing up to %s: %w", dest, err)
	}
	return nil
}

func escapeSQLString(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'')
		}
		out = append(out, s[i])
	}
	return string(out)
}

// ── 事务与访问 ──────────────────────────────────────────────────────────

// Reader 返回只读连接池。
func (s *Store) Reader() *sql.DB { return s.read }

// Writer 返回写连接（单连接）。
func (s *Store) Writer() *sql.DB { return s.write }

// Now 返回本 Store 使用的时钟。
func (s *Store) Now() time.Time { return s.now() }

// InTx 在一个写事务里执行 fn，失败自动回滚。
//
// **写路径一律走这里。** 期望状态的一次变更往往横跨多张表
// （component + role_instances + pack_bindings），中途失败留下的半份状态
// 比拒绝这次变更糟得多。
//
// fn 拿到的不是裸的 *sql.Tx，而是挂着这个事务的 context——repo 方法
// 内部的 wq(ctx)/rq(ctx) 会自动认出它并改用同一个事务，调用方因此可以
// 继续用平时那套 Repos 方法写代码，不需要另学一套「事务内怎么写」的
// API。fn 内部可以安全地调用任何只读取的东西（哪怕不认识
// 这个事务、falls back 到读连接池）——WAL 模式下并发读本来就不会被
// 一个进行中的写事务卡住。
//
// **可安全嵌套。** ctx 里已经挂着事务时，直接在那个事务里跑 fn，不再
// 开一个新的——`s.write` 只有一个连接，嵌套 BeginTx 会卡在等一个永远
// 不会被放出来的连接上（外层事务还没提交）。`InstanceRepo.Ensure`、
// `Repos.ClaimNode` 这类自带 InTx 的方法因此既能独立调用，也能被 Deploy
// 那样的更大事务包住而不用改代码——调用方完全不需要关心自己是不是
// 已经在事务里。
func (s *Store) InTx(ctx context.Context, fn func(context.Context) error) error {
	if _, ok := ctx.Value(txKey{}).(*sql.Tx); ok {
		return fn(ctx)
	}

	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: beginning transaction: %w", err)
	}
	defer func() {
		// 已提交时 Rollback 返回 ErrTxDone，无害
		_ = tx.Rollback()
	}()

	txCtx := context.WithValue(ctx, txKey{}, tx)
	if err := fn(txCtx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing transaction: %w", err)
	}
	return nil
}

// FormatTime 是本包统一的时间格式。
//
// SQLite 没有原生时间类型，存 RFC3339 字符串：**字典序即时间序**，
// 因此 ORDER BY 与范围查询都能直接用，不需要额外转换。
func FormatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// ParseTime 解析本包写入的时间。
func ParseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}
