package mcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"gitlab.com/zollneritsch/memex/internal/httpmw"
	"gitlab.com/zollneritsch/memex/internal/memory"
)

type ctxKey string

const ctxSource ctxKey = "source"

// MemoryService is the interface the MCP server needs.
type MemoryService interface {
	StoreMemory(ctx context.Context, content, source, sessionID string, tags []string, importance int) (*memory.StoreResult, error)
	RecallMemoryWithGraph(ctx context.Context, query string, topK int) (*memory.RecallResult, error)
	ListMemories(ctx context.Context, filter memory.Filter) ([]*memory.Memory, error)
	DeleteMemory(ctx context.Context, id uuid.UUID) error
}

type Server struct {
	svc       MemoryService
	authToken string

	// mu guards sessions. Every HTTP request runs on its own goroutine, so the
	// client-name lookup below must not touch shared state unguarded.
	mu       sync.Mutex
	sessions map[string]*clientSession
}

// clientSession remembers which MCP client owns a session id, so a memory is
// attributed to the tool that actually stored it. This has to be per-session
// rather than per-server: several clients (Claude Code, Cursor, Claude Desktop)
// talk to one memex instance concurrently, and a single shared field would tag
// every memory with whichever client initialized most recently.
type clientSession struct {
	name     string
	lastSeen time.Time
}

func NewServer(svc MemoryService, authToken string) *Server {
	return &Server{
		svc:       svc,
		authToken: authToken,
		sessions:  make(map[string]*clientSession),
	}
}

const (
	// sessionHeader is the MCP streamable-HTTP session header. The server
	// assigns an id on initialize; compliant clients echo it on every
	// subsequent request.
	sessionHeader = "Mcp-Session-Id"

	// sessionTTL is how long an idle session keeps its client attribution.
	sessionTTL = 24 * time.Hour

	// maxSessions caps the session table so repeated initialize calls cannot
	// grow it without bound.
	maxSessions = 1024
)

// registerSession records a client name and returns its new session id.
func (s *Server) registerSession(name string) string {
	id := uuid.NewString()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	s.sessions[id] = &clientSession{name: name, lastSeen: time.Now()}
	return id
}

// clientName returns the client that owns a session id, refreshing its TTL.
// An empty string means the session is unknown or expired.
func (s *Server) clientName(id string) string {
	if id == "" {
		return ""
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return ""
	}
	sess.lastSeen = time.Now()
	return sess.name
}

// pruneLocked drops expired sessions, then the least recently used ones if the
// table is still at capacity. Callers must hold s.mu.
func (s *Server) pruneLocked() {
	cutoff := time.Now().Add(-sessionTTL)
	for id, sess := range s.sessions {
		if sess.lastSeen.Before(cutoff) {
			delete(s.sessions, id)
		}
	}

	for len(s.sessions) >= maxSessions {
		var oldestID string
		var oldest time.Time
		for id, sess := range s.sessions {
			if oldestID == "" || sess.lastSeen.Before(oldest) {
				oldestID, oldest = id, sess.lastSeen
			}
		}
		delete(s.sessions, oldestID)
	}
}

// mcpRateLimitRequests/Window bound requests per client IP to slow down
// token brute-forcing against the MCP endpoint. The budget is generous — an
// agent can burst a dozen tool calls in a turn — because the endpoint is
// already gated by a high-entropy bearer token; the limiter only needs to make
// online guessing impractical.
const (
	mcpRateLimitRequests = 120
	mcpRateLimitWindow   = time.Minute
)

