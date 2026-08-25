package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookieName = "memex_session"

	// sessionIdleTTL is how long a login survives without activity. It slides
	// forward on every authenticated request, so an active browser is never
	// logged out mid-session while an abandoned one expires within the week.
	sessionIdleTTL = 7 * 24 * time.Hour

	// sessionMaxLifetime is a hard ceiling fixed at login and never extended.
	// Without it the sliding idle window renews forever, so a stolen cookie
	// polled once a week would outlive the session it was taken from. This
	// bounds that to a month regardless of how much traffic the token sees.
	sessionMaxLifetime = 30 * 24 * time.Hour

	// maxLoginBodyBytes caps the login form post. Two short fields need
	// nothing more, and the small ceiling keeps the one unauthenticated POST
	// in the app from accepting arbitrary bodies.
	maxLoginBodyBytes = 4 << 10

	// loginRateLimit is a second, much tighter budget layered on top of the
	// UI-wide limiter, applied only to credential submissions. The UI budget
	// has to clear normal browsing (four requests a second), which would still
	// allow a brisk password-guessing loop; this caps guesses per IP per minute.
	loginRateLimitRequests = 10
	loginRateLimitWindow   = time.Minute

	// staticPathPrefix is the one authenticated path exempt from no-store:
	// everything under it is vendored library code, not memory contents.
	staticPathPrefix = "/ui/static/"
)

// sessionEntry tracks the two independent deadlines a session has to clear.
type sessionEntry struct {
	// idle slides forward on every authenticated request.
	idle time.Time
	// absolute is set once at login and never extended.
	absolute time.Time
}

func (e sessionEntry) expired(now time.Time) bool {
	return now.After(e.idle) || now.After(e.absolute)
}

// sessionStore holds live login sessions in memory, keyed by an opaque random
// token handed to the browser in a cookie.
//
// Memory is deliberate: memex is a single-user, single-process deployment, so
// the only cost is that a server restart logs the browser out again. Nothing
// about a session is worth a database round-trip on every request.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]sessionEntry
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: map[string]sessionEntry{}}
}

// issue mints a new session token. The 256 bits of randomness are what makes
// the token unguessable, so a leaked cookie is the only realistic way in.
func (s *sessionStore) issue() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()
	// Logins are rare, so this is a cheap place to drop expired entries and
	// keep the map from growing across a long uptime.
	now := time.Now()
	for t, e := range s.sessions {
		if e.expired(now) {
			delete(s.sessions, t)
		}
	}
	s.sessions[token] = sessionEntry{
		idle:     now.Add(sessionIdleTTL),
		absolute: now.Add(sessionMaxLifetime),
	}
	return token, nil
}

// valid reports whether the token names a live session, sliding its idle
// window forward when it does. The absolute deadline is left alone: a session
// that has run its month is finished no matter how recently it was used.
func (s *sessionStore) valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.sessions[token]
	if !ok {
		return false
	}
	now := time.Now()
	if e.expired(now) {
		delete(s.sessions, token)
		return false
	}
	e.idle = now.Add(sessionIdleTTL)
	s.sessions[token] = e
	return true
}

// revokeAll drops every live session, not just the caller's.
//
// One person owns every session here, so "sign out" meaning "sign out
// everywhere" is the reading that matches the deployment — and it gives a
// suspected-stolen cookie a recovery path that does not involve restarting the
// server. Signing in from a second device stays harmless: it adds a session
// rather than displacing one.
func (s *sessionStore) revokeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.sessions)
}

// requireSession gates the UI on a valid session cookie.
func (h *Handler) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.authenticated(r) {
			setPrivateCacheHeader(w, r)
			next.ServeHTTP(w, r)
			return
		}

		loginURL := "/ui/login"
		if target := loginRedirectTarget(r); target != "" {
			loginURL += "?next=" + url.QueryEscape(target)
		}

		// htmx swaps the response body into the page, so a plain redirect
		// would paint the login form into a table cell. HX-Redirect makes the
		// browser navigate instead.
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", loginURL)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, loginURL, http.StatusSeeOther)
	})
}

