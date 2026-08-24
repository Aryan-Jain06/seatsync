// Package config loads and validates runtime configuration from the
// environment. Every setting has a documented default so the service starts
// with no configuration at all in development, while still failing loudly in
// production when something required is obviously wrong.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// PaymentMode selects the behaviour of the mock payment provider.
type PaymentMode string

const (
	// PaymentModeRandom fails a configurable fraction of attempts.
	PaymentModeRandom PaymentMode = "random"
	// PaymentModeAlwaysSuccess makes every attempt succeed. Used by the load test.
	PaymentModeAlwaysSuccess PaymentMode = "always_success"
	// PaymentModeAlwaysFail makes every attempt fail. Used by tests.
	PaymentModeAlwaysFail PaymentMode = "always_fail"
)

// Config is the fully resolved configuration for the server.
type Config struct {
	Port               string
	DatabaseURL        string
	RedisAddr          string
	RedisPassword      string
	CORSAllowedOrigins []string
	RunMigrations      bool

	JWTSecret     []byte
	JWTAccessTTL  time.Duration
	JWTRefreshTTL time.Duration

	HoldTTL             time.Duration
	MaxSeatsPerHold     int
	ExpirySweepInterval time.Duration

	PaymentMode        PaymentMode
	PaymentSuccessRate float64
	PaymentMinLatency  time.Duration
	PaymentMaxLatency  time.Duration

	// --- protection -------------------------------------------------------

	// RateLimitEnabled turns request throttling on. Disable it only for
	// load testing against a machine you control.
	RateLimitEnabled bool
	// RateLimitAuth guards sign-in and registration against credential
	// guessing. Keyed by client address, since there is no user yet.
	RateLimitAuthBurst     int
	RateLimitAuthPerMinute float64
	// RateLimitRead guards the public catalogue and seat map.
	RateLimitReadBurst     int
	RateLimitReadPerMinute float64
	// RateLimitWrite guards holds and payments, keyed by user.
	RateLimitWriteBurst     int
	RateLimitWritePerMinute float64

	// TrustProxyHeaders makes the server believe X-Forwarded-For. Enable it
	// only when something trustworthy sets that header, because a client can
	// otherwise forge its own address and evade rate limiting.
	TrustProxyHeaders bool

	// RequireAuthForBrowsing closes the catalogue and seat map to anonymous
	// callers. Off by default, since a ticketing site's listings are normally
	// public, but available when the API should not be readable by strangers.
	RequireAuthForBrowsing bool

	// EnableHSTS sends Strict-Transport-Security on TLS requests.
	EnableHSTS bool
}

// insecureDefaultSecret matches .env.example. It is fine for local work and
// rejected anywhere that looks like a real deployment.
const insecureDefaultSecret = "dev-only-insecure-secret-change-me"

