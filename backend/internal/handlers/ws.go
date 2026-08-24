package handlers

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/gorilla/websocket"

	"github.com/Aryan-Jain06/seatsync/backend/internal/httpx"
	"github.com/Aryan-Jain06/seatsync/backend/internal/realtime"
)

// WSHandler upgrades subscribers onto the realtime hub.
type WSHandler struct {
	hub      *realtime.Hub
	upgrader *websocket.Upgrader
}

// NewWSHandler builds a WSHandler.
//
// allowedOrigins is checked on upgrade. Browsers do not apply the same-origin
// policy to WebSocket handshakes, so without this check any page on the
// internet could open a socket against this server on a visitor's behalf.
func NewWSHandler(hub *realtime.Hub, allowedOrigins []string) *WSHandler {
	return &WSHandler{
		hub: hub,
		upgrader: &websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     originChecker(allowedOrigins),
		},
	}
}

// originChecker builds the upgrade guard.
func originChecker(allowed []string) func(*http.Request) bool {
	return func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		// Non-browser clients such as wscat or a load test send no Origin.
		// There is no browser session for them to ride on, so there is
		// nothing for the check to protect against.
		if origin == "" {
			return true
		}
		return slices.ContainsFunc(allowed, func(candidate string) bool {
			return strings.EqualFold(candidate, origin)
		})
	}
}

// Subscribe handles GET /ws/events/{id}.
func (h *WSHandler) Subscribe(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuidParam(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	if err := h.hub.Serve(w, r, h.upgrader, eventID); err != nil {
		// The upgrader has already replied, so this is only worth recording.
		slog.WarnContext(r.Context(), "websocket upgrade failed",
			"error", err, "event_id", eventID, "origin", r.Header.Get("Origin"))
	}
}
