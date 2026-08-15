package authn

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 本文件钉住 **M8 第 3 步**登录前那两道关与限流。
//
// 三者的分工必须分别验，否则很容易把「整体过了」当成「每一层都在工作」：
//
//	PoW    成本压制——一次性核销是它的全部依据
//	滑块   看得见的验证——**不提供安全性**
//	限流   挡慢速撞库——PoW 和滑块都挡不住

// solveFor 用暴力法找出正确答案——正是客户端要做的事。
func solveFor(t *testing.T, s *Store, c *Challenge, sliderX int) Answer {
	t.Helper()
	s.mu.Lock()
	got := s.m[c.ID]
	s.mu.Unlock()

	for n := 0; n < c.PoWDifficulty; n++ {
		if string(powHash(got.powSalt, n)) == string(got.powTarget) {
			return Answer{ID: c.ID, PoW: n, SliderX: sliderX}
		}
	}
	t.Fatal("在难度范围内没找到答案——出题逻辑有问题")
	return Answer{}
}

// realSliderX 取出服务端手里的正确缺口位置。
func realSliderX(s *Store, id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[id].sliderX
}

func TestIssueAndVerify(t *testing.T) {
	s := NewStore(nil)
	c, err := s.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if c.Background == "" || c.Piece == "" {
		t.Fatal("应当带上两张图")
	}
	if !strings.HasPrefix(c.Background, "data:image/png;base64,") {
		t.Errorf("背景应当是 data URI，实际 %.40s", c.Background)
	}

	a := solveFor(t, s, c, realSliderX(s, c.ID))
	if err := s.Verify(a); err != nil {
		t.Fatalf("正确答案应当通过: %v", err)
	}
}

// TestChallengeIsOneShot 是这一步最重要的一条。
//
// **不核销的话，PoW 的成本压制归零**：攻击者算一次合法答案，就能拿它重放
// 无数次登录尝试——而那正是 PoW 要阻止的事。
func TestChallengeIsOneShot(t *testing.T) {
	s := NewStore(nil)
	c, _ := s.Issue()
	a := solveFor(t, s, c, realSliderX(s, c.ID))

	if err := s.Verify(a); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(a); !errors.Is(err, ErrChallengeUnknown) {
		t.Fatalf("重放同一个 challenge 必须被拒，实际 %v", err)
	}
}

// TestFailedAttemptAlsoBurnsTheChallenge 钉住答错也要核销。
//
// 只在成功时核销的话，攻击者可以拿同一道题反复试滑块位置——**一次 PoW
// 的成本被摊到几十次尝试上**。
func TestFailedAttemptAlsoBurnsTheChallenge(t *testing.T) {
	s := NewStore(nil)
	c, _ := s.Issue()
	good := solveFor(t, s, c, realSliderX(s, c.ID))

	// 先用错的滑块位置试一次
	bad := good
	bad.SliderX = good.SliderX + 100
	if err := s.Verify(bad); err == nil {
		t.Fatal("错的滑块位置应当被拒")
	}
	// 现在拿正确答案来——也该被拒，因为题已经烧掉了
	if err := s.Verify(good); !errors.Is(err, ErrChallengeUnknown) {
		t.Fatalf("答错之后那道题也该作废，实际 %v", err)
	}
}

// TestWrongPoWRejected 钉住 PoW 答案要真的对。
func TestWrongPoWRejected(t *testing.T) {
	s := NewStore(nil)
	c, _ := s.Issue()
	a := solveFor(t, s, c, realSliderX(s, c.ID))
	a.PoW = (a.PoW + 1) % c.PoWDifficulty

	// 重新出一道，因为上面那道还没被核销
	c2, _ := s.Issue()
	good := solveFor(t, s, c2, realSliderX(s, c2.ID))
	bad := good
	bad.PoW = (good.PoW + 1) % c2.PoWDifficulty
	if err := s.Verify(bad); !errors.Is(err, ErrPoWWrong) {
		t.Fatalf("错的 PoW 应当被拒，实际 %v", err)
	}
}

// TestSliderTolerance 钉住容差：几像素的偏差要放行，大偏差要拒。
//
// 容差太小会让正常人反复失败（鼠标精度就那样），太大等于没验。
func TestSliderTolerance(t *testing.T) {
	for _, tc := range []struct {
		off  int
		pass bool
	}{{0, true}, {sliderTolerance, true}, {sliderTolerance + 1, false}, {-50, false}} {
		s := NewStore(nil)
		c, _ := s.Issue()
		a := solveFor(t, s, c, realSliderX(s, c.ID)+tc.off)
		err := s.Verify(a)
		if tc.pass && err != nil {
			t.Errorf("偏差 %d 应当放行，实际 %v", tc.off, err)
		}
		if !tc.pass && !errors.Is(err, ErrSliderWrong) {
			t.Errorf("偏差 %d 应当被拒，实际 %v", tc.off, err)
		}
	}
}

// TestExpired 钉住过期的题不认。
func TestExpired(t *testing.T) {
	now := time.Now()
	clock := &now
	s := NewStore(func() time.Time { return *clock })

	c, _ := s.Issue()
	a := solveFor(t, s, c, realSliderX(s, c.ID))

	*clock = now.Add(TTL + time.Second)
	if err := s.Verify(a); !errors.Is(err, ErrChallengeUnknown) {
		t.Fatalf("过期的题应当被拒，实际 %v", err)
	}
}

