// Package server assembles the HTTP router and owns the server lifecycle.
package server

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"

	"github.com/Aryan-Jain06/seatsync/backend/internal/config"
	"github.com/Aryan-Jain06/seatsync/backend/internal/handlers"
	"github.com/Aryan-Jain06/seatsync/backend/internal/httpx"
	"github.com/Aryan-Jain06/seatsync/backend/internal/middleware"
)

// Deps carries everything the router needs to mount its routes.
type Deps struct {
	Config *config.Config
	Auth   *handlers.AuthHandler
	Events *handlers.EventHandler
	Holds  *handlers.HoldHandler
	Pay    *handlers.PaymentHandler
	Health *handlers.HealthHandler
	Tokens middleware.Authenticator
}

// NewRouter builds the fully wired HTTP handler.
func NewRouter(deps Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: deps.Config.CORSAllowedOrigins,
		AllowedMethods: []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions},
		AllowedHeaders: []string{"Accept", "Authorization", "Content-Type", "Idempotency-Key", "X-Request-ID"},
		ExposedHeaders: []string{"X-Request-ID"},
		MaxAge:         300,
	}))

	requireAuth := middleware.RequireAuth(deps.Tokens)
	optionalAuth := middleware.OptionalAuth(deps.Tokens)

	r.Get("/health", deps.Health.Live)
	r.Get("/health/ready", deps.Health.Ready)

	r.Route("/auth", func(r chi.Router) {
		r.Post("/register", deps.Auth.Register)
		r.Post("/login", deps.Auth.Login)
		r.Post("/refresh", deps.Auth.Refresh)
		r.Post("/logout", deps.Auth.Logout)

		r.With(requireAuth).Get("/me", deps.Auth.Me)
	})

	// The catalogue is public. The seat map additionally reads the caller's
	// identity when a token happens to be present, to mark their own holds.
	r.Route("/events", func(r chi.Router) {
		r.Get("/", deps.Events.List)
		r.Get("/{id}", deps.Events.Get)
		r.With(optionalAuth).Get("/{id}/seatmap", deps.Events.SeatMap)

		// Holding seats commits inventory, so it always requires a caller.
		r.With(requireAuth).Post("/{id}/holds", deps.Holds.Create)
	})

	r.Route("/holds", func(r chi.Router) {
		r.Use(requireAuth)
		r.Delete("/{booking_id}", deps.Holds.Release)
	})

	r.Route("/bookings", func(r chi.Router) {
		r.Use(requireAuth)
		r.Get("/{booking_id}", deps.Holds.Get)
		r.Post("/{booking_id}/pay", deps.Pay.Pay)
	})

	r.Route("/me", func(r chi.Router) {
		r.Use(requireAuth)
		r.Get("/bookings", deps.Holds.List)
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, httpx.NotFound("No route matches that path."))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, &httpx.APIError{
			Status:  http.StatusMethodNotAllowed,
			Code:    httpx.CodeBadRequest,
			Message: "That method is not allowed on this route.",
		})
	})

	return r
}

// New builds an *http.Server with timeouts that keep a slow or idle client
// from occupying a connection indefinitely.
//
// WriteTimeout is deliberately generous: a payment request blocks for the
// mock provider's latency plus the confirm transaction.
func New(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}
