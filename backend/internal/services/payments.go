package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Aryan-Jain06/seatsync/backend/internal/httpx"
	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
	"github.com/Aryan-Jain06/seatsync/backend/internal/payments"
	"github.com/Aryan-Jain06/seatsync/backend/internal/repos"
)

// Bounds on the Idempotency-Key header.
const (
	minIdempotencyKeyLength = 8
	maxIdempotencyKeyLength = 255
)

// PaymentMutex serialises in-flight payment attempts for a single booking.
//
// The database already guarantees that a booking is confirmed at most once,
// but that guarantee arrives after the provider has been called. This mutex
// stops the provider being called twice in the first place, which against a
// real gateway is the difference between one charge and two.
type PaymentMutex interface {
	// AcquirePaymentSlot returns false when an attempt is already running.
	AcquirePaymentSlot(ctx context.Context, bookingID uuid.UUID, ttl time.Duration) (bool, error)
	// ReleasePaymentSlot frees the slot.
	ReleasePaymentSlot(ctx context.Context, bookingID uuid.UUID) error
}

// PaymentService drives the checkout: claim, charge, confirm.
type PaymentService struct {
	bookings    *repos.BookingRepo
	paymentRepo *repos.PaymentRepo
	locker      SeatLocker
	mutex       PaymentMutex
	provider    payments.Provider
	broadcaster SeatBroadcaster
}

// NewPaymentService builds a PaymentService.
func NewPaymentService(
	bookings *repos.BookingRepo,
	paymentRepo *repos.PaymentRepo,
	locker SeatLocker,
	mutex PaymentMutex,
	provider payments.Provider,
	broadcaster SeatBroadcaster,
) *PaymentService {
	if broadcaster == nil {
		broadcaster = NoopBroadcaster{}
	}
	return &PaymentService{
		bookings:    bookings,
		paymentRepo: paymentRepo,
		locker:      locker,
		mutex:       mutex,
		provider:    provider,
		broadcaster: broadcaster,
	}
}

// PayResult is the response to a payment attempt.
type PayResult struct {
	BookingID   uuid.UUID            `json:"booking_id"`
	Status      models.BookingStatus `json:"status"`
	Payment     paymentSummary       `json:"payment"`
	TotalAmount int64                `json:"total_amount"`
	ConfirmedAt *time.Time           `json:"confirmed_at,omitempty"`
	// Replayed marks a response served from a previous attempt rather than a
	// fresh charge, so a client can tell the two apart.
	Replayed bool `json:"replayed"`
}

type paymentSummary struct {
	ID          uuid.UUID            `json:"id"`
	Status      models.PaymentStatus `json:"status"`
	Amount      int64                `json:"amount"`
	ProviderRef string               `json:"provider_ref"`
	CreatedAt   time.Time            `json:"created_at"`
}

func summarise(p *models.Payment) paymentSummary {
	return paymentSummary{
		ID:          p.ID,
		Status:      p.Status,
		Amount:      p.Amount,
		ProviderRef: p.ProviderRef,
		CreatedAt:   p.CreatedAt,
	}
}

// paymentSlotTTL bounds how long one attempt may occupy the mutex. It exceeds
// the provider's worst-case latency so a slow charge is not cut short, while
// still releasing the slot if a process dies mid-attempt.
const paymentSlotTTL = 30 * time.Second

