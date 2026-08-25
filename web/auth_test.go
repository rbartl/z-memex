package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	testUser = "admin"
	testPass = "correct-horse-battery-staple"
)

// testServer builds the UI mounted at /ui, exactly as cmd/server does, so
// redirect targets and the cookie path are exercised as they ship.
func testServer(t *testing.T) (http.Handler, *Handler) {
	t.Helper()
	// templateDir "." — tests run from the web package directory.
	h := NewHandler(nil, nil, ".", testUser, testPass)
	root := chi.NewRouter()
	root.Mount("/ui", h.Router())
	return root, h
}

func do(t *testing.T, srv http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// login performs a real form post and returns the session cookie.
func login(t *testing.T, srv http.Handler, user, pass string) *http.Cookie {
	t.Helper()
	form := url.Values{"username": {user}, "password": {pass}}
	req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := do(t, srv, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatal("login did not set a session cookie")
	return nil
}

func TestUnauthenticatedPageRedirectsToLogin(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/ui/graph", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	// The requested page is carried through so the user lands where they aimed.
	if got, want := rec.Header().Get("Location"), "/ui/login?next=%2Fui%2Fgraph"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	// The old scheme's browser popup must not come back.
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate = %q, want it absent", got)
	}
}

// The login page has to be the kind of form a password manager recognises:
// a real POST form with named username/password fields and autocomplete hints.
func TestLoginPageIsAPasswordManagerFriendlyForm(t *testing.T) {
	srv, _ := testServer(t)

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/ui/login", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`method="post"`,
		`action="/ui/login"`,
		`name="username"`,
		`autocomplete="username"`,
		`type="password"`,
		`name="password"`,
		`autocomplete="current-password"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("login page missing %q", want)
		}
	}
}

func TestLoginRejectsWrongCredentials(t *testing.T) {
	srv, _ := testServer(t)

	cases := []struct {
		name, user, pass string
	}{
		{"wrong password", testUser, "guess"},
		{"wrong username", "root", testPass},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{"username": {tc.user}, "password": {tc.pass}}
			req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := do(t, srv, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			for _, c := range rec.Result().Cookies() {
				if c.Name == sessionCookieName && c.Value != "" {
					t.Error("a failed login issued a session cookie")
				}
			}
		})
	}
}

func TestSessionCookieGrantsAccess(t *testing.T) {
	srv, _ := testServer(t)

	cookie := login(t, srv, testUser, testPass)

	if !cookie.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
	// Scoped to the UI so it never rides along to the MCP endpoints.
	if cookie.Path != "/ui" {
		t.Errorf("cookie path = %q, want %q", cookie.Path, "/ui")
	}

	req := httptest.NewRequest(http.MethodGet, "/ui/setup", nil)
	req.AddCookie(cookie)
	if rec := do(t, srv, req); rec.Code != http.StatusOK {
		t.Errorf("authenticated request: status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// Signing out ends every session, so a cookie copied to another machine dies
// with the click rather than surviving until the server restarts.
func TestLogoutRevokesEverySession(t *testing.T) {
	srv, _ := testServer(t)
	first := login(t, srv, testUser, testPass)
	second := login(t, srv, testUser, testPass)
	if first.Value == second.Value {
		t.Fatal("two logins reused one token")
	}

	req := httptest.NewRequest(http.MethodPost, "/ui/logout", nil)
	req.AddCookie(second)
	rec := do(t, srv, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("logout status = %d, want %d", rec.Code, http.StatusSeeOther)
	}

	// Both tokens are dead server-side, not merely cleared in the browser.
	for name, c := range map[string]*http.Cookie{"signed-out session": second, "other session": first} {
		reuse := httptest.NewRequest(http.MethodGet, "/ui/setup", nil)
		reuse.AddCookie(c)
		if got := do(t, srv, reuse).Code; got != http.StatusSeeOther {
			t.Errorf("%s after logout: status = %d, want a redirect to login", name, got)
		}
	}
}

// The idle window slides on use, but the absolute ceiling set at login does
// not — otherwise a stolen cookie could be kept alive forever by polling.
func TestAbsoluteSessionLifetimeIsNotExtended(t *testing.T) {
	srv, h := testServer(t)
	cookie := login(t, srv, testUser, testPass)

	h.sessions.mu.Lock()
	e := h.sessions.sessions[cookie.Value]
	e.absolute = time.Now().Add(-time.Minute) // as if issued a month ago
	h.sessions.sessions[cookie.Value] = e
	h.sessions.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/ui/setup", nil)
	req.AddCookie(cookie)
	if got := do(t, srv, req).Code; got != http.StatusSeeOther {
		t.Errorf("session past its absolute lifetime: status = %d, want a redirect to login", got)
	}
}

// Pages behind the gate carry memory contents, so they must not be cached —
// otherwise the back button re-renders them from history after sign-out, and
// on a shared machine they linger in the disk cache.
func TestAuthenticatedPagesAreNotCached(t *testing.T) {
	srv, _ := testServer(t)
	cookie := login(t, srv, testUser, testPass)

	for _, path := range []string{"/ui/setup", "/ui/graph"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		if got := do(t, srv, req).Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
			t.Errorf("%s: Cache-Control = %q, want no-store", path, got)
		}
	}

	// The login page holds no memories, but credentials get posted to it.
	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/ui/login", nil))
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("/ui/login: Cache-Control = %q, want no-store", got)
	}
}

// The vendored scripts sit behind the session gate but are public library
// code, so they stay cacheable — and privately, never in a shared proxy.
func TestStaticAssetsStayPrivatelyCacheable(t *testing.T) {
	// The router resolves static files relative to the repo root.
	t.Chdir("..")
	h := NewHandler(nil, nil, "web", testUser, testPass)
	srv := chi.NewRouter()
	srv.Mount("/ui", h.Router())
	cookie := login(t, srv, testUser, testPass)

	req := httptest.NewRequest(http.MethodGet, "/ui/static/vendor/htmx.min.js", nil)
	req.AddCookie(cookie)
	rec := do(t, srv, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	cc := rec.Header().Get("Cache-Control")
	if !strings.Contains(cc, "private") {
		t.Errorf("Cache-Control = %q, want it privately cacheable", cc)
	}
	if strings.Contains(cc, "public") || strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want neither public nor no-store", cc)
	}
}

func TestExpiredSessionIsRejected(t *testing.T) {
	srv, h := testServer(t)
	cookie := login(t, srv, testUser, testPass)

	// Age the session past its idle window.
	h.sessions.mu.Lock()
	e := h.sessions.sessions[cookie.Value]
	e.idle = time.Now().Add(-time.Minute)
	h.sessions.sessions[cookie.Value] = e
	h.sessions.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/ui/setup", nil)
	req.AddCookie(cookie)
	if got := do(t, srv, req).Code; got != http.StatusSeeOther {
		t.Errorf("expired session: status = %d, want a redirect to login", got)
	}
}

func TestForgedSessionCookieIsRejected(t *testing.T) {
	srv, _ := testServer(t)

	req := httptest.NewRequest(http.MethodGet, "/ui/setup", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "not-a-real-token"})
	if got := do(t, srv, req).Code; got != http.StatusSeeOther {
		t.Errorf("forged cookie: status = %d, want a redirect to login", got)
	}
}

// htmx swaps response bodies into the page, so an expired session has to be
// answered with HX-Redirect rather than a redirect the fetch would follow.
func TestHtmxRequestGetsHXRedirect(t *testing.T) {
	srv, _ := testServer(t)

	req := httptest.NewRequest(http.MethodGet, "/ui/search?q=hello", nil)
	req.Header.Set("HX-Request", "true")
	rec := do(t, srv, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if got := rec.Header().Get("HX-Redirect"); got != "/ui/login" {
		t.Errorf("HX-Redirect = %q, want %q", got, "/ui/login")
	}
}

// A non-GET without a session gets a plain 401 — there is no page to return to.
func TestUnauthenticatedWriteIsUnauthorized(t *testing.T) {
	srv, _ := testServer(t)

	req := httptest.NewRequest(http.MethodDelete, "/ui/memories/"+strings.Repeat("0", 8), nil)
	if got := do(t, srv, req).Code; got != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", got, http.StatusUnauthorized)
	}
}

// The login endpoint must not be usable to bounce a victim off-site.
func TestLoginRedirectStaysOnSite(t *testing.T) {
	cases := []struct{ next, want string }{
		{"/ui/graph", "/ui/graph"},
		{"/ui/memories/abc?x=1", "/ui/memories/abc?x=1"},
		{"https://evil.example/", "/ui"},
		{"//evil.example/", "/ui"},
		{"/\\evil.example", "/ui"},
		{"/health", "/ui"},
		{"", "/ui"},
		// Siblings of /ui are not open redirects, but the router only 404s on
		// them, so they have no business surviving the check.
		{"/uievil", "/ui"},
		{"/ui.evil.example", "/ui"},
		{"/ui@evil.example", "/ui"},
		{"/ui/\\evil.example", "/ui"},
		// The UI root keeps its query string.
		{"/ui?tab=recent", "/ui?tab=recent"},
	}
	for _, tc := range cases {
		srv, _ := testServer(t)
		form := url.Values{
			"username": {testUser},
			"password": {testPass},
			"next":     {tc.next},
		}
		req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := do(t, srv, req)

		if got := rec.Header().Get("Location"); got != tc.want {
			t.Errorf("next=%q: Location = %q, want %q", tc.next, got, tc.want)
		}
	}
}

// Static assets stay behind the session gate; the login page is self-contained
// so it does not need them.
func TestStaticFilesRequireSession(t *testing.T) {
	srv, _ := testServer(t)

	req := httptest.NewRequest(http.MethodGet, "/ui/static/vendor/htmx.min.js", nil)
	if got := do(t, srv, req).Code; got != http.StatusSeeOther {
		t.Errorf("status = %d, want a redirect to login", got)
	}
}

func TestLoginBruteForceIsRateLimited(t *testing.T) {
	srv, _ := testServer(t)

	// The UI-wide budget is far higher, so anything blocking here is the
	// login-specific limiter.
	var limited bool
	for range loginRateLimitRequests + 1 {
		form := url.Values{"username": {testUser}, "password": {"guess"}}
		req := httptest.NewRequest(http.MethodPost, "/ui/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if do(t, srv, req).Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Errorf("guessing was never rate limited within %d attempts", loginRateLimitRequests+1)
	}
}
