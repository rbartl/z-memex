package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"gitlab.com/zollneritsch/memex/internal/memory"
)

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
	templates map[string]*template.Template
	username  string
	password  string
}

func NewHandler(svc MemoryService, templateDir, username, password string) *Handler {
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
	pages := []string{"memories.html", "memory.html", "graph.html", "setup.html"}
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

	return &Handler{svc: svc, templates: templates, username: username, password: password}
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
	json.NewEncoder(w).Encode(graph)
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
