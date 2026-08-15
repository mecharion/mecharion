// Package authn 做登录前的两道关：PoW 与滑块。
//
// **分工要说清楚，否则会高估整体强度**（[ADR-0037]）：
//
//	PoW    给每次尝试标价。安全性来自算力成本，不参与 AI 军备竞赛
//	滑块   熟悉的交互、看得见的验证。**不提供安全性**，ML + 轨迹重放可破
//	限流   挡慢速撞库（在 ratelimit.go）
//
// 三者缺一都不够，而把滑块当成防护是最容易犯的错。
//
// [ADR-0037]: ../../docs/adr/0037-login-is-full-privilege.md
package authn

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// PoW 的代价参数。
//
// **内存硬**（Argon2id）而不是 SHA-256：后者对 GPU 友好，攻击者用一张显卡
// 就能把「成本」这件事抹平。代价是浏览器要靠 WASM 才算得动。
//
// 参数取得小是**刻意的**：服务端出题与验证各只算一次，而客户端平均要算
// Difficulty/2 次。让单次便宜、次数多，才是这个不对称的来源。
const (
	powMemory     = 4 * 1024 // 4 MiB
	powTime       = 1
	powThreads    = 1
	powKeyLen     = 16
	powDifficulty = 300
)

// TTL 是一道挑战的有效期。
//
// 短是刻意的：它同时限制了「预先囤一批算好的答案」这种玩法。
const TTL = 3 * time.Minute

var (
	// ErrChallengeUnknown 表示这个 id 没见过、已过期、或者已经用掉了。
	//
	// **三种情况回同一句话**：区分了就等于告诉攻击者「这个 id 曾经存在」。
	ErrChallengeUnknown = errors.New("verification expired, please try again")
	// ErrPoWWrong 表示 PoW 答案不对。
	ErrPoWWrong = errors.New("verification failed")
	// ErrSliderWrong 表示滑块没对上。
	ErrSliderWrong = errors.New("puzzle piece not aligned")
)

// Challenge 是发给浏览器的一道题。
type Challenge struct {
	ID string `json:"id"`
	// 客户端要找一个 n∈[0,Difficulty) 使 argon2id(salt, n) == PoWTarget。
	//
	// **目标必须下发**——它就是题面本身，不给的话客户端无从判断算对没算对。
	// 下发它不削弱什么：服务端仍然要自己重算一遍来核对，而攻击者省下的
	// 只是「知道答案长什么样」，那本来就不是秘密。
	PoWSalt       string `json:"powSalt"`
	PoWTarget     string `json:"powTarget"`
	PoWDifficulty int    `json:"powDifficulty"`
	PoWMemory     int    `json:"powMemory"`
	PoWTime       int    `json:"powTime"`
	// Background 与 Piece 是滑块的两张图（data URI）。
	Background string `json:"background"`
	Piece      string `json:"piece"`
	// PieceY 是拼图块的纵向位置；横向由用户拖。
	PieceY    int    `json:"pieceY"`
	ExpiresAt string `json:"expiresAt"`
}

// Answer 是浏览器交回来的答案。
type Answer struct {
	ID string `json:"id"`
	// PoW 是客户端找到的那个 n。
	PoW int `json:"pow"`
	// SliderX 是用户把拼图块拖到的横坐标。
	SliderX int `json:"sliderX"`
}

// issued 是一道已发出、尚未核销的挑战。
type issued struct {
	powSalt   []byte
	powTarget []byte
	sliderX   int
	expiresAt time.Time
}

// MaxPendingChallenges 是内存里同时保留的未核销挑战数上限。
//
// ChallengeLimiter 已经把出题的**速率**摁住了（全局每分钟 200 道），
// 但那是间接的界——万一限流参数以后被调大、或者限流层本身被绕过
// （比如未来加了别的出题入口忘了接限流），这里要有一道独立的、显式的
// 硬上限兜底，不依赖另一层今天恰好配得多保守。撞到上限时
// 直接拒绝新的出题请求，而不是驱逐最老的——驱逐会让一个正在填验证码
// 的正常用户的题被别人的洪水冲掉，拒绝新请求更公平：先来的先服务。
const MaxPendingChallenges = 2000

// ErrTooManyPending 表示未核销的挑战已经堆到上限，暂时不再出新题。
var ErrTooManyPending = errors.New("too many verification requests, please try again later")

