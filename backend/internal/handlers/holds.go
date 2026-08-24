package handlers

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/Aryan-Jain06/seatsync/backend/internal/httpx"
	"github.com/Aryan-Jain06/seatsync/backend/internal/middleware"
	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
	"github.com/Aryan-Jain06/seatsync/backend/internal/services"
)

// HoldHandler serves the seat hold and booking endpoints.
type HoldHandler struct {
	holds *services.HoldService
}

// NewHoldHandler builds a HoldHandler.
func NewHoldHandler(holds *services.HoldService) *HoldHandler {
	return &HoldHandler{holds: holds}
}

type createHoldRequest struct {
	SeatIDs []uuid.UUID `json:"seat_ids"`
}

// Create handles POST /events/{id}/holds.
func (h *HoldHandler) Create(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	eventID, err := uuidParam(r, "id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	var req createHoldRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}

	result, err := h.holds.CreateHold(r.Context(), userID, eventID, req.SeatIDs)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, result)
}

// Release handles DELETE /holds/{booking_id}.
func (h *HoldHandler) Release(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	bookingID, err := uuidParam(r, "booking_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	if err := h.holds.ReleaseHold(r.Context(), userID, bookingID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}

type bookingListResponse struct {
	Bookings []models.BookingDetail `json:"bookings"`
}

// List handles GET /me/bookings.
func (h *HoldHandler) List(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	bookings, err := h.holds.ListBookings(r.Context(), userID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, bookingListResponse{Bookings: bookings})
}

// Get handles GET /bookings/{booking_id}.
func (h *HoldHandler) Get(w http.ResponseWriter, r *http.Request) {
	userID, err := middleware.MustUserID(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	bookingID, err := uuidParam(r, "booking_id")
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	booking, err := h.holds.GetBooking(r.Context(), userID, bookingID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, booking)
}
