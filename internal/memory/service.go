package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

type Extractor interface {
	Extract(ctx context.Context, content string) (*ExtractionResult, error)
	ExtractCrossRelations(ctx context.Context, contentA, contentB string) ([]ExtractedRelation, error)
	MergeMemories(ctx context.Context, existing, incoming string) (string, error)
}

// Store is the single storage interface — handles both relational data and vector search.
type Store interface {
	SaveMemory(ctx context.Context, m *Memory, embedding []float32) error
	FindByContentHash(ctx context.Context, hash string) (*Memory, error)
	GetMemory(ctx context.Context, id uuid.UUID) (*Memory, error)
	ListMemories(ctx context.Context, filter Filter) ([]*Memory, error)
	DeleteMemory(ctx context.Context, id uuid.UUID) error
	SearchSimilar(ctx context.Context, embedding []float32, topK int, minScore float32) ([]ScoredMemory, error)
	SearchHybrid(ctx context.Context, query string, embedding []float32, topK int, cfg ScoringConfig) ([]ScoredMemory, error)
	RecordAccess(ctx context.Context, ids []uuid.UUID) error
	UpsertEntity(ctx context.Context, name, entityType string) (uuid.UUID, error)
	UpsertRelation(ctx context.Context, fromID, toID uuid.UUID, relationship string, memoryID uuid.UUID, confidence float64) error
	LinkMemoryEntity(ctx context.Context, memoryID, entityID uuid.UUID) error
	GetGraphForMemories(ctx context.Context, memoryIDs []uuid.UUID) (*Graph, error)
	GetGraph(ctx context.Context, entityName string, depth int) (*Graph, error)
	GetFullGraph(ctx context.Context) (*Graph, error)
	UpdateSummary(ctx context.Context, id uuid.UUID, summary string) error
	GetMemoriesByEntity(ctx context.Context, entityID uuid.UUID) ([]*Memory, error)
	GetRelatedMemories(ctx context.Context, memoryID uuid.UUID) ([]RelatedMemory, error)
}

type Service struct {
	store   Store
	embed   Embedder
	extract Extractor
}

func NewService(store Store, embed Embedder, extract Extractor) *Service {
	return &Service{store: store, embed: embed, extract: extract}
}

const (
	mergeThresholdLow  = 0.85 // below this: insert as new
	mergeThresholdHigh = 0.99 // above this: update in place (near-identical)
)

// hashContent returns a SHA-256 hex digest of normalized (trimmed + lowered) content.
func hashContent(content string) string {
	normalized := strings.ToLower(strings.TrimSpace(content))
	h := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(h[:])
}

