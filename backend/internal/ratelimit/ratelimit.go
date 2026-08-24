// Package ratelimit throttles callers so a single client cannot exhaust the
// service for everybody else.
//
// Limits are keyed by identity rather than by connection: an authenticated
// caller is limited by user id, an anonymous one by client address. Keying
// everything by address would punish users behind a shared NAT and would be
// trivially defeated by rotating addresses.
package ratelimit

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed scripts/tokenbucket.lua
var tokenBucketScript string

// Limit describes an allowance.
type Limit struct {
	// Burst is the most requests allowed back to back.
	Burst int
	// PerSecond is the sustained rate the bucket refills at.
	PerSecond float64
}

// Decision is the outcome of one rate limit check.
type Decision struct {
	Allowed   bool
	Remaining int
	// RetryAfter is how long until the request would be allowed.
	RetryAfter time.Duration
}

// Limiter applies token bucket limits backed by Redis, so several instances
// of the service share one allowance per client rather than one each.
type Limiter struct {
	rdb    *redis.Client
	script *redis.Script
}

// NewLimiter builds a Limiter.
func NewLimiter(rdb *redis.Client) *Limiter {
	return &Limiter{rdb: rdb, script: redis.NewScript(tokenBucketScript)}
}

// Allow takes one token from the bucket named by key.
//
// A Redis failure allows the request. Rate limiting protects against abuse,
// and refusing all traffic because the limiter is unavailable would turn a
// cache outage into a full outage: failing open is the lesser harm here.
func (l *Limiter) Allow(ctx context.Context, key string, limit Limit) (Decision, error) {
	if limit.Burst <= 0 || limit.PerSecond <= 0 {
		return Decision{Allowed: true}, nil
	}

	raw, err := l.script.Run(ctx, l.rdb,
		[]string{"ratelimit:" + key},
		limit.Burst,
		limit.PerSecond,
		time.Now().UnixMilli(),
		1,
	).Slice()
	if err != nil {
		return Decision{Allowed: true}, fmt.Errorf("ratelimit: run token bucket: %w", err)
	}

	if len(raw) < 3 {
		return Decision{Allowed: true}, fmt.Errorf("ratelimit: script returned %d values, expected 3", len(raw))
	}

	allowed, _ := raw[0].(int64)
	remaining, _ := raw[1].(int64)
	retryAfterMillis, _ := raw[2].(int64)

	return Decision{
		Allowed:    allowed == 1,
		Remaining:  int(remaining),
		RetryAfter: time.Duration(retryAfterMillis) * time.Millisecond,
	}, nil
}
