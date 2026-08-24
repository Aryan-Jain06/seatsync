// Package locks implements seat locking on top of Redis.
//
// Two structures back every event:
//
//	lock:<eventId>:<seatId>     a string holding "<userId>:<lockToken>",
//	                            expiring on its own after the hold TTL
//	event:<eventId>:reserved    a sorted set of seat ids scored by expiry,
//	                            used to render the seat map in one query
//
// The lock keys are the authority on who holds what. The sorted set is a read
// model derived from them: it can lag, and reads tolerate that by skipping
// members whose lock key has gone.
package locks

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
)

//go:embed scripts/acquire.lua
var acquireScript string

//go:embed scripts/release.lua
var releaseScript string

//go:embed scripts/held.lua
var heldScript string

// Manager acquires, releases and reports seat locks.
type Manager struct {
	rdb *redis.Client
	ttl time.Duration

	acquire *redis.Script
	release *redis.Script
	held    *redis.Script
}

// NewManager builds a Manager. Scripts are wrapped in redis.Script, which
// invokes them by SHA and only ships the source when the server reports the
// script is unknown.
func NewManager(rdb *redis.Client, ttl time.Duration) *Manager {
	return &Manager{
		rdb:     rdb,
		ttl:     ttl,
		acquire: redis.NewScript(acquireScript),
		release: redis.NewScript(releaseScript),
		held:    redis.NewScript(heldScript),
	}
}

// TTL reports the configured hold lifetime.
func (m *Manager) TTL() time.Duration { return m.ttl }

// lockKey returns the lock key for one seat.
func lockKey(eventID, seatID uuid.UUID) string {
	return "lock:" + eventID.String() + ":" + seatID.String()
}

// lockPrefix returns the shared prefix of an event's lock keys.
func lockPrefix(eventID uuid.UUID) string {
	return "lock:" + eventID.String() + ":"
}

// reservedKey returns the sorted set key for an event.
func reservedKey(eventID uuid.UUID) string {
	return "event:" + eventID.String() + ":reserved"
}

// LockValue builds the value stored in a seat's lock key. Both the user and a
// per-booking random token are included: the user id alone would let one of a
// user's own bookings release another's seats.
func LockValue(userID uuid.UUID, lockToken string) string {
	return userID.String() + ":" + lockToken
}

// parseLockValue recovers the holder from a stored lock value.
func parseLockValue(value string) (uuid.UUID, string, bool) {
	userPart, token, found := strings.Cut(value, ":")
	if !found {
		return uuid.Nil, "", false
	}

	userID, err := uuid.Parse(userPart)
	if err != nil {
		return uuid.Nil, "", false
	}
	return userID, token, true
}

// ErrSeatsUnavailable reports that at least one requested seat was already
// locked. No lock was taken.
type ErrSeatsUnavailable struct {
	SeatIDs []uuid.UUID
}

func (e *ErrSeatsUnavailable) Error() string {
	return fmt.Sprintf("locks: %d seat(s) are already held", len(e.SeatIDs))
}

// Acquire locks every seat for the caller, or none of them.
//
// It returns ErrSeatsUnavailable listing the contended seats when any seat is
// already held.
func (m *Manager) Acquire(ctx context.Context, eventID, userID uuid.UUID, lockToken string, seatIDs []uuid.UUID) (expiresAt time.Time, err error) {
	if len(seatIDs) == 0 {
		return time.Time{}, errors.New("locks: no seats requested")
	}

	expiresAt = time.Now().Add(m.ttl)

	keys := make([]string, 0, len(seatIDs))
	for _, seatID := range seatIDs {
		keys = append(keys, lockKey(eventID, seatID))
	}

	args := make([]any, 0, 4+len(seatIDs))
	args = append(args,
		LockValue(userID, lockToken),
		int(m.ttl.Seconds()),
		reservedKey(eventID),
		expiresAt.UnixMilli(),
	)
	for _, seatID := range seatIDs {
		args = append(args, seatID.String())
	}

	raw, err := m.acquire.Run(ctx, m.rdb, keys, args...).Result()
	if err != nil {
		return time.Time{}, fmt.Errorf("locks: run acquire script: %w", err)
	}

	contended, err := parseSeatIDList(raw)
	if err != nil {
		return time.Time{}, err
	}
	if len(contended) > 0 {
		return time.Time{}, &ErrSeatsUnavailable{SeatIDs: contended}
	}
	return expiresAt, nil
}

