// Command migrate applies or rolls back database migrations.
//
//	go run ./cmd/migrate up
//	go run ./cmd/migrate down 1
//	go run ./cmd/migrate version
package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/Aryan-Jain06/seatsync/backend/internal/config"
	"github.com/Aryan-Jain06/seatsync/backend/internal/db"
)

func main() {
	if err := run(); err != nil {
		slog.Error("migrate failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	args := os.Args[1:]
	if len(args) == 0 {
		return fmt.Errorf("usage: migrate <up|down [steps]|version>")
	}

	switch args[0] {
	case "up":
		if err := db.MigrateUp(cfg.DatabaseURL); err != nil {
			return err
		}
		fmt.Println("migrations applied")

	case "down":
		steps := 1
		if len(args) > 1 {
			steps, err = strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("invalid step count %q: %w", args[1], err)
			}
		}
		if err := db.MigrateDown(cfg.DatabaseURL, steps); err != nil {
			return err
		}
		fmt.Printf("rolled back %d migration(s)\n", steps)

	case "version":
		version, dirty, err := db.MigrateVersion(cfg.DatabaseURL)
		if err != nil {
			return err
		}
		fmt.Printf("schema version: %d (dirty: %t)\n", version, dirty)

	default:
		return fmt.Errorf("unknown command %q: expected up, down or version", args[0])
	}
	return nil
}
