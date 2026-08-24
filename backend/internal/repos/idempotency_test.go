package repos_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
	"github.com/Aryan-Jain06/seatsync/backend/internal/repos"
)

func TestClaimIdempotencyKeyBindsTheBookingToOneKey(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool, 2)
	bookings := repos.NewBookingRepo(pool)
	ctx := context.Background()

	booking, err := bookings.CreatePending(ctx, f.Alice, f.EventID, f.SeatIDs, 200000, "token-a", time.Now().Add(5*time.Minute))
	require.NoError(t, err)
	require.Nil(t, booking.IdempotencyKey, "a fresh booking carries no key")

	key := uuid.NewString()

	claimed, err := bookings.ClaimIdempotencyKey(ctx, booking.ID, key)
	require.NoError(t, err)
	require.NotNil(t, claimed.IdempotencyKey)
	require.Equal(t, key, *claimed.IdempotencyKey)

	// Re-claiming with the same key is what a retry does, and must succeed.
	again, err := bookings.ClaimIdempotencyKey(ctx, booking.ID, key)
	require.NoError(t, err)
	require.Equal(t, key, *again.IdempotencyKey)
}

// A second key against the same booking would charge twice for one set of
// seats, so it is refused.
func TestClaimIdempotencyKeyRejectsADifferentKey(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool, 1)
	bookings := repos.NewBookingRepo(pool)
	ctx := context.Background()

	booking, err := bookings.CreatePending(ctx, f.Alice, f.EventID, f.SeatIDs, 100000, "token-a", time.Now().Add(5*time.Minute))
	require.NoError(t, err)

	original := uuid.NewString()
	_, err = bookings.ClaimIdempotencyKey(ctx, booking.ID, original)
	require.NoError(t, err)

	_, err = bookings.ClaimIdempotencyKey(ctx, booking.ID, uuid.NewString())
	require.ErrorIs(t, err, repos.ErrIdempotencyKeyMismatch)

	// The original claim survives the rejected attempt.
	after, err := bookings.GetByID(ctx, booking.ID)
	require.NoError(t, err)
	require.Equal(t, original, *after.IdempotencyKey)
}

// Reusing one key across two different bookings is refused by the unique
// index, which stops a client accidentally collapsing two purchases into one.
func TestOneKeyCannotSpanTwoBookings(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool, 2)
	bookings := repos.NewBookingRepo(pool)
	ctx := context.Background()

	hold := time.Now().Add(5 * time.Minute)

	first, err := bookings.CreatePending(ctx, f.Alice, f.EventID, f.SeatIDs[:1], 100000, "token-a", hold)
	require.NoError(t, err)
	second, err := bookings.CreatePending(ctx, f.Alice, f.EventID, f.SeatIDs[1:], 100000, "token-b", hold)
	require.NoError(t, err)

	key := uuid.NewString()

	_, err = bookings.ClaimIdempotencyKey(ctx, first.ID, key)
	require.NoError(t, err)

	_, err = bookings.ClaimIdempotencyKey(ctx, second.ID, key)
	require.ErrorIs(t, err, repos.ErrIdempotencyKeyMismatch)
}

