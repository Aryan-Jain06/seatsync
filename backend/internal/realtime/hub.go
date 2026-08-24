// Package realtime broadcasts seat state changes to connected clients over
// WebSocket.
//
// Clients are grouped into one room per event, since a browser watching an
// event only cares about that event's seats. Every client owns a buffered
// send channel and two goroutines, a reader and a writer; the hub itself
// never writes to a connection, so one unresponsive client cannot stall a
// broadcast to everybody else.
package realtime

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
)

const (
	// sendBuffer is how far a client may fall behind before it is dropped.
	// A hold covers at most six seats, so this absorbs a healthy burst of
	// activity without letting a stalled reader consume memory without bound.
	sendBuffer = 64

	// writeWait bounds a single write to a client.
	writeWait = 10 * time.Second

	// pongWait is how long a client may go silent before it is considered
	// gone. Browsers answer pings automatically, so silence past this means
	// the connection is dead even if TCP has not noticed yet.
	pongWait = 60 * time.Second

	// pingPeriod must be shorter than pongWait, leaving room for the reply.
	pingPeriod = (pongWait * 9) / 10

	// maxMessageSize caps what a client may send. Clients are not expected
	// to send anything at all, so this only needs to survive a stray frame.
	maxMessageSize = 512
)

// Message is what the server pushes to subscribers.
//
// Updates are batched: a six seat hold arrives as one message, so the UI
// applies it as a single change rather than redrawing six times.
type Message struct {
	Type    string              `json:"type"`
	EventID uuid.UUID           `json:"event_id"`
	Seats   []models.SeatUpdate `json:"seats,omitempty"`
}

// Message types.
const (
	// TypeConnected acknowledges a subscription. A client fetches the seat
	// map after receiving it, so the snapshot cannot predate the stream.
	TypeConnected = "connected"
	// TypeSeatUpdate carries one or more seat state changes.
	TypeSeatUpdate = "seat_update"
)

// Hub owns every room and the clients within them.
type Hub struct {
	mu     sync.RWMutex
	rooms  map[uuid.UUID]map[*Client]struct{}
	closed bool

	// wg tracks every client pump, so Close can wait for them to finish
	// rather than returning while goroutines are still running.
	wg sync.WaitGroup
}

// NewHub builds an empty Hub.
func NewHub() *Hub {
	return &Hub{rooms: make(map[uuid.UUID]map[*Client]struct{})}
}

// Client is one subscriber.
type Client struct {
	hub     *Hub
	eventID uuid.UUID
	conn    *websocket.Conn

	// send is buffered so a broadcast never blocks on a slow reader.
	send chan []byte

	// closeOnce guards the send channel: it is closed exactly once, by
	// whichever path removes the client first.
	closeOnce sync.Once
}

// BroadcastSeatUpdates pushes seat changes to everyone watching an event.
//
// It never blocks. A client whose buffer is full is dropped rather than
// allowed to slow the caller, which matters because this runs on the hold,
// confirm and expiry paths: a stalled browser must not delay a booking.
func (h *Hub) BroadcastSeatUpdates(eventID uuid.UUID, updates []models.SeatUpdate) {
	if len(updates) == 0 {
		return
	}

	payload, err := json.Marshal(Message{
		Type:    TypeSeatUpdate,
		EventID: eventID,
		Seats:   updates,
	})
	if err != nil {
		slog.Error("could not encode seat update", "error", err, "event_id", eventID)
		return
	}

	// Collect the laggards under a read lock, then drop them outside it:
	// removal needs the write lock, and taking it here would deadlock.
	var slow []*Client

	h.mu.RLock()
	for client := range h.rooms[eventID] {
		select {
		case client.send <- payload:
		default:
			slow = append(slow, client)
		}
	}
	h.mu.RUnlock()

	for _, client := range slow {
		slog.Warn("dropping a client that fell too far behind",
			"event_id", eventID, "buffer", sendBuffer)
		h.remove(client)
	}
}

