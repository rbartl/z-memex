package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"gitlab.com/zollneritsch/memex/internal/memory"
)

func entityServer(t *testing.T, svc *fakeService) (http.Handler, *http.Cookie) {
	t.Helper()
	h := NewHandler(svc, nil, ".", testUser, testPass)
	srv := chi.NewRouter()
	srv.Mount("/ui", h.Router())
	return srv, login(t, srv, testUser, testPass)
}

func get(t *testing.T, srv http.Handler, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	return do(t, srv, req)
}

func TestEntityRowsShowCountsAndAliases(t *testing.T) {
	svc := &fakeService{entities: []memory.EntityStat{
		{ID: uuid.New(), Name: "Maximilian Zollneritsch", Type: "person", Relations: 40, Memories: 12, Aliases: []string{"max"}},
	}}
	srv, cookie := entityServer(t, svc)

	body := get(t, srv, cookie, "/ui/entities/rows").Body.String()
	for _, want := range []string{"Maximilian Zollneritsch", "person", "40", "12", "max", "/merge"} {
		if !strings.Contains(body, want) {
			t.Errorf("row body missing %q: %s", want, body)
		}
	}
}

// The entity being merged away must never be offered as the thing to keep.
func TestMergeCandidatesExcludeTheSourceEntity(t *testing.T) {
	src := memory.EntityStat{ID: uuid.New(), Name: "Max", Type: "person", Relations: 91}
	other := memory.EntityStat{ID: uuid.New(), Name: "Maximilian Zollneritsch", Type: "person", Relations: 40}
	svc := &fakeService{entities: []memory.EntityStat{src, other}}
	srv, cookie := entityServer(t, svc)

	body := get(t, srv, cookie, "/ui/entities/"+src.ID.String()+"/merge/candidates").Body.String()
	// The source id appears in every form's action URL, so the check is on the
	// winner field specifically — that is what a submit would merge into.
	if strings.Contains(body, `name="winner" value="`+src.ID.String()+`"`) {
		t.Error("source entity offered as its own merge target")
	}
	if !strings.Contains(body, `name="winner" value="`+other.ID.String()+`"`) {
		t.Error("other entity missing from candidates")
	}
}

func TestMergeFormSeedsSearchWithEntityName(t *testing.T) {
	src := memory.EntityStat{ID: uuid.New(), Name: "Max", Type: "person"}
	svc := &fakeService{entities: []memory.EntityStat{src}}
	srv, cookie := entityServer(t, svc)

	body := get(t, srv, cookie, "/ui/entities/"+src.ID.String()+"/merge").Body.String()
	if !strings.Contains(body, `value="Max"`) {
		t.Errorf("merge form does not seed the search with the entity name: %s", body)
	}
}

func TestMergePostCallsServiceAndRedirects(t *testing.T) {
	src := memory.EntityStat{ID: uuid.New(), Name: "Max", Type: "person"}
	winner := memory.EntityStat{ID: uuid.New(), Name: "Maximilian Zollneritsch", Type: "person"}
	svc := &fakeService{entities: []memory.EntityStat{src, winner}}
	srv, cookie := entityServer(t, svc)

	form := url.Values{"winner": {winner.ID.String()}}
	req := httptest.NewRequest(http.MethodPost, "/ui/entities/"+src.ID.String()+"/merge", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := do(t, srv, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if len(svc.merged) != 1 || svc.merged[0] != [2]uuid.UUID{src.ID, winner.ID} {
		t.Errorf("merged = %v, want one (loser, winner) pair", svc.merged)
	}
}

func TestMergePostRejectsBadTarget(t *testing.T) {
	src := memory.EntityStat{ID: uuid.New(), Name: "Max"}
	svc := &fakeService{entities: []memory.EntityStat{src}}
	srv, cookie := entityServer(t, svc)

	form := url.Values{"winner": {"not-a-uuid"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/entities/"+src.ID.String()+"/merge", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)

	if got := do(t, srv, req).Code; got != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", got, http.StatusBadRequest)
	}
	if len(svc.merged) != 0 {
		t.Error("merge ran despite an invalid target")
	}
}

func TestEntitiesRequireSession(t *testing.T) {
	srv, _ := entityServer(t, &fakeService{})
	for _, path := range []string{"/ui/entities", "/ui/entities/rows"} {
		if got := do(t, srv, httptest.NewRequest(http.MethodGet, path, nil)).Code; got != http.StatusSeeOther {
			t.Errorf("%s: status = %d, want a redirect to login", path, got)
		}
	}
}
