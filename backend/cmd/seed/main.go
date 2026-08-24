// Command seed loads a deterministic development dataset: two venues, four
// events, and a full A-D / 10-row / 10-seat grid for each venue.
//
//	go run ./cmd/seed          # seed only if the database is empty
//	go run ./cmd/seed -reset   # wipe bookings and catalogue, then reseed
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/Aryan-Jain06/seatsync/backend/internal/config"
	"github.com/Aryan-Jain06/seatsync/backend/internal/db"
	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
)

// Seat grid dimensions, matching the project brief.
const (
	rowsPerSection = 10
	seatsPerRow    = 10
	demoPassword   = "password123"
	seedBcryptCost = bcrypt.DefaultCost
)

// sections maps a section name to its price multiplier, expressed in
// hundredths so the value inserted into NUMERIC(4,2) is exact.
var sections = []struct {
	Name          string
	MultiplierPct int
}{
	{"A", 150}, // closest to the stage
	{"B", 125},
	{"C", 100},
	{"D", 80}, // rear
}

func main() {
	if err := run(); err != nil {
		slog.Error("seed failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	reset := flag.Bool("reset", false, "delete existing data before seeding")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool, err := db.WaitForDatabase(ctx, cfg.DatabaseURL, 30*time.Second)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	if *reset {
		if err := truncateAll(ctx, pool); err != nil {
			return err
		}
		fmt.Println("existing data cleared")
	}

	var venueCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM venues`).Scan(&venueCount); err != nil {
		return fmt.Errorf("count venues: %w", err)
	}
	if venueCount > 0 {
		fmt.Println("database already contains data; nothing to do (use -reset to reseed)")
		return nil
	}

	if err := seed(ctx, pool); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("seed complete")
	fmt.Printf("  demo user : demo@seatsync.dev  / %s\n", demoPassword)
	fmt.Printf("  admin user: admin@seatsync.dev / %s\n", demoPassword)
	return nil
}

// truncateAll clears every table. RESTART IDENTITY is unnecessary for UUID
// keys but CASCADE is required to cut the foreign key graph in one statement.
func truncateAll(ctx context.Context, pool *pgxpool.Pool) error {
	const query = `
		TRUNCATE payments, booking_seats, bookings, events, seats, venues, refresh_tokens, users
		RESTART IDENTITY CASCADE`

	if _, err := pool.Exec(ctx, query); err != nil {
		return fmt.Errorf("truncate tables: %w", err)
	}
	return nil
}

func seed(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin seed transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := seedUsers(ctx, tx); err != nil {
		return err
	}

	venues := []struct {
		Name string
		City string
	}{
		{"Meridian Arena", "Mumbai"},
		{"The Ironworks", "Bengaluru"},
	}

	venueIDs := make([]uuid.UUID, 0, len(venues))
	for _, v := range venues {
		id, err := insertVenue(ctx, tx, v.Name, v.City)
		if err != nil {
			return err
		}
		venueIDs = append(venueIDs, id)

		seatCount, err := insertSeats(ctx, tx, id)
		if err != nil {
			return err
		}
		fmt.Printf("venue %-16s %s  (%d seats)\n", v.Name, v.City, seatCount)
	}

	// Four events spread across the two venues and a few weeks out.
	events := []struct {
		VenueIdx    int
		Title       string
		Description string
		DaysOut     int
		BasePrice   int64 // minor units
	}{
		{0, "Midnight Echoes — Live", "The reunion tour's only Indian date, with a full string section.", 14, 250000},
		{0, "Symphony Under Glass", "An evening of Ravel and Debussy performed beneath the arena's glass roof.", 28, 180000},
		{1, "Basement Sessions Vol. 9", "Four bands, one stage, no encores. Standing room converted to seating.", 7, 120000},
		{1, "The Long Way Round", "A one-man show about a cycle trip that went considerably wrong.", 21, 90000},
	}

	for _, e := range events {
		// Doors at 19:00 local time on the target day.
		day := time.Now().AddDate(0, 0, e.DaysOut)
		startsAt := time.Date(day.Year(), day.Month(), day.Day(), 19, 0, 0, 0, day.Location())
		id, err := insertEvent(ctx, tx, venueIDs[e.VenueIdx], e.Title, e.Description, startsAt, e.BasePrice)
		if err != nil {
			return err
		}
		fmt.Printf("event %s  %-28s %s\n", id, e.Title, startsAt.Format("2006-01-02 15:04"))
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit seed transaction: %w", err)
	}
	return nil
}

func seedUsers(ctx context.Context, tx pgx.Tx) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(demoPassword), seedBcryptCost)
	if err != nil {
		return fmt.Errorf("hash demo password: %w", err)
	}

	users := []struct {
		Email string
		Name  string
		Role  models.Role
	}{
		{"demo@seatsync.dev", "Demo User", models.RoleUser},
		{"admin@seatsync.dev", "Admin User", models.RoleAdmin},
	}

	for _, u := range users {
		const query = `INSERT INTO users (email, password_hash, name, role) VALUES ($1, $2, $3, $4)`
		if _, err := tx.Exec(ctx, query, u.Email, string(hash), u.Name, u.Role); err != nil {
			return fmt.Errorf("insert user %s: %w", u.Email, err)
		}
	}
	return nil
}

func insertVenue(ctx context.Context, tx pgx.Tx, name, city string) (uuid.UUID, error) {
	const query = `INSERT INTO venues (name, city) VALUES ($1, $2) RETURNING id`

	var id uuid.UUID
	if err := tx.QueryRow(ctx, query, name, city).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("insert venue %s: %w", name, err)
	}
	return id, nil
}

// insertSeats writes the full grid for a venue using COPY, which is
// substantially faster than 400 individual INSERTs.
func insertSeats(ctx context.Context, tx pgx.Tx, venueID uuid.UUID) (int64, error) {
	rows := make([][]any, 0, len(sections)*rowsPerSection*seatsPerRow)

	for _, section := range sections {
		for row := 1; row <= rowsPerSection; row++ {
			// A small premium for the front two rows of each section, so the
			// seat map has some price variety to render.
			multiplier := section.MultiplierPct
			if row <= 2 {
				multiplier += 10
			}

			for number := 1; number <= seatsPerRow; number++ {
				rows = append(rows, []any{
					venueID,
					section.Name,
					row,
					number,
					// NUMERIC(4,2) from an exact integer ratio.
					float64(multiplier) / 100.0,
				})
			}
		}
	}

	copied, err := tx.CopyFrom(ctx,
		pgx.Identifier{"seats"},
		[]string{"venue_id", "section", "row", "number", "price_multiplier"},
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return 0, fmt.Errorf("copy seats for venue %s: %w", venueID, err)
	}
	return copied, nil
}

func insertEvent(ctx context.Context, tx pgx.Tx, venueID uuid.UUID, title, description string, startsAt time.Time, basePrice int64) (uuid.UUID, error) {
	const query = `
		INSERT INTO events (venue_id, title, description, starts_at, base_price)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`

	var id uuid.UUID
	if err := tx.QueryRow(ctx, query, venueID, title, description, startsAt, basePrice).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("insert event %s: %w", title, err)
	}
	return id, nil
}
