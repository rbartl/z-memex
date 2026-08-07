package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
