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
	UpsertRelation(ctx context.Context, fromID, toID uuid.UUID, relationship, rawRelationship string, memoryID uuid.UUID, confidence float64) error
	LinkMemoryEntity(ctx context.Context, memoryID, entityID uuid.UUID) error
	GetGraphForMemories(ctx context.Context, memoryIDs []uuid.UUID) (*Graph, error)
	GetGraph(ctx context.Context, entityName string, depth int) (*Graph, error)
	GetFullGraph(ctx context.Context, includeWeak bool) (*Graph, error)
	UpdateSummary(ctx context.Context, id uuid.UUID, summary string) error
	GetMemoriesByEntity(ctx context.Context, entityID uuid.UUID) ([]*Memory, error)
	GetRelatedMemories(ctx context.Context, memoryID uuid.UUID) ([]RelatedMemory, error)
	ListEntities(ctx context.Context, query string, limit int) ([]EntityStat, error)
	MergeEntities(ctx context.Context, loserID, winnerID uuid.UUID) error
	EmbeddingDimension(ctx context.Context) (int, error)
	MemoriesMissingEmbedding(ctx context.Context, limit int) ([]*Memory, error)
	SetEmbedding(ctx context.Context, id uuid.UUID, embedding []float32) error
}

type Service struct {
	store   Store
	embed   Embedder
	extract Extractor
	queue   *extractionQueue
}

// NewService builds a service whose background extraction runs with the default
// bounds: one job at a time.
func NewService(store Store, embed Embedder, extract Extractor) *Service {
	return NewServiceWithConfig(store, embed, extract, ExtractionConfig{})
}

// NewServiceWithConfig is NewService with explicit bounds on the background
// extraction queue. Unset fields fall back to the defaults.
func NewServiceWithConfig(store Store, embed Embedder, extract Extractor, cfg ExtractionConfig) *Service {
	s := &Service{store: store, embed: embed, extract: extract}
	cfg = cfg.withDefaults()
	s.queue = newExtractionQueue(cfg, s.runExtraction)
	slog.Info("extraction queue started", "workers", cfg.Workers)
	return s
}

// embeddingBackfillBatch bounds how many memories are held in memory at once
// while re-embedding. The work is one model call per memory either way.
const embeddingBackfillBatch = 100

// BackfillEmbeddings embeds every memory that has no vector, and reports how
// many it wrote. Rows arrive here after migration 012 discards the corpus's
// embeddings, and after any store whose embedding call failed.
//
// It first checks the configured model against the column's declared width. A
// mismatch cannot be recovered from at runtime -- every insert would fail on
// the dimension check -- so the caller is expected to treat the error as fatal
// rather than serve a knowledge base that cannot be searched.
func (s *Service) BackfillEmbeddings(ctx context.Context) (int, error) {
	// Look for work before touching the model. A corpus that is fully embedded
	// is the normal case, and on that path the server must not need the
	// embedding backend to be reachable at all — it contacts Ollama lazily
	// otherwise, and a boot that fails without it would take the whole service
	// down whenever the model host is slow to start.
	batch, err := s.store.MemoriesMissingEmbedding(ctx, embeddingBackfillBatch)
	if err != nil {
		return 0, fmt.Errorf("list memories missing embeddings: %w", err)
	}
	if len(batch) == 0 {
		return 0, nil
	}

	// There is work, so the model's width has to agree with the column's. A
	// mismatch cannot be recovered from at runtime — every insert would fail
	// the dimension check — so the caller is expected to treat this as fatal
	// rather than serve a knowledge base that cannot be searched.
	want, err := s.store.EmbeddingDimension(ctx)
	if err != nil {
		return 0, fmt.Errorf("read embedding dimension: %w", err)
	}
	probe, err := s.embed.Embed(ctx, "dimension probe")
	if err != nil {
		return 0, fmt.Errorf("probe embedding model: %w", err)
	}
	if len(probe) != want {
		return 0, fmt.Errorf("embedding model returns %d dimensions but the schema declares %d: "+
			"set EMBED_MODEL to the model this schema was migrated for, or migrate the column", len(probe), want)
	}

	var done int
	for {
		for _, m := range batch {
			vec, err := s.embed.Embed(ctx, m.Content)
			if err != nil {
				return done, fmt.Errorf("embed memory %s: %w", m.ID, err)
			}
			if err := s.store.SetEmbedding(ctx, m.ID, vec); err != nil {
				return done, fmt.Errorf("store embedding %s: %w", m.ID, err)
			}
			done++
			if done%25 == 0 {
				slog.Info("backfilling embeddings", "done", done)
			}
		}

		batch, err = s.store.MemoriesMissingEmbedding(ctx, embeddingBackfillBatch)
		if err != nil {
			return done, fmt.Errorf("list memories missing embeddings: %w", err)
		}
		if len(batch) == 0 {
			return done, nil
		}
	}
}

// ExtractionQueueStats reports how much background extraction work is
// outstanding. The UI polls this so a long backlog is visible rather than
// looking like nothing is happening.
func (s *Service) ExtractionQueueStats() ExtractionStats {
	return s.queue.stats()
}

