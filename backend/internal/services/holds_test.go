package services

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Aryan-Jain06/seatsync/backend/internal/httpx"
)

func newHoldService(maxSeats int) *HoldService {
	return &HoldService{maxSeats: maxSeats, broadcaster: NoopBroadcaster{}}
}

func TestValidateSeatSelectionAcceptsAValidRequest(t *testing.T) {
	s := newHoldService(6)
	seats := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}

	got, err := s.validateSeatSelection(seats)
	require.NoError(t, err)
	require.Equal(t, seats, got)
}

// Duplicates are collapsed rather than rejected: asking for the same seat
// twice is a client slip, not an attempt to take two seats.
func TestValidateSeatSelectionRemovesDuplicates(t *testing.T) {
	s := newHoldService(6)
	first, second := uuid.New(), uuid.New()

	got, err := s.validateSeatSelection([]uuid.UUID{first, second, first, second, first})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{first, second}, got, "order of first appearance is preserved")
}

// The cap applies after de-duplication, so seven copies of one seat is a
// one-seat request rather than a limit breach.
func TestValidateSeatSelectionCountsDistinctSeats(t *testing.T) {
	s := newHoldService(6)
	seat := uuid.New()

	got, err := s.validateSeatSelection([]uuid.UUID{seat, seat, seat, seat, seat, seat, seat})
	require.NoError(t, err)
	require.Len(t, got, 1)
}

func TestValidateSeatSelectionRejectsTooManySeats(t *testing.T) {
	s := newHoldService(6)

	seats := make([]uuid.UUID, 0, 7)
	for range 7 {
		seats = append(seats, uuid.New())
	}

	_, err := s.validateSeatSelection(seats)

	var apiErr *httpx.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, httpx.CodeValidation, apiErr.Code)
	require.Contains(t, apiErr.Message, "at most 6")
}

func TestValidateSeatSelectionRejectsAnEmptyRequest(t *testing.T) {
	s := newHoldService(6)

	for _, seats := range [][]uuid.UUID{nil, {}} {
		_, err := s.validateSeatSelection(seats)

		var apiErr *httpx.APIError
		require.ErrorAs(t, err, &apiErr)
		require.Equal(t, httpx.CodeValidation, apiErr.Code)
	}
}

// The zero UUID is a valid value in Go but never a real seat, so it must be
// refused rather than sent on to the lock store.
func TestValidateSeatSelectionRejectsTheNilUUID(t *testing.T) {
	s := newHoldService(6)

	_, err := s.validateSeatSelection([]uuid.UUID{uuid.New(), uuid.Nil})

	var apiErr *httpx.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, httpx.CodeValidation, apiErr.Code)
}

func TestSeatsUnavailableErrorCarriesTheOffendingSeats(t *testing.T) {
	first, second := uuid.New(), uuid.New()

	err := seatsUnavailableError([]uuid.UUID{first, second}, "taken")

	require.Equal(t, httpx.CodeSeatsTaken, err.Code)
	require.Equal(t, 409, err.Status)
	require.Equal(t,
		[]string{first.String(), second.String()},
		err.Details["unavailable_seat_ids"],
		"the client needs the ids to highlight them on the seat map")
}

func TestIntersectPreservesRequestOrder(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	set := map[uuid.UUID]struct{}{c: {}, a: {}}

	require.Equal(t, []uuid.UUID{a, c}, intersect([]uuid.UUID{a, b, c}, set))
	require.Nil(t, intersect([]uuid.UUID{b}, set))
}
