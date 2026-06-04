package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"gitlab.com/zollneritsch/memex/internal/chat"
	"gitlab.com/zollneritsch/memex/internal/memory"
)

// ChatStreamer streams a chat completion token-by-token and can synthesize
// a context-aware search query from the conversation history.
type ChatStreamer interface {
	Stream(ctx context.Context, messages []chat.Message, onToken func(string) error) error
	SynthesizeQuery(ctx context.Context, history []chat.Message, latestUserMsg string) (string, error)
	Model() string
}

type MemoryService interface {
	ListMemories(ctx context.Context, filter memory.Filter) ([]*memory.Memory, error)
	GetMemory(ctx context.Context, id uuid.UUID) (*memory.Memory, error)
	RecallMemory(ctx context.Context, query string, topK int) ([]memory.ScoredMemory, error)
	DeleteMemory(ctx context.Context, id uuid.UUID) error
	GetGraph(ctx context.Context, entity string, depth int) (*memory.Graph, error)
	GetFullGraph(ctx context.Context) (*memory.Graph, error)
	GetMemoriesByEntity(ctx context.Context, entityID uuid.UUID) ([]*memory.Memory, error)
	GetRelatedMemories(ctx context.Context, memoryID uuid.UUID) ([]memory.RelatedMemory, error)
}

type Handler struct {
	svc       MemoryService
	chat      ChatStreamer
	templates map[string]*template.Template
	username  string
	password  string
}

func NewHandler(svc MemoryService, chatClient ChatStreamer, templateDir, username, password string) *Handler {
	funcMap := template.FuncMap{
		"truncate": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			return s[:n] + "..."
		},
		"joinTags": func(tags []string) string {
			return strings.Join(tags, ", ")
		},
		"toJSON": func(v any) template.JS {
			b, _ := json.Marshal(v)
			return template.JS(b)
		},
		"importanceLabel": func(level int) string {
			switch level {
			case 1:
				return "low"
			case 2:
				return "below normal"
			case 4:
				return "high"
			case 5:
				return "critical"
			default:
				return "normal"
			}
		},
		"entityColor": func(entityType string) string {
			colors := map[string]string{
				"person":       "#6c8aff",
				"project":      "#ff9f43",
				"technology":   "#54e0c7",
				"concept":      "#a78bfa",
				"organization": "#f472b6",
				"tool":         "#fbbf24",
				"file":         "#8b8fa3",
			}
			if c, ok := colors[entityType]; ok {
				return c
			}
			return "#94a3b8"
		},
	}

	layoutFile := filepath.Join(templateDir, "templates", "layout.html")
	partialsGlob := filepath.Join(templateDir, "partials", "*.html")

	// Parse each page template independently with the layout + partials.
	// This avoids "content" block name collisions between pages.
	pages := []string{"memories.html", "memory.html", "graph.html", "setup.html", "chat.html"}
	templates := make(map[string]*template.Template)
	for _, page := range pages {
		pageFile := filepath.Join(templateDir, "templates", page)
		t := template.Must(
			template.New("").Funcs(funcMap).ParseFiles(layoutFile, pageFile),
		)
		template.Must(t.ParseGlob(partialsGlob))
		templates[page] = t
	}

	// Partials-only template for htmx fragments (no layout).
	partials := template.Must(
		template.New("").Funcs(funcMap).ParseGlob(partialsGlob),
	)
	templates["memory_rows.html"] = partials

	return &Handler{svc: svc, chat: chatClient, templates: templates, username: username, password: password}
}

