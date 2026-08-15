package store

import (
	"context"
	"fmt"
	"time"

	"github.com/mecharion/mecharion/internal/store/sqlcgen"
)

type userRepo struct{ s *Store }

func (r *repos) Users() UserRepo { return &userRepo{s: r.s} }

func (r *userRepo) Create(ctx context.Context, name, hash string, at time.Time) (User, error) {
	row, err := r.s.wq(ctx).CreateUser(ctx, sqlcgen.CreateUserParams{
		Name: name, PasswordHash: hash,
		CreatedAt: FormatTime(at), UpdatedAt: FormatTime(at),
	})
	if err != nil {
		return User{}, fmt.Errorf("store: creating user: %w", err)
	}
	return userFrom(row)
}

func (r *userRepo) GetByName(ctx context.Context, name string) (User, error) {
	row, err := r.s.rq(ctx).GetUserByName(ctx, name)
	if err != nil {
		return User{}, notFound(err, "用户")
	}
	return userFrom(row)
}

func (r *userRepo) List(ctx context.Context) ([]User, error) {
	rows, err := r.s.rq(ctx).ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: listing users: %w", err)
	}
	out := make([]User, 0, len(rows))
	for _, row := range rows {
		u, err := userFrom(row)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

func (r *userRepo) Count(ctx context.Context) (int, error) {
	n, err := r.s.rq(ctx).CountUsers(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: counting users: %w", err)
	}
	return int(n), nil
}

func (r *userRepo) SetPassword(ctx context.Context, name, hash string, at time.Time) error {
	err := r.s.wq(ctx).SetUserPassword(ctx, sqlcgen.SetUserPasswordParams{
		PasswordHash: hash, UpdatedAt: FormatTime(at), Name: name,
	})
	if err != nil {
		return fmt.Errorf("store: changing password: %w", err)
	}
	return nil
}

func (r *userRepo) Delete(ctx context.Context, name string) error {
	if err := r.s.wq(ctx).DeleteUser(ctx, name); err != nil {
		return fmt.Errorf("store: deleting user: %w", err)
	}
	return nil
}

func userFrom(r sqlcgen.User) (User, error) {
	created, err := ParseTime(r.CreatedAt)
	if err != nil {
		return User{}, err
	}
	updated, err := ParseTime(r.UpdatedAt)
	if err != nil {
		return User{}, err
	}
	return User{
		ID: r.ID, Name: r.Name, PasswordHash: r.PasswordHash,
		CreatedAt: created, UpdatedAt: updated,
	}, nil
}
