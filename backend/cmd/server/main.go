// Command server runs the SeatSync HTTP API.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Aryan-Jain06/seatsync/backend/internal/auth"
	"github.com/Aryan-Jain06/seatsync/backend/internal/config"
	"github.com/Aryan-Jain06/seatsync/backend/internal/db"
	"github.com/Aryan-Jain06/seatsync/backend/internal/handlers"
	"github.com/Aryan-Jain06/seatsync/backend/internal/locks"
	"github.com/Aryan-Jain06/seatsync/backend/internal/payments"
	"github.com/Aryan-Jain06/seatsync/backend/internal/ratelimit"
	"github.com/Aryan-Jain06/seatsync/backend/internal/realtime"
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

	redisOptions := &redis.Options{
		Addr:         cfg.RedisAddr,
		Password:     cfg.RedisPassword,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
		// Sized to match the database pool so a burst of concurrent holds is
		// not throttled at the Redis client.
		PoolSize: 30,
	}

	// Hosted Redis providers accept TLS connections only. The server name is
	// taken from the address so the certificate is verified against the host
	// actually being dialled rather than skipped.
	if cfg.RedisTLS {
		host, _, err := net.SplitHostPort(cfg.RedisAddr)
		if err != nil {
			return fmt.Errorf("parse REDIS_ADDR %q for TLS: %w", cfg.RedisAddr, err)
		}
		redisOptions.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: host,
		}
	}

	rdb := redis.NewClient(redisOptions)
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

	if !cfg.RateLimitEnabled {
		slog.Warn("rate limiting is DISABLED; do not run this way in production")
	}

	// Trusting forwarding headers without a proxy that overwrites them lets
	// any client forge its address, which turns rate limiting into
	// decoration and reopens the login endpoint to password guessing. The
	// combination is worth calling out loudly, because nothing about the
	// running service looks wrong when it is misconfigured this way.
	if cfg.TrustProxyHeaders && cfg.RateLimitEnabled {
		slog.Warn("TRUST_PROXY_HEADERS is on: X-Forwarded-For is believed, " +
			"so rate limits are only as trustworthy as the proxy in front. " +
			"If this service is reachable directly, turn it off")
	}

	// --- wiring ---------------------------------------------------------
	tokenIssuer := auth.NewTokenIssuer(cfg.JWTSecret, cfg.JWTAccessTTL, cfg.JWTRefreshTTL)

	userRepo := repos.NewUserRepo(pool)
	refreshRepo := repos.NewRefreshTokenRepo(pool)
	eventRepo := repos.NewEventRepo(pool)
	bookingRepo := repos.NewBookingRepo(pool)
	paymentRepo := repos.NewPaymentRepo(pool)

	lockManager := locks.NewManager(rdb, cfg.HoldTTL)

	hub := realtime.NewHub()
	// Drained after the HTTP server stops accepting, so no subscriber is cut
	// off mid-message and no pump goroutine outlives the process.
	defer hub.Close()

	// Relay updates through Redis so every instance's hub delivers them.
	// Without this the hub is process-local: a user connected to one instance
	// never learns about a hold placed through another.
	relay := realtime.NewPubSub(hub, rdb)
	defer relay.Close()

	var broadcaster services.SeatBroadcaster = relay

	authService := services.NewAuthService(userRepo, refreshRepo, tokenIssuer, 0)
	catalogService := services.NewCatalogService(eventRepo, lockManager)
	holdService := services.NewHoldService(eventRepo, bookingRepo, lockManager, broadcaster, cfg.MaxSeatsPerHold)
	paymentService := services.NewPaymentService(
		bookingRepo, paymentRepo, lockManager, lockManager,
		payments.NewMockProvider(cfg), broadcaster,
	)

	router := server.NewRouter(server.Deps{
		Config:  cfg,
		Limiter: ratelimit.NewLimiter(rdb),
		Auth:    handlers.NewAuthHandler(authService),
		Events:  handlers.NewEventHandler(catalogService),
		Holds:   handlers.NewHoldHandler(holdService),
		Pay:     handlers.NewPaymentHandler(paymentService),
		WS:      handlers.NewWSHandler(hub, cfg.CORSAllowedOrigins),
		Health:  handlers.NewHealthHandler(pool, rdb),
		Tokens:  tokenIssuer,
	})

	httpServer := server.New(":"+cfg.Port, router)

	// --- background workers ---------------------------------------------
	// workersDone closes once every background goroutine has returned, so
	// shutdown can wait for them rather than exiting mid-sweep.
	var workers sync.WaitGroup
	expiryWorker := services.NewExpiryWorker(bookingRepo, broadcaster, cfg.ExpirySweepInterval)

	workers.Add(1)
	go func() {
		defer workers.Done()
		expiryWorker.Run(ctx)
	}()

	workers.Add(1)
	go func() {
		defer workers.Done()
		if err := relay.Run(ctx); err != nil {
			slog.Error("seat update relay stopped", "error", err)
		}
	}()

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

	// Close subscribers only once the HTTP server has stopped accepting, so
	// nobody is disconnected while a request could still be broadcasting.
	relay.Close()
	hub.Close()

	// The workers watch ctx, which is already cancelled, so this waits only
	// for their current iteration to finish.
	workers.Wait()

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
