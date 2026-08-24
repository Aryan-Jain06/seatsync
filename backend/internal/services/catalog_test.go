package services

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
	"github.com/Aryan-Jain06/seatsync/backend/internal/repos"
)

// seatRow builds a seat at multiplier 1.00 unless told otherwise.
func seatRow(id uuid.UUID, confirmed bool) repos.SeatRow {
	return repos.SeatRow{
		Seat: models.Seat{
			ID:                id,
			Section:           "A",
			Row:               1,
			Number:            1,
			PriceMultiplierBP: 100,
		},
		Confirmed: confirmed,
	}
}

func TestSeatMapMarksAvailableHeldAndConfirmed(t *testing.T) {
	available, heldByOther, confirmed := uuid.New(), uuid.New(), uuid.New()
	otherUser := uuid.New()

	rows := []repos.SeatRow{
		seatRow(available, false),
		seatRow(heldByOther, false),
		seatRow(confirmed, true),
	}
	held := map[uuid.UUID]Hold{
		heldByOther: {UserID: otherUser, ExpiresAt: time.Now().Add(time.Minute)},
	}

	got := buildSeatMap(uuid.New(), 1000, rows, held, uuid.Nil)

	require.Equal(t, 1, got.Available)
	require.Equal(t, 1, got.Held)
	require.Equal(t, 1, got.Confirmed)
	require.Len(t, got.Seats, 3)

	byID := map[uuid.UUID]models.SeatMapEntry{}
	for _, s := range got.Seats {
		byID[s.SeatID] = s
	}
	require.Equal(t, models.SeatAvailable, byID[available].Status)
	require.Equal(t, models.SeatHeld, byID[heldByOther].Status)
	require.Equal(t, models.SeatConfirmed, byID[confirmed].Status)
}

// A sale is permanent and a lock is not, so a confirmed seat must never be
// downgraded to "held" by a lock that outlived the purchase.
func TestConfirmedBeatsAStaleHold(t *testing.T) {
	seatID := uuid.New()
	holder := uuid.New()

	rows := []repos.SeatRow{seatRow(seatID, true)}
	held := map[uuid.UUID]Hold{
		seatID: {UserID: holder, ExpiresAt: time.Now().Add(time.Minute)},
	}

	got := buildSeatMap(uuid.New(), 1000, rows, held, holder)

	require.Equal(t, models.SeatConfirmed, got.Seats[0].Status)
	require.False(t, got.Seats[0].HeldByMe, "a confirmed seat is not reported as held by anyone")
	require.Equal(t, 1, got.Confirmed)
	require.Zero(t, got.Held)
}

func TestHeldByMeOnlyForTheHolder(t *testing.T) {
	mine, theirs := uuid.New(), uuid.New()
	me, them := uuid.New(), uuid.New()

	rows := []repos.SeatRow{seatRow(mine, false), seatRow(theirs, false)}
	held := map[uuid.UUID]Hold{
		mine:   {UserID: me, ExpiresAt: time.Now().Add(time.Minute)},
		theirs: {UserID: them, ExpiresAt: time.Now().Add(time.Minute)},
	}

	got := buildSeatMap(uuid.New(), 1000, rows, held, me)

	byID := map[uuid.UUID]models.SeatMapEntry{}
	for _, s := range got.Seats {
		byID[s.SeatID] = s
	}
	require.True(t, byID[mine].HeldByMe)
	require.False(t, byID[theirs].HeldByMe)
	require.Equal(t, 2, got.Held)
}

// An anonymous caller must never be told a seat is theirs, even though the
// zero UUID is a valid map key.
func TestAnonymousViewerOwnsNothing(t *testing.T) {
	seatID := uuid.New()

	rows := []repos.SeatRow{seatRow(seatID, false)}
	held := map[uuid.UUID]Hold{
		seatID: {UserID: uuid.Nil, ExpiresAt: time.Now().Add(time.Minute)},
	}

	got := buildSeatMap(uuid.New(), 1000, rows, held, uuid.Nil)

	require.Equal(t, models.SeatHeld, got.Seats[0].Status)
	require.False(t, got.Seats[0].HeldByMe)
}

func TestSeatPricingUsesMultiplier(t *testing.T) {
	tests := []struct {
		name          string
		basePrice     int64
		multiplierBP  int64
		expectedPrice int64
	}{
		{"plain multiplier", 250000, 100, 250000},
		{"premium section", 250000, 150, 375000},
		{"front row premium", 250000, 160, 400000},
		{"discounted rear", 250000, 80, 200000},
		{"rounds half up", 999, 150, 1499}, // 1498.5 -> 1499
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			seat := models.Seat{PriceMultiplierBP: tc.multiplierBP}
			require.Equal(t, tc.expectedPrice, seat.Price(tc.basePrice))
		})
	}
}

func TestEmptySeatMapIsAnEmptySliceNotNull(t *testing.T) {
	got := buildSeatMap(uuid.New(), 1000, nil, nil, uuid.Nil)

	require.NotNil(t, got.Seats, "must serialise as [] rather than null")
	require.Empty(t, got.Seats)
}
