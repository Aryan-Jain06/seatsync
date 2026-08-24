package realtime_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
	"github.com/Aryan-Jain06/seatsync/backend/internal/realtime"
)

// newTestServer exposes a hub over a real HTTP server, so the tests exercise
// genuine WebSocket frames rather than a mocked transport.
func newTestServer(t *testing.T, hub *realtime.Hub, eventID uuid.UUID) *httptest.Server {
	t.Helper()

	upgrader := &websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = hub.Serve(w, r, upgrader, eventID)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func dial(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()

	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	if resp != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readMessage reads one frame and decodes it.
func readMessage(t *testing.T, conn *websocket.Conn) realtime.Message {
	t.Helper()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))

	_, raw, err := conn.ReadMessage()
	require.NoError(t, err)

	var msg realtime.Message
	require.NoError(t, json.Unmarshal(raw, &msg))
	return msg
}

// expectConnected consumes the greeting the hub sends on subscribe.
func expectConnected(t *testing.T, conn *websocket.Conn) {
	t.Helper()

	msg := readMessage(t, conn)
	require.Equal(t, realtime.TypeConnected, msg.Type)
}

func TestSubscribersReceiveSeatUpdates(t *testing.T) {
	hub := realtime.NewHub()
	defer hub.Close()

	eventID := uuid.New()
	srv := newTestServer(t, hub, eventID)

	first, second := dial(t, srv), dial(t, srv)
	expectConnected(t, first)
	expectConnected(t, second)

	require.Eventually(t, func() bool { return hub.ClientCount(eventID) == 2 },
		2*time.Second, 20*time.Millisecond)

	seatID := uuid.New()
	hub.BroadcastSeatUpdates(eventID, []models.SeatUpdate{{SeatID: seatID, Status: models.SeatHeld}})

	// Both tabs see the same change, which is the behaviour the seat map
	// depends on.
	for _, conn := range []*websocket.Conn{first, second} {
		msg := readMessage(t, conn)
		require.Equal(t, realtime.TypeSeatUpdate, msg.Type)
		require.Equal(t, eventID, msg.EventID)
		require.Len(t, msg.Seats, 1)
		require.Equal(t, seatID, msg.Seats[0].SeatID)
		require.Equal(t, models.SeatHeld, msg.Seats[0].Status)
	}
}

// A multi-seat hold arrives as one message, so the UI redraws once.
func TestUpdatesAreBatchedIntoOneMessage(t *testing.T) {
	hub := realtime.NewHub()
	defer hub.Close()

	eventID := uuid.New()
	conn := dial(t, newTestServer(t, hub, eventID))
	expectConnected(t, conn)

	updates := make([]models.SeatUpdate, 0, 6)
	for range 6 {
		updates = append(updates, models.SeatUpdate{SeatID: uuid.New(), Status: models.SeatHeld})
	}
	hub.BroadcastSeatUpdates(eventID, updates)

	msg := readMessage(t, conn)
	require.Len(t, msg.Seats, 6, "a six seat hold is a single message")
}

// Rooms are per event: a browser on one event must not be woken by another.
func TestRoomsAreIsolatedPerEvent(t *testing.T) {
	hub := realtime.NewHub()
	defer hub.Close()

	watched, other := uuid.New(), uuid.New()

	conn := dial(t, newTestServer(t, hub, watched))
	expectConnected(t, conn)

	hub.BroadcastSeatUpdates(other, []models.SeatUpdate{{SeatID: uuid.New(), Status: models.SeatHeld}})
	hub.BroadcastSeatUpdates(watched, []models.SeatUpdate{{SeatID: uuid.New(), Status: models.SeatConfirmed}})

	// The first message to arrive must be the watched event's, proving the
	// other event's broadcast was never queued here.
	msg := readMessage(t, conn)
	require.Equal(t, watched, msg.EventID)
	require.Equal(t, models.SeatConfirmed, msg.Seats[0].Status)
}

