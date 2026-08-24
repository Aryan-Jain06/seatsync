package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
)

// channelPrefix namespaces the pub/sub channels this package owns.
const channelPrefix = "seatupdates:"

// channelFor names the channel carrying one event's updates.
//
// Updates are published per event rather than to a single firehose, so an
// instance is not woken by traffic for events nobody on it is watching.
func channelFor(eventID uuid.UUID) string {
	return channelPrefix + eventID.String()
}

// envelope wraps updates with the identity of the instance that produced
// them, so a publisher can recognise and skip its own message.
type envelope struct {
	Origin  string              `json:"origin"`
	EventID uuid.UUID           `json:"event_id"`
	Seats   []models.SeatUpdate `json:"seats"`
}

// PubSub extends a Hub across processes.
//
// A bare Hub keeps its subscribers in memory, so a user connected to one
// instance never learns about a hold placed through another. PubSub relays
// every update through Redis, so each instance's hub delivers it to its own
// subscribers.
//
// Local delivery happens immediately and never depends on Redis. Only the
// relay to other instances goes through it, so a Redis outage degrades a
// multi-instance deployment to the single-instance behaviour rather than
// silencing updates altogether.
type PubSub struct {
	hub *Hub
	rdb *redis.Client

	// origin identifies this process. A message tagged with it has already
	// been delivered locally by the publish call and must not be delivered
	// again when it arrives back over the subscription.
	origin string

	// sub is the live subscription, retained so Close can end it.
	mu     sync.Mutex
	sub    *redis.PubSub
	closed bool
}

// NewPubSub wraps a Hub so its updates reach every instance.
func NewPubSub(hub *Hub, rdb *redis.Client) *PubSub {
	return &PubSub{
		hub:    hub,
		rdb:    rdb,
		origin: uuid.NewString(),
	}
}

// publishTimeout bounds a single publish. Broadcasting runs on the hold,
// confirm and expiry paths, so it must not stall a booking when Redis is slow.
const publishTimeout = 2 * time.Second

// BroadcastSeatUpdates delivers updates locally and relays them to the other
// instances.
//
// It satisfies services.SeatBroadcaster, so it substitutes for a bare Hub
// wherever one is wired in.
func (p *PubSub) BroadcastSeatUpdates(eventID uuid.UUID, updates []models.SeatUpdate) {
	if len(updates) == 0 {
		return
	}

	// Local subscribers first, and unconditionally. They are served whether
	// or not Redis is reachable.
	p.hub.BroadcastSeatUpdates(eventID, updates)

	payload, err := json.Marshal(envelope{
		Origin:  p.origin,
		EventID: eventID,
		Seats:   updates,
	})
	if err != nil {
		slog.Error("could not encode seat update for relay", "error", err, "event_id", eventID)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()

	if err := p.rdb.Publish(ctx, channelFor(eventID), payload).Err(); err != nil {
		// Local subscribers already have the update; only other instances
		// miss it. Their clients recover on their next seat map fetch.
		slog.Warn("could not relay seat update to other instances",
			"error", err, "event_id", eventID)
	}
}

// Run consumes updates published by other instances until ctx is cancelled.
//
// It reconnects on failure: a dropped subscription would otherwise leave this
// instance silently stale, which is worse than a noisy retry.
func (p *PubSub) Run(ctx context.Context) error {
	const (
		minBackoff = 250 * time.Millisecond
		maxBackoff = 5 * time.Second
	)

	backoff := minBackoff

	for {
		if err := p.consume(ctx); err != nil {
			if ctx.Err() != nil || p.isClosed() {
				return nil
			}

			slog.Warn("seat update subscription dropped, reconnecting",
				"error", err, "retry_in", backoff)

			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}

			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		return nil
	}
}

// consume runs one subscription until it fails or ctx ends.
func (p *PubSub) consume(ctx context.Context) error {
	sub := p.rdb.PSubscribe(ctx, channelPrefix+"*")

	// Confirm the subscription is live before treating it as healthy, so a
	// failure to connect is retried rather than mistaken for an idle stream.
	if _, err := sub.Receive(ctx); err != nil {
		_ = sub.Close()
		return fmt.Errorf("subscribe to seat updates: %w", err)
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = sub.Close()
		return nil
	}
	p.sub = sub
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		if p.sub == sub {
			p.sub = nil
		}
		p.mu.Unlock()
		_ = sub.Close()
	}()

	slog.Info("relaying seat updates between instances", "origin", p.origin)

	channel := sub.Channel()

	for {
		select {
		case <-ctx.Done():
			return nil

		case msg, ok := <-channel:
			if !ok {
				if ctx.Err() != nil || p.isClosed() {
					return nil
				}
				return errors.New("seat update channel closed")
			}
			p.deliver(msg.Payload)
		}
	}
}

// deliver hands one relayed message to the local hub.
func (p *PubSub) deliver(payload string) {
	var env envelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		slog.Warn("discarding malformed seat update", "error", err)
		return
	}

	// Our own message, already delivered locally when it was published.
	if env.Origin == p.origin {
		return
	}

	p.hub.BroadcastSeatUpdates(env.EventID, env.Seats)
}

// Close ends the subscription. The wrapped Hub is not closed: its lifetime is
// owned by whoever constructed it.
func (p *PubSub) Close() {
	p.mu.Lock()
	p.closed = true
	sub := p.sub
	p.sub = nil
	p.mu.Unlock()

	if sub != nil {
		_ = sub.Close()
	}
}

func (p *PubSub) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}