// Store 保管已发出的挑战。
//
// **放内存，不落库**——与会话相反，这是刻意的：
//
//	挑战  丢了就重来一次，用户只是多点一下。TTL 三分钟，落库纯属浪费
//	会话  丢了所有人被登出，因此必须落库（见 mechd/session.go）
//
// 两者的取舍不同，因为「丢失的代价」差着量级。
type Store struct {
	mu   sync.Mutex
	m    map[string]issued
	now  func() time.Time
	last time.Time // 上次清理时刻
}

// NewStore 构造一个挑战存储。now 可替换，供测试固定时间。
func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{m: map[string]issued{}, now: now}
}

// Issue 出一道新题。
func (s *Store) Issue() (*Challenge, error) {
	// 硬上限检查放在**任何一次随机数生成、Argon2、画图之前**——
	// 拒绝本身必须便宜，否则这道防线自己也能被刷成一条 DoS。
	checkNow := s.now()
	s.mu.Lock()
	s.sweepLocked(checkNow)
	pending := len(s.m)
	s.mu.Unlock()
	if pending >= MaxPendingChallenges {
		return nil, ErrTooManyPending
	}

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generating salt: %w", err)
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, fmt.Errorf("generating id: %w", err)
	}

	// 服务端随机挑一个 n，算出目标；客户端要把它试出来。
	// **服务端只算一次**，客户端平均算 Difficulty/2 次——不对称就在这里。
	n, err := randInt(powDifficulty)
	if err != nil {
		return nil, err
	}
	target := powHash(salt, n)

	gapX, err := randInt(sliderMaxX - sliderMinX)
	if err != nil {
		return nil, err
	}
	gapX += sliderMinX
	gapY, err := randInt(bgHeight - pieceSize - 20)
	if err != nil {
		return nil, err
	}
	gapY += 10

	bg, piece, err := renderSlider(gapX, gapY)
	if err != nil {
		return nil, err
	}

	now := s.now()
	exp := now.Add(TTL)
	key := hex.EncodeToString(id)

	s.mu.Lock()
	s.sweepLocked(now)
	s.m[key] = issued{powSalt: salt, powTarget: target, sliderX: gapX, expiresAt: exp}
	s.mu.Unlock()

	return &Challenge{
		ID: key, PoWSalt: hex.EncodeToString(salt),
		PoWTarget:     hex.EncodeToString(target),
		PoWDifficulty: powDifficulty, PoWMemory: powMemory, PoWTime: powTime,
		Background: bg, Piece: piece, PieceY: gapY,
		ExpiresAt: exp.UTC().Format(time.RFC3339),
	}, nil
}

// Verify 核销一道题。
//
// **无论对错都先把它删掉**：一道题只能用一次。不删的话，攻击者算出一个
// 合法答案就能拿它重放无数次登录尝试，PoW 的成本压制归零。
func (s *Store) Verify(a Answer) error {
	now := s.now()

	s.mu.Lock()
	c, ok := s.m[a.ID]
	delete(s.m, a.ID) // ← 一次性核销，放在任何判定之前
	s.sweepLocked(now)
	s.mu.Unlock()

	if !ok || now.After(c.expiresAt) {
		return ErrChallengeUnknown
	}
	// 先验滑块：它便宜，而 PoW 要算一次 Argon2
	if abs(a.SliderX-c.sliderX) > sliderTolerance {
		return ErrSliderWrong
	}
	if a.PoW < 0 || a.PoW >= powDifficulty {
		return ErrPoWWrong
	}
	if subtle.ConstantTimeCompare(powHash(c.powSalt, a.PoW), c.powTarget) != 1 {
		return ErrPoWWrong
	}
	return nil
}

// Pending 返回还没被核销的题数，供测试与诊断。
func (s *Store) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// sweepLocked 清掉过期的题。
//
// 顺带做而不是起一条后台 goroutine：出题与核销本来就是这张表唯一的入口，
// 借它们的时机清理足够了，而少一条 goroutine 就少一处要关的东西。
func (s *Store) sweepLocked(now time.Time) {
	if now.Sub(s.last) < time.Minute {
		return
	}
	s.last = now
	for k, v := range s.m {
		if now.After(v.expiresAt) {
			delete(s.m, k)
		}
	}
}

func powHash(salt []byte, n int) []byte {
	return argon2.IDKey([]byte(fmt.Sprint(n)), salt,
		powTime, powMemory, powThreads, powKeyLen)
}

func randInt(max int) (int, error) {
	if max <= 0 {
		return 0, nil
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return 0, fmt.Errorf("generating random number: %w", err)
	}
	return int(n.Int64()), nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
