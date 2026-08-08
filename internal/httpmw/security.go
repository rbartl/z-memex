// Package httpmw holds small HTTP middlewares shared between the web UI and
// MCP servers.
package httpmw

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httprate"
)

// RateLimitByClientIP bounds requests per client IP over the given window.
//
// The client is identified by the peer address of the TCP connection. memex
// publishes its port directly (see docker-compose.yml), so that address is the
// real caller and cannot be spoofed — unlike X-Forwarded-For, which any client
// could set to dodge the limit.
//
// A deployment that fronts memex with a reverse proxy, load balancer, or CDN
// must swap ClientIPFromRemoteAddr for middleware.ClientIPFromXFF (or
// ClientIPFromHeader) and name the trusted proxies. Otherwise every client
// arrives wearing the proxy's address and shares a single bucket, so one busy
// user locks out everyone else.
func RateLimitByClientIP(requests int, window time.Duration) func(http.Handler) http.Handler {
	limiter := httprate.LimitBy(requests, window, func(r *http.Request) (string, error) {
		return httprate.CanonicalizeIP(middleware.GetClientIP(r.Context())), nil
	})

	// ClientIPFromRemoteAddr must run first so the resolved IP is in the
	// context by the time the limiter reads it.
	return func(next http.Handler) http.Handler {
		return middleware.ClientIPFromRemoteAddr(limiter(next))
	}
}

// SecurityHeaders sets a baseline of defensive response headers.
//
// The CSP script-src allows both 'unsafe-inline' and 'unsafe-eval': the UI is
// built on Alpine.js, whose standard build compiles directive expressions with
// the Function constructor (needs 'unsafe-eval') and wires them through inline
// attributes/tags (needs 'unsafe-inline'). That makes script-src a weak layer,
// so XSS is defended primarily by html/template escaping and DOMPurify on the
// one dynamic-HTML sink (chat markdown). The remaining directives still add
// real value: frame-ancestors blocks clickjacking, and connect/img/base/form
// restrict exfiltration channels and injection of external resources.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Strict-Transport-Security", "max-age=31536000")
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'unsafe-inline' 'unsafe-eval'; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; "+
				"connect-src 'self'; "+
				"frame-ancestors 'none'; "+
				"base-uri 'self'; "+
				"form-action 'self'")
		next.ServeHTTP(w, r)
	})
}
