package repos_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
	"github.com/Aryan-Jain06/seatsync/backend/internal/repos"
)

func TestConfirmMakesTheSaleDurable(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool, 3)
	bookings := repos.NewBookingRepo(pool)
	ctx := context.Background()

	booking, err := bookings.CreatePending(ctx, f.Alice, f.EventID, f.SeatIDs, 300000, "token-a", time.Now().Add(5*time.Minute))
	require.NoError(t, err)
	require.Equal(t, models.BookingPending, booking.Status)
	require.Nil(t, booking.ConfirmedAt)

	confirmed, payment, err := bookings.Confirm(ctx, booking.ID, 300000, "ref_123", nil)
	require.NoError(t, err)

	require.Equal(t, models.BookingConfirmed, confirmed.Status)
	require.NotNil(t, confirmed.ConfirmedAt, "confirmed_at is set in the same transaction")
	require.Equal(t, models.PaymentSucceeded, payment.Status)
	require.Equal(t, int64(300000), payment.Amount)
	require.Equal(t, "ref_123", payment.ProviderRef)

	// Every seat is flagged, which is what brings them under the unique index.
	var confirmedSeats int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM booking_seats WHERE booking_id = $1 AND confirmed`, booking.ID).Scan(&confirmedSeats))
	require.Equal(t, 3, confirmedSeats)
}

// The headline invariant, exercised through the repository rather than raw
// SQL: a second booking over the same seat cannot be confirmed.
func TestConfirmRefusesASeatAlreadySold(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool, 1)
	bookings := repos.NewBookingRepo(pool)
	ctx := context.Background()

	hold := time.Now().Add(5 * time.Minute)

	aliceBooking, err := bookings.CreatePending(ctx, f.Alice, f.EventID, f.SeatIDs, 100000, "token-a", hold)
	require.NoError(t, err)

	// Two pending bookings over one seat is legitimate; the index does not
	// apply until a booking is confirmed.
	bobBooking, err := bookings.CreatePending(ctx, f.Bob, f.EventID, f.SeatIDs, 100000, "token-b", hold)
	require.NoError(t, err)

	_, _, err = bookings.Confirm(ctx, aliceBooking.ID, 100000, "ref_alice", nil)
	require.NoError(t, err)

	_, _, err = bookings.Confirm(ctx, bobBooking.ID, 100000, "ref_bob", nil)
	require.ErrorIs(t, err, repos.ErrSeatConfirmed, "the partial unique index must refuse the second sale")

	// Bob's booking must be untouched: the whole transaction rolled back.
	bobAfter, err := bookings.GetByID(ctx, bobBooking.ID)
	require.NoError(t, err)
	require.Equal(t, models.BookingPending, bobAfter.Status)
	require.Nil(t, bobAfter.ConfirmedAt)

	var bobPayments int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM payments WHERE booking_id = $1`, bobBooking.ID).Scan(&bobPayments))
	require.Zero(t, bobPayments, "a rolled back confirm must not leave a payment behind")

	// And the duplicate check the load test runs finds nothing.
	var duplicates int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT event_id, seat_id FROM booking_seats
			WHERE confirmed GROUP BY event_id, seat_id HAVING count(*) > 1
		) d`).Scan(&duplicates))
	require.Zero(t, duplicates)
}

// Two transactions confirming the same seat at the same instant: one wins,
// one is refused, and nothing is half-written.
func TestConcurrentConfirmOfOneSeatYieldsOneSale(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool, 1)
	bookings := repos.NewBookingRepo(pool)
	ctx := context.Background()

	hold := time.Now().Add(5 * time.Minute)
	const contenders = 20

	bookingIDs := make([]uuid.UUID, 0, contenders)
	for i := range contenders {
		user := f.Alice
		if i%2 == 1 {
			user = f.Bob
		}
		b, err := bookings.CreatePending(ctx, user, f.EventID, f.SeatIDs, 100000, uuid.NewString(), hold)
		require.NoError(t, err)
		bookingIDs = append(bookingIDs, b.ID)
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		refused   int
		other     []error
	)

	start := make(chan struct{})

	for _, id := range bookingIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			_, _, err := bookings.Confirm(ctx, id, 100000, "ref_"+id.String(), nil)

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, repos.ErrSeatConfirmed):
				refused++
			default:
				other = append(other, err)
			}
		}()
	}

	close(start)
	wg.Wait()

	require.Empty(t, other, "no unexpected errors")
	require.Equal(t, 1, succeeded, "exactly one booking may be confirmed")
	require.Equal(t, contenders-1, refused)

	var confirmedRows int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM booking_seats WHERE confirmed AND event_id = $1`, f.EventID).Scan(&confirmedRows))
	require.Equal(t, 1, confirmedRows)
}

// A failing lock check inside the transaction must roll back everything, so
// no money is recorded for seats the caller no longer holds.
func TestConfirmRollsBackWhenTheLockCheckFails(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool, 2)
	bookings := repos.NewBookingRepo(pool)
	ctx := context.Background()

	booking, err := bookings.CreatePending(ctx, f.Alice, f.EventID, f.SeatIDs, 200000, "token-a", time.Now().Add(5*time.Minute))
	require.NoError(t, err)

	lockLost := errors.New("hold expired")

	_, _, err = bookings.Confirm(ctx, booking.ID, 200000, "ref_x", func(context.Context) error {
		return lockLost
	})
	require.ErrorIs(t, err, lockLost)

	after, err := bookings.GetByID(ctx, booking.ID)
	require.NoError(t, err)
	require.Equal(t, models.BookingPending, after.Status, "the booking stays payable")

	var payments, confirmedSeats int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM payments WHERE booking_id = $1`, booking.ID).Scan(&payments))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM booking_seats WHERE booking_id = $1 AND confirmed`, booking.ID).Scan(&confirmedSeats))
	require.Zero(t, payments, "no charge may be recorded when the seats were lost")
	require.Zero(t, confirmedSeats)
}

func TestConfirmRefusesABookingThatIsNoLongerPending(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool, 1)
	bookings := repos.NewBookingRepo(pool)
	ctx := context.Background()

	booking, err := bookings.CreatePending(ctx, f.Alice, f.EventID, f.SeatIDs, 100000, "token-a", time.Now().Add(5*time.Minute))
	require.NoError(t, err)

	cancelled, err := bookings.MarkCancelled(ctx, booking.ID)
	require.NoError(t, err)
	require.True(t, cancelled)

	_, _, err = bookings.Confirm(ctx, booking.ID, 100000, "ref_x", nil)
	require.ErrorIs(t, err, repos.ErrBookingNotPending)
}