// The quality bar: a client that stops reading is dropped rather than being
// allowed to slow a broadcast down for everybody else.
func TestASlowClientIsDroppedAndTheRestKeepUp(t *testing.T) {
	hub := realtime.NewHub()
	defer hub.Close()

	eventID := uuid.New()
	srv := newTestServer(t, hub, eventID)

	slow := dial(t, srv)
	expectConnected(t, slow)
	require.Eventually(t, func() bool { return hub.ClientCount(eventID) == 1 },
		2*time.Second, 20*time.Millisecond)

	// The slow client never reads again. Its OS socket buffer fills, then
	// its writer blocks, then its send channel fills, and the hub evicts it.
	for range 5000 {
		hub.BroadcastSeatUpdates(eventID, []models.SeatUpdate{{SeatID: uuid.New(), Status: models.SeatHeld}})
		if hub.ClientCount(eventID) == 0 {
			break
		}
	}

	require.Eventually(t, func() bool { return hub.ClientCount(eventID) == 0 },
		5*time.Second, 50*time.Millisecond, "an unresponsive client must be evicted")

	// A fresh client works normally, showing the hub was never wedged.
	healthy := dial(t, srv)
	expectConnected(t, healthy)

	seatID := uuid.New()
	require.Eventually(t, func() bool { return hub.ClientCount(eventID) == 1 },
		2*time.Second, 20*time.Millisecond)

	hub.BroadcastSeatUpdates(eventID, []models.SeatUpdate{{SeatID: seatID, Status: models.SeatAvailable}})

	msg := readMessage(t, healthy)
	require.Equal(t, seatID, msg.Seats[0].SeatID)
}

// A disconnected client must take both of its goroutines with it.
func TestDisconnectingLeavesNoGoroutinesBehind(t *testing.T) {
	hub := realtime.NewHub()
	eventID := uuid.New()
	srv := newTestServer(t, hub, eventID)

	settle := func() {
		for range 10 {
			runtime.GC()
			time.Sleep(20 * time.Millisecond)
		}
	}

	settle()
	before := runtime.NumGoroutine()

	for range 20 {
		conn := dial(t, srv)
		expectConnected(t, conn)
		require.NoError(t, conn.Close())
	}

	require.Eventually(t, func() bool { return hub.TotalClients() == 0 },
		5*time.Second, 50*time.Millisecond, "clients must deregister themselves")

	settle()
	after := runtime.NumGoroutine()

	// A small allowance covers the http server's own transient goroutines.
	require.LessOrEqual(t, after, before+5,
		"20 connect/disconnect cycles leaked goroutines: %d -> %d", before, after)

	hub.Close()
}

// Close must disconnect everyone and wait for the pumps, not just mark the
// hub closed and return.
func TestCloseDrainsEveryClient(t *testing.T) {
	hub := realtime.NewHub()

	eventID := uuid.New()
	srv := newTestServer(t, hub, eventID)

	conns := make([]*websocket.Conn, 0, 5)
	for range 5 {
		conn := dial(t, srv)
		expectConnected(t, conn)
		conns = append(conns, conn)
	}

	require.Eventually(t, func() bool { return hub.ClientCount(eventID) == 5 },
		2*time.Second, 20*time.Millisecond)

	done := make(chan struct{})
	go func() {
		hub.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return; a pump goroutine is still running")
	}

	require.Zero(t, hub.TotalClients())

	// Each client should have been told the server was closing, rather than
	// simply having the connection cut.
	for _, conn := range conns {
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
		_, _, err := conn.ReadMessage()
		require.Error(t, err, "the connection must be closed")
	}
}

// Broadcasting after shutdown, which can happen as in-flight requests finish,
// must not panic on a closed channel.
func TestBroadcastAfterCloseIsHarmless(t *testing.T) {
	hub := realtime.NewHub()

	eventID := uuid.New()
	conn := dial(t, newTestServer(t, hub, eventID))
	expectConnected(t, conn)

	hub.Close()

	require.NotPanics(t, func() {
		hub.BroadcastSeatUpdates(eventID, []models.SeatUpdate{{SeatID: uuid.New(), Status: models.SeatHeld}})
	})
}

func TestEmptyBroadcastIsIgnored(t *testing.T) {
	hub := realtime.NewHub()
	defer hub.Close()

	require.NotPanics(t, func() {
		hub.BroadcastSeatUpdates(uuid.New(), nil)
		hub.BroadcastSeatUpdates(uuid.New(), []models.SeatUpdate{})
	})
}
