package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// stubStore satisfies Store by embedding the interface: only the methods the
// backfill actually calls are implemented, and any other call panics loudly
// rather than passing silently.
type stubStore struct {
	Store
	dim     int
	pending []*Memory
	written map[uuid.UUID][]float32
}

func (s *stubStore) EmbeddingDimension(context.Context) (int, error) { return s.dim, nil }

func (s *stubStore) MemoriesMissingEmbedding(_ context.Context, limit int) ([]*Memory, error) {
	if len(s.pending) == 0 {
		return nil, nil
	}
	if limit > len(s.pending) {
		limit = len(s.pending)
	}
	batch := s.pending[:limit]
	s.pending = s.pending[limit:]
	return batch, nil
}

func (s *stubStore) SetEmbedding(_ context.Context, id uuid.UUID, e []float32) error {
	if s.written == nil {
		s.written = map[uuid.UUID][]float32{}
	}
	s.written[id] = e
	return nil
}

// stubEmbedder returns a vector of a fixed width.
type stubEmbedder struct{ dim int }

func (e stubEmbedder) Embed(context.Context, string) ([]float32, error) {
	return make([]float32, e.dim), nil
}

func newBackfillService(store Store, dim int) *Service {
	s := &Service{store: store, embed: stubEmbedder{dim: dim}}
	s.queue = newExtractionQueue(ExtractionConfig{}, func(context.Context, extractionJob) {})
	return s
}

func TestBackfillEmbedsEveryMemoryMissingOne(t *testing.T) {
	store := &stubStore{dim: 1024}
	for i := 0; i < 250; i++ { // more than one batch
		store.pending = append(store.pending, &Memory{ID: uuid.New(), Content: "a fact"})
	}
	svc := newBackfillService(store, 1024)
	t.Cleanup(func() { _ = svc.Shutdown(context.Background()) })

	n, err := svc.BackfillEmbeddings(context.Background())
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 250 || len(store.written) != 250 {
		t.Errorf("embedded %d (wrote %d), want 250", n, len(store.written))
	}
}

// A model of the wrong width cannot be recovered from at runtime: every insert
// would fail the dimension check, so the server must refuse to start.
func TestBackfillRejectsWrongDimension(t *testing.T) {
	store := &stubStore{dim: 1024, pending: []*Memory{{ID: uuid.New(), Content: "a fact"}}}
	svc := newBackfillService(store, 768)
	t.Cleanup(func() { _ = svc.Shutdown(context.Background()) })

	_, err := svc.BackfillEmbeddings(context.Background())
	if err == nil {
		t.Fatal("a 768-dimension model was accepted against a 1024 column")
	}
	for _, want := range []string{"768", "1024", "EMBED_MODEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if len(store.written) != 0 {
		t.Error("wrote embeddings despite the mismatch")
	}
}

func TestBackfillIsANoOpWhenNothingIsMissing(t *testing.T) {
	store := &stubStore{dim: 1024}
	svc := newBackfillService(store, 1024)
	t.Cleanup(func() { _ = svc.Shutdown(context.Background()) })

	n, err := svc.BackfillEmbeddings(context.Background())
	if err != nil || n != 0 {
		t.Errorf("backfill on a full corpus = (%d, %v), want (0, nil)", n, err)
	}
}