// TestUnknownAndExpiredLookTheSame 钉住三种失效说同一句话。
//
// 区分了就等于告诉攻击者「这个 id 曾经存在」。
func TestUnknownAndExpiredLookTheSame(t *testing.T) {
	s := NewStore(nil)
	if err := s.Verify(Answer{ID: "从没见过"}); !errors.Is(err, ErrChallengeUnknown) {
		t.Errorf("没见过的 id 应当回 ErrChallengeUnknown，实际 %v", err)
	}
}

// TestSweepDropsExpired 钉住过期的题会被清掉。
//
// 不清的话，一个只出题不作答的脚本能把内存撑爆——而出题是**不需要任何
// 凭据**的（登录页在登录前就要拿到题）。
func TestSweepDropsExpired(t *testing.T) {
	now := time.Now()
	clock := &now
	s := NewStore(func() time.Time { return *clock })

	for i := 0; i < 20; i++ {
		if _, err := s.Issue(); err != nil {
			t.Fatal(err)
		}
	}
	if s.Pending() != 20 {
		t.Fatalf("应当有 20 道待答，实际 %d", s.Pending())
	}

	*clock = now.Add(TTL + time.Minute + time.Second)
	if _, err := s.Issue(); err != nil {
		t.Fatal(err)
	}
	if n := s.Pending(); n != 1 {
		t.Errorf("过期的应当被清掉，只剩刚出的那道，实际 %d", n)
	}
}

// TestIssueRejectsWhenPendingCapReached 验证的是另一道独立防线：
// ChallengeLimiter 挡的是出题的**速率**，这里挡的是内存里囤积的
// **总量**——两者是独立的两道防线，万一限流参数以后被调宽，或者未来
// 加了别的出题入口忘了接限流，这里仍然兜得住「一直不核销、囤到内存
// 撑爆」这条路。
func TestIssueRejectsWhenPendingCapReached(t *testing.T) {
	now := time.Now()
	s := NewStore(func() time.Time { return now })

	// 直接灌 s.m 而不是真调 MaxPendingChallenges 次 Issue()：后者每次都要
	// 算一次 Argon2、画两张图，2000 次会让这条测试跑到二十秒——那种测试
	// 没人会在改一行代码之后愿意等。这里只关心「到了上限该不该拒」，
	// 不重复验证 Issue() 出的题本身对不对，那部分已经被别的测试钉住了。
	s.mu.Lock()
	for i := 0; i < MaxPendingChallenges; i++ {
		s.m[fmt.Sprintf("dummy-%d", i)] = issued{expiresAt: now.Add(TTL)}
	}
	s.mu.Unlock()

	if _, err := s.Issue(); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("达到上限后应当拒绝，实际 %v", err)
	}
	if s.Pending() != MaxPendingChallenges {
		t.Errorf("待答数应当停在上限 %d，实际 %d", MaxPendingChallenges, s.Pending())
	}
}

// ── 限流 ────────────────────────────────────────────────────────────────

func TestLimiterLocksAfterRepeatedFailures(t *testing.T) {
	now := time.Now()
	clock := &now
	l := NewLimiter(func() time.Time { return *clock })

	for i := 0; i < MaxFailures-1; i++ {
		l.Fail("10.0.0.1")
		if err := l.Check("10.0.0.1"); err != nil {
			t.Fatalf("第 %d 次失败后不该锁: %v", i+1, err)
		}
	}
	l.Fail("10.0.0.1")

	var locked *ErrLockedOut
	err := l.Check("10.0.0.1")
	if !errors.As(err, &locked) {
		t.Fatalf("连续失败 %d 次之后应当锁定，实际 %v", MaxFailures, err)
	}
	// **错误信息要说清等多久**，否则用户只能瞎试
	if !strings.Contains(locked.Error(), "try again in") {
		t.Errorf("锁定信息该说清要等多久，实际 %q", locked.Error())
	}
}

// TestLimiterIsPerSource 钉住一个来源被锁不影响别人。
//
// 按用户锁的话，只有一个账号意味着**任何人都能靠反复输错把管理员锁死**。
func TestLimiterIsPerSource(t *testing.T) {
	l := NewLimiter(nil)
	for i := 0; i < MaxFailures; i++ {
		l.Fail("10.0.0.1")
	}
	if err := l.Check("10.0.0.1"); err == nil {
		t.Fatal("该来源应当被锁")
	}
	if err := l.Check("10.0.0.2"); err != nil {
		t.Errorf("别的来源不该受影响: %v", err)
	}
}

// TestLockoutBacksOffButHasACeiling 钉住指数退避与上限。
//
// **上限不能省**：这个系统没有自助找回，无上限的锁定会让一次误操作把人
// 永久关在门外。
func TestLockoutBacksOffButHasACeiling(t *testing.T) {
	now := time.Now()
	clock := &now
	l := NewLimiter(func() time.Time { return *clock })

	for i := 0; i < 40; i++ {
		l.Fail("10.0.0.1")
	}
	var locked *ErrLockedOut
	if !errors.As(l.Check("10.0.0.1"), &locked) {
		t.Fatal("应当被锁")
	}
	if locked.Retry > LockoutMax {
		t.Errorf("锁定时长应当有上限 %s，实际 %s", LockoutMax, locked.Retry)
	}
}

// TestSuccessClearsFailures 钉住登录成功之后计数清零。
func TestSuccessClearsFailures(t *testing.T) {
	l := NewLimiter(nil)
	for i := 0; i < MaxFailures-1; i++ {
		l.Fail("10.0.0.1")
	}
	l.Succeed("10.0.0.1")
	for i := 0; i < MaxFailures-1; i++ {
		l.Fail("10.0.0.1")
	}
	if err := l.Check("10.0.0.1"); err != nil {
		t.Errorf("成功之后计数该清零，实际又被锁了: %v", err)
	}
}
