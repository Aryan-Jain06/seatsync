package locks_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Aryan-Jain06/seatsync/backend/internal/locks"
)

// newManager connects to the Redis given by REDIS_ADDR, skipping the test
// when none is reachable so `go test ./...` still works without one.
func newManager(t *testing.T, ttl time.Duration) (*locks.Manager, *redis.Client) {
	t.Helper()

	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	rdb := redis.NewClient(&redis.Options{Addr: addr, PoolSize: 40})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		t.Skipf("redis not reachable at %s: %v", addr, err)
	}

	t.Cleanup(func() { _ = rdb.Close() })
	return locks.NewManager(rdb, ttl), rdb
}

func seatIDs(n int) []uuid.UUID {
	ids := make([]uuid.UUID, 0, n)
	for range n {
		ids = append(ids, uuid.New())
	}
	return ids
}

func TestAcquireThenReleaseRoundTrip(t *testing.T) {
	m, _ := newManager(t, time.Minute)
	ctx := context.Background()

	eventID, userID := uuid.New(), uuid.New()
	token := uuid.NewString()
	seats := seatIDs(3)

	expiresAt, err := m.Acquire(ctx, eventID, userID, token, seats)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(time.Minute), expiresAt, 5*time.Second)

	held, err := m.HeldSeats(ctx, eventID)
	require.NoError(t, err)
	require.Len(t, held, 3)
	for _, seatID := range seats {
		hold, ok := held[seatID]
		require.True(t, ok, "seat %s should be held", seatID)
		require.Equal(t, userID, hold.UserID)
	}

	released, err := m.Release(ctx, eventID, userID, token, seats)
	require.NoError(t, err)
	require.Equal(t, 3, released)

	held, err = m.HeldSeats(ctx, eventID)
	require.NoError(t, err)
	require.Empty(t, held, "released seats must not remain in the reserved set")
}

// A hold is all or nothing: overlapping by a single seat must fail the whole
// request and leave the caller holding nothing.
func TestAcquireIsAllOrNothing(t *testing.T) {
	m, _ := newManager(t, time.Minute)
	ctx := context.Background()

	eventID := uuid.New()
	first, second := uuid.New(), uuid.New()
	seats := seatIDs(4)

	_, err := m.Acquire(ctx, eventID, first, uuid.NewString(), seats[:2])
	require.NoError(t, err)

	// Seats 1..3, of which seat index 1 is already taken.
	_, err = m.Acquire(ctx, eventID, second, uuid.NewString(), seats[1:])

	var unavailable *locks.ErrSeatsUnavailable
	require.ErrorAs(t, err, &unavailable)
	require.Equal(t, []uuid.UUID{seats[1]}, unavailable.SeatIDs)

	// The uncontended seats must NOT have been taken by the failed attempt.
	held, err := m.HeldSeats(ctx, eventID)
	require.NoError(t, err)
	require.Len(t, held, 2, "a failed acquire must not lock anything")
	require.Equal(t, first, held[seats[0]].UserID)
	require.NotContains(t, held, seats[2])
	require.NotContains(t, held, seats[3])
}

// The headline guarantee: many callers racing for one seat, exactly one wins.
func TestConcurrentAcquireYieldsExactlyOneWinner(t *testing.T) {
	m, _ := newManager(t, time.Minute)
	ctx := context.Background()

	eventID := uuid.New()
	seats := seatIDs(1)

	const contenders = 100

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		winners  []uuid.UUID
		failures int
	)

	start := make(chan struct{})

	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			userID := uuid.New()

			<-start // release every goroutine at once

			_, err := m.Acquire(ctx, eventID, userID, uuid.NewString(), seats)

			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				winners = append(winners, userID)
			} else {
				failures++
			}
		}()
	}

	close(start)
	wg.Wait()

	require.Len(t, winners, 1, "exactly one caller may hold the seat")
	require.Equal(t, contenders-1, failures)

	held, err := m.HeldSeats(ctx, eventID)
	require.NoError(t, err)
	require.Len(t, held, 1)
	require.Equal(t, winners[0], held[seats[0]].UserID)
}

