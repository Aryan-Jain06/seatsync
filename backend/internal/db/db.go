// Package db owns the PostgreSQL connection pool and schema migrations.
package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aryan-Jain06/seatsync/backend/migrations"
)

// Connect opens a pooled connection to Postgres and verifies it is reachable.
//
// The pool is sized for the load test's burst of concurrent bookings: the
// default of 4 connections would serialise those requests and turn a
// concurrency test into a queueing test.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	cfg.MaxConns = 25
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// WaitForDatabase blocks until Postgres accepts a connection or the timeout
// elapses. Compose healthchecks cover the normal case; this makes the binary
// robust when started against a database that is still booting.
func WaitForDatabase(ctx context.Context, databaseURL string, timeout time.Duration) (*pgxpool.Pool, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error

	for attempt := 1; ; attempt++ {
		pool, err := Connect(ctx, databaseURL)
		if err == nil {
			return pool, nil
		}
		lastErr = err

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("database unreachable after %s: %w", timeout, lastErr)
		}

		slog.Warn("database not ready, retrying", "attempt", attempt, "error", err)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// newMigrator builds a migrate instance over the embedded SQL files.
func newMigrator(databaseURL string) (*migrate.Migrate, error) {
	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("open embedded migrations: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", source, normalizeURL(databaseURL))
	if err != nil {
		return nil, fmt.Errorf("initialise migrator: %w", err)
	}
	return m, nil
}

// normalizeURL rewrites a postgres:// URL to the pgx5:// scheme that
// golang-migrate's pgx/v5 driver registers itself under.
func normalizeURL(databaseURL string) string {
	for _, prefix := range []string{"postgres://", "postgresql://"} {
		if len(databaseURL) >= len(prefix) && databaseURL[:len(prefix)] == prefix {
			return "pgx5://" + databaseURL[len(prefix):]
		}
	}
	return databaseURL
}

// MigrateUp applies every outstanding migration. It is safe to call on every
// boot: with no pending migrations it is a no-op.
func MigrateUp(databaseURL string) error {
	m, err := newMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer closeMigrator(m)

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}

	version, dirty, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return fmt.Errorf("read schema version: %w", err)
	}
	slog.Info("migrations applied", "version", version, "dirty", dirty)
	return nil
}

// MigrateDown rolls back the given number of migrations.
func MigrateDown(databaseURL string, steps int) error {
	if steps <= 0 {
		return fmt.Errorf("steps must be positive, got %d", steps)
	}

	m, err := newMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer closeMigrator(m)

	if err := m.Steps(-steps); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("roll back %d migration(s): %w", steps, err)
	}
	return nil
}

// MigrateVersion reports the applied schema version.
func MigrateVersion(databaseURL string) (version uint, dirty bool, err error) {
	m, err := newMigrator(databaseURL)
	if err != nil {
		return 0, false, err
	}
	defer closeMigrator(m)

	version, dirty, err = m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read schema version: %w", err)
	}
	return version, dirty, nil
}

// closeMigrator reports close failures without masking the caller's result.
func closeMigrator(m *migrate.Migrate) {
	if sourceErr, dbErr := m.Close(); sourceErr != nil || dbErr != nil {
		slog.Warn("migrator close reported errors", "source_error", sourceErr, "db_error", dbErr)
	}
}

// Ensure the pgx/v5 migrate driver is linked in.
var _ = migratepgx.Postgres{}