// maxRequestBytes caps the JSON-RPC request body. Memories are prose, so a
// megabyte is far beyond any legitimate call, and the cap keeps a malformed or
// hostile client from forcing an unbounded allocation.
const maxRequestBytes = 1 << 20

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(httpmw.SecurityHeaders)

	// Health check (no auth, not rate limited — used by uptime monitors).
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// MCP over HTTP (JSON-RPC style)
	r.Group(func(r chi.Router) {
		r.Use(httpmw.RateLimitByClientIP(mcpRateLimitRequests, mcpRateLimitWindow))
		r.Use(s.authMiddleware)
		r.Post("/mcp", s.handleMCP)
	})

	return r
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		token = strings.TrimPrefix(token, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.authToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- MCP JSON-RPC Protocol ---

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *mcpError `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	var req mcpRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, nil, codeInvalidRequest, "request body too large")
			return
		}
		s.writeError(w, nil, codeParseError, "parse error")
		return
	}

	sessionID := r.Header.Get(sessionHeader)

	if req.Method == "initialize" {
		var initParams struct {
			ClientInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		}
		var name string
		if err := json.Unmarshal(req.Params, &initParams); err == nil {
			name = initParams.ClientInfo.Name
		}
		if name == "" {
			name = r.UserAgent()
		}

		sessionID = s.registerSession(name)
		w.Header().Set(sessionHeader, sessionID)
		slog.Info("new client connected",
			"remote_addr", r.RemoteAddr,
			"client", name,
			"session", sessionID,
		)
	}

	// Attribute the memory to the client that owns this session. Clients that
	// don't echo the session header fall back to their User-Agent rather than
	// borrowing another client's identity.
	source := s.clientName(sessionID)
	if source == "" {
		source = r.UserAgent()
	}
	if source == "" {
		source = "unknown"
	}
	ctx := context.WithValue(r.Context(), ctxSource, source)
	result, err := s.dispatch(ctx, req.Method, req.Params)
	if err != nil {
		var re *rpcError
		if errors.As(err, &re) {
			s.writeError(w, req.ID, re.code, re.msg)
			return
		}
		slog.Error("mcp request failed", "method", req.Method, "err", err)
		s.writeError(w, req.ID, codeInternalError, "internal error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(mcpResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func (s *Server) writeError(w http.ResponseWriter, id any, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &mcpError{Code: code, Message: msg},
	})
}

func (s *Server) dispatch(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return s.handleInitialize()
	case "tools/list":
		return s.handleToolsList()
	case "tools/call":
		return s.handleToolCall(ctx, params)
	default:
		return nil, notFound("unknown method: %s", method)
	}
}

func (s *Server) handleInitialize() (any, error) {
	return map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo": map[string]any{
			"name":    "memex",
			"version": "1.0.0",
		},
	}, nil
}

func (s *Server) handleToolsList() (any, error) {
	return map[string]any{
		"tools": []map[string]any{
			{
				"name": "store_memory",
				"description": `Store a piece of knowledge in the shared memory system. Automatically embeds the content, deduplicates, and builds knowledge graph connections.

YOU SHOULD PROACTIVELY CALL THIS TOOL when any of the following occur during a conversation:
- The user states a **preference or convention** (e.g. "I prefer tabs over spaces", "always use Go for new services")
- The user shares **architectural decisions** (e.g. "we use event sourcing for the order service", "our API uses REST not GraphQL")
- You learn something about the **project structure**, key file paths, or deployment setup
- The user describes a **workflow or process** they follow
- A **non-obvious bug fix or workaround** is discovered — store the root cause and solution
- The user shares **credentials, endpoints, or configuration** that would be useful in future sessions (never store raw secrets — store references like "DB is on port 5433")
- A **decision is made** after discussion (store the decision and brief rationale)
- The user explicitly asks you to **remember something**

DO NOT store:
- Trivial or obvious information (e.g. "the sky is blue")
- Raw code dumps — summarize what the code does and why it matters instead
- Temporary debugging context that has no future value
- Information that is already stored (use recall_memory first if unsure)

When storing, write the content as a clear, self-contained statement that will be useful without the original conversation context. Include relevant tags for categorization.`,
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content":    map[string]any{"type": "string", "description": "A clear, self-contained statement of the knowledge to remember. Write it so it makes sense without conversation context."},
						"session_id": map[string]any{"type": "string", "description": "Optional session identifier for grouping related memories"},
						"tags":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Categorization tags, e.g. [\"preference\", \"go\"] or [\"architecture\", \"database\"]"},
						"importance": map[string]any{"type": "integer", "description": "How important this memory is, 1-5. 1=low/trivia, 3=normal (default), 5=critical/must-never-forget. Use 4-5 for core preferences, architectural decisions, and important conventions."},
					},
					"required": []string{"content"},
				},
			},
			{
				"name": "recall_memory",
				"description": `Semantically search the shared knowledge base. Returns the most relevant memories ranked by similarity, along with a knowledge graph showing how entities in those memories are connected.

YOU SHOULD PROACTIVELY CALL THIS TOOL:
- At the **start of a conversation** to check for relevant context about the user, project, or topic
- Before storing a new memory, to **avoid duplicates**
- When the user asks about something that may have been discussed in a **previous session**
- When you need to recall **user preferences, decisions, or project details**

The response is compact text: each memory with its relevance score, id, importance, source, date and tags, followed by the strongest relationships between the entities those memories mention (e.g. "Go --uses--> chi router").`,
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{"type": "string", "description": "Natural language search query — describe what you're looking for conversationally"},
						"top_k": map[string]any{"type": "integer", "description": "Number of results to return (default 5)"},
					},
					"required": []string{"query"},
				},
			},
			{
				"name":        "manage_memories",
				"description": "List and browse stored memories for housekeeping. Use this to review what's stored, find duplicates, or clean up. For semantic search, use recall_memory instead.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"source": map[string]any{"type": "string", "description": "Filter by source (e.g. \"claude-code\", \"cursor\")"},
						"tags":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Filter by tags"},
						"limit":  map[string]any{"type": "integer", "description": "Max results (default 20)"},
					},
				},
			},
			{
				"name":        "delete_memory",
				"description": "Delete a memory by its ID. Use manage_memories or recall_memory first to find the ID.",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"id": map[string]any{"type": "string", "description": "UUID of the memory to delete"}},
					"required":   []string{"id"},
				},
			},
		},
	}, nil
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) handleToolCall(ctx context.Context, raw json.RawMessage) (any, error) {
	var p toolCallParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, invalidInput("malformed tools/call params: %v", err)
	}

	// Arguments are optional (manage_memories takes none), but if present they
	// must parse — silently dropping them would surface as a confusing
	// "required argument missing" further down.
	var args map[string]any
	if len(p.Arguments) > 0 {
		if err := json.Unmarshal(p.Arguments, &args); err != nil {
			return nil, invalidInput("malformed arguments for %s: %v", p.Name, err)
		}
	}

	switch p.Name {
	case "store_memory":
		return s.toolStoreMemory(ctx, args)
	case "recall_memory":
		return s.toolRecallMemory(ctx, args)
	case "manage_memories":
		return s.toolManageMemories(ctx, args)
	case "delete_memory":
		return s.toolDeleteMemory(ctx, args)
	default:
		return nil, notFound("unknown tool: %s", p.Name)
	}
}

// --- Tool implementations ---

func (s *Server) toolStoreMemory(ctx context.Context, args map[string]any) (any, error) {
	content := stringArg(args, "content")
	if content == "" {
		return nil, invalidInput("content is required")
	}
	source, _ := ctx.Value(ctxSource).(string)
	sessionID := stringArg(args, "session_id")
	tags := stringSliceArg(args, "tags")
	importance := intArg(args, "importance", 0)

	result, err := s.svc.StoreMemory(ctx, content, source, sessionID, tags, importance)
	if err != nil {
		return nil, err
	}

	slog.Info("stored memory", "id", result.Memory.ID, "action", result.Action, "source", source)

	switch result.Action {
	case "skipped":
		return toolResult(fmt.Sprintf("Exact duplicate already exists. Memory ID: %s (no changes made).", result.Memory.ID)), nil
	case "merged":
		return toolResult(fmt.Sprintf("Merged with existing memory (similarity: %.2f). Memory ID: %s. Content combined to preserve all unique facts. Relationship extraction running in background.", result.Score, result.Memory.ID)), nil
	case "updated":
		return toolResult(fmt.Sprintf("Updated near-identical memory (similarity: %.2f). Memory ID: %s. Relationship extraction running in background.", result.Score, result.Memory.ID)), nil
	default:
		return toolResult(fmt.Sprintf("Memory stored. ID: %s. Relationship extraction running in background.", result.Memory.ID)), nil
	}
}

func (s *Server) toolRecallMemory(ctx context.Context, args map[string]any) (any, error) {
	query := stringArg(args, "query")
	topK := intArg(args, "top_k", 5)

	result, err := s.svc.RecallMemoryWithGraph(ctx, query, topK)
	if err != nil {
		return nil, err
	}

	return toolResult(formatRecall(query, result)), nil
}

func (s *Server) toolManageMemories(ctx context.Context, args map[string]any) (any, error) {
	filter := memory.Filter{
		Source: stringArg(args, "source"),
		Tags:   stringSliceArg(args, "tags"),
		Limit:  intArg(args, "limit", 20),
	}
	memories, err := s.svc.ListMemories(ctx, filter)
	if err != nil {
		return nil, err
	}
	out, _ := json.MarshalIndent(memories, "", "  ")
	return toolResult(string(out)), nil
}

func (s *Server) toolDeleteMemory(ctx context.Context, args map[string]any) (any, error) {
	idStr := stringArg(args, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, invalidInput("invalid id: %v", err)
	}
	if err := s.svc.DeleteMemory(ctx, id); err != nil {
		return nil, err
	}
	return toolResult("Memory deleted."), nil
}

// --- Helpers ---

// JSON-RPC 2.0 error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// rpcError is a client-facing error: its message is safe to return verbatim
// and it carries the JSON-RPC code to report. Everything else — DB, embedding,
// and LLM failures — is logged server-side and reported as a bare internal
// error, so implementation details never reach the client.
type rpcError struct {
	code int
	msg  string
}

func (e *rpcError) Error() string { return e.msg }

// invalidInput reports a malformed or missing tool argument.
func invalidInput(format string, args ...any) error {
	return &rpcError{code: codeInvalidParams, msg: fmt.Sprintf(format, args...)}
}

// notFound reports an unknown method or tool name.
func notFound(format string, args ...any) error {
	return &rpcError{code: codeMethodNotFound, msg: fmt.Sprintf(format, args...)}
}

func toolResult(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}
}

func stringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func intArg(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return def
}

func stringSliceArg(args map[string]any, key string) []string {
	v, ok := args[key].([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(v))
	for _, item := range v {
		if s, ok := item.(string); ok {
			result = append(result, s)
		}
	}
	return result
}
