package services

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
	"github.com/Aryan-Jain06/seatsync/backend/internal/repos"
)

// expiryBatchSize bounds one sweep. Without a cap, a backlog after an outage
// would be claimed in a single long transaction that blocks ordinary traffic.
const expiryBatchSize = 200

// ExpiryWorker reaps pending bookings whose holds have lapsed.
//
// Redis expires the seat locks by itself, so the seats are already free the
// moment the TTL passes. What this worker does is reconcile Postgres to that
// fact, moving the abandoned bookings to `expired` and telling connected
// clients the seats are available again. Without it those bookings would sit
// as `pending` for ever and the seat map would only correct itself when a
// client happened to reload.
type ExpiryWorker struct {
	bookings    *repos.BookingRepo
	broadcaster SeatBroadcaster
	interval    time.Duration
}

// NewExpiryWorker builds an ExpiryWorker.
func NewExpiryWorker(bookings *repos.BookingRepo, broadcaster SeatBroadcaster, interval time.Duration) *ExpiryWorker {
	if broadcaster == nil {
		broadcaster = NoopBroadcaster{}
	}
	return &ExpiryWorker{bookings: bookings, broadcaster: broadcaster, interval: interval}
}

// Run sweeps until the context is cancelled. It is meant to be started in its
// own goroutine and returns once that context is done.
func (w *ExpiryWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	slog.Info("expiry worker started", "interval", w.interval.String())

	for {
		select {
		case <-ctx.Done():
			slog.Info("expiry worker stopped")
			return
		case <-ticker.C:
			w.sweepOnce(ctx)
		}
	}
}

// sweepOnce claims one batch of lapsed bookings.
//
// Errors are logged and swallowed: a failed sweep must not kill the worker,
// because the next tick will simply try again and the rows are still there.
func (w *ExpiryWorker) sweepOnce(ctx context.Context) {
	// Bound each sweep so a slow database cannot stack up overlapping runs.
	sweepCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	expired, err := w.bookings.ClaimExpired(sweepCtx, expiryBatchSize)
	if err != nil {
		// A cancelled context during shutdown is expected, not a fault.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		slog.ErrorContext(ctx, "expiry sweep failed", "error", err)
		return
	}
	if len(expired) == 0 {
		return
	}

	seatCount := 0
	for _, booking := range expired {
		seatCount += len(booking.SeatIDs)

		updates := make([]models.SeatUpdate, 0, len(booking.SeatIDs))
		for _, seatID := range booking.SeatIDs {
			updates = append(updates, models.SeatUpdate{SeatID: seatID, Status: models.SeatAvailable})
		}
		w.broadcaster.BroadcastSeatUpdates(booking.EventID, updates)
	}

	slog.Info("expired lapsed holds", "bookings", len(expired), "seats", seatCount)
}
