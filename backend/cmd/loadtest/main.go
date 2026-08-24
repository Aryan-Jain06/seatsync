// Command loadtest provisions a clean scenario for the concurrency proof: a
// fresh venue with a fixed number of seats, one event over them, and a pool
// of users with ready-minted access tokens.
//
// Tokens are minted here rather than obtained by logging in, because bcrypt
// costs roughly 60ms per login and 500 of them would spend half a minute
// warming up a test that is meant to measure seat contention.
//
//	go run ./cmd/loadtest -seats 50 -users 500 > scenario.json
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"flag"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/Aryan-Jain06/seatsync/backend/internal/auth"
	"github.com/Aryan-Jain06/seatsync/backend/internal/config"
	"github.com/Aryan-Jain06/seatsync/backend/internal/db"
	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
)

// Scenario is the JSON handed to the k6 script.
type Scenario struct {
	EventID   string   `json:"event_id"`
	VenueID   string   `json:"venue_id"`
	SeatIDs   []string `json:"seat_ids"`
	Tokens    []string `json:"tokens"`
	SeatCount int      `json:"seat_count"`
	UserCount int      `json:"user_count"`
	BasePrice int64    `json:"base_price"`
}

func main() {
	if err := run(); err != nil {
		slog.Error("loadtest setup failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	seats := flag.Int("seats", 50, "number of seats in the test event")
	users := flag.Int("users", 500, "number of users to provision")
	flag.Parse()

	if *seats < 1 || *users < 1 {
		return fmt.Errorf("seats and users must both be positive")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	pool, err := db.WaitForDatabase(ctx, cfg.DatabaseURL, 30*time.Second)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// A unique suffix keeps repeated runs from colliding on the user email
	// unique index, and leaves previous runs' data intact for inspection.
	runID := uuid.NewString()[:8]

	var venueID uuid.UUID
	if err := tx.QueryRow(ctx,
		`INSERT INTO venues (name, city) VALUES ($1, 'Load Test') RETURNING id`,
		"Load Test Arena "+runID).Scan(&venueID); err != nil {
		return fmt.Errorf("insert venue: %w", err)
	}

	// One flat section, every seat the same price, so the proof is about
	// contention and not about pricing.
	seatRows := make([][]any, 0, *seats)
	for i := 1; i <= *seats; i++ {
		seatRows = append(seatRows, []any{venueID, "A", 1, i, 1.00})
	}
	if _, err := tx.CopyFrom(ctx,
		pgx.Identifier{"seats"},
		[]string{"venue_id", "section", "row", "number", "price_multiplier"},
		pgx.CopyFromRows(seatRows),
	); err != nil {
		return fmt.Errorf("insert seats: %w", err)
	}

	var eventID uuid.UUID
	if err := tx.QueryRow(ctx,
		`INSERT INTO events (venue_id, title, description, starts_at, base_price)
		 VALUES ($1, $2, 'Concurrency proof', now() + interval '30 days', 100000)
		 RETURNING id`,
		venueID, "Load Test Event "+runID).Scan(&eventID); err != nil {
		return fmt.Errorf("insert event: %w", err)
	}

	// One bcrypt hash, reused for every user: these accounts are never
	// logged into, so the hash only needs to be well formed.
	hash, err := bcrypt.GenerateFromPassword([]byte("loadtest-password"), bcrypt.MinCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	userRows := make([][]any, 0, *users)
	userIDs := make([]uuid.UUID, 0, *users)
	for i := range *users {
		id := uuid.New()
		userIDs = append(userIDs, id)
		userRows = append(userRows, []any{
			id,
			fmt.Sprintf("load-%s-%d@seatsync.test", runID, i),
			string(hash),
			fmt.Sprintf("Load Tester %d", i),
			models.RoleUser,
		})
	}
	if _, err := tx.CopyFrom(ctx,
		pgx.Identifier{"users"},
		[]string{"id", "email", "password_hash", "name", "role"},
		pgx.CopyFromRows(userRows),
	); err != nil {
		return fmt.Errorf("insert users: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// Long-lived tokens so the run cannot fail on an expiry mid-test.
	issuer := auth.NewTokenIssuer(cfg.JWTSecret, time.Hour, time.Hour)
	tokens := make([]string, 0, len(userIDs))
	for _, id := range userIDs {
		token, _, err := issuer.NewAccessToken(id, models.RoleUser)
		if err != nil {
			return fmt.Errorf("mint access token: %w", err)
		}
		tokens = append(tokens, token)
	}

	seatIDs, err := loadSeatIDs(ctx, pool, venueID)
	if err != nil {
		return err
	}

	scenario := Scenario{
		EventID:   eventID.String(),
		VenueID:   venueID.String(),
		SeatIDs:   seatIDs,
		Tokens:    tokens,
		SeatCount: len(seatIDs),
		UserCount: len(tokens),
		BasePrice: 100000,
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(scenario)
}

func loadSeatIDs(ctx context.Context, pool interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, venueID uuid.UUID) ([]string, error) {
	rows, err := pool.Query(ctx, `SELECT id FROM seats WHERE venue_id = $1 ORDER BY number`, venueID)
	if err != nil {
		return nil, fmt.Errorf("load seat ids: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan seat id: %w", err)
		}
		ids = append(ids, id.String())
	}
	return ids, rows.Err()
}
