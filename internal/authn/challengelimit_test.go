package authn

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

// 本文件验证：/auth/challenge 不需要凭据就能触发，而每次出题都要
// 服务端真算一次 Argon2、画两张滑块图——此前只有 Limiter.Check（只问
// 「这个来源有没有因为登录失败被锁定」，出题本身从不记录），所以拼命
// 出题（哪怕从不失败）是一条现成的 DoS。

func TestChallengeLimiterAllowsUpToPerIPLimit(t *testing.T) {
	now := time.Now()
	clock := &now
	l := NewChallengeLimiter(func() time.Time { return *clock })

	for i := 0; i < ChallengePerIPLimit; i++ {
		if err := l.Allow("10.0.0.1"); err != nil {
			t.Fatalf("第 %d 次不该被拒: %v", i+1, err)
		}
	}
	var rl *ErrChallengeRateLimited
	if err := l.Allow("10.0.0.1"); !errors.As(err, &rl) {
		t.Fatalf("第 %d 次应当被拒，实际 %v", ChallengePerIPLimit+1, err)
	}
}

func TestChallengeLimiterIsPerIP(t *testing.T) {
	l := NewChallengeLimiter(nil)
	for i := 0; i < ChallengePerIPLimit; i++ {
		if err := l.Allow("10.0.0.1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Allow("10.0.0.2"); err != nil {
		t.Errorf("别的来源不该受影响: %v", err)
	}
}

// TestChallengeLimiterGlobalCapCatchesDistributedSources 钉住全局限额：
// 单挡 IP 挡不住很多个来源各自不超限、合起来却超了的情况（僵尸网络、
// 代理池）。
func TestChallengeLimiterGlobalCapCatchesDistributedSources(t *testing.T) {
	l := NewChallengeLimiter(nil)
	allowed := 0
	for i := 0; i < ChallengeGlobalLimit+50; i++ {
		ip := "10.0.0." + strconv.Itoa(i%50) // 50 个不同来源，各自远低于单 IP 限额
		if err := l.Allow(ip); err == nil {
			allowed++
		}
	}
	if allowed > ChallengeGlobalLimit {
		t.Fatalf("放行次数 = %d，不该超过全局上限 %d", allowed, ChallengeGlobalLimit)
	}
}

// TestChallengeLimiterWindowSlides 钉住这是滑动窗口，不是「用一次封死」：
// 过了窗口期应当重新放行。
func TestChallengeLimiterWindowSlides(t *testing.T) {
	now := time.Now()
	clock := &now
	l := NewChallengeLimiter(func() time.Time { return *clock })

	for i := 0; i < ChallengePerIPLimit; i++ {
		if err := l.Allow("10.0.0.1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Allow("10.0.0.1"); err == nil {
		t.Fatal("此刻应当被限流")
	}

	*clock = clock.Add(ChallengeWindow + time.Second)
	if err := l.Allow("10.0.0.1"); err != nil {
		t.Errorf("窗口过后应当重新放行，实际仍被拒: %v", err)
	}
}

// TestChallengeLimiterRetryDurationIsMeaningful 钉住报错要说清等多久，
// 不能是「限流了」这种用户没法据此行动的信息。
func TestChallengeLimiterRetryDurationIsMeaningful(t *testing.T) {
	now := time.Now()
	clock := &now
	l := NewChallengeLimiter(func() time.Time { return *clock })

	for i := 0; i < ChallengePerIPLimit; i++ {
		if err := l.Allow("10.0.0.1"); err != nil {
			t.Fatal(err)
		}
	}
	var rl *ErrChallengeRateLimited
	err := l.Allow("10.0.0.1")
	if !errors.As(err, &rl) {
		t.Fatalf("应当是 ErrChallengeRateLimited，实际 %v", err)
	}
	if rl.Retry <= 0 || rl.Retry > ChallengeWindow {
		t.Errorf("Retry = %s，期望落在 (0, %s] 之间", rl.Retry, ChallengeWindow)
	}
}