// StoreMemory embeds + saves with three-tier dedup:
//   - Exact hash match → skip (no embedding, no save)
//   - Cosine 0.85–0.99 → LLM-assisted merge
//   - Cosine ≥ 0.99 → update in place
//   - Below 0.85 → insert as new memory
//
// importance should be 1-5 (3=normal). Pass 0 to use the default (3).
func (s *Service) StoreMemory(ctx context.Context, content, source, sessionID string, tags []string, importance int) (*StoreResult, error) {
	if importance < 1 || importance > 5 {
		importance = 3
	}

	// Tags are free text chosen by whichever client is storing, so the same tag
	// arrives in several spellings. Fold them here rather than at each call site.
	tags = NormalizeTags(tags)

	hash := hashContent(content)

	// Tier 1: exact content hash match — skip entirely.
	existing, err := s.store.FindByContentHash(ctx, hash)
	if err == nil && existing != nil {
		slog.Info("exact duplicate skipped", "id", existing.ID)
		return &StoreResult{
			Memory: existing,
			Action: "skipped",
			Score:  1.0,
		}, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("content hash lookup failed, proceeding", "err", err)
	}

	// Embed the content for semantic comparison.
	vec, err := s.embed.Embed(ctx, content)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}

	now := time.Now()

	// Tier 2/3: check cosine similarity.
	similar, err := s.store.SearchSimilar(ctx, vec, 1, 0)
	if err != nil {
		slog.Warn("duplicate check failed, proceeding with insert", "err", err)
	} else if len(similar) > 0 && similar[0].Score >= mergeThresholdLow {
		m := similar[0].Memory
		score := similar[0].Score

		if score >= mergeThresholdHigh {
			// Tier 2a: near-identical — update in place.
			m.Content = content
			m.ContentHash = hash
			if source != "" {
				m.Source = source
			}
			if sessionID != "" {
				m.SessionID = sessionID
			}
			if len(tags) > 0 {
				m.Tags = tags
			}
			m.Importance = importance
			m.UpdatedAt = now

			if err := s.store.SaveMemory(ctx, m, vec); err != nil {
				return nil, fmt.Errorf("update existing memory: %w", err)
			}
			s.asyncExtractAndConnect(m, vec)
			return &StoreResult{Memory: m, Action: "updated", Score: score}, nil
		}

		// Tier 2b: merge zone — LLM merges old + new content.
		merged, err := s.extract.MergeMemories(ctx, m.Content, content)
		if err != nil {
			slog.Warn("merge failed, falling back to update", "err", err)
			merged = content
		}

		// Re-embed the merged content.
		mergedVec, err := s.embed.Embed(ctx, merged)
		if err != nil {
			return nil, fmt.Errorf("embed merged: %w", err)
		}

		m.Content = merged
		m.ContentHash = hashContent(merged)
		if source != "" {
			m.Source = source
		}
		if sessionID != "" {
			m.SessionID = sessionID
		}
		if len(tags) > 0 {
			m.Tags = tags
		}
		m.Importance = importance
		m.UpdatedAt = now

		if err := s.store.SaveMemory(ctx, m, mergedVec); err != nil {
			return nil, fmt.Errorf("save merged memory: %w", err)
		}
		s.asyncExtractAndConnect(m, mergedVec)
		return &StoreResult{Memory: m, Action: "merged", Score: score}, nil
	}

	// Tier 4: distinct — insert as new memory.
	m := &Memory{
		ID:          uuid.New(),
		Content:     content,
		ContentHash: hash,
		Source:      source,
		SessionID:   sessionID,
		Tags:        tags,
		Importance:  importance,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if err := s.store.SaveMemory(ctx, m, vec); err != nil {
		return nil, fmt.Errorf("save memory: %w", err)
	}
	s.asyncExtractAndConnect(m, vec)
	return &StoreResult{Memory: m, Action: "created"}, nil
}

// asyncExtractAndConnect runs entity extraction and cross-connection in the background.
func (s *Service) asyncExtractAndConnect(m *Memory, vec []float32) {
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := s.extractAndGraph(bgCtx, m); err != nil {
			slog.Warn("relation extraction failed", "memory_id", m.ID, "err", err)
		}
		s.crossConnect(bgCtx, m, vec)
	}()
}

func (s *Service) extractAndGraph(ctx context.Context, m *Memory) error {
	result, err := s.extract.Extract(ctx, m.Content)
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}

	if result.Summary != "" {
		if err := s.store.UpdateSummary(ctx, m.ID, result.Summary); err != nil {
			slog.Warn("failed to save summary", "memory_id", m.ID, "err", err)
		}
	}

	for _, r := range result.Relations {
		fromType, toType := canonicalPair(r)
		fromID, err := s.store.UpsertEntity(ctx, r.FromEntity, fromType)
		if err != nil {
			slog.Warn("upsert entity failed", "entity", r.FromEntity, "err", err)
			continue
		}
		toID, err := s.store.UpsertEntity(ctx, r.ToEntity, toType)
		if err != nil {
			slog.Warn("upsert entity failed", "entity", r.ToEntity, "err", err)
			continue
		}
		if err := s.store.UpsertRelation(ctx, fromID, toID, r.Relationship, m.ID, r.Confidence); err != nil {
			slog.Warn("upsert relation failed", "from", r.FromEntity, "to", r.ToEntity, "err", err)
		}
		if err := s.store.LinkMemoryEntity(ctx, m.ID, fromID); err != nil {
			slog.Warn("link memory entity failed", "memory_id", m.ID, "entity_id", fromID, "err", err)
		}
		if err := s.store.LinkMemoryEntity(ctx, m.ID, toID); err != nil {
			slog.Warn("link memory entity failed", "memory_id", m.ID, "entity_id", toID, "err", err)
		}
	}
	slog.Info("extracted relations", "memory_id", m.ID, "count", len(result.Relations))
	return nil
}

