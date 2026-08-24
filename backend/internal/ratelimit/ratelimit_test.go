package ratelimit_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Aryan-Jain06/seatsync/backend/internal/ratelimit"
)

func newLimiter(t *testing.T) *ratelimit.Limiter {
	t.Helper()

	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	rdb := redis.NewClient(&redis.Options{Addr: addr, PoolSize: 30})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		t.Skipf("redis not reachable at %s: %v", addr, err)
	}

	t.Cleanup(func() { _ = rdb.Close() })
	return ratelimit.NewLimiter(rdb)
}

func TestBurstIsAllowedThenRefused(t *testing.T) {
	limiter := newLimiter(t)
	ctx := context.Background()
	key := uuid.NewString()

	// A slow refill, so the bucket does not top up during the test.
	limit := ratelimit.Limit{Burst: 5, PerSecond: 0.1}

	for i := 1; i <= 5; i++ {
		decision, err := limiter.Allow(ctx, key, limit)
		require.NoError(t, err)
		require.True(t, decision.Allowed, "request %d should be within the burst", i)
	}

	decision, err := limiter.Allow(ctx, key, limit)
	require.NoError(t, err)
	require.False(t, decision.Allowed, "the request after the burst must be refused")
	require.Positive(t, decision.RetryAfter, "a refused caller must be told when to return")
}

// Each caller gets its own bucket, so one abusive client cannot exhaust the
// allowance of everybody else.
func TestBucketsAreIsolatedPerKey(t *testing.T) {
	limiter := newLimiter(t)
	ctx := context.Background()
	limit := ratelimit.Limit{Burst: 3, PerSecond: 0.1}

	noisy, quiet := uuid.NewString(), uuid.NewString()

	for range 3 {
		decision, err := limiter.Allow(ctx, noisy, limit)
		require.NoError(t, err)
		require.True(t, decision.Allowed)
	}

	exhausted, err := limiter.Allow(ctx, noisy, limit)
	require.NoError(t, err)
	require.False(t, exhausted.Allowed)

	// The quiet caller is unaffected.
	decision, err := limiter.Allow(ctx, quiet, limit)
	require.NoError(t, err)
	require.True(t, decision.Allowed, "one caller's abuse must not spend another's budget")
}

func TestBucketRefillsOverTime(t *testing.T) {
	limiter := newLimiter(t)
	ctx := context.Background()
	key := uuid.NewString()

	// 20 tokens per second: one token accrues every 50ms.
	limit := ratelimit.Limit{Burst: 2, PerSecond: 20}

	for range 2 {
		decision, err := limiter.Allow(ctx, key, limit)
		require.NoError(t, err)
		require.True(t, decision.Allowed)
	}

	refused, err := limiter.Allow(ctx, key, limit)
	require.NoError(t, err)
	require.False(t, refused.Allowed)

	require.Eventually(t, func() bool {
		decision, err := limiter.Allow(ctx, key, limit)
		return err == nil && decision.Allowed
	}, 2*time.Second, 25*time.Millisecond, "the bucket must refill")
}

// The limiter must not hand out more than the burst when hit concurrently,
// which is the whole reason the accounting lives in a Lua script.
func TestConcurrentCallersCannotExceedTheBurst(t *testing.T) {
	limiter := newLimiter(t)
	ctx := context.Background()
	key := uuid.NewString()

	const burst = 10
	const callers = 100

	limit := ratelimit.Limit{Burst: burst, PerSecond: 0.01}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		allowed int
	)

	start := make(chan struct{})

	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			decision, err := limiter.Allow(ctx, key, limit)
			if err == nil && decision.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}

	close(start)
	wg.Wait()

	require.Equal(t, burst, allowed,
		"exactly the burst may pass, however many callers arrive together")
}

// A limit of zero disables throttling rather than blocking everything, which
// is what makes the feature safe to switch off.
func TestAZeroLimitAllowsEverything(t *testing.T) {
	limiter := newLimiter(t)
	ctx := context.Background()
	key := uuid.NewString()

	for range 20 {
		decision, err := limiter.Allow(ctx, key, ratelimit.Limit{Burst: 0, PerSecond: 0})
		require.NoError(t, err)
		require.True(t, decision.Allowed)
	}
}