// add registers a client in its room.
func (h *Hub) add(c *Client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return false
	}
	if h.rooms[c.eventID] == nil {
		h.rooms[c.eventID] = make(map[*Client]struct{})
	}
	h.rooms[c.eventID][c] = struct{}{}
	return true
}

// remove unregisters a client and closes its send channel, which is what
// stops its writer goroutine.
func (h *Hub) remove(c *Client) {
	h.mu.Lock()
	if room, ok := h.rooms[c.eventID]; ok {
		delete(room, c)
		// Drop empty rooms so a long-lived process does not accumulate one
		// map per event it has ever served.
		if len(room) == 0 {
			delete(h.rooms, c.eventID)
		}
	}
	h.mu.Unlock()

	c.closeOnce.Do(func() { close(c.send) })
}

// Close shuts every connection down and waits for the goroutines to finish.
//
// Waiting is the point: without it the process could exit while writers were
// mid-frame, and a test could not tell a clean shutdown from a leak.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true

	clients := make([]*Client, 0)
	for _, room := range h.rooms {
		for client := range room {
			clients = append(clients, client)
		}
	}
	h.rooms = make(map[uuid.UUID]map[*Client]struct{})
	h.mu.Unlock()

	for _, c := range clients {
		c.closeOnce.Do(func() { close(c.send) })
	}

	h.wg.Wait()
	slog.Info("realtime hub drained", "clients", len(clients))
}

// ClientCount reports how many subscribers an event has. Used by tests and
// the health endpoint.
func (h *Hub) ClientCount(eventID uuid.UUID) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.rooms[eventID])
}

// TotalClients reports every connected subscriber.
func (h *Hub) TotalClients() int {
	h.mu.RLock()
	defer h.mu.RUnlock()

	total := 0
	for _, room := range h.rooms {
		total += len(room)
	}
	return total
}

// Serve upgrades an HTTP request and runs the client until it disconnects or
// the hub shuts down.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, upgrader *websocket.Upgrader, eventID uuid.UUID) error {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written its own error response.
		return err
	}

	client := &Client{
		hub:     h,
		eventID: eventID,
		conn:    conn,
		send:    make(chan []byte, sendBuffer),
	}

	if !h.add(client) {
		// The server is shutting down; decline rather than leaving a
		// connection nobody will ever service.
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutting down"),
			time.Now().Add(writeWait))
		_ = conn.Close()
		return nil
	}

	// Greet the client so it knows when to fetch its seat map snapshot.
	greeting, err := json.Marshal(Message{Type: TypeConnected, EventID: eventID})
	if err == nil {
		select {
		case client.send <- greeting:
		default:
		}
	}

	h.wg.Add(2)
	go client.writePump()
	go client.readPump()

	return nil
}

// writePump owns the connection's write side. Having exactly one writer is
// required: concurrent writes to a websocket.Conn are not safe.
func (c *Client) writePump() {
	defer c.hub.wg.Done()

	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()

	for {
		select {
		case payload, ok := <-c.send:
			if err := c.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				return
			}
			if !ok {
				// The hub closed the channel: say goodbye politely so the
				// client can distinguish a shutdown from a network fault.
				_ = c.conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}

		case <-ticker.C:
			if err := c.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				return
			}
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// readPump exists to notice the client going away and to keep the read
// deadline moving. Clients are not expected to send anything meaningful.
func (c *Client) readPump() {
	defer func() {
		c.hub.wg.Done()
		// Removing here is what closes the send channel and therefore stops
		// the writer, so a dropped connection tears down both goroutines.
		c.hub.remove(c)
		_ = c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	if err := c.conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		return
	}
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				slog.Debug("websocket closed unexpectedly", "error", err, "event_id", c.eventID)
			}
			return
		}
		// Anything a client sends is ignored; the read is only a liveness
		// signal and a way to observe the close handshake.
	}
}
