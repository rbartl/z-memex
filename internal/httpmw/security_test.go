package httpmw

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// send issues one request from the given peer address and returns the status.
func send(h http.Handler, remoteAddr string, headers map[string]string) int {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = remoteAddr
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestRateLimitByClientIP(t *testing.T) {
	const limit = 3
	h := RateLimitByClientIP(limit, time.Minute)(okHandler())

	for i := range limit {
		if got := send(h, "192.0.2.10:1234", nil); got != http.StatusOK {
			t.Fatalf("request %d: status = %d, want %d", i+1, got, http.StatusOK)
		}
	}
	if got := send(h, "192.0.2.10:1234", nil); got != http.StatusTooManyRequests {
		t.Errorf("request past the limit: status = %d, want %d", got, http.StatusTooManyRequests)
	}
}

// Each client IP gets its own budget — one busy caller must not lock out others.
func TestRateLimitIsPerClient(t *testing.T) {
	const limit = 2
	h := RateLimitByClientIP(limit, time.Minute)(okHandler())

	for range limit + 1 {
		send(h, "192.0.2.10:1234", nil)
	}
	if got := send(h, "192.0.2.10:1234", nil); got != http.StatusTooManyRequests {
		t.Fatalf("first client should be limited, got %d", got)
	}

	if got := send(h, "198.51.100.20:1234", nil); got != http.StatusOK {
		t.Errorf("second client: status = %d, want %d", got, http.StatusOK)
	}
}

// The limiter keys off the connection peer address, so a client cannot escape
// its bucket by forging a forwarded-for header.
func TestForwardedHeadersDoNotBypassLimit(t *testing.T) {
	const limit = 2
	h := RateLimitByClientIP(limit, time.Minute)(okHandler())

	for range limit {
		send(h, "192.0.2.10:1234", nil)
	}

	spoofed := []map[string]string{
		{"X-Forwarded-For": "203.0.113.1"},
		{"X-Real-IP": "203.0.113.2"},
		{"Forwarded": "for=203.0.113.3"},
	}
	for _, headers := range spoofed {
		if got := send(h, "192.0.2.10:1234", headers); got != http.StatusTooManyRequests {
			t.Errorf("headers %v bypassed the limit: status = %d, want %d",
				headers, got, http.StatusTooManyRequests)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := SecurityHeaders(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for header, expected := range want {
		if got := rec.Header().Get(header); got != expected {
			t.Errorf("%s = %q, want %q", header, got, expected)
		}
	}

	csp := rec.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'self'", "frame-ancestors 'none'", "base-uri 'self'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP missing %q; got %q", directive, csp)
		}
	}
}
