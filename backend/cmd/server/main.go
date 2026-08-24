// Command server runs the SeatSync HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Aryan-Jain06/seatsync/backend/internal/auth"
	"github.com/Aryan-Jain06/seatsync/backend/internal/config"
	"github.com/Aryan-Jain06/seatsync/backend/internal/db"
	"github.com/Aryan-Jain06/seatsync/backend/internal/handlers"
	"github.com/Aryan-Jain06/seatsync/backend/internal/repos"
	"github.com/Aryan-Jain06/seatsync/backend/internal/server"
	"github.com/Aryan-Jain06/seatsync/backend/internal/services"
)

// shutdownGrace bounds how long in-flight requests have to finish once a
// termination signal arrives.
const shutdownGrace = 20 * time.Second

func main() {
	// The container image is built FROM scratch and so has no shell or curl.
	// `server -healthcheck` gives Docker something to run instead.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck())
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if err := run(); err != nil {
		slog.Error("server exited with error", "error", err)
		os.Exit(1)
	}
	slog.Info("server stopped cleanly")
}

func run() error {
	// Cancelled on SIGINT/SIGTERM; every long-lived component watches it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if cfg.RunMigrations {
		slog.Info("applying migrations")
		if err := db.MigrateUp(cfg.DatabaseURL); err != nil {
			return fmt.Errorf("run migrations: %w", err)
		}
	}

	pool, err := db.WaitForDatabase(ctx, cfg.DatabaseURL, 30*time.Second)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()
	slog.Info("connected to postgres")

	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.RedisAddr,
		Password:     cfg.RedisPassword,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		// Sized to match the database pool so a burst of concurrent holds is
		// not throttled at the Redis client.
		PoolSize: 30,
	})
	defer func() {
		if err := rdb.Close(); err != nil {
			slog.Warn("closing redis client", "error", err)
		}
	}()

	pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
	err = rdb.Ping(pingCtx).Err()
	cancelPing()
	if err != nil {
		return fmt.Errorf("connect to redis: %w", err)
	}
	slog.Info("connected to redis")

	// --- wiring ---------------------------------------------------------
	tokenIssuer := auth.NewTokenIssuer(cfg.JWTSecret, cfg.JWTAccessTTL, cfg.JWTRefreshTTL)

	userRepo := repos.NewUserRepo(pool)
	refreshRepo := repos.NewRefreshTokenRepo(pool)
	eventRepo := repos.NewEventRepo(pool)

	authService := services.NewAuthService(userRepo, refreshRepo, tokenIssuer, 0)
	// Holds are not backed by Redis yet, so every seat reads as available or
	// confirmed. Phase 3 replaces the reader without touching the seat map.
	catalogService := services.NewCatalogService(eventRepo, services.NoopHoldReader{})

	router := server.NewRouter(server.Deps{
		Config: cfg,
		Auth:   handlers.NewAuthHandler(authService),
		Events: handlers.NewEventHandler(catalogService),
		Health: handlers.NewHealthHandler(pool, rdb),
		Tokens: tokenIssuer,
	})

	httpServer := server.New(":"+cfg.Port, router)

	// --- serve ----------------------------------------------------------
	serveErr := make(chan error, 1)
	go func() {
		slog.Info("http server listening", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("listen and serve: %w", err)
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining connections", "grace", shutdownGrace.String())
	}

	// Detached from ctx, which is already cancelled, so the drain gets its
	// full grace period rather than being torn down immediately.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		// Force the remaining connections closed so the process still exits.
		if closeErr := httpServer.Close(); closeErr != nil {
			slog.Error("forcing server close", "error", closeErr)
		}
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return <-serveErr
}

// healthcheck probes the local /health endpoint and reports an exit code
// Docker can interpret. It deliberately avoids the config package so a
// malformed environment cannot make the probe fail for the wrong reason.
func healthcheck() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	client := &http.Client{Timeout: 3 * time.Second}

	resp, err := client.Get("http://127.0.0.1:" + port + "/health")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
