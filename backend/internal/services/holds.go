package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/Aryan-Jain06/seatsync/backend/internal/httpx"
	"github.com/Aryan-Jain06/seatsync/backend/internal/locks"
	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
	"github.com/Aryan-Jain06/seatsync/backend/internal/repos"
)

// SeatLocker is the lock store the hold service drives. Declaring it here,
// rather than importing the Redis implementation, keeps the service testable
// with a fake and free of a direct Redis dependency.
type SeatLocker interface {
	Acquire(ctx context.Context, eventID, userID uuid.UUID, lockToken string, seatIDs []uuid.UUID) (time.Time, error)
	Release(ctx context.Context, eventID, userID uuid.UUID, lockToken string, seatIDs []uuid.UUID) (int, error)
	HoldsFor(ctx context.Context, eventID, userID uuid.UUID, lockToken string, seatIDs []uuid.UUID) ([]uuid.UUID, error)
}

// SeatBroadcaster publishes seat state changes to interested clients.
//
// Like HoldReader, this is a seam: Phase 5 supplies a WebSocket hub, and
// until then a no-op keeps the hold path complete.
type SeatBroadcaster interface {
	BroadcastSeatUpdates(eventID uuid.UUID, updates []models.SeatUpdate)
}

// NoopBroadcaster discards every update.
type NoopBroadcaster struct{}

// BroadcastSeatUpdates does nothing.
func (NoopBroadcaster) BroadcastSeatUpdates(uuid.UUID, []models.SeatUpdate) {}

// HoldService creates and releases seat holds.
type HoldService struct {
	events      *repos.EventRepo
	bookings    *repos.BookingRepo
	locker      SeatLocker
	broadcaster SeatBroadcaster
	maxSeats    int
}

// NewHoldService builds a HoldService.
func NewHoldService(
	events *repos.EventRepo,
	bookings *repos.BookingRepo,
	locker SeatLocker,
	broadcaster SeatBroadcaster,
	maxSeats int,
) *HoldService {
	if broadcaster == nil {
		broadcaster = NoopBroadcaster{}
	}
	return &HoldService{
		events:      events,
		bookings:    bookings,
		locker:      locker,
		broadcaster: broadcaster,
		maxSeats:    maxSeats,
	}
}

// HoldResult is what a successful hold returns to the client.
type HoldResult struct {
	BookingID   uuid.UUID           `json:"booking_id"`
	EventID     uuid.UUID           `json:"event_id"`
	ExpiresAt   time.Time           `json:"expires_at"`
	TotalAmount int64               `json:"total_amount"`
	Seats       []models.BookedSeat `json:"seats"`
}

// CreateHold locks the requested seats for the caller and opens a pending
// booking over them.
//
// The ordering is deliberate. Redis is claimed before Postgres is written,
// because the lock is what excludes competing callers and the booking row is
// merely the record of it. If the write then fails, the locks are handed back
// rather than left to time out, so a database blip does not strand seats for
// the full hold period.
func (s *HoldService) CreateHold(ctx context.Context, userID, eventID uuid.UUID, seatIDs []uuid.UUID) (*HoldResult, error) {
	seatIDs, err := s.validateSeatSelection(seatIDs)
	if err != nil {
		return nil, err
	}

	event, err := s.events.GetByID(ctx, eventID)
	if err != nil {
		if errors.Is(err, repos.ErrNotFound) {
			return nil, httpx.NotFound("Event not found.")
		}
		return nil, httpx.Internal(fmt.Errorf("load event: %w", err))
	}

	if !event.StartsAt.After(time.Now()) {
		return nil, httpx.Conflict(httpx.CodeConflict, "This event has already started.")
	}

	// Loading the seats both prices them and proves every requested id is a
	// real seat at this event's venue.
	seats, err := s.events.SeatsByIDs(ctx, event.VenueID, seatIDs)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("load seats: %w", err))
	}
	if len(seats) != len(seatIDs) {
		return nil, httpx.Validation("One or more of those seats do not belong to this event.")
	}

	// A sold seat has no Redis lock, since the lock is dropped once payment
	// succeeds, so the lock store alone would happily hand it out again.
	confirmed, err := s.events.ConfirmedSeatIDs(ctx, eventID)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("load confirmed seats: %w", err))
	}
	if taken := intersect(seatIDs, confirmed); len(taken) > 0 {
		return nil, seatsUnavailableError(taken, "Some of those seats have already been sold.")
	}

	lockToken := uuid.NewString()

	expiresAt, err := s.locker.Acquire(ctx, eventID, userID, lockToken, seatIDs)
	if err != nil {
		var unavailable *locks.ErrSeatsUnavailable
		if errors.As(err, &unavailable) {
			return nil, seatsUnavailableError(unavailable.SeatIDs, "Some of those seats are already held by someone else.")
		}
		return nil, httpx.Internal(fmt.Errorf("acquire seat locks: %w", err))
	}

	var total int64
	bookedSeats := make([]models.BookedSeat, 0, len(seats))
	for _, seat := range seats {
		price := seat.Price(event.BasePrice)
		total += price
		bookedSeats = append(bookedSeats, models.BookedSeat{
			SeatID:  seat.ID,
			Section: seat.Section,
			Row:     seat.Row,
			Number:  seat.Number,
			Price:   price,
		})
	}

	booking, err := s.bookings.CreatePending(ctx, userID, eventID, seatIDs, total, lockToken, expiresAt)
	if err != nil {
		// Compensate: the locks were taken on the strength of a booking that
		// does not exist, so give the seats straight back.
		s.releaseQuietly(ctx, eventID, userID, lockToken, seatIDs, "booking insert failed")
		return nil, httpx.Internal(fmt.Errorf("create pending booking: %w", err))
	}

	s.broadcast(eventID, seatIDs, models.SeatHeld)

	return &HoldResult{
		BookingID:   booking.ID,
		EventID:     eventID,
		ExpiresAt:   expiresAt,
		TotalAmount: total,
		Seats:       bookedSeats,
	}, nil
}

