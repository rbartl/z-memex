package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
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
	svc        MemoryService
	authToken  string
	clientName string // populated from MCP initialize clientInfo
}

func NewServer(svc MemoryService, authToken string) *Server {
	return &Server{
		svc:       svc,
		authToken: authToken,
	}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(s.authMiddleware)

	// MCP over HTTP (JSON-RPC style)
	r.Post("/mcp", s.handleMCP)

	// Health check (no auth)
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	return r
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		token := r.Header.Get("Authorization")
		token = strings.TrimPrefix(token, "Bearer ")
		if token != s.authToken {
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, nil, -32700, "parse error")
		return
	}

	if req.Method == "initialize" {
		var initParams struct {
			ClientInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		}
		if err := json.Unmarshal(req.Params, &initParams); err == nil && initParams.ClientInfo.Name != "" {
			s.clientName = initParams.ClientInfo.Name
		}
		slog.Info("new client connected",
			"remote_addr", r.RemoteAddr,
			"client", s.clientName,
		)
	}

	source := s.clientName
	if source == "" {
		source = r.UserAgent()
	}
	if source == "" {
		source = "unknown"
	}
	ctx := context.WithValue(r.Context(), ctxSource, source)
	result, err := s.dispatch(ctx, req.Method, req.Params)
	if err != nil {
		s.writeError(w, req.ID, -32603, err.Error())
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
		return nil, fmt.Errorf("unknown method: %s", method)
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

The response includes:
- **memories**: ranked results with content and similarity scores
- **graph**: entities and relationships extracted from those memories (e.g. "Go" --uses--> "chi router")`,
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
		return nil, err
	}

	var args map[string]any
	_ = json.Unmarshal(p.Arguments, &args)

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
		return nil, fmt.Errorf("unknown tool: %s", p.Name)
	}
}

// --- Tool implementations ---

func (s *Server) toolStoreMemory(ctx context.Context, args map[string]any) (any, error) {
	content := stringArg(args, "content")
	if content == "" {
		return nil, fmt.Errorf("content is required")
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

	out, _ := json.MarshalIndent(result, "", "  ")
	return toolResult(string(out)), nil
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
		return nil, fmt.Errorf("invalid id: %w", err)
	}
	if err := s.svc.DeleteMemory(ctx, id); err != nil {
		return nil, err
	}
	return toolResult("Memory deleted."), nil
}

// --- Helpers ---

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

func floatArg(args map[string]any, key string, def float32) float32 {
	switch v := args[key].(type) {
	case float64:
		return float32(v)
	case float32:
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