// Release drops the caller's locks over the given seats and returns how many
// were actually released. Locks held by somebody else are left untouched.
func (m *Manager) Release(ctx context.Context, eventID, userID uuid.UUID, lockToken string, seatIDs []uuid.UUID) (int, error) {
	if len(seatIDs) == 0 {
		return 0, nil
	}

	keys := make([]string, 0, len(seatIDs))
	for _, seatID := range seatIDs {
		keys = append(keys, lockKey(eventID, seatID))
	}

	args := make([]any, 0, 2+len(seatIDs))
	args = append(args, LockValue(userID, lockToken), reservedKey(eventID))
	for _, seatID := range seatIDs {
		args = append(args, seatID.String())
	}

	released, err := m.release.Run(ctx, m.rdb, keys, args...).Int()
	if err != nil {
		return 0, fmt.Errorf("locks: run release script: %w", err)
	}
	return released, nil
}

// HeldSeats returns the live holds for an event, satisfying the seat map's
// HoldReader. Expired entries are swept as a side effect of reading.
func (m *Manager) HeldSeats(ctx context.Context, eventID uuid.UUID) (map[uuid.UUID]models.SeatHold, error) {
	raw, err := m.held.Run(ctx, m.rdb,
		[]string{reservedKey(eventID)},
		time.Now().UnixMilli(),
		lockPrefix(eventID),
	).Result()
	if err != nil {
		return nil, fmt.Errorf("locks: run held script: %w", err)
	}

	entries, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("locks: held script returned %T, expected an array", raw)
	}

	holds := make(map[uuid.UUID]models.SeatHold, len(entries)/3)

	for i := 0; i+2 < len(entries); i += 3 {
		seatID, err := uuid.Parse(fmt.Sprint(entries[i]))
		if err != nil {
			// A malformed member should not fail the whole seat map.
			continue
		}

		expiresMillis, err := strconv.ParseInt(fmt.Sprint(entries[i+1]), 10, 64)
		if err != nil {
			continue
		}

		userID, _, ok := parseLockValue(fmt.Sprint(entries[i+2]))
		if !ok {
			continue
		}

		holds[seatID] = models.SeatHold{
			UserID:    userID,
			ExpiresAt: time.UnixMilli(expiresMillis),
		}
	}
	return holds, nil
}

// HoldsFor reports which of the given seats the caller still holds. The
// confirm path uses it to check its locks survived the payment round trip.
func (m *Manager) HoldsFor(ctx context.Context, eventID, userID uuid.UUID, lockToken string, seatIDs []uuid.UUID) (missing []uuid.UUID, err error) {
	if len(seatIDs) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(seatIDs))
	for _, seatID := range seatIDs {
		keys = append(keys, lockKey(eventID, seatID))
	}

	values, err := m.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("locks: read lock values: %w", err)
	}

	expected := LockValue(userID, lockToken)
	for i, value := range values {
		if value == nil || fmt.Sprint(value) != expected {
			missing = append(missing, seatIDs[i])
		}
	}
	return missing, nil
}

// parseSeatIDList converts the acquire script's reply into seat ids.
func parseSeatIDList(raw any) ([]uuid.UUID, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("locks: acquire script returned %T, expected an array", raw)
	}

	ids := make([]uuid.UUID, 0, len(items))
	for _, item := range items {
		id, err := uuid.Parse(fmt.Sprint(item))
		if err != nil {
			return nil, fmt.Errorf("locks: acquire script returned an invalid seat id %q: %w", item, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