// Concurrent callers requesting overlapping seat ranges must never end up
// sharing a seat.
func TestConcurrentOverlappingRangesNeverShareASeat(t *testing.T) {
	m, _ := newManager(t, time.Minute)
	ctx := context.Background()

	eventID := uuid.New()
	seats := seatIDs(10)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		ownerOf  = map[uuid.UUID]uuid.UUID{}
		conflict error
	)

	start := make(chan struct{})

	// Each contender asks for a sliding window of 3 seats, so neighbours
	// overlap by two.
	for offset := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			userID := uuid.New()
			window := seats[offset : offset+3]

			<-start

			if _, err := m.Acquire(ctx, eventID, userID, uuid.NewString(), window); err != nil {
				return // contended, which is a legitimate outcome
			}

			mu.Lock()
			defer mu.Unlock()
			for _, seatID := range window {
				if existing, taken := ownerOf[seatID]; taken {
					conflict = &doubleBooking{seat: seatID, first: existing, second: userID}
				}
				ownerOf[seatID] = userID
			}
		}()
	}

	close(start)
	wg.Wait()

	require.NoError(t, conflict, "two callers must never hold the same seat")
}

type doubleBooking struct {
	seat          uuid.UUID
	first, second uuid.UUID
}

func (e *doubleBooking) Error() string {
	return "seat " + e.seat.String() + " held by both " + e.first.String() + " and " + e.second.String()
}

// Releasing must never touch a lock the caller does not own.
func TestReleaseIgnoresSomebodyElsesLock(t *testing.T) {
	m, _ := newManager(t, time.Minute)
	ctx := context.Background()

	eventID := uuid.New()
	owner, attacker := uuid.New(), uuid.New()
	ownerToken := uuid.NewString()
	seats := seatIDs(2)

	_, err := m.Acquire(ctx, eventID, owner, ownerToken, seats)
	require.NoError(t, err)

	// Wrong user entirely.
	released, err := m.Release(ctx, eventID, attacker, uuid.NewString(), seats)
	require.NoError(t, err)
	require.Zero(t, released)

	// Right user, wrong token: one of a user's own bookings must not be able
	// to release another's seats.
	released, err = m.Release(ctx, eventID, owner, uuid.NewString(), seats)
	require.NoError(t, err)
	require.Zero(t, released)

	held, err := m.HeldSeats(ctx, eventID)
	require.NoError(t, err)
	require.Len(t, held, 2, "the owner's locks must survive both attempts")
}

// An expired hold must disappear from the seat map on its own.
func TestHoldsExpireAndLeaveTheSeatMap(t *testing.T) {
	m, _ := newManager(t, time.Second)
	ctx := context.Background()

	eventID, userID := uuid.New(), uuid.New()
	seats := seatIDs(2)

	_, err := m.Acquire(ctx, eventID, userID, uuid.NewString(), seats)
	require.NoError(t, err)

	held, err := m.HeldSeats(ctx, eventID)
	require.NoError(t, err)
	require.Len(t, held, 2)

	require.Eventually(t, func() bool {
		held, err := m.HeldSeats(ctx, eventID)
		return err == nil && len(held) == 0
	}, 5*time.Second, 100*time.Millisecond, "expired holds must vanish from the seat map")

	// The seat is free for somebody else once the hold lapses.
	_, err = m.Acquire(ctx, eventID, uuid.New(), uuid.NewString(), seats)
	require.NoError(t, err)
}

func TestHoldsForReportsMissingLocks(t *testing.T) {
	m, _ := newManager(t, time.Minute)
	ctx := context.Background()

	eventID, userID := uuid.New(), uuid.New()
	token := uuid.NewString()
	seats := seatIDs(3)

	_, err := m.Acquire(ctx, eventID, userID, token, seats)
	require.NoError(t, err)

	missing, err := m.HoldsFor(ctx, eventID, userID, token, seats)
	require.NoError(t, err)
	require.Empty(t, missing)

	// Drop one lock behind the manager's back, as an expiry would.
	released, err := m.Release(ctx, eventID, userID, token, seats[:1])
	require.NoError(t, err)
	require.Equal(t, 1, released)

	missing, err = m.HoldsFor(ctx, eventID, userID, token, seats)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{seats[0]}, missing)

	// A different token must see every seat as missing.
	missing, err = m.HoldsFor(ctx, eventID, userID, uuid.NewString(), seats)
	require.NoError(t, err)
	require.Len(t, missing, 3)
}
