package store

import (
	"context"
	"fmt"
	"time"

	"github.com/mecharion/mecharion/internal/store/sqlcgen"
)

type sessionRepo struct{ s *Store }

func (r *repos) Sessions() SessionRepo { return &sessionRepo{s: r.s} }

func (r *sessionRepo) Create(ctx context.Context, s Session) error {
	err := r.s.wq(ctx).CreateSession(ctx, sqlcgen.CreateSessionParams{
		TokenHash: s.TokenHash, UserName: s.UserName,
		CreatedAt: FormatTime(s.CreatedAt), ExpiresAt: FormatTime(s.ExpiresAt),
	})
	if err != nil {
		return fmt.Errorf("store: creating session: %w", err)
	}
	return nil
}

func (r *sessionRepo) Get(ctx context.Context, hash string) (Session, error) {
	row, err := r.s.rq(ctx).GetSession(ctx, hash)
	if err != nil {
		return Session{}, notFound(err, "会话")
	}
	created, err := ParseTime(row.CreatedAt)
	if err != nil {
		return Session{}, err
	}
	expires, err := ParseTime(row.ExpiresAt)
	if err != nil {
		return Session{}, err
	}
	return Session{
		TokenHash: row.TokenHash, UserName: row.UserName,
		CreatedAt: created, ExpiresAt: expires,
	}, nil
}

func (r *sessionRepo) Delete(ctx context.Context, hash string) error {
	if err := r.s.wq(ctx).DeleteSession(ctx, hash); err != nil {
		return fmt.Errorf("store: deleting session: %w", err)
	}
	return nil
}

func (r *sessionRepo) DeleteOfUser(ctx context.Context, name string) error {
	if err := r.s.wq(ctx).DeleteSessionsOfUser(ctx, name); err != nil {
		return fmt.Errorf("store: clearing sessions: %w", err)
	}
	return nil
}

func (r *sessionRepo) DeleteExpired(ctx context.Context, before time.Time) error {
	if err := r.s.wq(ctx).DeleteExpiredSessions(ctx, FormatTime(before)); err != nil {
		return fmt.Errorf("store: clearing expired sessions: %w", err)
	}
	return nil
}