// setPrivateCacheHeader keeps signed-in responses out of the browser's cache.
//
// Pages, htmx fragments and the graph JSON all carry memory contents, so
// no-store is what stops the back button from re-rendering them after sign-out
// and keeps them off disk on a shared machine. Static assets are the
// exception: they are public library code that happens to sit behind the gate,
// and refetching them on every page load buys nothing.
func setPrivateCacheHeader(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, staticPathPrefix) {
		w.Header().Set("Cache-Control", "private, max-age=3600")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
}

func (h *Handler) authenticated(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	return h.sessions.valid(c.Value)
}

func (h *Handler) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if h.authenticated(r) {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	h.renderLogin(w, r, http.StatusOK, "", "")
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBodyBytes)
	if err := r.ParseForm(); err != nil {
		h.renderLogin(w, r, http.StatusBadRequest, "", "Invalid form submission.")
		return
	}

	username := r.PostFormValue("username")
	password := r.PostFormValue("password")

	// Compare both fields unconditionally — a short-circuit on the username
	// would leak, through response timing, whether it was the right one.
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(h.username))
	passOK := subtle.ConstantTimeCompare([]byte(password), []byte(h.password))
	if userOK&passOK != 1 {
		slog.Warn("failed login attempt", "remote_addr", r.RemoteAddr)
		// The message stays vague on purpose: naming which field was wrong
		// tells an attacker when they have found a valid username.
		h.renderLogin(w, r, http.StatusUnauthorized, username, "Incorrect username or password.")
		return
	}

	token, err := h.sessions.issue()
	if err != nil {
		slog.Error("session token generation failed", "err", err)
		h.renderLogin(w, r, http.StatusInternalServerError, username, "Could not start a session. Try again.")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:  sessionCookieName,
		Value: token,
		// Scoped to the UI so the cookie is never sent to the MCP endpoints,
		// which authenticate with their own bearer token.
		Path:     "/ui",
		HttpOnly: true,
		// Lax keeps the cookie off cross-site POST/DELETE requests, which is
		// what stands in for CSRF tokens on the delete and chat endpoints.
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		MaxAge:   int(sessionIdleTTL / time.Second),
	})

	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	h.sessions.revokeAll()
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/ui",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

func (h *Handler) renderLogin(w http.ResponseWriter, r *http.Request, status int, username, errMsg string) {
	next := r.URL.Query().Get("next")
	if next == "" {
		next = r.PostFormValue("next")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Credentials were just posted here; keep the page out of caches.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	t, ok := h.templates["login.html"]
	if !ok {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	if err := t.ExecuteTemplate(w, "login", map[string]any{
		"Username": username,
		"Error":    errMsg,
		"Next":     safeNext(next),
	}); err != nil {
		slog.Error("login template render failed", "err", err)
	}
}

// safeNext keeps post-login redirects inside the UI. Anything else — an
// absolute URL, a protocol-relative "//evil.example", a path outside /ui —
// falls back to the home page, so the login endpoint can't be used as an open
// redirect.
//
// The prefix test is against "/ui/" rather than "/ui" so that sibling paths
// like "/uievil" are not swept in. None of those were an open redirect (they
// resolve against our own host), but a target the router will only 404 on has
// no business surviving the check. The comparison runs on the path alone so a
// query string on the UI root is preserved.
func safeNext(next string) string {
	// Some browsers treat a backslash as a path separator, so it could still
	// carry a target off-site.
	if strings.Contains(next, "\\") {
		return "/ui"
	}
	path := next
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	if path != "/ui" && !strings.HasPrefix(path, "/ui/") {
		return "/ui"
	}
	return next
}

// loginRedirectTarget is the path to return to after logging in, for requests
// where returning makes sense (a page load, not an htmx fragment fetch).
func loginRedirectTarget(r *http.Request) string {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return ""
	}
	if r.Header.Get("HX-Request") == "true" {
		return ""
	}
	return safeNext(r.URL.RequestURI())
}

// isHTTPS reports whether the browser reached us over TLS, including through a
// terminating proxy. A forged X-Forwarded-Proto can only add the Secure flag,
// which fails closed: the cookie stops being sent, it never leaks.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
