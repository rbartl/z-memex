package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"gitlab.com/zollneritsch/memex/internal/memory"
)

const testToken = "test-token"

// stubService satisfies MemoryService. StoreMemory fails with an internal-style
// error so tests can assert that its details never reach the client.
type stubService struct{}

func (stubService) StoreMemory(context.Context, string, string, string, []string, int) (*memory.StoreResult, error) {
	return nil, errors.New("pgx: connection refused to 10.0.0.5:5432")
}

// recordingService captures the source each StoreMemory call was attributed to.
type recordingService struct {
	stubService
	mu      sync.Mutex
	sources []string
}

func (r *recordingService) StoreMemory(_ context.Context, _, source, _ string, _ []string, _ int) (*memory.StoreResult, error) {
	r.mu.Lock()
	r.sources = append(r.sources, source)
	r.mu.Unlock()
	return &memory.StoreResult{Memory: &memory.Memory{ID: uuid.New()}, Action: "created"}, nil
}

func (stubService) RecallMemoryWithGraph(context.Context, string, int) (*memory.RecallResult, error) {
	return &memory.RecallResult{}, nil
}

func (stubService) ListMemories(context.Context, memory.Filter) ([]*memory.Memory, error) {
	return nil, nil
}

func (stubService) DeleteMemory(context.Context, uuid.UUID) error { return nil }

// post sends a raw JSON-RPC body to /mcp and returns the decoded response.
func post(t *testing.T, body string) (*httptest.ResponseRecorder, mcpResponse) {
	t.Helper()
	srv := NewServer(stubService{}, testToken)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	var resp mcpResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v (raw: %s)", err, rec.Body.String())
	}
	return rec, resp
}

func TestErrorCodes(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode int
		wantMsg  string // substring
	}{
		{
			name:     "unknown method is method-not-found, not internal",
			body:     `{"jsonrpc":"2.0","id":1,"method":"nope"}`,
			wantCode: codeMethodNotFound,
			wantMsg:  "unknown method",
		},
		{
			name:     "unknown tool is method-not-found, not internal",
			body:     `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`,
			wantCode: codeMethodNotFound,
			wantMsg:  "unknown tool",
		},
		{
			name:     "missing required argument is invalid-params",
			body:     `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"store_memory","arguments":{}}}`,
			wantCode: codeInvalidParams,
			wantMsg:  "content is required",
		},
		{
			name:     "malformed arguments are invalid-params",
			body:     `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"store_memory","arguments":"not-an-object"}}`,
			wantCode: codeInvalidParams,
			wantMsg:  "malformed arguments",
		},
		{
			name:     "unparseable body is a parse error",
			body:     `{not json`,
			wantCode: codeParseError,
			wantMsg:  "parse error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, resp := post(t, tc.body)
			if resp.Error == nil {
				t.Fatalf("expected an error, got result: %+v", resp.Result)
			}
			if resp.Error.Code != tc.wantCode {
				t.Errorf("code = %d, want %d (msg: %q)", resp.Error.Code, tc.wantCode, resp.Error.Message)
			}
			if !strings.Contains(resp.Error.Message, tc.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", resp.Error.Message, tc.wantMsg)
			}
		})
	}
}

