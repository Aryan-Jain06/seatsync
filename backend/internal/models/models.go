// Package models holds the domain types shared by the repository, service and
// handler layers, together with the JSON shapes returned by the API.
package models

import (
	"time"

	"github.com/google/uuid"
)

// Role is a user's authorisation level.
type Role string

const (
	RoleUser  Role = "user"
	RoleAdmin Role = "admin"
)

// BookingStatus tracks a booking through its lifecycle.
//
//	pending   -> seats are held in Redis, payment not yet completed
//	confirmed -> paid; seats belong to the user permanently
//	cancelled -> the user released the hold
//	expired   -> the hold lapsed before payment completed
type BookingStatus string

const (
	BookingPending   BookingStatus = "pending"
	BookingConfirmed BookingStatus = "confirmed"
	BookingCancelled BookingStatus = "cancelled"
	BookingExpired   BookingStatus = "expired"
)

// PaymentStatus is the outcome of a single payment attempt.
type PaymentStatus string

const (
	PaymentSucceeded PaymentStatus = "succeeded"
	PaymentFailed    PaymentStatus = "failed"
)

// SeatStatus is a seat's availability for one event at one moment.
type SeatStatus string

const (
	// SeatAvailable means nobody holds or owns the seat.
	SeatAvailable SeatStatus = "available"
	// SeatHeld means a live Redis lock covers the seat.
	SeatHeld SeatStatus = "held"
	// SeatConfirmed means the seat has been paid for and is gone for good.
	SeatConfirmed SeatStatus = "confirmed"
)

// User is an account. PasswordHash never leaves the repository layer.
type User struct {
	ID           uuid.UUID `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	Name         string    `json:"name"`
	Role         Role      `json:"role"`
	CreatedAt    time.Time `json:"created_at"`
}

// Venue is a physical location that owns a fixed set of seats.
type Venue struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	City string    `json:"city"`
}

// Event is a scheduled performance at a venue.
//
// BasePrice is in minor units (paise/cents).
type Event struct {
	ID          uuid.UUID `json:"id"`
	VenueID     uuid.UUID `json:"venue_id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	StartsAt    time.Time `json:"starts_at"`
	BasePrice   int64     `json:"base_price"`
	CreatedAt   time.Time `json:"created_at"`
}

// EventWithVenue is the list/detail shape returned by the events endpoints.
type EventWithVenue struct {
	Event
	Venue Venue `json:"venue"`
	// SeatsTotal and SeatsConfirmed give a cheap availability summary without
	// requiring the caller to fetch the whole seat map.
	SeatsTotal     int `json:"seats_total"`
	SeatsConfirmed int `json:"seats_confirmed"`
}

// Seat is a physical seat belonging to a venue.
//
// PriceMultiplierBP is the multiplier in basis points of 1.00 scaled by 100,
// i.e. the NUMERIC(4,2) column multiplied by 100. Keeping it as an integer
// avoids floating-point drift when computing prices.
type Seat struct {
	ID                uuid.UUID `json:"id"`
	VenueID           uuid.UUID `json:"venue_id"`
	Section           string    `json:"section"`
	Row               int       `json:"row"`
	Number            int       `json:"number"`
	PriceMultiplierBP int64     `json:"-"`
}

// Price returns the seat's price for an event, in minor units.
func (s Seat) Price(basePrice int64) int64 {
	// Round half up rather than truncating, so a 1.50 multiplier on an odd
	// base price does not silently lose a unit.
	return (basePrice*s.PriceMultiplierBP + 50) / 100
}

// SeatMapEntry is one seat as rendered on the seat map.
type SeatMapEntry struct {
	SeatID  uuid.UUID  `json:"seat_id"`
	Section string     `json:"section"`
	Row     int        `json:"row"`
	Number  int        `json:"number"`
	Price   int64      `json:"price"`
	Status  SeatStatus `json:"status"`
	// HeldByMe distinguishes the caller's own holds from other people's, so
	// the UI can colour them differently. Always false for anonymous callers.
	HeldByMe bool `json:"held_by_me"`
}

// SeatMap is the full seat map for one event.
type SeatMap struct {
	EventID   uuid.UUID      `json:"event_id"`
	Seats     []SeatMapEntry `json:"seats"`
	Available int            `json:"available"`
	Held      int            `json:"held"`
	Confirmed int            `json:"confirmed"`
}

// Booking is a user's claim over a set of seats for an event.
type Booking struct {
	ID             uuid.UUID     `json:"id"`
	UserID         uuid.UUID     `json:"user_id"`
	EventID        uuid.UUID     `json:"event_id"`
	Status         BookingStatus `json:"status"`
	TotalAmount    int64         `json:"total_amount"`
	IdempotencyKey *string       `json:"-"`
	LockToken      string        `json:"-"`
	HoldExpiresAt  time.Time     `json:"hold_expires_at"`
	CreatedAt      time.Time     `json:"created_at"`
	ConfirmedAt    *time.Time    `json:"confirmed_at,omitempty"`
}

// BookedSeat describes one seat attached to a booking.
type BookedSeat struct {
	SeatID  uuid.UUID `json:"seat_id"`
	Section string    `json:"section"`
	Row     int       `json:"row"`
	Number  int       `json:"number"`
	Price   int64     `json:"price"`
}

// BookingDetail is a booking together with its seats and event summary.
type BookingDetail struct {
	Booking
	EventTitle    string       `json:"event_title"`
	EventStartsAt time.Time    `json:"event_starts_at"`
	VenueName     string       `json:"venue_name"`
	Seats         []BookedSeat `json:"seats"`
}

// Payment records one attempt against the payment provider.
type Payment struct {
	ID          uuid.UUID     `json:"id"`
	BookingID   uuid.UUID     `json:"booking_id"`
	Status      PaymentStatus `json:"status"`
	Amount      int64         `json:"amount"`
	ProviderRef string        `json:"provider_ref"`
	CreatedAt   time.Time     `json:"created_at"`
}

// SeatHold is a live claim over a seat, as recorded by the lock store.
type SeatHold struct {
	// UserID is who holds the seat.
	UserID uuid.UUID `json:"user_id"`
	// ExpiresAt is when the hold lapses and the seat frees itself.
	ExpiresAt time.Time `json:"expires_at"`
}

// SeatUpdate is broadcast over WebSocket whenever a seat changes state.
type SeatUpdate struct {
	SeatID uuid.UUID  `json:"seat_id"`
	Status SeatStatus `json:"status"`
}