func (h *Handler) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(user), []byte(h.username)) != 1 ||
			subtle.ConstantTimeCompare([]byte(pass), []byte(h.password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="memex"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(h.basicAuth)

	r.Get("/", h.handleMemories)
	r.Get("/memories/{id}", h.handleMemoryDetail)
	r.Delete("/memories/{id}", h.handleDeleteMemory)
	r.Get("/search", h.handleSearch)
	r.Get("/graph", h.handleGraph)
	r.Get("/graph/data", h.handleGraphData)
	r.Get("/setup", h.handleSetup)
	r.Get("/chat", h.handleChat)
	r.Post("/chat/stream", h.handleChatStream)

	// Static files
	r.Handle("/static/*", http.StripPrefix("/ui/static/", http.FileServer(http.Dir(filepath.Join("web", "static")))))

	return r
}

func (h *Handler) handleMemories(w http.ResponseWriter, r *http.Request) {
	memories, err := h.svc.ListMemories(r.Context(), memory.Filter{Limit: 100})
	if err != nil {
		slog.Error("list memories failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.render(w, "memories.html", "layout", map[string]any{
		"Title":    "Memories",
		"Memories": memories,
	})
}

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		memories, err := h.svc.ListMemories(r.Context(), memory.Filter{Limit: 100})
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		h.render(w, "memory_rows.html", "memory_rows.html", map[string]any{"Memories": memories})
		return
	}

	results, err := h.svc.RecallMemory(r.Context(), query, 20)
	if err != nil {
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}

	// Convert ScoredMemory to Memory list for the template
	memories := make([]*memory.Memory, len(results))
	for i, r := range results {
		memories[i] = r.Memory
	}

	h.render(w, "memory_rows.html", "memory_rows.html", map[string]any{"Memories": memories})
}

func (h *Handler) handleMemoryDetail(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	mem, err := h.svc.GetMemory(r.Context(), id)
	if err != nil {
		http.Error(w, "memory not found", http.StatusNotFound)
		return
	}

	related, err := h.svc.GetRelatedMemories(r.Context(), id)
	if err != nil {
		slog.Warn("related memories failed", "memory_id", id, "err", err)
	}

	// Group by entity so the template can render "via X → [memories]".
	entityGroups := groupByEntity(related)

	h.render(w, "memory.html", "layout", map[string]any{
		"Title":        "Memory Detail",
		"Memory":       mem,
		"EntityGroups": entityGroups,
	})
}