// Pay charges for a booking and, on success, confirms it.
//
// The sequence is deliberate:
//
//  1. Claim the Idempotency-Key under a row lock, so the booking is bound to
//     one attempt and a replay is recognisable.
//  2. Take the payment mutex, so only one attempt reaches the provider.
//  3. Check the seat locks before charging, to avoid taking money for seats
//     that have already gone.
//  4. Charge, outside any transaction. Holding one open across a one to two
//     second network call would pin a connection and block the sweep.
//  5. Confirm in a single transaction, re-verifying the locks inside it.
//  6. Release the seat locks only after the commit, because until then the
//     purchase could still roll back.
func (s *PaymentService) Pay(ctx context.Context, userID, bookingID uuid.UUID, idempotencyKey string) (*PayResult, error) {
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return nil, err
	}

	booking, err := s.bookings.GetByID(ctx, bookingID)
	if err != nil {
		if errors.Is(err, repos.ErrNotFound) {
			return nil, httpx.NotFound("Booking not found.")
		}
		return nil, httpx.Internal(fmt.Errorf("load booking: %w", err))
	}
	if booking.UserID != userID {
		return nil, httpx.NotFound("Booking not found.")
	}

	// A booking that is already paid for replays its stored result, which is
	// what makes a retry safe rather than a second charge.
	if booking.Status == models.BookingConfirmed {
		return s.replay(ctx, booking, idempotencyKey)
	}

	claimed, err := s.bookings.ClaimIdempotencyKey(ctx, bookingID, idempotencyKey)
	switch {
	case errors.Is(err, repos.ErrIdempotencyKeyMismatch):
		return nil, httpx.Conflict(httpx.CodeConflict,
			"This booking is already being paid for under a different Idempotency-Key.")
	case errors.Is(err, repos.ErrBookingNotPending):
		if claimed != nil && claimed.Status == models.BookingConfirmed {
			return s.replay(ctx, claimed, idempotencyKey)
		}
		return nil, httpx.Conflict(httpx.CodeHoldExpired, "This booking is no longer payable; the hold has lapsed.")
	case err != nil:
		return nil, httpx.Internal(fmt.Errorf("claim idempotency key: %w", err))
	}
	booking = claimed

	acquired, err := s.mutex.AcquirePaymentSlot(ctx, bookingID, paymentSlotTTL)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("acquire payment slot: %w", err))
	}
	if !acquired {
		return nil, httpx.Conflict(httpx.CodeConflict, "A payment for this booking is already in progress.")
	}
	defer s.releaseSlot(ctx, bookingID)

	seatIDs, err := s.bookings.SeatIDsFor(ctx, bookingID)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("load booking seats: %w", err))
	}

	// Fail before taking money if the hold has already gone.
	if err := s.verifySeatLocks(ctx, booking, seatIDs); err != nil {
		return nil, err
	}

	result, err := s.provider.Charge(ctx, booking.TotalAmount, idempotencyKey)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("charge payment: %w", err))
	}

	if !result.Succeeded {
		return s.recordDecline(ctx, booking, result)
	}

	return s.confirm(ctx, booking, seatIDs, result)
}

// confirm writes the successful purchase and frees the locks afterwards.
func (s *PaymentService) confirm(ctx context.Context, booking *models.Booking, seatIDs []uuid.UUID, charge *payments.Result) (*PayResult, error) {
	confirmed, payment, err := s.bookings.Confirm(ctx, booking.ID, booking.TotalAmount, charge.Reference,
		func(txCtx context.Context) error {
			return s.verifySeatLocks(txCtx, booking, seatIDs)
		})

	switch {
	case errors.Is(err, repos.ErrSeatConfirmed):
		// The partial unique index refused a second sale of a seat. This is
		// the guarantee firing, and it means a competing transaction won.
		slog.WarnContext(ctx, "confirm rejected by the unique index; a competing booking won the seats",
			"booking_id", booking.ID)
		return nil, httpx.Conflict(httpx.CodeSeatsTaken,
			"Someone else completed payment for one of those seats first.").
			WithDetails(map[string]any{"booking_id": booking.ID.String()})

	case errors.Is(err, repos.ErrBookingNotPending):
		if confirmed != nil && confirmed.Status == models.BookingConfirmed {
			return s.replay(ctx, confirmed, "")
		}
		return nil, httpx.Conflict(httpx.CodeHoldExpired, "This booking is no longer payable; the hold has lapsed.")

	case err != nil:
		var apiErr *httpx.APIError
		if errors.As(err, &apiErr) {
			// verifySeatLocks failed inside the transaction.
			return nil, apiErr
		}
		return nil, httpx.Internal(fmt.Errorf("confirm booking: %w", err))
	}

	// Only now is the sale durable, so only now may the seats be unlocked.
	// Releasing before the commit would expose them to a competing hold that
	// a rollback would then have to fight over.
	s.releaseSeatLocks(ctx, confirmed, seatIDs)

	updates := make([]models.SeatUpdate, 0, len(seatIDs))
	for _, seatID := range seatIDs {
		updates = append(updates, models.SeatUpdate{SeatID: seatID, Status: models.SeatConfirmed})
	}
	s.broadcaster.BroadcastSeatUpdates(confirmed.EventID, updates)

	return &PayResult{
		BookingID:   confirmed.ID,
		Status:      confirmed.Status,
		Payment:     summarise(payment),
		TotalAmount: confirmed.TotalAmount,
		ConfirmedAt: confirmed.ConfirmedAt,
	}, nil
}