// Internal failures must be logged server-side but reported as a bare
// "internal error" — never leaking DB hosts, drivers, or query text.
func TestInternalErrorIsNotLeaked(t *testing.T) {
	_, resp := post(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call",`+
		`"params":{"name":"store_memory","arguments":{"content":"hello"}}}`)

	if resp.Error == nil {
		t.Fatal("expected an error")
	}
	if resp.Error.Code != codeInternalError {
		t.Errorf("code = %d, want %d", resp.Error.Code, codeInternalError)
	}
	if resp.Error.Message != "internal error" {
		t.Errorf("message = %q, want exactly %q", resp.Error.Message, "internal error")
	}
	if strings.Contains(resp.Error.Message, "10.0.0.5") {
		t.Error("response leaked the database address")
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	huge := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"store_memory",` +
		`"arguments":{"content":"` + strings.Repeat("a", maxRequestBytes+1) + `"}}}`

	_, resp := post(t, huge)
	if resp.Error == nil {
		t.Fatal("expected an error")
	}
	if resp.Error.Code != codeInvalidRequest {
		t.Errorf("code = %d, want %d (msg: %q)", resp.Error.Code, codeInvalidRequest, resp.Error.Message)
	}
}

func TestAuth(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   int
	}{
		{"valid token", "Bearer " + testToken, http.StatusOK},
		{"wrong token", "Bearer wrong", http.StatusUnauthorized},
		{"missing header", "", http.StatusUnauthorized},
		{"token without prefix", testToken, http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(stubService{}, testToken)
			req := httptest.NewRequest(http.MethodPost, "/mcp",
				strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// initialize must hand back a session id, and that session must carry the
// client's own name on later requests.
func TestSessionCarriesClientName(t *testing.T) {
	svc := &recordingService{}
	srv := NewServer(svc, testToken)

	sessionID := initialize(t, srv, "claude-code")
	if sessionID == "" {
		t.Fatal("initialize returned no " + sessionHeader)
	}

	storeMemory(t, srv, sessionID, "remember this")

	if len(svc.sources) != 1 {
		t.Fatalf("got %d StoreMemory calls, want 1", len(svc.sources))
	}
	if svc.sources[0] != "claude-code" {
		t.Errorf("source = %q, want %q", svc.sources[0], "claude-code")
	}
}

// The bug this replaced: a single shared clientName field meant the most recent
// initialize won, so every client's memories were attributed to it. Each
// session must keep its own identity.
func TestSessionsDoNotShareIdentity(t *testing.T) {
	svc := &recordingService{}
	srv := NewServer(svc, testToken)

	codeSession := initialize(t, srv, "claude-code")
	cursorSession := initialize(t, srv, "cursor")

	if codeSession == cursorSession {
		t.Fatal("two initializes returned the same session id")
	}

	// Store from the first client *after* the second one connected — the old
	// implementation would have attributed this to "cursor".
	storeMemory(t, srv, codeSession, "from claude code")
	storeMemory(t, srv, cursorSession, "from cursor")

	want := []string{"claude-code", "cursor"}
	if len(svc.sources) != len(want) {
		t.Fatalf("got sources %v, want %v", svc.sources, want)
	}
	for i, w := range want {
		if svc.sources[i] != w {
			t.Errorf("source[%d] = %q, want %q", i, svc.sources[i], w)
		}
	}
}

// An unknown or absent session must fall back to the User-Agent rather than
// borrowing whatever identity happens to be registered.
func TestUnknownSessionFallsBackToUserAgent(t *testing.T) {
	svc := &recordingService{}
	srv := NewServer(svc, testToken)

	initialize(t, srv, "claude-code")

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(storeBody("no session")))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("User-Agent", "some-other-tool/1.0")
	// Deliberately no Mcp-Session-Id.
	srv.Router().ServeHTTP(httptest.NewRecorder(), req)

	if len(svc.sources) != 1 {
		t.Fatalf("got %d StoreMemory calls, want 1", len(svc.sources))
	}
	if svc.sources[0] != "some-other-tool/1.0" {
		t.Errorf("source = %q, want the User-Agent", svc.sources[0])
	}
}

// Concurrent traffic across sessions must be race-free; run under -race.
func TestConcurrentSessionsAreRaceFree(t *testing.T) {
	svc := &recordingService{}
	srv := NewServer(svc, testToken)

	const clients, callsEach = 8, 20

	var wg sync.WaitGroup
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("client-%d", i)
			session := initialize(t, srv, name)
			for range callsEach {
				storeMemory(t, srv, session, "concurrent write")
			}
		}()
	}
	wg.Wait()

	if len(svc.sources) != clients*callsEach {
		t.Fatalf("got %d StoreMemory calls, want %d", len(svc.sources), clients*callsEach)
	}

	// Every call must be attributed to a real client, and each client must
	// appear exactly as many times as it wrote.
	counts := map[string]int{}
	for _, s := range svc.sources {
		counts[s]++
	}
	for i := range clients {
		name := fmt.Sprintf("client-%d", i)
		if counts[name] != callsEach {
			t.Errorf("client %q attributed %d times, want %d", name, counts[name], callsEach)
		}
	}
}

// The session table must not grow without bound.
func TestSessionTableIsBounded(t *testing.T) {
	srv := NewServer(stubService{}, testToken)

	for range maxSessions + 50 {
		srv.registerSession("noisy-client")
	}

	srv.mu.Lock()
	n := len(srv.sessions)
	srv.mu.Unlock()

	if n > maxSessions {
		t.Errorf("session table holds %d entries, want at most %d", n, maxSessions)
	}
}

// initialize performs an MCP initialize and returns the assigned session id.
func initialize(t *testing.T, srv *Server, clientName string) string {
	t.Helper()
	body := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":%q}}}`,
		clientName)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("initialize status = %d, want %d", rec.Code, http.StatusOK)
	}
	return rec.Header().Get(sessionHeader)
}

func storeBody(content string) string {
	return fmt.Sprintf(
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"store_memory","arguments":{"content":%q}}}`,
		content)
}

func storeMemory(t *testing.T, srv *Server, sessionID, content string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(storeBody(content)))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set(sessionHeader, sessionID)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("store status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// The health endpoint is used by uptime monitors and must stay open.
func TestHealthNeedsNoAuth(t *testing.T) {
	srv := NewServer(stubService{}, testToken)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	// Security headers apply to every response, including unauthenticated ones.
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("missing Content-Security-Policy header")
	}
}
