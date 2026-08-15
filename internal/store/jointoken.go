package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/mecharion/mecharion/internal/store/sqlcgen"
)

type joinTokenRepo struct{ s *Store }

func (r *repos) JoinTokens() JoinTokenRepo { return &joinTokenRepo{s: r.s} }

func (r *joinTokenRepo) Create(ctx context.Context, t JoinToken) (JoinToken, error) {
	row, err := r.s.wq(ctx).CreateJoinToken(ctx, sqlcgen.CreateJoinTokenParams{
		SiteID: t.SiteID, TokenHash: t.Hash, NodeName: t.NodeName,
		ExpiresAt: FormatTime(t.ExpiresAt), MaxUses: int64(t.MaxUses),
		CreatedAt: FormatTime(t.CreatedAt), CreatedBy: t.CreatedBy,
	})
	if err != nil {
		return JoinToken{}, fmt.Errorf("store: creating join token: %w", err)
	}
	return joinTokenFrom(row)
}

func (r *joinTokenRepo) GetByHash(ctx context.Context, hash string) (JoinToken, error) {
	row, err := r.s.rq(ctx).GetJoinTokenByHash(ctx, hash)
	if err != nil {
		return JoinToken{}, notFound(err, "join token")
	}
	return joinTokenFrom(row)
}

func (r *joinTokenRepo) List(ctx context.Context, siteID int64) ([]JoinToken, error) {
	rows, err := r.s.rq(ctx).ListJoinTokens(ctx, siteID)
	if err != nil {
		return nil, fmt.Errorf("store: listing join tokens: %w", err)
	}
	out := make([]JoinToken, 0, len(rows))
	for _, row := range rows {
		t, err := joinTokenFrom(row)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (r *joinTokenRepo) Revoke(ctx context.Context, id int64, at time.Time) error {
	err := r.s.wq(ctx).RevokeJoinToken(ctx, sqlcgen.RevokeJoinTokenParams{
		RevokedAt: sql.NullString{String: FormatTime(at), Valid: true}, ID: id,
	})
	if err != nil {
		return fmt.Errorf("store: revoking join token: %w", err)
	}
	return nil
}

func joinTokenFrom(r sqlcgen.JoinToken) (JoinToken, error) {
	exp, err := ParseTime(r.ExpiresAt)
	if err != nil {
		return JoinToken{}, fmt.Errorf("store: parsing join_tokens.expires_at: %w", err)
	}
	created, err := ParseTime(r.CreatedAt)
	if err != nil {
		return JoinToken{}, fmt.Errorf("store: parsing join_tokens.created_at: %w", err)
	}
	out := JoinToken{
		ID: r.ID, SiteID: r.SiteID, Hash: r.TokenHash, NodeName: r.NodeName,
		ExpiresAt: exp, MaxUses: int(r.MaxUses), Used: int(r.Used),
		CreatedAt: created, CreatedBy: r.CreatedBy,
	}
	if r.RevokedAt.Valid {
		t, err := ParseTime(r.RevokedAt.String)
		if err != nil {
			return JoinToken{}, fmt.Errorf("store: parsing join_tokens.revoked_at: %w", err)
		}
		out.RevokedAt = &t
	}
	return out, nil
}
