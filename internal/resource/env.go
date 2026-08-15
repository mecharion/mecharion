package resource

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/mecharion/mecharion/internal/command"
	"github.com/mecharion/mecharion/internal/spec"
)

// Env 是资源共享的运行环境。
//
// 它承载的都是「资源自己无从得知、又不该各自去猜」的东西：载荷在哪、
// Pack 解在哪、外部命令怎么执行。显式传入而非全局变量，是为了让每种
// 资源类型都能在测试里被完整替身化。
type Env struct {
	// PackRoot 是解开的 Pack 逻辑目录，`file.source` 与 hook 脚本的根。
	PackRoot string

	// BlobDir 是内容寻址的 blob 根目录（<dataDir>/blobs）。
	BlobDir string

	// Blobs 是本 spec 声明的载荷，按名字索引。
	Blobs map[string]spec.BlobRef

	// Runner 执行外部命令。测试可替换。
	Runner Runner

	// Digests 免掉重复的整文件哈希；为 nil 时每次都真算。
	Digests DigestCache

	// uidCache/gidCache 缓存一次调和内的名字解析结果，
	// 避免每个文件都 fork 一次 getent。nameCache 是它们的反向。
	mu        sync.Mutex
	uidCache  map[string]int
	gidCache  map[string]int
	nameCache map[string]string
}

// NewEnv 构造一个使用真实外部命令的环境。
func NewEnv(packRoot, blobDir string, blobs []spec.BlobRef) *Env {
	m := make(map[string]spec.BlobRef, len(blobs))
	for _, b := range blobs {
		m[b.Name] = b
	}
	return &Env{
		PackRoot: packRoot,
		BlobDir:  blobDir,
		Blobs:    m,
		Runner:   ExecRunner{},
	}
}

// EnvFor 从一份已解析规格构造环境。
func EnvFor(s *spec.ResolvedSpec, packRoot, blobDir string) *Env {
	return NewEnv(packRoot, blobDir, s.Blobs)
}

// runner 返回 Runner，未设置时用真实实现。
func (e *Env) runner() Runner {
	if e.Runner == nil {
		return ExecRunner{}
	}
	return e.Runner
}

// digests 返回摘要缓存，可能为 nil。
func (e *Env) digests() DigestCache { return e.Digests }

// ── 载荷解析 ────────────────────────────────────────────────────────────

// Blob 返回名字对应的载荷声明。
func (e *Env) Blob(name string) (spec.BlobRef, error) {
	b, ok := e.Blobs[name]
	if !ok {
		known := make([]string, 0, len(e.Blobs))
		for k := range e.Blobs {
			known = append(known, k)
		}
		return spec.BlobRef{}, Permanentf("解析载荷",
			"规格中没有名为 %q 的 blob（已声明：%s）", name, strings.Join(known, ", "))
	}
	return b, nil
}

// BlobPath 返回载荷在本机 blob 存储中的路径。
//
// 布局是 <blobDir>/sha256/<前两位>/<完整摘要>——两级分片避免单目录下
// 堆积上万个文件。
func (e *Env) BlobPath(name string) (string, spec.BlobRef, error) {
	b, err := e.Blob(name)
	if err != nil {
		return "", spec.BlobRef{}, err
	}
	if len(b.SHA256) < 2 {
		return "", b, Permanentf("解析载荷", "blob %q 的 sha256 非法: %q", name, b.SHA256)
	}
	p := filepath.Join(e.BlobDir, "sha256", b.SHA256[:2], b.SHA256)
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			// 缺载荷是「还没下载完」，重试有意义。
			return "", b, Transient("解析载荷", fmt.Errorf(
				"blob %q (%s) 尚未就位于 %s", name, shortSum(b.SHA256), p))
		}
		return "", b, Transient("解析载荷", err)
	}
	return p, b, nil
}

// SourcePath 把 `file.source` 的 Pack 内相对路径解析为绝对路径，
// 并确保它没有逃出 Pack 根。
func (e *Env) SourcePath(rel string) (string, error) {
	if e.PackRoot == "" {
		return "", Permanentf("解析 source", "本环境没有配置 Pack 根目录，无法解析 source: %s", rel)
	}
	if !filepath.IsLocal(rel) {
		return "", Permanentf("解析 source", "source 必须是 Pack 内的相对路径，实际 %q", rel)
	}
	return filepath.Join(e.PackRoot, filepath.FromSlash(rel)), nil
}