// crossConnect finds semantically similar memories and extracts cross-memory relationships.
func (s *Service) crossConnect(ctx context.Context, m *Memory, vec []float32) {
	neighbors, err := s.store.SearchSimilar(ctx, vec, 6, 0.5)
	if err != nil {
		slog.Warn("cross-connect search failed", "memory_id", m.ID, "err", err)
		return
	}

	var connected int
	for _, n := range neighbors {
		if n.Memory.ID == m.ID {
			continue
		}

		relations, err := s.extract.ExtractCrossRelations(ctx, m.Content, n.Memory.Content)
		if err != nil {
			slog.Warn("cross-relation extraction failed", "memory_id", m.ID, "neighbor_id", n.Memory.ID, "err", err)
			continue
		}

		for _, r := range relations {
			fromType, toType := canonicalPair(r)
			fromID, err := s.store.UpsertEntity(ctx, r.FromEntity, fromType)
			if err != nil {
				slog.Warn("upsert entity failed", "entity", r.FromEntity, "err", err)
				continue
			}
			toID, err := s.store.UpsertEntity(ctx, r.ToEntity, toType)
			if err != nil {
				slog.Warn("upsert entity failed", "entity", r.ToEntity, "err", err)
				continue
			}
			if err := s.store.UpsertRelation(ctx, fromID, toID, r.Relationship, m.ID, r.Confidence); err != nil {
				slog.Warn("upsert relation failed", "from", r.FromEntity, "to", r.ToEntity, "err", err)
			}
			// Link both memories to the entities
			if err := s.store.LinkMemoryEntity(ctx, m.ID, fromID); err != nil {
				slog.Warn("link memory entity failed", "memory_id", m.ID, "entity_id", fromID, "err", err)
			}
			if err := s.store.LinkMemoryEntity(ctx, m.ID, toID); err != nil {
				slog.Warn("link memory entity failed", "memory_id", m.ID, "entity_id", toID, "err", err)
			}
			if err := s.store.LinkMemoryEntity(ctx, n.Memory.ID, fromID); err != nil {
				slog.Warn("link memory entity failed", "memory_id", n.Memory.ID, "entity_id", fromID, "err", err)
			}
			if err := s.store.LinkMemoryEntity(ctx, n.Memory.ID, toID); err != nil {
				slog.Warn("link memory entity failed", "memory_id", n.Memory.ID, "entity_id", toID, "err", err)
			}
			connected++
		}
	}

	if connected > 0 {
		slog.Info("cross-connected memory", "memory_id", m.ID, "new_relations", connected)
	}
}

// RecallMemory does hybrid search using RRF over vector + full-text candidates,
// boosted by importance, recency, and access frequency.
func (s *Service) RecallMemory(ctx context.Context, query string, topK int) ([]ScoredMemory, error) {
	switch {
	case topK <= 0:
		topK = 5
	case topK > maxRecallTopK:
		topK = maxRecallTopK
	}

	vec, err := s.embed.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	results, err := s.store.SearchHybrid(ctx, query, vec, topK, DefaultScoringConfig)
	if err != nil {
		return nil, err
	}

	// Record access asynchronously for returned memories.
	if len(results) > 0 {
		ids := make([]uuid.UUID, len(results))
		for i, r := range results {
			ids[i] = r.Memory.ID
		}
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.store.RecordAccess(bgCtx, ids); err != nil {
				slog.Warn("failed to record access", "err", err)
			}
		}()
	}

	return results, nil
}

