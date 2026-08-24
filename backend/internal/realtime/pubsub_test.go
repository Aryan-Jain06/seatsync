package realtime_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
	"github.com/Aryan-Jain06/seatsync/backend/internal/realtime"
)

func newRedis(t *testing.T) *redis.Client {
	t.Helper()

	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	rdb := redis.NewClient(&redis.Options{Addr: addr})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		t.Skipf("redis not reachable at %s: %v", addr, err)
	}

	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// instance stands in for one running server process: a hub plus the relay
// that carries updates to and from its peers.
type instance struct {
	hub   *realtime.Hub
	relay *realtime.PubSub
}

func newInstance(t *testing.T, ctx context.Context) *instance {
	t.Helper()

	hub := realtime.NewHub()
	relay := realtime.NewPubSub(hub, newRedis(t))

	go func() { _ = relay.Run(ctx) }()

	// Let the subscription establish before a test publishes anything.
	time.Sleep(300 * time.Millisecond)

	t.Cleanup(func() {
		relay.Close()
		hub.Close()
	})

	return &instance{hub: hub, relay: relay}
}

// The point of the whole mechanism: a hold placed on one instance must reach
// a subscriber connected to a different one.
func TestUpdatesCrossBetweenInstances(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	instanceA := newInstance(t, ctx)
	instanceB := newInstance(t, ctx)

	eventID := uuid.New()
	seatID := uuid.New()

	// The subscriber is attached to B.
	conn := dial(t, newTestServer(t, instanceB.hub, eventID))
	expectConnected(t, conn)

	// The update is published through A.
	instanceA.relay.BroadcastSeatUpdates(eventID, []models.SeatUpdate{
		{SeatID: seatID, Status: models.SeatHeld},
	})

	msg := readMessage(t, conn)
	require.Equal(t, realtime.TypeSeatUpdate, msg.Type)
	require.Len(t, msg.Seats, 1)
	require.Equal(t, seatID, msg.Seats[0].SeatID)
	require.Equal(t, models.SeatHeld, msg.Seats[0].Status)
}

// A publisher's own subscribers get the update exactly once: from the local
// delivery, not again when the message arrives back over the subscription.
func TestPublisherDeliversLocallyExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	inst := newInstance(t, ctx)

	eventID := uuid.New()
	seatID := uuid.New()

	conn := dial(t, newTestServer(t, inst.hub, eventID))
	expectConnected(t, conn)

	inst.relay.BroadcastSeatUpdates(eventID, []models.SeatUpdate{
		{SeatID: seatID, Status: models.SeatConfirmed},
	})

	first := readMessage(t, conn)
	require.Len(t, first.Seats, 1)
	require.Equal(t, seatID, first.Seats[0].SeatID)

	// A second frame would be the echo of our own publish.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(1500*time.Millisecond)))
	_, _, err := conn.ReadMessage()
	require.Error(t, err, "the publisher's own message must not be delivered twice")
}

// Local subscribers are served even when Redis is unreachable, so an outage
// degrades a multi-instance deployment to single-instance behaviour rather
// than silencing updates entirely.
func TestLocalDeliverySurvivesRedisBeingUnreachable(t *testing.T) {
	hub := realtime.NewHub()
	t.Cleanup(hub.Close)

	// Pointed at a port with nothing behind it.
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = dead.Close() })

	relay := realtime.NewPubSub(hub, dead)
	t.Cleanup(relay.Close)

	eventID := uuid.New()
	seatID := uuid.New()

	conn := dial(t, newTestServer(t, hub, eventID))
	expectConnected(t, conn)

	relay.BroadcastSeatUpdates(eventID, []models.SeatUpdate{
		{SeatID: seatID, Status: models.SeatAvailable},
	})

	msg := readMessage(t, conn)
	require.Len(t, msg.Seats, 1)
	require.Equal(t, seatID, msg.Seats[0].SeatID)
}

// Updates for one event must not leak to subscribers watching another, even
// though every instance subscribes to the same channel pattern.
func TestRelayKeepsEventsIsolated(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	publisher := newInstance(t, ctx)
	subscriber := newInstance(t, ctx)

	watched, other := uuid.New(), uuid.New()

	conn := dial(t, newTestServer(t, subscriber.hub, watched))
	expectConnected(t, conn)

	publisher.relay.BroadcastSeatUpdates(other, []models.SeatUpdate{
		{SeatID: uuid.New(), Status: models.SeatHeld},
	})

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(1500*time.Millisecond)))
	_, _, err := conn.ReadMessage()
	require.Error(t, err, "an update for another event must not be delivered")
}

// Close must be safe to call more than once, since shutdown defers it and
// also calls it explicitly.
func TestRelayCloseIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	inst := newInstance(t, ctx)

	require.NotPanics(t, func() {
		inst.relay.Close()
		inst.relay.Close()
	})
}