func shortSum(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

// ── 身份解析 ────────────────────────────────────────────────────────────

// LookupUID 把用户名解析为 uid。纯数字的名字按 uid 直接返回。
//
// 走 getent 而非 os/user：CGO_ENABLED=0 下 os/user 只读 /etc/passwd，
// 看不见 LDAP / SSSD 提供的用户（ADR-0015 要求静态链接，因此这不是
// 可以放弃的约束）。getent 不可用时（非 Linux 开发机）退回 os/user。
func (e *Env) LookupUID(ctx context.Context, name string) (int, error) {
	if n, err := strconv.Atoi(name); err == nil {
		return n, nil
	}
	e.mu.Lock()
	if id, ok := e.uidCache[name]; ok {
		e.mu.Unlock()
		return id, nil
	}
	e.mu.Unlock()

	id, err := e.lookupID(ctx, "passwd", name)
	if err != nil {
		return 0, err
	}
	e.mu.Lock()
	if e.uidCache == nil {
		e.uidCache = map[string]int{}
	}
	e.uidCache[name] = id
	e.mu.Unlock()
	return id, nil
}

// LookupGID 把组名解析为 gid。
func (e *Env) LookupGID(ctx context.Context, name string) (int, error) {
	if n, err := strconv.Atoi(name); err == nil {
		return n, nil
	}
	e.mu.Lock()
	if id, ok := e.gidCache[name]; ok {
		e.mu.Unlock()
		return id, nil
	}
	e.mu.Unlock()

	id, err := e.lookupID(ctx, "group", name)
	if err != nil {
		return 0, err
	}
	e.mu.Lock()
	if e.gidCache == nil {
		e.gidCache = map[string]int{}
	}
	e.gidCache[name] = id
	e.mu.Unlock()
	return id, nil
}

// NameForID 把 uid/gid 反查成名字；查不到时返回数字本身。
//
// 反查必须和正查走同一条路（getent）：属主的比对是**按名字**做的，
// 若正查认得 LDAP 用户而反查不认得，一个正确的属主会被永远报成漂移，
// 而且怎么 Apply 都收敛不掉。
//
// db 取 "passwd" 或 "group"。
func (e *Env) NameForID(ctx context.Context, db string, id int) string {
	key := db + ":" + strconv.Itoa(id)

	e.mu.Lock()
	if n, ok := e.nameCache[key]; ok {
		e.mu.Unlock()
		return n
	}
	e.mu.Unlock()

	name := strconv.Itoa(id)
	if ent, err := lookupNSS(ctx, e.runner(), db, name, true); err == nil &&
		len(ent) > 0 && ent[0] != "" {
		name = ent[0]
	}

	e.mu.Lock()
	if e.nameCache == nil {
		e.nameCache = map[string]string{}
	}
	e.nameCache[key] = name
	e.mu.Unlock()
	return name
}

// lookupID 从 getent 的输出里取第三段（passwd 与 group 的 id 位置相同）。
func (e *Env) lookupID(ctx context.Context, db, name string) (int, error) {
	ent, err := getent(ctx, e.runner(), db, name)
	if err != nil {
		return 0, err
	}
	if ent == nil {
		return 0, Permanentf("解析身份", "%s 中不存在 %q", db, name)
	}
	if len(ent) < 3 {
		return 0, Permanentf("解析身份", "getent %s %s 的输出无法解析: %v", db, name, ent)
	}
	id, err := strconv.Atoi(ent[2])
	if err != nil {
		return 0, Permanentf("解析身份", "getent %s %s 返回的 id 非法: %q", db, name, ent[2])
	}
	return id, nil
}

// getent 按名字查询一条 NSS 记录。
func getent(ctx context.Context, r Runner, db, name string) ([]string, error) {
	return lookupNSS(ctx, r, db, name, false)
}

// lookupNSS 查询一条 NSS 记录。byID 说明 key 是 id 还是名字——真正的
// getent 两者都认，只有退化路径需要区分。
//
// 返回值约定：
//   - (字段切片, nil)  找到
//   - (nil, nil)       确定不存在（getent 退出码 2）
//   - (nil, err)       读不出来——调用方应转成 Observed{StateUnknown}
func lookupNSS(ctx context.Context, r Runner, db, key string, byID bool) ([]string, error) {
	res, err := r.Run(ctx, "getent", db, key)
	if err != nil {
		if command.IsNotFound(err) {
			// 非 Linux 开发机上没有 getent，退回纯 Go 的 /etc/passwd 读取。
			return getentFallback(db, key, byID)
		}
		return nil, Transient("查询 "+db, err)
	}
	switch res.ExitCode {
	case 0:
		line := strings.TrimRight(res.Stdout, "\n")
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line = line[:i]
		}
		return strings.Split(line, ":"), nil
	case 2:
		return nil, nil
	default:
		return nil, Transient("查询 "+db, fmt.Errorf(
			"getent %s %s 退出码 %d: %s", db, key, res.ExitCode, strings.TrimSpace(res.Stderr)))
	}
}

// ── 命令执行 ────────────────────────────────────────────────────────────

// 命令执行封装在 internal/command —— Runtime 也要 fork systemctl，
// 两边共用同一套「退出码不是错误」的约定与同一个测试替身。
type (
	// Runner 执行外部命令。
	Runner = command.Runner
	// CmdResult 是一次外部命令的结果。
	CmdResult = command.Result
	// ExecRunner 是基于 os/exec 的真实实现。
	ExecRunner = command.Exec
)

// mustRun 执行命令并要求退出码为 0。
func mustRun(ctx context.Context, r Runner, op, name string, args ...string) error {
	return command.MustRun(ctx, r, op, name, args...)
}
