package repos

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
)

// EventRepo reads the event catalogue and seat inventory.
type EventRepo struct {
	pool *pgxpool.Pool
}

// NewEventRepo builds an EventRepo.
func NewEventRepo(pool *pgxpool.Pool) *EventRepo { return &EventRepo{pool: pool} }

// listQuery joins each event to its venue and counts seats in one pass.
//
// The seat total comes from the venue's inventory while the confirmed count is
// scoped to this event, so the same physical seats are reported independently
// for every event at that venue.
const listQuery = `
	SELECT e.id, e.venue_id, e.title, e.description, e.starts_at, e.base_price, e.created_at,
	       v.id, v.name, v.city,
	       (SELECT count(*) FROM seats s WHERE s.venue_id = v.id) AS seats_total,
	       (SELECT count(*) FROM booking_seats bs
	          WHERE bs.event_id = e.id AND bs.confirmed) AS seats_confirmed
	FROM events e
	JOIN venues v ON v.id = e.venue_id`

// List returns upcoming events first, then past ones, both by start time.
func (r *EventRepo) List(ctx context.Context) ([]models.EventWithVenue, error) {
	const query = listQuery + `
		ORDER BY (e.starts_at < now()), e.starts_at ASC`

	rows, err := r.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", classify(err))
	}
	defer rows.Close()

	events := make([]models.EventWithVenue, 0, 8)
	for rows.Next() {
		e, err := scanEventWithVenue(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", classify(err))
	}
	return events, nil
}

// GetByID loads a single event with its venue and seat counts.
func (r *EventRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.EventWithVenue, error) {
	const query = listQuery + ` WHERE e.id = $1`

	rows, err := r.pool.Query(ctx, query, id)
	if err != nil {
		return nil, fmt.Errorf("get event: %w", classify(err))
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("get event: %w", classify(err))
		}
		return nil, fmt.Errorf("get event: %w", ErrNotFound)
	}

	e, err := scanEventWithVenue(rows)
	if err != nil {
		return nil, err
	}
	return e, nil
}

// scanRow is the subset of pgx.Rows needed to scan one record.
type scanRow interface {
	Scan(dest ...any) error
}

func scanEventWithVenue(row scanRow) (*models.EventWithVenue, error) {
	var e models.EventWithVenue
	err := row.Scan(
		&e.ID, &e.VenueID, &e.Title, &e.Description, &e.StartsAt, &e.BasePrice, &e.CreatedAt,
		&e.Venue.ID, &e.Venue.Name, &e.Venue.City,
		&e.SeatsTotal, &e.SeatsConfirmed,
	)
	if err != nil {
		return nil, fmt.Errorf("scan event: %w", classify(err))
	}
	return &e, nil
}

// SeatRow is one seat plus whether it is already sold for the event in
// question. Redis-backed holds are merged on top of this by the service layer.
type SeatRow struct {
	Seat      models.Seat
	Confirmed bool
}

// SeatsForEvent returns every seat at the event's venue, flagging those
// already confirmed for this event.
//
// The LEFT JOIN is restricted to confirmed rows so that a pending booking,
// which may legitimately exist for a seat somebody else later buys, never
// makes a seat look sold.
func (r *EventRepo) SeatsForEvent(ctx context.Context, eventID, venueID uuid.UUID) ([]SeatRow, error) {
	const query = `
		SELECT s.id, s.venue_id, s.section, s."row", s.number,
		       (s.price_multiplier * 100)::bigint AS multiplier_bp,
		       (bs.seat_id IS NOT NULL) AS confirmed
		FROM seats s
		LEFT JOIN booking_seats bs
		       ON bs.seat_id = s.id
		      AND bs.event_id = $1
		      AND bs.confirmed
		WHERE s.venue_id = $2
		ORDER BY s.section, s."row", s.number`

	rows, err := r.pool.Query(ctx, query, eventID, venueID)
	if err != nil {
		return nil, fmt.Errorf("load seats for event: %w", classify(err))
	}
	defer rows.Close()

	seats := make([]SeatRow, 0, 400)
	for rows.Next() {
		var sr SeatRow
		if err := rows.Scan(
			&sr.Seat.ID, &sr.Seat.VenueID, &sr.Seat.Section, &sr.Seat.Row, &sr.Seat.Number,
			&sr.Seat.PriceMultiplierBP, &sr.Confirmed,
		); err != nil {
			return nil, fmt.Errorf("scan seat: %w", classify(err))
		}
		seats = append(seats, sr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate seats: %w", classify(err))
	}
	return seats, nil
}

// SeatsByIDs loads specific seats, verifying they belong to the given venue.
// Used when creating a hold, to price the seats and to reject IDs that are not
// part of this event's venue.
func (r *EventRepo) SeatsByIDs(ctx context.Context, venueID uuid.UUID, seatIDs []uuid.UUID) ([]models.Seat, error) {
	const query = `
		SELECT s.id, s.venue_id, s.section, s."row", s.number,
		       (s.price_multiplier * 100)::bigint AS multiplier_bp
		FROM seats s
		WHERE s.venue_id = $1 AND s.id = ANY($2)
		ORDER BY s.section, s."row", s.number`

	rows, err := r.pool.Query(ctx, query, venueID, seatIDs)
	if err != nil {
		return nil, fmt.Errorf("load seats by id: %w", classify(err))
	}
	defer rows.Close()

	seats := make([]models.Seat, 0, len(seatIDs))
	for rows.Next() {
		var s models.Seat
		if err := rows.Scan(&s.ID, &s.VenueID, &s.Section, &s.Row, &s.Number, &s.PriceMultiplierBP); err != nil {
			return nil, fmt.Errorf("scan seat: %w", classify(err))
		}
		seats = append(seats, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate seats: %w", classify(err))
	}
	return seats, nil
}

// ConfirmedSeatIDs returns the seats already sold for an event. The confirm
// path uses this as a pre-check; the partial unique index remains the actual
// guarantee.
func (r *EventRepo) ConfirmedSeatIDs(ctx context.Context, eventID uuid.UUID) (map[uuid.UUID]struct{}, error) {
	const query = `SELECT seat_id FROM booking_seats WHERE event_id = $1 AND confirmed`

	rows, err := r.pool.Query(ctx, query, eventID)
	if err != nil {
		return nil, fmt.Errorf("load confirmed seats: %w", classify(err))
	}
	defer rows.Close()

	confirmed := make(map[uuid.UUID]struct{})
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan confirmed seat: %w", classify(err))
		}
		confirmed[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate confirmed seats: %w", classify(err))
	}
	return confirmed, nil
}
