package middleware

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/Aryan-Jain06/seatsync/backend/internal/httpx"
	"github.com/Aryan-Jain06/seatsync/backend/internal/ratelimit"
)

// RateLimit throttles requests, keyed by the caller's identity.
//
// scope names the class of endpoint being protected, so a client's budget for
// signing in is separate from its budget for browsing. Without that, a burst
// of reads would use up the allowance that protects the login endpoint from
// password guessing.
func RateLimit(limiter *ratelimit.Limiter, scope string, limit ratelimit.Limit) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limiter == nil {
				next.ServeHTTP(w, r)
				return
			}

			decision, err := limiter.Allow(r.Context(), scope+":"+callerKey(r), limit)
			if err != nil {
				// Allow returns Allowed on error, so this only records that
				// the limiter is degraded.
				slog.WarnContext(r.Context(), "rate limiter unavailable, allowing request", "error", err)
			}

			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit.Burst))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(decision.Remaining))

			if !decision.Allowed {
				retryAfterSeconds := int(decision.RetryAfter.Seconds())
				if retryAfterSeconds < 1 {
					retryAfterSeconds = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))

				httpx.Error(w, r, &httpx.APIError{
					Status:  http.StatusTooManyRequests,
					Code:    httpx.CodeRateLimited,
					Message: "Too many requests. Please slow down and try again shortly.",
					Details: map[string]any{"retry_after_seconds": retryAfterSeconds},
				})
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// callerKey identifies who to charge for a request.
//
// An authenticated caller is charged by user id, so one person cannot dodge
// their limit by changing address, and users sharing an office network do not
// share an allowance. Anonymous callers fall back to their address.
func callerKey(r *http.Request) string {
	if userID, ok := UserIDFrom(r.Context()); ok {
		return "user:" + userID.String()
	}
	return "ip:" + ClientIP(r)
}

// ClientIP extracts the caller's address.
//
// X-Forwarded-For is honoured only when the server is told it sits behind a
// trusted proxy. Trusting it unconditionally would let any client spoof its
// address and defeat rate limiting entirely by sending a different header
// value on every request.
var trustProxyHeaders bool

// SetTrustProxyHeaders configures whether forwarding headers are believed.
func SetTrustProxyHeaders(trust bool) { trustProxyHeaders = trust }

// ClientIP returns the caller's IP address.
func ClientIP(r *http.Request) string {
	if trustProxyHeaders {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			// The left-most entry is the original client; the rest are the
			// proxies it passed through.
			if client, _, found := strings.Cut(forwarded, ","); found || client != "" {
				if trimmed := strings.TrimSpace(client); trimmed != "" {
					return trimmed
				}
			}
		}
		if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
			return realIP
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
