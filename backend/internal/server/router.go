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
	"github.com/Aryan-Jain06/seatsync/backend/internal/ratelimit"
)

// Deps carries everything the router needs to mount its routes.
type Deps struct {
	Config *config.Config
	Auth   *handlers.AuthHandler
	Events *handlers.EventHandler
	Holds  *handlers.HoldHandler
	Pay    *handlers.PaymentHandler
	WS     *handlers.WSHandler
	Health *handlers.HealthHandler
	Tokens middleware.Authenticator
	// Limiter may be nil, in which case throttling is skipped.
	Limiter *ratelimit.Limiter
}

// NewRouter builds the fully wired HTTP handler.
func NewRouter(deps Deps) http.Handler {
	r := chi.NewRouter()

	middleware.SetTrustProxyHeaders(deps.Config.TrustProxyHeaders)

	r.Use(middleware.RequestID)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.SecurityHeaders)
	r.Use(middleware.HSTS(deps.Config.EnableHSTS))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: deps.Config.CORSAllowedOrigins,
		AllowedMethods: []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions},
		AllowedHeaders: []string{"Accept", "Authorization", "Content-Type", "Idempotency-Key", "X-Request-ID"},
		ExposedHeaders: []string{"X-Request-ID"},
		MaxAge:         300,
	}))

	requireAuth := middleware.RequireAuth(deps.Tokens)
	optionalAuth := middleware.OptionalAuth(deps.Tokens)

	// Three separate budgets. A burst of browsing must not consume the
	// allowance that protects sign-in from password guessing, and a signed-in
	// user's booking activity is metered independently of both.
	limiter := deps.Limiter
	if !deps.Config.RateLimitEnabled {
		limiter = nil
	}
	perMinute := func(rate float64) float64 { return rate / 60 }

	limitAuth := middleware.RateLimit(limiter, "auth", ratelimit.Limit{
		Burst:     deps.Config.RateLimitAuthBurst,
		PerSecond: perMinute(deps.Config.RateLimitAuthPerMinute),
	})
	limitRead := middleware.RateLimit(limiter, "read", ratelimit.Limit{
		Burst:     deps.Config.RateLimitReadBurst,
		PerSecond: perMinute(deps.Config.RateLimitReadPerMinute),
	})
	limitWrite := middleware.RateLimit(limiter, "write", ratelimit.Limit{
		Burst:     deps.Config.RateLimitWriteBurst,
		PerSecond: perMinute(deps.Config.RateLimitWritePerMinute),
	})

	// Reads are public by default, since a ticketing catalogue is meant to be
	// browsable. Deployments that should not be readable by strangers set
	// REQUIRE_AUTH_FOR_BROWSING and get the same routes behind a token.
	browseAuth := optionalAuth
	if deps.Config.RequireAuthForBrowsing {
		browseAuth = requireAuth
	}

	r.Get("/health", deps.Health.Live)
	r.Get("/health/ready", deps.Health.Ready)

	r.Route("/auth", func(r chi.Router) {
		// Keyed by client address, because these are the endpoints a caller
		// reaches before having an identity to be keyed by.
		r.Use(limitAuth)

		r.Post("/register", deps.Auth.Register)
		r.Post("/login", deps.Auth.Login)
		r.Post("/refresh", deps.Auth.Refresh)
		r.Post("/logout", deps.Auth.Logout)

		r.With(requireAuth).Get("/me", deps.Auth.Me)
	})

	// The catalogue is public. The seat map additionally reads the caller's
	// identity when a token happens to be present, to mark their own holds.
	r.Route("/events", func(r chi.Router) {
		r.With(browseAuth, limitRead).Get("/", deps.Events.List)
		r.With(browseAuth, limitRead).Get("/{id}", deps.Events.Get)
		r.With(browseAuth, limitRead).Get("/{id}/seatmap", deps.Events.SeatMap)

		// Holding seats commits inventory, so it always requires a caller.
		r.With(requireAuth, limitWrite).Post("/{id}/holds", deps.Holds.Create)
	})

	r.Route("/holds", func(r chi.Router) {
		r.Use(requireAuth, limitWrite)
		r.Delete("/{booking_id}", deps.Holds.Release)
	})

	r.Route("/bookings", func(r chi.Router) {
		r.Use(requireAuth)
		r.With(limitRead).Get("/{booking_id}", deps.Holds.Get)
		r.With(limitWrite).Post("/{booking_id}/pay", deps.Pay.Pay)
	})

	r.Route("/me", func(r chi.Router) {
		r.Use(requireAuth, limitRead)
		r.Get("/bookings", deps.Holds.List)
	})

	// Seat state is not user specific, so a subscriber needs no credentials
	// unless the deployment has closed browsing generally. The connection is
	// still metered, since sockets are the most expensive thing to hold open.
	r.With(browseAuth, limitRead).Get("/ws/events/{id}", deps.WS.Subscribe)

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
