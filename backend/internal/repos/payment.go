package repos

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
)

// Errors specific to the payment and confirmation path.
var (
	// ErrBookingNotPending means the booking left the pending state before
	// the caller got here, so it can no longer be paid for.
	ErrBookingNotPending = errors.New("repos: booking is no longer pending")
	// ErrIdempotencyKeyMismatch means the booking was already claimed by a
	// different Idempotency-Key.
	ErrIdempotencyKeyMismatch = errors.New("repos: booking claimed by a different idempotency key")
)

// PaymentRepo records payment attempts.
type PaymentRepo struct {
	pool *pgxpool.Pool
}

// NewPaymentRepo builds a PaymentRepo.
func NewPaymentRepo(pool *pgxpool.Pool) *PaymentRepo { return &PaymentRepo{pool: pool} }

const paymentColumns = `id, booking_id, status, amount, provider_ref, created_at`

func scanPayment(row scanRow) (*models.Payment, error) {
	var p models.Payment
	if err := row.Scan(&p.ID, &p.BookingID, &p.Status, &p.Amount, &p.ProviderRef, &p.CreatedAt); err != nil {
		return nil, fmt.Errorf("scan payment: %w", classify(err))
	}
	return &p, nil
}

// RecordFailure stores a declined attempt. The booking stays pending so the
// caller may try again with the same key while the hold lasts.
func (r *PaymentRepo) RecordFailure(ctx context.Context, bookingID uuid.UUID, amount int64, providerRef string) (*models.Payment, error) {
	const query = `
		INSERT INTO payments (booking_id, status, amount, provider_ref)
		VALUES ($1, 'failed', $2, $3)
		RETURNING ` + paymentColumns

	return scanPayment(r.pool.QueryRow(ctx, query, bookingID, amount, providerRef))
}

// SucceededFor returns the successful payment for a booking, if there is one.
// It backs the idempotent replay of an already-completed purchase.
func (r *PaymentRepo) SucceededFor(ctx context.Context, bookingID uuid.UUID) (*models.Payment, error) {
	const query = `
		SELECT ` + paymentColumns + `
		FROM payments
		WHERE booking_id = $1 AND status = 'succeeded'`

	return scanPayment(r.pool.QueryRow(ctx, query, bookingID))
}

// ClaimIdempotencyKey records which Idempotency-Key owns this booking's
// payment, and reports the booking's state at the moment of the claim.
//
// The row is locked FOR UPDATE so two requests arriving together cannot both
// read a null key and both believe they claimed it.
func (r *BookingRepo) ClaimIdempotencyKey(ctx context.Context, bookingID uuid.UUID, key string) (*models.Booking, error) {
	var booking *models.Booking

	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		const selectForUpdate = `SELECT ` + bookingColumns + ` FROM bookings WHERE id = $1 FOR UPDATE`

		current, err := scanBooking(tx.QueryRow(ctx, selectForUpdate, bookingID))
		if err != nil {
			return err
		}

		// A key already on the row must match. Two different keys against one
		// booking is a client bug, and honouring the second could charge for
		// the same seats twice.
		if current.IdempotencyKey != nil && *current.IdempotencyKey != key {
			return ErrIdempotencyKeyMismatch
		}

		if current.Status != models.BookingPending {
			// Hand the booking back anyway: the caller distinguishes an
			// already-confirmed booking, which replays, from a cancelled one,
			// which cannot be paid for.
			booking = current
			return ErrBookingNotPending
		}

		if current.IdempotencyKey == nil {
			const claim = `
				UPDATE bookings SET idempotency_key = $2
				WHERE id = $1
				RETURNING ` + bookingColumns

			current, err = scanBooking(tx.QueryRow(ctx, claim, bookingID, key))
			if err != nil {
				if errors.Is(err, ErrDuplicateKey) {
					// The key is globally unique, so this means the same key
					// was sent for a different booking.
					return ErrIdempotencyKeyMismatch
				}
				return fmt.Errorf("claim idempotency key: %w", err)
			}
		}

		booking = current
		return nil
	})

	// ErrBookingNotPending is returned alongside a populated booking, so the
	// caller can inspect it rather than being told only that something failed.
	if err != nil && !errors.Is(err, ErrBookingNotPending) {
		return nil, err
	}
	return booking, err
}

// Confirm turns a paid-for pending booking into a permanent sale.
//
// Everything that must be true together happens in one transaction: the
// booking is re-checked under a row lock, the caller's seat locks are
// verified, the seats are flagged confirmed, the booking is confirmed and the
// payment is recorded. Any failure rolls back the lot, so there is no state
// where the money is recorded but the seats are not, or the reverse.
//
// verifyLocks runs inside the transaction, immediately before the writes. It
// is a cross-system check and therefore advisory: the caller's Redis lock
// could lapse a microsecond after it returns. The partial unique index on
// booking_seats is what actually makes a double sale impossible, and a
// violation of it surfaces here as ErrSeatConfirmed.
func (r *BookingRepo) Confirm(
	ctx context.Context,
	bookingID uuid.UUID,
	amount int64,
	providerRef string,
	verifyLocks func(context.Context) error,
) (*models.Booking, *models.Payment, error) {
	var (
		booking *models.Booking
		payment *models.Payment
	)

	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		const selectForUpdate = `SELECT ` + bookingColumns + ` FROM bookings WHERE id = $1 FOR UPDATE`

		current, err := scanBooking(tx.QueryRow(ctx, selectForUpdate, bookingID))
		if err != nil {
			return err
		}
		if current.Status != models.BookingPending {
			booking = current
			return ErrBookingNotPending
		}

		if verifyLocks != nil {
			if err := verifyLocks(ctx); err != nil {
				return err
			}
		}

		// The partial unique index only covers confirmed rows, so this UPDATE
		// is the moment the seats become exclusive. A competing transaction
		// that got here first makes this fail with a unique violation, which
		// classify turns into ErrSeatConfirmed.
		const confirmSeats = `UPDATE booking_seats SET confirmed = true WHERE booking_id = $1`
		if _, err := tx.Exec(ctx, confirmSeats, bookingID); err != nil {
			return fmt.Errorf("confirm booking seats: %w", classify(err))
		}

		const confirmBooking = `
			UPDATE bookings
			SET status = 'confirmed', confirmed_at = now()
			WHERE id = $1
			RETURNING ` + bookingColumns

		booking, err = scanBooking(tx.QueryRow(ctx, confirmBooking, bookingID))
		if err != nil {
			return fmt.Errorf("confirm booking: %w", err)
		}

		const insertPayment = `
			INSERT INTO payments (booking_id, status, amount, provider_ref)
			VALUES ($1, 'succeeded', $2, $3)
			RETURNING ` + paymentColumns

		payment, err = scanPayment(tx.QueryRow(ctx, insertPayment, bookingID, amount, providerRef))
		if err != nil {
			return fmt.Errorf("record payment: %w", err)
		}
		return nil
	})
	if err != nil {
		return booking, nil, err
	}
	return booking, payment, nil
}
