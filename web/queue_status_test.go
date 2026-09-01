package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gitlab.com/zollneritsch/memex/internal/memory"
)

func queueServer(t *testing.T, stats memory.ExtractionStats) (http.Handler, *http.Cookie) {
	t.Helper()
	h := NewHandler(&fakeService{stats: stats}, nil, ".", testUser, testPass)
	srv := chi.NewRouter()
	srv.Mount("/ui", h.Router())
	return srv, login(t, srv, testUser, testPass)
}

func TestQueueStatusShowsBacklog(t *testing.T) {
	srv, cookie := queueServer(t, memory.ExtractionStats{Running: 1, Pending: 7, Workers: 1})

	req := httptest.NewRequest(http.MethodGet, "/ui/queue", nil)
	req.AddCookie(cookie)
	rec := do(t, srv, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "extracting 8") {
		t.Errorf("body = %q, want the running + pending total", body)
	}
	if !strings.Contains(body, "queue-badge-busy") {
		t.Errorf("body = %q, want the busy styling while work is outstanding", body)
	}
}

func TestQueueStatusShowsIdle(t *testing.T) {
	srv, cookie := queueServer(t, memory.ExtractionStats{Workers: 1})

	req := httptest.NewRequest(http.MethodGet, "/ui/queue", nil)
	req.AddCookie(cookie)
	body := do(t, srv, req).Body.String()

	if !strings.Contains(body, "idle") {
		t.Errorf("body = %q, want an idle badge", body)
	}
	if strings.Contains(body, "queue-badge-busy") {
		t.Errorf("body = %q, want no busy styling on an empty queue", body)
	}
}

// The badge is only useful if the layout actually polls for it.
func TestQueueStatusIsPolledFromNav(t *testing.T) {
	srv, cookie := queueServer(t, memory.ExtractionStats{Workers: 1})

	req := httptest.NewRequest(http.MethodGet, "/ui/setup", nil)
	req.AddCookie(cookie)
	body := do(t, srv, req).Body.String()

	if !strings.Contains(body, `hx-get="/ui/queue"`) {
		t.Error("layout does not fetch the queue badge")
	}
}

func TestQueueStatusRequiresSession(t *testing.T) {
	srv, _ := queueServer(t, memory.ExtractionStats{})

	rec := do(t, srv, httptest.NewRequest(http.MethodGet, "/ui/queue", nil))
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want a redirect to login", rec.Code)
	}
}