// RecallMemoryWithGraph performs semantic recall and enriches results with
// the subgraph of entities and relations connected to the returned memories.
func (s *Service) RecallMemoryWithGraph(ctx context.Context, query string, topK int) (*RecallResult, error) {
	memories, err := s.RecallMemory(ctx, query, topK)
	if err != nil {
		return nil, err
	}

	result := &RecallResult{Memories: memories}

	if len(memories) > 0 {
		ids := make([]uuid.UUID, len(memories))
		for i, m := range memories {
			ids[i] = m.Memory.ID
		}
		graph, err := s.store.GetGraphForMemories(ctx, ids)
		if err != nil {
			slog.Warn("failed to fetch graph for recall results", "err", err)
		} else {
			result.Graph = graph
		}
	}

	return result, nil
}

// GetContext returns a formatted context block ready for AI system prompts.
func (s *Service) GetContext(ctx context.Context, query string, topK int) (string, error) {
	memories, err := s.RecallMemory(ctx, query, topK)
	if err != nil {
		return "", err
	}
	if len(memories) == 0 {
		return "", nil
	}

	var sb strings.Builder
	sb.WriteString("## Relevant context from your personal knowledge base\n\n")
	for i, sm := range memories {
		fmt.Fprintf(&sb, "### [%d] (score: %.2f, source: %s)\n%s\n\n",
			i+1, sm.Score, sm.Memory.Source, sm.Memory.Content)
	}
	return sb.String(), nil
}

// Result-set bounds. These values arrive from MCP tool arguments, so they are
// clamped rather than trusted: a negative LIMIT makes Postgres error outright,
// and an unbounded one would pull the whole table into a tool response.
const (
	defaultListLimit = 50
	maxListLimit     = 500
	maxRecallTopK    = 100
)

func (s *Service) ListMemories(ctx context.Context, filter Filter) ([]*Memory, error) {
	switch {
	case filter.Limit <= 0:
		filter.Limit = defaultListLimit
	case filter.Limit > maxListLimit:
		filter.Limit = maxListLimit
	}
	return s.store.ListMemories(ctx, filter)
}

func (s *Service) DeleteMemory(ctx context.Context, id uuid.UUID) error {
	return s.store.DeleteMemory(ctx, id)
}

func (s *Service) GetMemory(ctx context.Context, id uuid.UUID) (*Memory, error) {
	return s.store.GetMemory(ctx, id)
}

func (s *Service) GetMemoriesByEntity(ctx context.Context, entityID uuid.UUID) ([]*Memory, error) {
	return s.store.GetMemoriesByEntity(ctx, entityID)
}

func (s *Service) GetRelatedMemories(ctx context.Context, memoryID uuid.UUID) ([]RelatedMemory, error) {
	return s.store.GetRelatedMemories(ctx, memoryID)
}

// validEntityType checks structural sanity only — the LLM owns the taxonomy.
// Rejects empty, "other", overly long strings, or types containing spaces/special chars.
// canonicalPair folds an extracted relation's two entity types onto the closed
// vocabulary. Types the vocabulary cannot place become FallbackEntityType rather
// than dropping the relation: an odd type label is a smaller loss than a missing
// edge, and the warning makes new drift visible.
func canonicalPair(r ExtractedRelation) (fromType, toType string) {
	fromType, okFrom := CanonicalEntityType(r.FromType)
	toType, okTo := CanonicalEntityType(r.ToType)
	if !okFrom || !okTo {
		slog.Warn("entity type outside vocabulary, using fallback",
			"from", r.FromEntity, "from_type", r.FromType, "from_mapped", fromType,
			"to", r.ToEntity, "to_type", r.ToType, "to_mapped", toType)
	}
	return fromType, toType
}

func (s *Service) GetGraph(ctx context.Context, entity string, depth int) (*Graph, error) {
	if depth == 0 {
		depth = 2
	}
	return s.store.GetGraph(ctx, entity, depth)
}

func (s *Service) GetFullGraph(ctx context.Context) (*Graph, error) {
	return s.store.GetFullGraph(ctx)
}
