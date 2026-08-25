package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitlab.com/zollneritsch/memex/internal/mcp"
	"gitlab.com/zollneritsch/memex/web"
)

// TestRouterPaths pins the top-level URL layout. The landing page took over
// "/", which forced the MCP router off Mount and onto Handle, so the MCP and
// health endpoints need a guard proving they still answer at their own paths.
func TestRouterPaths(t *testing.T) {
	// Routing needs neither storage nor a chat backend: no handler exercised
	// here reaches them.
	r := router(
		mcp.NewServer(nil, "test-token"),
		web.NewHandler(nil, nil, "../../web", "user", "pass"),
	)

	cases := []struct {
		name   string
		method string
		path   string
		status int
		body   string
	}{
		{"landing", http.MethodGet, "/", http.StatusOK, `id="field"`},
		{"health", http.MethodGet, "/health", http.StatusOK, `"status":"ok"`},
		{"mcp rejects anonymous callers", http.MethodPost, "/mcp", http.StatusUnauthorized, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))

			if rec.Code != tc.status {
				t.Fatalf("%s %s: got status %d, want %d", tc.method, tc.path, rec.Code, tc.status)
			}
			if tc.body != "" && !strings.Contains(rec.Body.String(), tc.body) {
				t.Errorf("%s %s: body does not contain %q", tc.method, tc.path, tc.body)
			}
		})
	}

	// The UI is mounted, not absent: it must not fall through to the 404.
	t.Run("ui is mounted", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui", nil))
		if rec.Code == http.StatusNotFound {
			t.Fatalf("GET /ui: got 404, want the UI router to handle it")
		}
	})
}
