package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/Aryan-Jain06/seatsync/backend/internal/httpx"
	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
	"github.com/Aryan-Jain06/seatsync/backend/internal/repos"
)

// HoldReader exposes the live holds for an event.
//
// This interface is the seam between the seat map and Redis. Phase 2 supplies
// a no-op implementation so the read path can be built and tested against
// Postgres alone; Phase 3 swaps in the Redis-backed one without the seat map
// code changing.
type HoldReader interface {
	// HeldSeats returns the seats currently held for an event, keyed by seat
	// ID. Expired entries must not be included.
	HeldSeats(ctx context.Context, eventID uuid.UUID) (map[uuid.UUID]models.SeatHold, error)
}

// NoopHoldReader reports that nothing is held. It keeps the seat map working
// before the lock store exists, and keeps the service testable without Redis.
type NoopHoldReader struct{}

// HeldSeats always returns an empty map.
func (NoopHoldReader) HeldSeats(context.Context, uuid.UUID) (map[uuid.UUID]models.SeatHold, error) {
	return map[uuid.UUID]models.SeatHold{}, nil
}

// CatalogService serves the read path: events and seat maps.
type CatalogService struct {
	events *repos.EventRepo
	holds  HoldReader
}

// NewCatalogService builds a CatalogService. A nil holds reader degrades to
// reporting no holds rather than failing.
func NewCatalogService(events *repos.EventRepo, holds HoldReader) *CatalogService {
	if holds == nil {
		holds = NoopHoldReader{}
	}
	return &CatalogService{events: events, holds: holds}
}

// ListEvents returns the event catalogue.
func (s *CatalogService) ListEvents(ctx context.Context) ([]models.EventWithVenue, error) {
	events, err := s.events.List(ctx)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("list events: %w", err))
	}
	return events, nil
}

// GetEvent returns one event, or a 404 when it does not exist.
func (s *CatalogService) GetEvent(ctx context.Context, id uuid.UUID) (*models.EventWithVenue, error) {
	event, err := s.events.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, repos.ErrNotFound) {
			return nil, httpx.NotFound("Event not found.")
		}
		return nil, httpx.Internal(fmt.Errorf("get event: %w", err))
	}
	return event, nil
}

// SeatMap assembles the seat map for an event.
//
// Two sources are merged. Postgres supplies the permanent truth, which seats
// are sold, and the lock store supplies the transient truth, which seats are
// held right now. Confirmed always wins: once a seat is paid for it can never
// read as merely held, whatever a stale lock might claim.
//
// viewer may be uuid.Nil for anonymous callers, in which case no seat is
// reported as held by the viewer.
func (s *CatalogService) SeatMap(ctx context.Context, eventID, viewer uuid.UUID) (*models.SeatMap, error) {
	event, err := s.GetEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}

	seatRows, err := s.events.SeatsForEvent(ctx, eventID, event.VenueID)
	if err != nil {
		return nil, httpx.Internal(fmt.Errorf("load seats: %w", err))
	}

	held, err := s.holds.HeldSeats(ctx, eventID)
	if err != nil {
		// The seat map is still useful without hold data: every seat simply
		// reads as available or confirmed. Failing the whole request would
		// take the event page down over a cache outage, so degrade instead
		// and make the degradation visible in the logs.
		slog.ErrorContext(ctx, "could not read seat holds; serving seat map without them",
			"error", err, "event_id", eventID)
		held = map[uuid.UUID]models.SeatHold{}
	}

	return buildSeatMap(eventID, event.BasePrice, seatRows, held, viewer), nil
}

// buildSeatMap merges the durable and transient views of seat state.
//
// It is a pure function so the precedence rules, which are the subtle part,
// can be tested without a database or a Redis instance.
func buildSeatMap(
	eventID uuid.UUID,
	basePrice int64,
	seatRows []repos.SeatRow,
	held map[uuid.UUID]models.SeatHold,
	viewer uuid.UUID,
) *models.SeatMap {
	seatMap := &models.SeatMap{
		EventID: eventID,
		Seats:   make([]models.SeatMapEntry, 0, len(seatRows)),
	}

	for _, row := range seatRows {
		entry := models.SeatMapEntry{
			SeatID:  row.Seat.ID,
			Section: row.Seat.Section,
			Row:     row.Seat.Row,
			Number:  row.Seat.Number,
			Price:   row.Seat.Price(basePrice),
			Status:  models.SeatAvailable,
		}

		// Order matters: a confirmed seat reads as confirmed even if a stale
		// lock still claims it, because the sale is permanent and the lock is
		// not. Getting this backwards would show a sold seat as merely held
		// and invite a doomed booking attempt.
		switch hold, isHeld := held[row.Seat.ID]; {
		case row.Confirmed:
			entry.Status = models.SeatConfirmed
			seatMap.Confirmed++
		case isHeld:
			entry.Status = models.SeatHeld
			entry.HeldByMe = viewer != uuid.Nil && hold.UserID == viewer
			seatMap.Held++
		default:
			seatMap.Available++
		}

		seatMap.Seats = append(seatMap.Seats, entry)
	}

	return seatMap
}