func (h *Handler) handleDeleteMemory(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := h.svc.DeleteMemory(r.Context(), id); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("HX-Redirect", "/ui")
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleGraph(w http.ResponseWriter, r *http.Request) {
	h.render(w, "graph.html", "layout", map[string]any{
		"Title": "Knowledge Graph",
	})
}

func (h *Handler) handleSetup(w http.ResponseWriter, r *http.Request) {
	h.render(w, "setup.html", "layout", map[string]any{
		"Title": "Setup",
	})
}

func (h *Handler) handleGraphData(w http.ResponseWriter, r *http.Request) {
	entity := r.URL.Query().Get("entity")

	var graph *memory.Graph
	var err error
	if entity == "" {
		graph, err = h.svc.GetFullGraph(r.Context())
	} else {
		graph, err = h.svc.GetGraph(r.Context(), entity, 2)
	}
	if err != nil {
		http.Error(w, "graph query failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(graph); err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
	}
}

// groupByEntity re-organises a flat RelatedMemory list into per-entity groups.
func groupByEntity(related []memory.RelatedMemory) []memory.EntityGroup {
	index := map[uuid.UUID]*memory.EntityGroup{}
	var order []uuid.UUID
	for _, rm := range related {
		for _, ent := range rm.ViaEntities {
			if _, ok := index[ent.ID]; !ok {
				g := &memory.EntityGroup{Entity: ent}
				index[ent.ID] = g
				order = append(order, ent.ID)
			}
			index[ent.ID].Memories = append(index[ent.ID].Memories, rm.Memory)
		}
	}
	groups := make([]memory.EntityGroup, 0, len(order))
	for _, id := range order {
		groups = append(groups, *index[id])
	}
	return groups
}

func (h *Handler) handleChat(w http.ResponseWriter, r *http.Request) {
	h.render(w, "chat.html", "layout", map[string]any{
		"Title": "Chat",
		"Model": h.chat.Model(),
	})
}

// chatStreamRequest is the JSON body sent by the browser for each turn.
type chatStreamRequest struct {
	Message      string         `json:"message"`
	History      []chat.Message `json:"history"`
	PriorSources []chatSource   `json:"prior_sources"` // accumulated from previous turns
}

// chatSource is a single retrieved memory serialised into the sources SSE event.
type chatSource struct {
	ID      string  `json:"id"`
	Content string  `json:"content"`
	Score   float32 `json:"score"`
	Source  string  `json:"source"`
}

func (h *Handler) handleChatStream(w http.ResponseWriter, r *http.Request) {
	var req chatStreamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// Ask the model to synthesize the best search query given the full
	// conversation context, so follow-up questions resolve correctly.
	searchQuery, err := h.chat.SynthesizeQuery(r.Context(), req.History, req.Message)
	if err != nil {
		slog.Warn("query synthesis failed, falling back to raw message", "err", err)
		searchQuery = req.Message
	}

	// Retrieve relevant memories (skip if the model decided no lookup is needed).
	var recalled []memory.ScoredMemory
	if searchQuery != "" {
		slog.Info("chat recall", "query", searchQuery)
		recalled, err = h.svc.RecallMemory(r.Context(), searchQuery, 5)
		if err != nil {
			slog.Error("chat recall failed", "err", err)
			http.Error(w, "recall failed", http.StatusInternalServerError)
			return
		}
	}

	// Set SSE headers before writing anything.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	sseEvent := func(event, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data) //nolint:errcheck
		flusher.Flush()
	}

	// Merge newly retrieved memories with those accumulated from prior turns,
	// deduplicating by ID so the context grows richer without repetition.
	seen := make(map[string]bool, len(req.PriorSources)+len(recalled))
	merged := make([]chatSource, 0, len(req.PriorSources)+len(recalled))

	// Prior sources first (they carry lower scores but broaden context).
	for _, ps := range req.PriorSources {
		if !seen[ps.ID] {
			seen[ps.ID] = true
			merged = append(merged, ps)
		}
	}
	// Newly retrieved sources override nothing — just add unseen ones.
	newSources := make([]chatSource, 0, len(recalled))
	for _, sm := range recalled {
		id := sm.Memory.ID.String()
		src := chatSource{
			ID:      id,
			Content: sm.Memory.Content,
			Score:   sm.Score,
			Source:  sm.Memory.Source,
		}
		newSources = append(newSources, src)
		if !seen[id] {
			seen[id] = true
			merged = append(merged, src)
		}
	}

	// Build system prompt from the full merged context.
	var contextBuilder strings.Builder
	contextBuilder.WriteString("You are a helpful assistant with access to the user's personal knowledge base. Answer based on the context below. If the context is insufficient, say so honestly.\n\n## Knowledge base context\n\n")
	for i, src := range merged {
		fmt.Fprintf(&contextBuilder, "### Memory %d (source: %s)\n%s\n\n", i+1, src.Source, src.Content)
	}

	// Send only the newly retrieved sources to the UI (the frontend accumulates them).
	sources := newSources

	srcJSON, _ := json.Marshal(sources)
	sseEvent("sources", string(srcJSON))

	// Build the messages for Ollama: system prompt + history + new user message.
	messages := []chat.Message{
		{Role: "system", Content: contextBuilder.String()},
	}
	messages = append(messages, req.History...)
	messages = append(messages, chat.Message{Role: "user", Content: req.Message})

	// Stream the model response token by token.
	if err := h.chat.Stream(r.Context(), messages, func(token string) error {
		encoded, _ := json.Marshal(token)
		_, err := fmt.Fprintf(w, "event: token\ndata: %s\n\n", encoded)
		flusher.Flush()
		return err
	}); err != nil {
		slog.Error("chat stream error", "err", err)
		sseEvent("error", `"stream error"`)
		return
	}

	sseEvent("done", "{}")
}

// render executes the named block from the page's template set.
func (h *Handler) render(w http.ResponseWriter, page, block string, data any) {
	t, ok := h.templates[page]
	if !ok {
		slog.Error("template not found", "page", page)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, block, data); err != nil {
		slog.Error("template render failed", "page", page, "block", block, "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}
