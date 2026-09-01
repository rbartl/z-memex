package web

import (
	"context"

	"github.com/google/uuid"

	"gitlab.com/zollneritsch/memex/internal/memory"
)

// fakeService satisfies MemoryService for the handler tests. Each test fills in
// only the fields the endpoint it exercises reads; everything else stays zero.
type fakeService struct {
	stats    memory.ExtractionStats
	entities []memory.EntityStat
	merged   [][2]uuid.UUID
	mergeErr error
}

func (f *fakeService) ExtractionQueueStats() memory.ExtractionStats { return f.stats }

func (f *fakeService) ListEntities(_ context.Context, query string, _ int) ([]memory.EntityStat, error) {
	if query == "" {
		return f.entities, nil
	}
	var out []memory.EntityStat
	for _, e := range f.entities {
		if memory.NormalizeName(e.Name) == memory.NormalizeName(query) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeService) MergeEntities(_ context.Context, loser, winner uuid.UUID) error {
	if f.mergeErr != nil {
		return f.mergeErr
	}
	f.merged = append(f.merged, [2]uuid.UUID{loser, winner})
	return nil
}

func (f *fakeService) ListMemories(context.Context, memory.Filter) ([]*memory.Memory, error) {
	return nil, nil
}
func (f *fakeService) GetMemory(context.Context, uuid.UUID) (*memory.Memory, error) { return nil, nil }
func (f *fakeService) RecallMemory(context.Context, string, int) ([]memory.ScoredMemory, error) {
	return nil, nil
}
func (f *fakeService) DeleteMemory(context.Context, uuid.UUID) error { return nil }
func (f *fakeService) GetGraph(context.Context, string, int) (*memory.Graph, error) {
	return nil, nil
}
func (f *fakeService) GetFullGraph(context.Context, bool) (*memory.Graph, error) { return nil, nil }
func (f *fakeService) GetMemoriesByEntity(context.Context, uuid.UUID) ([]*memory.Memory, error) {
	return nil, nil
}
func (f *fakeService) GetRelatedMemories(context.Context, uuid.UUID) ([]memory.RelatedMemory, error) {
	return nil, nil
}
