package handlers

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Aryan-Jain06/seatsync/backend/internal/httpx"
	"github.com/Aryan-Jain06/seatsync/backend/internal/middleware"
	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
	"github.com/Aryan-Jain06/seatsync/backend/internal/services"
)

// EventHandler serves the catalogue and seat map endpoints.
type EventHandler struct {
	catalog *services.CatalogService
}

// NewEventHandler builds an EventHandler.
func NewEventHandler(catalog *services.CatalogService) *EventHandler {
	return &EventHandler{catalog: catalog}
}

// eventListResponse wraps the collection in an object rather than returning a
// bare array, leaving room to add pagination without a breaking change.
type eventListResponse struct {
	Events []models.EventWithVenue `json:"events"`
}

// List handles GET /events.
func (h *EventHandler) List(w http.ResponseWriter, r *http.Request) {
	events, err := h.catalog.ListEvents(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, eventListResponse{Events: events})
}

// Get handles GET /events/{id}.
func (h *EventHandler) Get(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuidParam(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	event, err := h.catalog.GetEvent(r.Context(), eventID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, event)
}

// SeatMap handles GET /events/{id}/seatmap.
//
// The endpoint is public, but a signed-in caller additionally learns which of
// the held seats are their own, so the UI can colour them differently.
func (h *EventHandler) SeatMap(w http.ResponseWriter, r *http.Request) {
	eventID, err := uuidParam(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Absent for anonymous callers, which SeatMap handles as uuid.Nil.
	viewer, _ := middleware.UserIDFrom(r.Context())

	seatMap, err := h.catalog.SeatMap(r.Context(), eventID, viewer)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// Seat state changes constantly; a cached seat map would show seats that
	// are already gone.
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, seatMap)
}

// uuidParam reads and validates a UUID from the URL path.
func uuidParam(r *http.Request, name string) (uuid.UUID, error) {
	raw := chi.URLParam(r, name)
	if raw == "" {
		return uuid.Nil, httpx.BadRequest("Missing " + name + " in path.")
	}

	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, httpx.BadRequest("The " + name + " in the path is not a valid UUID.")
	}
	return id, nil
}