// Shutdown stops accepting extraction jobs and waits for the outstanding ones,
// giving up when ctx expires.
func (s *Service) Shutdown(ctx context.Context) error {
	return s.queue.stop(ctx)
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

// asyncExtractAndConnect hands entity extraction and cross-connection to the
// background queue, which runs them a bounded number at a time. It never blocks
// the store that produced the memory.
func (s *Service) asyncExtractAndConnect(m *Memory, vec []float32) {
	s.queue.enqueue(extractionJob{memory: m, vec: vec})
}

// runExtraction is the unit of work a queue worker executes.
func (s *Service) runExtraction(ctx context.Context, job extractionJob) {
	if err := s.extractAndGraph(ctx, job.memory); err != nil {
		slog.Warn("relation extraction failed", "memory_id", job.memory.ID, "err", err)
	}
	s.crossConnect(ctx, job.memory, job.vec)
}

// applyRelation folds one extracted relation onto the closed vocabularies and
// writes it, linking every memory in linkTo to both of its endpoints. It reports
// whether an edge was written.
//
// Both extraction paths funnel through here so the folding rules — entity type,
// predicate, direction — are applied in exactly one place.
func (s *Service) applyRelation(ctx context.Context, r ExtractedRelation, memoryID uuid.UUID, linkTo ...uuid.UUID) bool {
	fromType, toType := canonicalPair(r)

	label, reversed, ok := CanonicalRelationship(r.Relationship)
	if !ok {
		slog.Warn("relationship outside vocabulary, using fallback",
			"relationship", r.Relationship, "mapped", label,
			"from", r.FromEntity, "to", r.ToEntity)
	}

	fromName, toName := r.FromEntity, r.ToEntity
	if reversed {
		// The model stated the relation against the predicate's canonical
		// direction ("Acme employs Max" for "Max works-at Acme"), so
		// the endpoints swap along with the label.
		fromName, toName = toName, fromName
		fromType, toType = toType, fromType
	}

	fromID, err := s.store.UpsertEntity(ctx, fromName, fromType)
	if err != nil {
		slog.Warn("upsert entity failed", "entity", fromName, "err", err)
		return false
	}
	toID, err := s.store.UpsertEntity(ctx, toName, toType)
	if err != nil {
		slog.Warn("upsert entity failed", "entity", toName, "err", err)
		return false
	}

	if fromID == toID {
		// Both names resolved to one node. An edge from a thing to itself
		// carries no information, and the schema now refuses it outright.
		slog.Debug("skipping self-relation", "entity", fromName, "relationship", label)
		return false
	}

	if IsSymmetricRelationship(label) && fromID.String() > toID.String() {
		// Endpoint order means nothing for a symmetric predicate, so it is fixed
		// here — otherwise "A same-as B" and "B same-as A" are two edges.
		// Migration 010 orders the same way: Postgres compares uuid by the bytes
		// its hex spells, which is the order String() gives.
		fromID, toID = toID, fromID
	}

	if err := s.store.UpsertRelation(ctx, fromID, toID, label, r.Relationship, memoryID, r.Confidence); err != nil {
		slog.Warn("upsert relation failed", "from", fromName, "to", toName, "err", err)
		return false
	}

	for _, id := range linkTo {
		if err := s.store.LinkMemoryEntity(ctx, id, fromID); err != nil {
			slog.Warn("link memory entity failed", "memory_id", id, "entity_id", fromID, "err", err)
		}
		if err := s.store.LinkMemoryEntity(ctx, id, toID); err != nil {
			slog.Warn("link memory entity failed", "memory_id", id, "entity_id", toID, "err", err)
		}
	}
	return true
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
		s.applyRelation(ctx, r, m.ID, m.ID)
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
			// A cross-relation belongs to both memories, so both are linked to
			// the entities it names.
			if s.applyRelation(ctx, r, m.ID, m.ID, n.Memory.ID) {
				connected++
			}
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

// ListEntities returns graph nodes with their edge and memory counts, filtered
// by a name substring when query is non-empty.
func (s *Service) ListEntities(ctx context.Context, query string, limit int) ([]EntityStat, error) {
	switch {
	case limit <= 0:
		limit = defaultListLimit
	case limit > maxListLimit:
		limit = maxListLimit
	}
	return s.store.ListEntities(ctx, query, limit)
}

// MergeEntities folds one entity into another, recording the loser's name as an
// alias so later extractions resolve to the survivor. Aliases are the half of
// identity that normalization cannot reach: "Max" and "Maximilian Zollneritsch"
// share no characters to fold, so a person has to say they are one node.
//
// The merge cannot be undone, so it is only ever reached from an explicit
// action in the UI — never from extraction.
func (s *Service) MergeEntities(ctx context.Context, loserID, winnerID uuid.UUID) error {
	if loserID == winnerID {
		return fmt.Errorf("cannot merge an entity into itself")
	}
	if err := s.store.MergeEntities(ctx, loserID, winnerID); err != nil {
		return err
	}
	slog.Info("merged entities", "loser", loserID, "winner", winnerID)
	return nil
}

func (s *Service) GetGraph(ctx context.Context, entity string, depth int) (*Graph, error) {
	if depth == 0 {
		depth = 2
	}
	return s.store.GetGraph(ctx, entity, depth)
}

// GetFullGraph returns the whole graph. includeWeak brings back the entities
// that have not earned a place in it (see storage.weakEntityCondition).
func (s *Service) GetFullGraph(ctx context.Context, includeWeak bool) (*Graph, error) {
	return s.store.GetFullGraph(ctx, includeWeak)
}
