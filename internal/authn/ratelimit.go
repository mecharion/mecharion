package authn

import (
	"fmt"
	"sync"
	"time"
)

// 限流参数。
//
// **限流才是挡住慢速撞库的那一层**（[ADR-0037]）。PoW 给每次尝试标价，
// 但一秒一次的人工尝试算力成本可以忽略；滑块干脆不提供安全性。
// 三者里只有这一条管得住「慢慢试」。
const (
	// MaxFailures 是锁定前允许的连续失败次数。
	MaxFailures = 5
	// LockoutBase 是首次锁定时长，之后按次数翻倍。
	LockoutBase = 30 * time.Second
	// LockoutMax 是锁定时长上限。
	//
	// 有上限是刻意的：无上限会让一次误操作把自己永久关在门外，而这个系统
	// **没有自助找回**——那时唯一的出路是回服务器敲命令。
	LockoutMax = 15 * time.Minute
	// FailureWindow 是失败计数的遗忘期：这么久没再失败就清零。
	FailureWindow = 30 * time.Minute
)

// ErrLockedOut 表示这个来源暂时被挡住了。
type ErrLockedOut struct{ Retry time.Duration }

func (e *ErrLockedOut) Error() string {
	return fmt.Sprintf("too many attempts, try again in %s", e.Retry.Round(time.Second))
}

type bucket struct {
	failures int
	last     time.Time
	until    time.Time
}

// Limiter 按来源做失败计数与锁定。
//
// **按 IP 而不是按用户**：只有一个账号，按用户等于全局——那样任何人都能
// 靠反复输错把真正的管理员锁在门外。按 IP 至少把影响限制在攻击者自己身上。
//
// 代价照实说：NAT 后面的一群人共享一个出口 IP，其中一个人反复输错会连累
// 其他人。在边缘/内网场景里这个代价可接受，而按用户锁的代价是**任何人都能
// 单方面把管理员锁死**，那更糟。
type Limiter struct {
	mu  sync.Mutex
	m   map[string]*bucket
	now func() time.Time
}

// NewLimiter 构造限流器。now 可替换，供测试固定时间。
func NewLimiter(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{m: map[string]*bucket{}, now: now}
}

// Check 在尝试之前调用；被锁定时返回 *ErrLockedOut。
func (l *Limiter) Check(key string) error {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now)

	b := l.m[key]
	if b == nil {
		return nil
	}
	if now.Before(b.until) {
		return &ErrLockedOut{Retry: b.until.Sub(now)}
	}
	return nil
}

// Fail 记一次失败，必要时开始锁定。
func (l *Limiter) Fail(key string) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.m[key]
	if b == nil || now.Sub(b.last) > FailureWindow {
		b = &bucket{}
		l.m[key] = b
	}
	b.failures++
	b.last = now

	if b.failures >= MaxFailures {
		// **指数退避**：连着试就越锁越久，而偶尔手滑一次几乎不受影响
		d := LockoutBase << (b.failures - MaxFailures)
		if d > LockoutMax || d <= 0 { // d<=0 防移位溢出
			d = LockoutMax
		}
		b.until = now.Add(d)
	}
}

// Succeed 清掉一个来源的失败记录。
func (l *Limiter) Succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, key)
}

func (l *Limiter) sweepLocked(now time.Time) {
	for k, b := range l.m {
		if now.Sub(b.last) > FailureWindow && now.After(b.until) {
			delete(l.m, k)
		}
	}
}
