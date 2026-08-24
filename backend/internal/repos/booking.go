package repos

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
)

// BookingRepo reads and writes bookings and the seats attached to them.
type BookingRepo struct {
	pool *pgxpool.Pool
}

// NewBookingRepo builds a BookingRepo.
func NewBookingRepo(pool *pgxpool.Pool) *BookingRepo { return &BookingRepo{pool: pool} }

// Pool exposes the underlying pool for callers that need to run their own
// transaction spanning several repositories, such as the confirm path.
func (r *BookingRepo) Pool() *pgxpool.Pool { return r.pool }

const bookingColumns = `
	id, user_id, event_id, status, total_amount, idempotency_key,
	lock_token, hold_expires_at, created_at, confirmed_at`

func scanBooking(row scanRow) (*models.Booking, error) {
	var b models.Booking
	err := row.Scan(
		&b.ID, &b.UserID, &b.EventID, &b.Status, &b.TotalAmount, &b.IdempotencyKey,
		&b.LockToken, &b.HoldExpiresAt, &b.CreatedAt, &b.ConfirmedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scan booking: %w", classify(err))
	}
	return &b, nil
}

// CreatePending inserts a pending booking together with its seats.
//
// Both writes happen in one transaction: a booking without its seats would be
// unpayable, and seats without a booking would be unreachable orphans.
func (r *BookingRepo) CreatePending(
	ctx context.Context,
	userID, eventID uuid.UUID,
	seatIDs []uuid.UUID,
	totalAmount int64,
	lockToken string,
	holdExpiresAt time.Time,
) (*models.Booking, error) {
	var booking *models.Booking

	err := InTx(ctx, r.pool, func(tx pgx.Tx) error {
		const insertBooking = `
			INSERT INTO bookings (user_id, event_id, status, total_amount, lock_token, hold_expires_at)
			VALUES ($1, $2, 'pending', $3, $4, $5)
			RETURNING ` + bookingColumns

		var err error
		booking, err = scanBooking(tx.QueryRow(ctx, insertBooking, userID, eventID, totalAmount, lockToken, holdExpiresAt))
		if err != nil {
			return fmt.Errorf("insert booking: %w", err)
		}

		rows := make([][]any, 0, len(seatIDs))
		for _, seatID := range seatIDs {
			// confirmed stays false: the partial unique index does not apply
			// until payment succeeds, so several pending bookings may name
			// the same seat without colliding.
			rows = append(rows, []any{booking.ID, seatID, eventID, false})
		}

		if _, err := tx.CopyFrom(ctx,
			pgx.Identifier{"booking_seats"},
			[]string{"booking_id", "seat_id", "event_id", "confirmed"},
			pgx.CopyFromRows(rows),
		); err != nil {
			return fmt.Errorf("insert booking seats: %w", classify(err))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return booking, nil
}

// GetByID loads one booking.
func (r *BookingRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.Booking, error) {
	const query = `SELECT ` + bookingColumns + ` FROM bookings WHERE id = $1`
	return scanBooking(r.pool.QueryRow(ctx, query, id))
}

// SeatIDsFor returns the seat ids attached to a booking, in a stable order.
//
// The ordering matters for the confirm path: taking row locks in a consistent
// order across concurrent transactions is what stops two of them deadlocking
// by grabbing the same pair of seats in opposite orders.
func (r *BookingRepo) SeatIDsFor(ctx context.Context, bookingID uuid.UUID) ([]uuid.UUID, error) {
	const query = `SELECT seat_id FROM booking_seats WHERE booking_id = $1 ORDER BY seat_id`

	rows, err := r.pool.Query(ctx, query, bookingID)
	if err != nil {
		return nil, fmt.Errorf("load booking seats: %w", classify(err))
	}
	defer rows.Close()

	seatIDs := make([]uuid.UUID, 0, 6)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan booking seat: %w", classify(err))
		}
		seatIDs = append(seatIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate booking seats: %w", classify(err))
	}
	return seatIDs, nil
}

// MarkCancelled moves a pending booking to cancelled.
//
// The status guard makes this a no-op on a booking that has since been
// confirmed or expired, so a late release cannot undo a completed purchase.
func (r *BookingRepo) MarkCancelled(ctx context.Context, id uuid.UUID) (bool, error) {
	const query = `UPDATE bookings SET status = 'cancelled' WHERE id = $1 AND status = 'pending'`

	tag, err := r.pool.Exec(ctx, query, id)
	if err != nil {
		return false, fmt.Errorf("cancel booking: %w", classify(err))
	}
	return tag.RowsAffected() > 0, nil
}

// ExpiredBooking is a booking whose hold lapsed, along with the seats it
// was occupying so they can be advertised as free again.
type ExpiredBooking struct {
	BookingID uuid.UUID
	EventID   uuid.UUID
	SeatIDs   []uuid.UUID
}

// ClaimExpired atomically marks lapsed pending bookings as expired and returns
// what it claimed.
//
// FOR UPDATE SKIP LOCKED means several server instances can run this sweep
// concurrently: each claims a disjoint batch instead of blocking on, or
// double-processing, the same rows.
func (r *BookingRepo) ClaimExpired(ctx context.Context, limit int) ([]ExpiredBooking, error) {
	const query = `
		WITH claimed AS (
			SELECT id
			FROM bookings
			WHERE status = 'pending' AND hold_expires_at <= now()
			ORDER BY hold_expires_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		), expired AS (
			UPDATE bookings b
			SET status = 'expired'
			FROM claimed c
			WHERE b.id = c.id
			RETURNING b.id, b.event_id
		)
		SELECT e.id, e.event_id, bs.seat_id
		FROM expired e
		JOIN booking_seats bs ON bs.booking_id = e.id
		ORDER BY e.id`

	rows, err := r.pool.Query(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("claim expired bookings: %w", classify(err))
	}
	defer rows.Close()

	// Rows arrive one per seat, grouped by booking.
	var (
		claimed []ExpiredBooking
		current *ExpiredBooking
	)

	for rows.Next() {
		var bookingID, eventID, seatID uuid.UUID
		if err := rows.Scan(&bookingID, &eventID, &seatID); err != nil {
			return nil, fmt.Errorf("scan expired booking: %w", classify(err))
		}

		if current == nil || current.BookingID != bookingID {
			claimed = append(claimed, ExpiredBooking{BookingID: bookingID, EventID: eventID})
			current = &claimed[len(claimed)-1]
		}
		current.SeatIDs = append(current.SeatIDs, seatID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired bookings: %w", classify(err))
	}
	return claimed, nil
}

// detailColumns joins a booking to the event and venue a client needs to
// render it, without a second round trip.
const detailColumns = `
	b.id, b.user_id, b.event_id, b.status, b.total_amount, b.idempotency_key,
	b.lock_token, b.hold_expires_at, b.created_at, b.confirmed_at,
	e.title, e.starts_at, v.name`

// ListForUser returns a user's bookings, newest first, with their seats.
func (r *BookingRepo) ListForUser(ctx context.Context, userID uuid.UUID) ([]models.BookingDetail, error) {
	const query = `
		SELECT ` + detailColumns + `,
		       s.id, s.section, s."row", s.number,
		       ((e.base_price * s.price_multiplier) + 0.5)::bigint AS seat_price
		FROM bookings b
		JOIN events e ON e.id = b.event_id
		JOIN venues v ON v.id = e.venue_id
		JOIN booking_seats bs ON bs.booking_id = b.id
		JOIN seats s ON s.id = bs.seat_id
		WHERE b.user_id = $1
		ORDER BY b.created_at DESC, s.section, s."row", s.number`

	rows, err := r.pool.Query(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("list bookings: %w", classify(err))
	}
	defer rows.Close()

	var (
		bookings []models.BookingDetail
		current  *models.BookingDetail
	)

	for rows.Next() {
		var (
			d    models.BookingDetail
			seat models.BookedSeat
		)
		if err := rows.Scan(
			&d.ID, &d.UserID, &d.EventID, &d.Status, &d.TotalAmount, &d.IdempotencyKey,
			&d.LockToken, &d.HoldExpiresAt, &d.CreatedAt, &d.ConfirmedAt,
			&d.EventTitle, &d.EventStartsAt, &d.VenueName,
			&seat.SeatID, &seat.Section, &seat.Row, &seat.Number, &seat.Price,
		); err != nil {
			return nil, fmt.Errorf("scan booking detail: %w", classify(err))
		}

		if current == nil || current.ID != d.ID {
			d.Seats = []models.BookedSeat{}
			bookings = append(bookings, d)
			current = &bookings[len(bookings)-1]
		}
		current.Seats = append(current.Seats, seat)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate bookings: %w", classify(err))
	}

	if bookings == nil {
		bookings = []models.BookingDetail{}
	}
	return bookings, nil
}

// GetDetail loads one booking with its seats and event summary.
func (r *BookingRepo) GetDetail(ctx context.Context, bookingID uuid.UUID) (*models.BookingDetail, error) {
	const query = `
		SELECT ` + detailColumns + `,
		       s.id, s.section, s."row", s.number,
		       ((e.base_price * s.price_multiplier) + 0.5)::bigint AS seat_price
		FROM bookings b
		JOIN events e ON e.id = b.event_id
		JOIN venues v ON v.id = e.venue_id
		JOIN booking_seats bs ON bs.booking_id = b.id
		JOIN seats s ON s.id = bs.seat_id
		WHERE b.id = $1
		ORDER BY s.section, s."row", s.number`

	rows, err := r.pool.Query(ctx, query, bookingID)
	if err != nil {
		return nil, fmt.Errorf("load booking detail: %w", classify(err))
	}
	defer rows.Close()

	var detail *models.BookingDetail

	for rows.Next() {
		var (
			d    models.BookingDetail
			seat models.BookedSeat
		)
		if err := rows.Scan(
			&d.ID, &d.UserID, &d.EventID, &d.Status, &d.TotalAmount, &d.IdempotencyKey,
			&d.LockToken, &d.HoldExpiresAt, &d.CreatedAt, &d.ConfirmedAt,
			&d.EventTitle, &d.EventStartsAt, &d.VenueName,
			&seat.SeatID, &seat.Section, &seat.Row, &seat.Number, &seat.Price,
		); err != nil {
			return nil, fmt.Errorf("scan booking detail: %w", classify(err))
		}

		if detail == nil {
			d.Seats = []models.BookedSeat{}
			detail = &d
		}
		detail.Seats = append(detail.Seats, seat)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate booking detail: %w", classify(err))
	}
	if detail == nil {
		return nil, fmt.Errorf("load booking detail: %w", ErrNotFound)
	}
	return detail, nil
}
