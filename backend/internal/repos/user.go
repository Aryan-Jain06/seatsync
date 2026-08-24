package repos

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
)

// UserRepo reads and writes user accounts.
type UserRepo struct {
	pool *pgxpool.Pool
}

// NewUserRepo builds a UserRepo.
func NewUserRepo(pool *pgxpool.Pool) *UserRepo { return &UserRepo{pool: pool} }

const userColumns = `id, email, password_hash, name, role, created_at`

// Create inserts a user. It returns ErrDuplicateKey when the email is taken.
func (r *UserRepo) Create(ctx context.Context, email, passwordHash, name string, role models.Role) (*models.User, error) {
	const query = `
		INSERT INTO users (email, password_hash, name, role)
		VALUES ($1, $2, $3, $4)
		RETURNING ` + userColumns

	var u models.User
	err := r.pool.QueryRow(ctx, query, email, passwordHash, name, role).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Name, &u.Role, &u.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", classify(err))
	}
	return &u, nil
}

// GetByEmail looks a user up case-insensitively, matching the unique index.
func (r *UserRepo) GetByEmail(ctx context.Context, email string) (*models.User, error) {
	const query = `SELECT ` + userColumns + ` FROM users WHERE lower(email) = lower($1)`

	var u models.User
	err := r.pool.QueryRow(ctx, query, email).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Name, &u.Role, &u.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get user by email: %w", classify(err))
	}
	return &u, nil
}

// GetByID loads a user by primary key.
func (r *UserRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.User, error) {
	const query = `SELECT ` + userColumns + ` FROM users WHERE id = $1`

	var u models.User
	err := r.pool.QueryRow(ctx, query, id).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Name, &u.Role, &u.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get user by id: %w", classify(err))
	}
	return &u, nil
}
