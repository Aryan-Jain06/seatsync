// Package repos contains the data access layer. Every method takes a context
// and returns wrapped errors; no method panics.
package repos

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel errors the service layer maps onto HTTP responses.
var (
	ErrNotFound      = errors.New("repos: record not found")
	ErrDuplicateKey  = errors.New("repos: record already exists")
	ErrSeatConfirmed = errors.New("repos: seat is already confirmed for this event")
)

// Postgres error codes worth distinguishing.
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
)

// confirmedSeatIndex is the partial unique index that guarantees a seat is
// sold at most once per event. A violation of specifically this index means a
// concurrent booking won the race, which callers translate into a 409.
const confirmedSeatIndex = "uq_booking_seats_confirmed_seat"

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx, letting repository
// methods run either standalone or inside a caller's transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// classify converts a driver error into one of the package's sentinels.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgUniqueViolation:
			if pgErr.ConstraintName == confirmedSeatIndex {
				return ErrSeatConfirmed
			}
			return ErrDuplicateKey
		case pgForeignKeyViolation:
			return ErrNotFound
		}
	}
	return err
}

// InTx runs fn inside a transaction, committing on success and rolling back on
// error or panic. The rollback is best-effort: once fn has failed, a rollback
// failure adds no information the caller can act on.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	if err = fn(tx); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