// recordDecline stores a failed attempt and reports it.
//
// The booking stays pending on purpose: the seats are still held, so the user
// can retry with the same key until the hold lapses.
func (s *PaymentService) recordDecline(ctx context.Context, booking *models.Booking, charge *payments.Result) (*PayResult, error) {
	payment, err := s.paymentRepo.RecordFailure(ctx, booking.ID, booking.TotalAmount, charge.Reference)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("record failed payment: %w", err))
	}

	message := charge.DeclineReason
	if message == "" {
		message = "The payment was declined."
	}

	return nil, (&httpx.APIError{
		Status:  httpx.StatusPaymentRequired,
		Code:    httpx.CodePaymentFailed,
		Message: message + " Your seats are still held; you can try again.",
	}).WithDetails(map[string]any{
		"booking_id":    booking.ID.String(),
		"payment_id":    payment.ID.String(),
		"provider_ref":  payment.ProviderRef,
		"hold_expires":  booking.HoldExpiresAt.Format(time.RFC3339),
		"retry_allowed": true,
	})
}

// replay returns the stored outcome of a completed purchase.
func (s *PaymentService) replay(ctx context.Context, booking *models.Booking, presentedKey string) (*PayResult, error) {
	// A different key against an already-paid booking is a client bug worth
	// surfacing rather than silently treating as the same attempt.
	if presentedKey != "" && booking.IdempotencyKey != nil && *booking.IdempotencyKey != presentedKey {
		return nil, httpx.Conflict(httpx.CodeConflict,
			"This booking was already paid for under a different Idempotency-Key.")
	}

	payment, err := s.paymentRepo.SucceededFor(ctx, booking.ID)
	if err != nil {
		if errors.Is(err, repos.ErrNotFound) {
			// Confirmed with no successful payment row should be impossible,
			// since both are written in the same transaction.
			return nil, httpx.Internal(fmt.Errorf("booking %s is confirmed but has no successful payment", booking.ID))
		}
		return nil, httpx.Internal(fmt.Errorf("load payment for replay: %w", err))
	}

	return &PayResult{
		BookingID:   booking.ID,
		Status:      booking.Status,
		Payment:     summarise(payment),
		TotalAmount: booking.TotalAmount,
		ConfirmedAt: booking.ConfirmedAt,
		Replayed:    true,
	}, nil
}

// verifySeatLocks checks the caller still holds every seat.
func (s *PaymentService) verifySeatLocks(ctx context.Context, booking *models.Booking, seatIDs []uuid.UUID) error {
	missing, err := s.locker.HoldsFor(ctx, booking.EventID, booking.UserID, booking.LockToken, seatIDs)
	if err != nil {
		return httpx.Internal(fmt.Errorf("verify seat locks: %w", err))
	}
	if len(missing) == 0 {
		return nil
	}

	ids := make([]string, 0, len(missing))
	for _, id := range missing {
		ids = append(ids, id.String())
	}
	return httpx.Conflict(httpx.CodeHoldExpired,
		"Your hold on one or more of those seats has expired.").
		WithDetails(map[string]any{"expired_seat_ids": ids})
}

// releaseSeatLocks drops the locks over seats that are now permanently sold.
func (s *PaymentService) releaseSeatLocks(ctx context.Context, booking *models.Booking, seatIDs []uuid.UUID) {
	// Detached from the request context: the sale is committed, and the locks
	// must be cleared even if the client has since disconnected.
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if _, err := s.locker.Release(releaseCtx, booking.EventID, booking.UserID, booking.LockToken, seatIDs); err != nil {
		// Not fatal. The locks expire on their own, and the seats already
		// read as confirmed from Postgres, which outranks any stale hold.
		slog.ErrorContext(ctx, "could not release seat locks after confirming",
			"error", err, "booking_id", booking.ID)
	}
}

func (s *PaymentService) releaseSlot(ctx context.Context, bookingID uuid.UUID) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := s.mutex.ReleasePaymentSlot(releaseCtx, bookingID); err != nil {
		slog.ErrorContext(ctx, "could not release payment slot", "error", err, "booking_id", bookingID)
	}
}

// validateIdempotencyKey enforces the header's presence and shape.
func validateIdempotencyKey(key string) error {
	key = strings.TrimSpace(key)

	if key == "" {
		return httpx.BadRequest("An Idempotency-Key header is required to pay for a booking.")
	}
	if len(key) < minIdempotencyKeyLength {
		return httpx.Validation(fmt.Sprintf("Idempotency-Key must be at least %d characters.", minIdempotencyKeyLength))
	}
	if len(key) > maxIdempotencyKeyLength {
		return httpx.Validation(fmt.Sprintf("Idempotency-Key must be at most %d characters.", maxIdempotencyKeyLength))
	}
	return nil
}
