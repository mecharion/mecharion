package mechd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/mecharion/mecharion/internal/store"
)

// SessionTTL 是一次登录的有效期。
//
// 不做「记住我」：那是一个把有效期拉到几十天的开关，而这个系统只有一个
// 全权账号——会话被偷走的代价与 admin token 泄漏相当。
const SessionTTL = 12 * time.Hour

// SessionCookie 是会话 cookie 的名字。
const SessionCookie = "m7n_session"

// ErrNoSession 表示没有有效会话。
var ErrNoSession = errors.New("not signed in, or the session has expired")

// hashSession 返回会话 token 的存储形态。
func hashSession(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// NewSession 建一次会话，返回**明文 token**（只在这一刻存在）。
func (s *Service) NewSession(ctx context.Context, user string) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("generating session token: %w", err)
	}
	tok := hex.EncodeToString(raw)
	now := s.now()
	exp := now.Add(SessionTTL)

	// 顺手清过期的：会话表只有登录与校验两个入口，借它们的时机清理就够了，
	// 少一条后台 goroutine 就少一处要关的东西
	_ = s.Repos.Sessions().DeleteExpired(ctx, now)

	if err := s.Repos.Sessions().Create(ctx, store.Session{
		TokenHash: hashSession(tok), UserName: user,
		CreatedAt: now, ExpiresAt: exp,
	}); err != nil {
		return "", time.Time{}, err
	}
	return tok, exp, nil
}

// LookupSession 校验会话 token，返回它属于谁。
func (s *Service) LookupSession(ctx context.Context, tok string) (string, error) {
	if tok == "" {
		return "", ErrNoSession
	}
	sess, err := s.Repos.Sessions().Get(ctx, hashSession(tok))
	if err != nil {
		return "", ErrNoSession
	}
	if s.now().After(sess.ExpiresAt) {
		// 过期的顺手删掉，别让它一直躺在表里
		_ = s.Repos.Sessions().Delete(ctx, sess.TokenHash)
		return "", ErrNoSession
	}
	return sess.UserName, nil
}

// EndSession 结束一次会话。
func (s *Service) EndSession(ctx context.Context, tok string) error {
	if tok == "" {
		return nil
	}
	return s.Repos.Sessions().Delete(ctx, hashSession(tok))
}

// EndAllSessions 清掉某人的全部会话。
//
// **改口令之后必须调它**：不清的话，一个已经被偷走的会话在改完口令之后
// 仍然有效——而改口令的动机通常正是「怀疑被偷了」。
func (s *Service) EndAllSessions(ctx context.Context, user string) error {
	return s.Repos.Sessions().DeleteOfUser(ctx, user)
}
