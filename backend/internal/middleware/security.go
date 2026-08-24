package middleware

import "net/http"

// SecurityHeaders sets response headers that constrain what a browser will do
// with the API's responses.
//
// This is a JSON API, so the headers are chosen for the case where a response
// is somehow rendered rather than parsed: nothing here should ever be treated
// as a document, a script, or a frame.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()

		// Never let a browser second-guess the declared content type. Without
		// this, a JSON response containing attacker-influenced text could be
		// sniffed as HTML and executed.
		header.Set("X-Content-Type-Options", "nosniff")

		// The API returns no HTML, so there is never a reason to frame it.
		header.Set("X-Frame-Options", "DENY")

		// Do not leak the path a user came from to third parties.
		header.Set("Referrer-Policy", "no-referrer")

		// A restrictive policy for the same reason as nosniff: if a response
		// is ever rendered, nothing in it may execute or load.
		header.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")

		// Deny access to device APIs outright.
		header.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), payment=()")

		next.ServeHTTP(w, r)
	})
}

// HSTS instructs browsers to reach this origin over TLS only.
//
// It is applied solely to requests that already arrived over TLS: sending it
// over plain HTTP is meaningless, and sending it in local development would
// pin a developer's browser to https://localhost.
func HSTS(enabled bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if enabled && (r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https") {
				w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}
