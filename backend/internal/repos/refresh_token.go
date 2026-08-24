package repos

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RefreshToken is a stored refresh credential. Only the hash is persisted.
type RefreshToken struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// Active reports whether the token may still be exchanged.
func (t RefreshToken) Active() bool {
	return t.RevokedAt == nil && time.Now().Before(t.ExpiresAt)
}

// RefreshTokenRepo stores refresh tokens so sessions can be revoked.
type RefreshTokenRepo struct {
	pool *pgxpool.Pool
}

// NewRefreshTokenRepo builds a RefreshTokenRepo.
func NewRefreshTokenRepo(pool *pgxpool.Pool) *RefreshTokenRepo {
	return &RefreshTokenRepo{pool: pool}
}

// Create stores a refresh token hash for a user.
func (r *RefreshTokenRepo) Create(ctx context.Context, userID uuid.UUID, hash []byte, expiresAt time.Time) error {
	const query = `
		INSERT INTO refresh_tokens (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)`

	if _, err := r.pool.Exec(ctx, query, userID, hash, expiresAt); err != nil {
		return fmt.Errorf("create refresh token: %w", classify(err))
	}
	return nil
}

// GetByHash loads a token by its stored hash, whether or not it is still
// valid. Callers inspect Active to decide what to do, because a presented but
// already-revoked token is a meaningful signal rather than a plain miss.
func (r *RefreshTokenRepo) GetByHash(ctx context.Context, hash []byte) (*RefreshToken, error) {
	const query = `
		SELECT id, user_id, expires_at, revoked_at
		FROM refresh_tokens
		WHERE token_hash = $1`

	var t RefreshToken
	err := r.pool.QueryRow(ctx, query, hash).Scan(&t.ID, &t.UserID, &t.ExpiresAt, &t.RevokedAt)
	if err != nil {
		return nil, fmt.Errorf("get refresh token: %w", classify(err))
	}
	return &t, nil
}

// Revoke marks a single token as spent. Revoking an already-revoked token is
// a no-op, which keeps rotation idempotent under a retry.
func (r *RefreshTokenRepo) Revoke(ctx context.Context, id uuid.UUID) error {
	const query = `UPDATE refresh_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`

	if _, err := r.pool.Exec(ctx, query, id); err != nil {
		return fmt.Errorf("revoke refresh token: %w", classify(err))
	}
	return nil
}

// RevokeAllForUser invalidates every live session for a user. This runs when a
// revoked token is presented again, which suggests the token was stolen: the
// safe response is to end every session and force a fresh login.
func (r *RefreshTokenRepo) RevokeAllForUser(ctx context.Context, userID uuid.UUID) error {
	const query = `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`

	if _, err := r.pool.Exec(ctx, query, userID); err != nil {
		return fmt.Errorf("revoke user refresh tokens: %w", classify(err))
	}
	return nil
}

// DeleteExpired purges tokens that are past their expiry, keeping the table
// from growing without bound.
func (r *RefreshTokenRepo) DeleteExpired(ctx context.Context) (int64, error) {
	const query = `DELETE FROM refresh_tokens WHERE expires_at < now()`

	tag, err := r.pool.Exec(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("delete expired refresh tokens: %w", classify(err))
	}
	return tag.RowsAffected(), nil
}
