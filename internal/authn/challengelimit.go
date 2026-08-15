package authn

import (
	"fmt"
	"sync"
	"time"
)

// ChallengeLimiter 限出题的**速率**，与 Limiter 是两件不同的事。
//
// Limiter 挡的是「反复试错」——只有失败才计数，登录之前的 Check 本身不
// 消耗任何配额。出题（/auth/challenge）不需要任何凭据就能触发，且每次
// 都要服务端真算一次 Argon2、画两张滑块图；不失败、只是拼命出题本身
// 就是一条现成的 DoS。这里按来源 IP 与全局各设一个滑动窗口
// 限额，在做那些昂贵的工作**之前**拒绝。
const (
	// ChallengePerIPLimit 是单个来源每个窗口内允许出的题数。
	ChallengePerIPLimit = 10
	// ChallengeGlobalLimit 是全部来源合计每个窗口内允许出的题数——
	// 单挡 IP 挡不住分布式来源（僵尸网络、代理池）合起来的洪水。
	ChallengeGlobalLimit = 200
	// ChallengeWindow 是滑动窗口的宽度。
	ChallengeWindow = time.Minute
)

// ErrChallengeRateLimited 表示这一次出题请求被限流拒绝。
type ErrChallengeRateLimited struct{ Retry time.Duration }

func (e *ErrChallengeRateLimited) Error() string {
	return fmt.Sprintf("出题过于频繁，请 %s 后再试", e.Retry.Round(time.Second))
}

// ChallengeLimiter 是出题速率限流器。
type ChallengeLimiter struct {
	mu     sync.Mutex
	perIP  map[string][]time.Time
	global []time.Time
	now    func() time.Time
	// lastSweep 节流 IP 表的清理——与 Limiter.sweepLocked 同一个理由：
	// 清理本身不值得单独开一条 goroutine，借请求的时机顺便做。
	lastSweep time.Time
}

// NewChallengeLimiter 构造出题限流器。now 可替换，供测试固定时间。
func NewChallengeLimiter(now func() time.Time) *ChallengeLimiter {
	if now == nil {
		now = time.Now
	}
	return &ChallengeLimiter{perIP: map[string][]time.Time{}, now: now}
}

// Allow 判断这一次出题是否放行；放行的同时**原子地**记一次配额消耗。
//
// 先查全局、再查单 IP：全局限额兜住的是「谁都没超自己的份额，但合起来
// 已经太多」——只查 IP 挡不住这种情况。
func (l *ChallengeLimiter) Allow(ip string) error {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweepLocked(now)

	l.global = prune(l.global, now)
	if len(l.global) >= ChallengeGlobalLimit {
		return &ErrChallengeRateLimited{Retry: retryAfter(l.global, now)}
	}
	bucket := prune(l.perIP[ip], now)
	if len(bucket) >= ChallengePerIPLimit {
		l.perIP[ip] = bucket
		return &ErrChallengeRateLimited{Retry: retryAfter(bucket, now)}
	}

	l.global = append(l.global, now)
	l.perIP[ip] = append(bucket, now)
	return nil
}

// sweepLocked 清掉长时间没有新请求的 IP 桶。
//
// **不清的话 perIP 这张 map 本身会无界增长**——即便每个桶都被窗口限住，
// 攻击者只要换着 IP 打（或者哪怕是正常流量里来去的访客），key 的数量
// 也会一直涨。与 ChallengeWindow 同一个节奏，够用且不需要独立的过期
// 时间戳字段。
func (l *ChallengeLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < ChallengeWindow {
		return
	}
	l.lastSweep = now
	for ip, times := range l.perIP {
		if len(prune(times, now)) == 0 {
			delete(l.perIP, ip)
		}
	}
}

// prune 丢掉窗口之外的时间戳，保留顺序（旧到新）。
func prune(times []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-ChallengeWindow)
	i := 0
	for i < len(times) && times[i].Before(cutoff) {
		i++
	}
	if i == 0 {
		return times
	}
	return append([]time.Time{}, times[i:]...)
}

// retryAfter 算出窗口里最早一条记录多久后过期，即最快什么时候能再试。
func retryAfter(times []time.Time, now time.Time) time.Duration {
	if len(times) == 0 {
		return 0
	}
	d := times[0].Add(ChallengeWindow).Sub(now)
	if d < 0 {
		return 0
	}
	return d
}
