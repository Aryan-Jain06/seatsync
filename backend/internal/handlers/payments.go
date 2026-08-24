package handlers

import (
	"net/http"

	"github.com/Aryan-Jain06/seatsync/backend/internal/httpx"
	"github.com/Aryan-Jain06/seatsync/backend/internal/middleware"
	"github.com/Aryan-Jain06/seatsync/backend/internal/services"
)

// PaymentHandler serves the checkout endpoint.
type PaymentHandler struct {
	payments *services.PaymentService
}

// NewPaymentHandler builds a PaymentHandler.
func NewPaymentHandler(payments *services.PaymentService) *PaymentHandler {
	return &PaymentHandler{payments: payments}
}

// Pay handles POST /bookings/{booking_id}/pay.
//
// The Idempotency-Key header is mandatory. A client generates it once per
// checkout and reuses it across retries, which is what lets a retry after a
// timeout or a decline be distinguished from a genuine second purchase.
func (h *PaymentHandler) Pay(w http.ResponseWriter, r *http.Request) {
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

	result, err := h.payments.Pay(r.Context(), userID, bookingID, r.Header.Get("Idempotency-Key"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, result)
}