// ReleaseHold gives up a pending booking's seats early.
func (s *HoldService) ReleaseHold(ctx context.Context, userID, bookingID uuid.UUID) error {
	booking, err := s.bookings.GetByID(ctx, bookingID)
	if err != nil {
		if errors.Is(err, repos.ErrNotFound) {
			return httpx.NotFound("Booking not found.")
		}
		return httpx.Internal(fmt.Errorf("load booking: %w", err))
	}

	// Report a missing booking rather than a forbidden one, so this endpoint
	// cannot be used to discover which booking ids exist.
	if booking.UserID != userID {
		return httpx.NotFound("Booking not found.")
	}

	switch booking.Status {
	case models.BookingConfirmed:
		return httpx.Conflict(httpx.CodeConflict, "This booking has already been paid for and cannot be released.")
	case models.BookingCancelled, models.BookingExpired:
		// Already released. The caller's intent is satisfied, so treat a
		// repeat as success rather than an error.
		return nil
	}

	seatIDs, err := s.bookings.SeatIDsFor(ctx, bookingID)
	if err != nil {
		return httpx.Internal(fmt.Errorf("load booking seats: %w", err))
	}

	if _, err := s.locker.Release(ctx, booking.EventID, userID, booking.LockToken, seatIDs); err != nil {
		// The locks will lapse on their own, so record it and still cancel
		// the booking rather than leaving the user stuck with it.
		slog.ErrorContext(ctx, "could not release seat locks", "error", err, "booking_id", bookingID)
	}

	if _, err := s.bookings.MarkCancelled(ctx, bookingID); err != nil {
		return httpx.Internal(fmt.Errorf("cancel booking: %w", err))
	}

	s.broadcast(booking.EventID, seatIDs, models.SeatAvailable)
	return nil
}

// GetBooking returns one of the caller's bookings.
func (s *HoldService) GetBooking(ctx context.Context, userID, bookingID uuid.UUID) (*models.BookingDetail, error) {
	detail, err := s.bookings.GetDetail(ctx, bookingID)
	if err != nil {
		if errors.Is(err, repos.ErrNotFound) {
			return nil, httpx.NotFound("Booking not found.")
		}
		return nil, httpx.Internal(fmt.Errorf("load booking: %w", err))
	}
	if detail.UserID != userID {
		return nil, httpx.NotFound("Booking not found.")
	}
	return detail, nil
}

// ListBookings returns the caller's bookings, newest first.
func (s *HoldService) ListBookings(ctx context.Context, userID uuid.UUID) ([]models.BookingDetail, error) {
	bookings, err := s.bookings.ListForUser(ctx, userID)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("list bookings: %w", err))
	}
	return bookings, nil
}

// validateSeatSelection checks the request and removes duplicates.
func (s *HoldService) validateSeatSelection(seatIDs []uuid.UUID) ([]uuid.UUID, error) {
	if len(seatIDs) == 0 {
		return nil, httpx.Validation("Select at least one seat.")
	}

	seen := make(map[uuid.UUID]struct{}, len(seatIDs))
	unique := make([]uuid.UUID, 0, len(seatIDs))
	for _, id := range seatIDs {
		if id == uuid.Nil {
			return nil, httpx.Validation("Seat ids must not be empty.")
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}

	if len(unique) > s.maxSeats {
		return nil, httpx.Validation(fmt.Sprintf("You can hold at most %d seats at a time.", s.maxSeats))
	}
	return unique, nil
}

// releaseQuietly hands seats back on a compensating path, where the caller is
// already returning an error and a second failure changes nothing.
func (s *HoldService) releaseQuietly(ctx context.Context, eventID, userID uuid.UUID, lockToken string, seatIDs []uuid.UUID, reason string) {
	// The request context may already be cancelled; the locks still need
	// releasing, so detach and bound it separately.
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if _, err := s.locker.Release(releaseCtx, eventID, userID, lockToken, seatIDs); err != nil {
		slog.ErrorContext(ctx, "could not release seat locks after a failure",
			"error", err, "reason", reason, "event_id", eventID)
	}
}

// broadcast publishes one status for a set of seats.
func (s *HoldService) broadcast(eventID uuid.UUID, seatIDs []uuid.UUID, status models.SeatStatus) {
	updates := make([]models.SeatUpdate, 0, len(seatIDs))
	for _, seatID := range seatIDs {
		updates = append(updates, models.SeatUpdate{SeatID: seatID, Status: status})
	}
	s.broadcaster.BroadcastSeatUpdates(eventID, updates)
}

// seatsUnavailableError builds the 409 returned when seats are contended,
// listing the offending seats so the UI can highlight them.
func seatsUnavailableError(seatIDs []uuid.UUID, message string) *httpx.APIError {
	ids := make([]string, 0, len(seatIDs))
	for _, id := range seatIDs {
		ids = append(ids, id.String())
	}
	return httpx.Conflict(httpx.CodeSeatsTaken, message).
		WithDetails(map[string]any{"unavailable_seat_ids": ids})
}

// intersect returns the ids present in both inputs, preserving request order.
func intersect(ids []uuid.UUID, set map[uuid.UUID]struct{}) []uuid.UUID {
	var found []uuid.UUID
	for _, id := range ids {
		if _, ok := set[id]; ok {
			found = append(found, id)
		}
	}
	return found
}