// Load reads configuration from the environment, falling back to a .env file
// in the working directory or its parent when present.
func Load() (*Config, error) {
	loadDotEnv()

	accessTTL, err := envDuration("JWT_ACCESS_TTL", 15*time.Minute)
	if err != nil {
		return nil, err
	}
	refreshTTL, err := envDuration("JWT_REFRESH_TTL", 720*time.Hour)
	if err != nil {
		return nil, err
	}
	holdTTL, err := envDuration("HOLD_TTL", 5*time.Minute)
	if err != nil {
		return nil, err
	}
	sweep, err := envDuration("EXPIRY_SWEEP_INTERVAL", 2*time.Second)
	if err != nil {
		return nil, err
	}
	minLatency, err := envDuration("PAYMENT_MIN_LATENCY", time.Second)
	if err != nil {
		return nil, err
	}
	maxLatency, err := envDuration("PAYMENT_MAX_LATENCY", 2*time.Second)
	if err != nil {
		return nil, err
	}
	maxSeats, err := envInt("MAX_SEATS_PER_HOLD", 6)
	if err != nil {
		return nil, err
	}
	successRate, err := envFloat("PAYMENT_SUCCESS_RATE", 0.9)
	if err != nil {
		return nil, err
	}

	authBurst, err := envInt("RATE_LIMIT_AUTH_BURST", 10)
	if err != nil {
		return nil, err
	}
	authRate, err := envFloat("RATE_LIMIT_AUTH_PER_MINUTE", 10)
	if err != nil {
		return nil, err
	}
	readBurst, err := envInt("RATE_LIMIT_READ_BURST", 120)
	if err != nil {
		return nil, err
	}
	readRate, err := envFloat("RATE_LIMIT_READ_PER_MINUTE", 240)
	if err != nil {
		return nil, err
	}
	writeBurst, err := envInt("RATE_LIMIT_WRITE_BURST", 30)
	if err != nil {
		return nil, err
	}
	writeRate, err := envFloat("RATE_LIMIT_WRITE_PER_MINUTE", 60)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Port:                envString("PORT", "8080"),
		DatabaseURL:         envString("DATABASE_URL", "postgres://seatsync:seatsync@localhost:5432/seatsync?sslmode=disable"),
		RedisAddr:           envString("REDIS_ADDR", "localhost:6379"),
		RedisPassword:       envString("REDIS_PASSWORD", ""),
		CORSAllowedOrigins:  splitAndTrim(envString("CORS_ALLOWED_ORIGINS", "http://localhost:3000")),
		RunMigrations:       envBool("RUN_MIGRATIONS", false),
		JWTSecret:           []byte(envString("JWT_SECRET", insecureDefaultSecret)),
		JWTAccessTTL:        accessTTL,
		JWTRefreshTTL:       refreshTTL,
		HoldTTL:             holdTTL,
		MaxSeatsPerHold:     maxSeats,
		ExpirySweepInterval: sweep,
		PaymentMode:         PaymentMode(envString("PAYMENT_MODE", string(PaymentModeRandom))),
		PaymentSuccessRate:  successRate,
		PaymentMinLatency:   minLatency,
		PaymentMaxLatency:   maxLatency,

		RateLimitEnabled:        envBool("RATE_LIMIT_ENABLED", true),
		RateLimitAuthBurst:      authBurst,
		RateLimitAuthPerMinute:  authRate,
		RateLimitReadBurst:      readBurst,
		RateLimitReadPerMinute:  readRate,
		RateLimitWriteBurst:     writeBurst,
		RateLimitWritePerMinute: writeRate,

		TrustProxyHeaders:      envBool("TRUST_PROXY_HEADERS", false),
		RequireAuthForBrowsing: envBool("REQUIRE_AUTH_FOR_BROWSING", false),
		EnableHSTS:             envBool("ENABLE_HSTS", false),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if len(c.JWTSecret) == 0 {
		return fmt.Errorf("config: JWT_SECRET must not be empty")
	}
	// Refuse to boot with the sample secret outside development, where a
	// forged token would be trivial to mint.
	if string(c.JWTSecret) == insecureDefaultSecret && strings.EqualFold(os.Getenv("APP_ENV"), "production") {
		return fmt.Errorf("config: JWT_SECRET is still the example value; set a real secret in production")
	}
	if c.MaxSeatsPerHold < 1 {
		return fmt.Errorf("config: MAX_SEATS_PER_HOLD must be at least 1, got %d", c.MaxSeatsPerHold)
	}
	if c.HoldTTL <= 0 {
		return fmt.Errorf("config: HOLD_TTL must be positive, got %s", c.HoldTTL)
	}
	if c.ExpirySweepInterval <= 0 {
		return fmt.Errorf("config: EXPIRY_SWEEP_INTERVAL must be positive, got %s", c.ExpirySweepInterval)
	}
	if c.JWTAccessTTL <= 0 || c.JWTRefreshTTL <= 0 {
		return fmt.Errorf("config: JWT TTLs must be positive")
	}
	switch c.PaymentMode {
	case PaymentModeRandom, PaymentModeAlwaysSuccess, PaymentModeAlwaysFail:
	default:
		return fmt.Errorf("config: PAYMENT_MODE must be one of random|always_success|always_fail, got %q", c.PaymentMode)
	}
	if c.PaymentSuccessRate < 0 || c.PaymentSuccessRate > 1 {
		return fmt.Errorf("config: PAYMENT_SUCCESS_RATE must be within [0,1], got %v", c.PaymentSuccessRate)
	}
	if c.PaymentMinLatency < 0 || c.PaymentMaxLatency < 0 {
		return fmt.Errorf("config: payment latencies must not be negative")
	}
	if c.RateLimitEnabled {
		for name, pair := range map[string][2]float64{
			"RATE_LIMIT_AUTH":  {float64(c.RateLimitAuthBurst), c.RateLimitAuthPerMinute},
			"RATE_LIMIT_READ":  {float64(c.RateLimitReadBurst), c.RateLimitReadPerMinute},
			"RATE_LIMIT_WRITE": {float64(c.RateLimitWriteBurst), c.RateLimitWritePerMinute},
		} {
			if pair[0] < 1 || pair[1] <= 0 {
				return fmt.Errorf("config: %s burst and rate must both be positive when rate limiting is enabled", name)
			}
		}
	}
	if c.PaymentMaxLatency < c.PaymentMinLatency {
		return fmt.Errorf("config: PAYMENT_MAX_LATENCY (%s) must not be below PAYMENT_MIN_LATENCY (%s)",
			c.PaymentMaxLatency, c.PaymentMinLatency)
	}
	return nil
}

// loadDotEnv makes `go run ./cmd/server` from within backend/ pick up the
// repository-root .env without requiring the caller to export anything.
func loadDotEnv() {
	for _, path := range []string{".env", "../.env"} {
		if _, err := os.Stat(path); err == nil {
			_ = godotenv.Load(path)
			return
		}
	}
}

func envString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s is not a valid duration (e.g. 15m, 2s): %w", key, err)
	}
	return d, nil
}

func envInt(key string, fallback int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s is not a valid integer: %w", key, err)
	}
	return n, nil
}

func envFloat(key string, fallback float64) (float64, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s is not a valid number: %w", key, err)
	}
	return f, nil
}

func envBool(key string, fallback bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return b
}

func splitAndTrim(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
