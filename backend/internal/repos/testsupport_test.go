package repos_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/Aryan-Jain06/seatsync/backend/internal/db"
)

// testDatabaseURL points these tests at a scratch database so they can
// truncate freely without touching development data.
func testDatabaseURL(t *testing.T) string {
	t.Helper()

	base := os.Getenv("TEST_DATABASE_URL")
	if base != "" {
		return base
	}

	base = os.Getenv("DATABASE_URL")
	if base == "" {
		base = "postgres://seatsync:seatsync@localhost:5432/seatsync?sslmode=disable"
	}
	// Redirect to a sibling database named <db>_test.
	return strings.Replace(base, "/seatsync?", "/seatsync_test?", 1)
}

// newTestPool prepares the scratch database and returns a pool to it.
// The suite skips when no Postgres is reachable.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := testDatabaseURL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := ensureDatabase(ctx, url); err != nil {
		t.Skipf("postgres not available for integration tests: %v", err)
	}

	if err := db.MigrateUp(url); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}

	pool, err := db.Connect(ctx, url)
	require.NoError(t, err)

	t.Cleanup(pool.Close)
	truncateAll(t, pool)
	return pool
}

// ensureDatabase creates the scratch database if it is missing.
func ensureDatabase(ctx context.Context, url string) error {
	adminURL := strings.Replace(url, "/seatsync_test?", "/postgres?", 1)

	admin, err := db.Connect(ctx, adminURL)
	if err != nil {
		return err
	}
	defer admin.Close()

	var exists bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'seatsync_test')`).Scan(&exists); err != nil {
		return fmt.Errorf("check for test database: %w", err)
	}
	if exists {
		return nil
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE seatsync_test`); err != nil {
		return fmt.Errorf("create test database: %w", err)
	}
	return nil
}

func truncateAll(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	const query = `
		TRUNCATE payments, booking_seats, bookings, events, seats, venues, refresh_tokens, users
		RESTART IDENTITY CASCADE`

	_, err := pool.Exec(context.Background(), query)
	require.NoError(t, err)
}

// fixture is a minimal but complete scenario: one venue, one event, some
// seats and two users.
type fixture struct {
	VenueID uuid.UUID
	EventID uuid.UUID
	SeatIDs []uuid.UUID
	Alice   uuid.UUID
	Bob     uuid.UUID
}

func newFixture(t *testing.T, pool *pgxpool.Pool, seatCount int) fixture {
	t.Helper()
	ctx := context.Background()

	var f fixture

	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO venues (name, city) VALUES ('Test Arena', 'Pune') RETURNING id`).Scan(&f.VenueID))

	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO events (venue_id, title, starts_at, base_price)
		 VALUES ($1, 'Test Event', now() + interval '30 days', 100000) RETURNING id`,
		f.VenueID).Scan(&f.EventID))

	for i := 1; i <= seatCount; i++ {
		var seatID uuid.UUID
		require.NoError(t, pool.QueryRow(ctx,
			`INSERT INTO seats (venue_id, section, "row", number, price_multiplier)
			 VALUES ($1, 'A', 1, $2, 1.00) RETURNING id`,
			f.VenueID, i).Scan(&seatID))
		f.SeatIDs = append(f.SeatIDs, seatID)
	}

	for _, u := range []struct {
		email string
		into  *uuid.UUID
	}{
		{"alice@test.local", &f.Alice},
		{"bob@test.local", &f.Bob},
	} {
		require.NoError(t, pool.QueryRow(ctx,
			`INSERT INTO users (email, password_hash, name) VALUES ($1, 'x', 'Test') RETURNING id`,
			u.email).Scan(u.into))
	}

	return f
}