// Concurrent claims with one key must not let two callers both believe they
// won the right to charge.
func TestConcurrentClaimsWithOneKeyAreSerialised(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool, 1)
	bookings := repos.NewBookingRepo(pool)
	ctx := context.Background()

	booking, err := bookings.CreatePending(ctx, f.Alice, f.EventID, f.SeatIDs, 100000, "token-a", time.Now().Add(5*time.Minute))
	require.NoError(t, err)

	key := uuid.NewString()

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		errs   []error
		claims int
	)

	start := make(chan struct{})

	for range 15 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			claimed, err := bookings.ClaimIdempotencyKey(ctx, booking.ID, key)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			require.Equal(t, key, *claimed.IdempotencyKey)
			claims++
		}()
	}

	close(start)
	wg.Wait()

	// Every caller presented the same key, so every one is a legitimate
	// retry: all succeed and all see the identical key on the row.
	require.Empty(t, errs)
	require.Equal(t, 15, claims)

	var storedKeys int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(DISTINCT idempotency_key) FROM bookings WHERE id = $1`, booking.ID).Scan(&storedKeys))
	require.Equal(t, 1, storedKeys)
}

func TestClaimReportsABookingThatCanNoLongerBePaid(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool, 1)
	bookings := repos.NewBookingRepo(pool)
	ctx := context.Background()

	booking, err := bookings.CreatePending(ctx, f.Alice, f.EventID, f.SeatIDs, 100000, "token-a", time.Now().Add(5*time.Minute))
	require.NoError(t, err)

	_, err = bookings.MarkCancelled(ctx, booking.ID)
	require.NoError(t, err)

	claimed, err := bookings.ClaimIdempotencyKey(ctx, booking.ID, uuid.NewString())
	require.ErrorIs(t, err, repos.ErrBookingNotPending)
	require.NotNil(t, claimed, "the booking is returned so the caller can tell cancelled from confirmed")
	require.Equal(t, models.BookingCancelled, claimed.Status)
}

// The replay path: a confirmed booking must expose exactly one successful
// payment, so a retry returns the original result instead of charging again.
func TestSucceededPaymentIsRecoverableForReplay(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool, 2)
	bookings := repos.NewBookingRepo(pool)
	paymentRepo := repos.NewPaymentRepo(pool)
	ctx := context.Background()

	booking, err := bookings.CreatePending(ctx, f.Alice, f.EventID, f.SeatIDs, 200000, "token-a", time.Now().Add(5*time.Minute))
	require.NoError(t, err)

	// Two declines before the eventual success, as a real checkout might see.
	_, err = paymentRepo.RecordFailure(ctx, booking.ID, 200000, "ref_declined_1")
	require.NoError(t, err)
	_, err = paymentRepo.RecordFailure(ctx, booking.ID, 200000, "ref_declined_2")
	require.NoError(t, err)

	_, payment, err := bookings.Confirm(ctx, booking.ID, 200000, "ref_success", nil)
	require.NoError(t, err)

	replayed, err := paymentRepo.SucceededFor(ctx, booking.ID)
	require.NoError(t, err)
	require.Equal(t, payment.ID, replayed.ID, "replay returns the original charge")
	require.Equal(t, "ref_success", replayed.ProviderRef)
	require.Equal(t, models.PaymentSucceeded, replayed.Status)

	// The declines remain as an audit trail rather than being overwritten.
	var attempts int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM payments WHERE booking_id = $1`, booking.ID).Scan(&attempts))
	require.Equal(t, 3, attempts)
}

// The database refuses a second successful charge even if the service layer
// were bypassed entirely.
func TestASecondSuccessfulPaymentIsRefusedByTheDatabase(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool, 1)
	bookings := repos.NewBookingRepo(pool)
	ctx := context.Background()

	booking, err := bookings.CreatePending(ctx, f.Alice, f.EventID, f.SeatIDs, 100000, "token-a", time.Now().Add(5*time.Minute))
	require.NoError(t, err)

	_, _, err = bookings.Confirm(ctx, booking.ID, 100000, "ref_1", nil)
	require.NoError(t, err)

	_, err = pool.Exec(ctx,
		`INSERT INTO payments (booking_id, status, amount, provider_ref) VALUES ($1, 'succeeded', $2, 'ref_2')`,
		booking.ID, 100000)
	require.Error(t, err, "uq_payments_one_success_per_booking must refuse a second charge")
}

func TestSucceededForReportsNotFoundWhenUnpaid(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool, 1)
	bookings := repos.NewBookingRepo(pool)
	paymentRepo := repos.NewPaymentRepo(pool)
	ctx := context.Background()

	booking, err := bookings.CreatePending(ctx, f.Alice, f.EventID, f.SeatIDs, 100000, "token-a", time.Now().Add(5*time.Minute))
	require.NoError(t, err)

	_, err = paymentRepo.SucceededFor(ctx, booking.ID)
	require.ErrorIs(t, err, repos.ErrNotFound)
}
