// Package middleware holds the cross-cutting HTTP concerns: request
// identification, structured logging, panic recovery and authentication.
package middleware

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/Aryan-Jain06/seatsync/backend/internal/auth"
	"github.com/Aryan-Jain06/seatsync/backend/internal/httpx"
	"github.com/Aryan-Jain06/seatsync/backend/internal/models"
)

// ctxKey is unexported so no other package can collide with these keys.
type ctxKey string

const (
	ctxKeyUserID ctxKey = "user_id"
	ctxKeyRole   ctxKey = "role"
)

// Authenticator verifies access tokens. Satisfied by *auth.TokenIssuer.
type Authenticator interface {
	ParseAccessToken(raw string) (*auth.Claims, error)
}

// RequestID assigns each request an ID and echoes it back, so a log line and a
// client-visible failure can be tied together.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), middleware.RequestIDKey, id)))
	})
}

// Logger records one structured line per request.
func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// WebSocket upgrades hijack the connection and run for minutes; a
		// completion log line for them is noise.
		if isWebSocketUpgrade(r) {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", middleware.GetReqID(r.Context()),
		}

		switch {
		case ww.Status() >= http.StatusInternalServerError:
			slog.Error("request", attrs...)
		case ww.Status() >= http.StatusBadRequest:
			slog.Warn("request", attrs...)
		default:
			slog.Info("request", attrs...)
		}
	})
}

// Recoverer turns a panic in a handler into a 500 instead of killing the
// process and dropping every other in-flight connection.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// A hijacked connection cannot be written to as an HTTP response.
			if errors.Is(toError(rec), http.ErrAbortHandler) {
				panic(rec)
			}

			slog.Error("panic recovered in handler",
				"panic", rec,
				"method", r.Method,
				"path", r.URL.Path,
				"request_id", middleware.GetReqID(r.Context()),
				"stack", string(debug.Stack()),
			)
			httpx.Error(w, r, httpx.Internal(fmt.Errorf("panic: %v", rec)))
		}()

		next.ServeHTTP(w, r)
	})
}

func toError(v any) error {
	if err, ok := v.(error); ok {
		return err
	}
	return fmt.Errorf("%v", v)
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// RequireAuth rejects requests without a valid access token.
func RequireAuth(a Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, err := claimsFromRequest(r, a)
			if err != nil {
				httpx.Error(w, r, err)
				return
			}

			userID, parseErr := claims.UserID()
			if parseErr != nil {
				httpx.Error(w, r, httpx.Unauthorized("Access token is invalid."))
				return
			}

			next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), userID, claims.Role)))
		})
	}
}

// OptionalAuth attaches the caller's identity when a valid token is present
// and otherwise lets the request through anonymously. The seat map uses this:
// anyone may read it, but a signed-in caller additionally learns which of the
// held seats are their own.
func OptionalAuth(a Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, err := claimsFromRequest(r, a)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}

			userID, parseErr := claims.UserID()
			if parseErr != nil {
				next.ServeHTTP(w, r)
				return
			}

			next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), userID, claims.Role)))
		})
	}
}

// RequireAdmin rejects callers that are not administrators. It must be mounted
// behind RequireAuth.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role, ok := RoleFrom(r.Context())
		if !ok {
			httpx.Error(w, r, httpx.Unauthorized("Authentication is required."))
			return
		}
		if role != models.RoleAdmin {
			httpx.Error(w, r, httpx.Forbidden("This action requires an administrator account."))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// claimsFromRequest extracts and verifies the bearer token.
func claimsFromRequest(r *http.Request, a Authenticator) (*auth.Claims, error) {
	raw, err := bearerToken(r)
	if err != nil {
		return nil, err
	}

	claims, err := a.ParseAccessToken(raw)
	if err != nil {
		if errors.Is(err, auth.ErrTokenExpired) {
			// A distinct message lets the client know to refresh rather than
			// bounce the user to the login page.
			return nil, httpx.Unauthorized("Access token has expired.")
		}
		return nil, httpx.Unauthorized("Access token is invalid.")
	}
	return claims, nil
}

// bearerToken pulls the credential out of the Authorization header, falling
// back to a query parameter for WebSocket connections, which cannot carry
// custom headers from the browser API.
func bearerToken(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	if header != "" {
		scheme, token, found := strings.Cut(header, " ")
		if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
			return "", httpx.Unauthorized("Authorization header must be of the form: Bearer <token>.")
		}
		return strings.TrimSpace(token), nil
	}

	if isWebSocketUpgrade(r) {
		if token := strings.TrimSpace(r.URL.Query().Get("access_token")); token != "" {
			return token, nil
		}
	}
	return "", httpx.Unauthorized("Authentication is required.")
}

func withIdentity(ctx context.Context, userID uuid.UUID, role models.Role) context.Context {
	ctx = context.WithValue(ctx, ctxKeyUserID, userID)
	return context.WithValue(ctx, ctxKeyRole, role)
}

// UserIDFrom returns the authenticated caller's ID.
func UserIDFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(ctxKeyUserID).(uuid.UUID)
	return id, ok
}

// RoleFrom returns the authenticated caller's role.
func RoleFrom(ctx context.Context) (models.Role, bool) {
	role, ok := ctx.Value(ctxKeyRole).(models.Role)
	return role, ok
}

// MustUserID returns the caller's ID or an unauthorized error. Handlers behind
// RequireAuth use this to avoid repeating the same check.
func MustUserID(ctx context.Context) (uuid.UUID, error) {
	id, ok := UserIDFrom(ctx)
	if !ok {
		return uuid.Nil, httpx.Unauthorized("Authentication is required.")
	}
	return id, nil
}
