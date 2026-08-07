package storage

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"gitlab.com/zollneritsch/memex/internal/memory"
)

type Postgres struct {
	pool *pgxpool.Pool
}

func NewPostgres(ctx context.Context, url string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.ParseConfig: %w", err)
	}

	// An HNSW index scan returns at most hnsw.ef_search rows. The pgvector
	// default (40) sits below the candidate pool SearchHybrid asks for, which
	// would quietly starve the vector half of the hybrid ranking, so raise it
	// on every pooled connection to match.
	efSearch := max(memory.DefaultScoringConfig.CandidateK, hnswDefaultEfSearch)
	cfg.ConnConfig.RuntimeParams["hnsw.ef_search"] = strconv.Itoa(efSearch)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.NewWithConfig: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

// hnswDefaultEfSearch mirrors pgvector's own default, used as a floor so a
// small CandidateK can never lower recall below stock behaviour.
const hnswDefaultEfSearch = 40

func (p *Postgres) Close() {
	p.pool.Close()
}

// --- Memory operations ---

func (p *Postgres) SaveMemory(ctx context.Context, m *memory.Memory, embedding []float32) error {
	var vec *pgvector.Vector
	if len(embedding) > 0 {
		v := pgvector.NewVector(embedding)
		vec = &v
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO memories (id, content, content_hash, summary, source, session_id, tags, embedding, importance, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (id) DO UPDATE SET
			content      = EXCLUDED.content,
			content_hash = EXCLUDED.content_hash,
			summary      = EXCLUDED.summary,
			source       = EXCLUDED.source,
			session_id   = EXCLUDED.session_id,
			tags         = EXCLUDED.tags,
			embedding    = EXCLUDED.embedding,
			importance   = EXCLUDED.importance,
			access_count = memories.access_count,
			last_accessed_at = memories.last_accessed_at,
			updated_at   = EXCLUDED.updated_at
	`, m.ID, m.Content, nilIfEmpty(m.ContentHash), m.Summary, m.Source, m.SessionID, m.Tags, vec, m.Importance, m.CreatedAt, m.UpdatedAt)
	return err
}

func (p *Postgres) FindByContentHash(ctx context.Context, hash string) (*memory.Memory, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id, content, content_hash, summary, source, session_id, tags, importance, access_count, last_accessed_at, created_at, updated_at
		FROM memories WHERE content_hash = $1
	`, hash)
	m := &memory.Memory{}
	err := row.Scan(&m.ID, &m.Content, &m.ContentHash, &m.Summary, &m.Source, &m.SessionID, &m.Tags, &m.Importance, &m.AccessCount, &m.LastAccessedAt, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (p *Postgres) GetMemory(ctx context.Context, id uuid.UUID) (*memory.Memory, error) {
	row := p.pool.QueryRow(ctx, `
		SELECT id, content, content_hash, summary, source, session_id, tags, importance, access_count, last_accessed_at, created_at, updated_at
		FROM memories WHERE id = $1
	`, id)
	m := &memory.Memory{}
	return m, row.Scan(&m.ID, &m.Content, &m.ContentHash, &m.Summary, &m.Source, &m.SessionID, &m.Tags, &m.Importance, &m.AccessCount, &m.LastAccessedAt, &m.CreatedAt, &m.UpdatedAt)
}

func (p *Postgres) ListMemories(ctx context.Context, filter memory.Filter) ([]*memory.Memory, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, content, content_hash, summary, source, session_id, tags, importance, access_count, last_accessed_at, created_at, updated_at
		FROM memories
		WHERE ($1::text IS NULL OR source = $1)
		  AND ($2::text[] IS NULL OR tags && $2)
		ORDER BY created_at DESC
		LIMIT $3
	`, nilIfEmpty(filter.Source), nilIfSliceEmpty(filter.Tags), filter.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var memories []*memory.Memory
	for rows.Next() {
		m := &memory.Memory{}
		if err := rows.Scan(&m.ID, &m.Content, &m.ContentHash, &m.Summary, &m.Source, &m.SessionID, &m.Tags, &m.Importance, &m.AccessCount, &m.LastAccessedAt, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		memories = append(memories, m)
	}
	return memories, nil
}

func (p *Postgres) DeleteMemory(ctx context.Context, id uuid.UUID) error {
	// Delete the memory (cascades to memory_entities, sets relations.memory_id = NULL).
	_, err := p.pool.Exec(ctx, `DELETE FROM memories WHERE id = $1`, id)
	if err != nil {
		return err
	}

	// Clean up orphaned relations (memory_id became NULL).
	_, err = p.pool.Exec(ctx, `DELETE FROM relations WHERE memory_id IS NULL`)
	if err != nil {
		return fmt.Errorf("cleanup orphaned relations: %w", err)
	}

	// Clean up orphaned entities (no remaining memory links).
	_, err = p.pool.Exec(ctx, `
		DELETE FROM entities e
		WHERE NOT EXISTS (
			SELECT 1 FROM memory_entities me WHERE me.entity_id = e.id
		)
	`)
	if err != nil {
		return fmt.Errorf("cleanup orphaned entities: %w", err)
	}

	return nil
}

// SearchSimilar performs cosine similarity search using pgvector.
// Returns memories ordered by similarity (closest first).
func (p *Postgres) SearchSimilar(ctx context.Context, embedding []float32, topK int, minScore float32) ([]memory.ScoredMemory, error) {
	vec := pgvector.NewVector(embedding)
	rows, err := p.pool.Query(ctx, `
		SELECT id, content, content_hash, summary, source, session_id, tags, importance, access_count, last_accessed_at, created_at, updated_at,
		       1 - (embedding <=> $1) AS score
		FROM memories
		WHERE embedding IS NOT NULL
		  AND 1 - (embedding <=> $1) >= $3
		ORDER BY embedding <=> $1
		LIMIT $2
	`, vec, topK, minScore)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []memory.ScoredMemory
	for rows.Next() {
		m := &memory.Memory{}
		var score float32
		if err := rows.Scan(
			&m.ID, &m.Content, &m.ContentHash, &m.Summary, &m.Source, &m.SessionID,
			&m.Tags, &m.Importance, &m.AccessCount, &m.LastAccessedAt, &m.CreatedAt, &m.UpdatedAt, &score,
		); err != nil {
			return nil, err
		}
		results = append(results, memory.ScoredMemory{Memory: m, Score: score})
	}
	return results, nil
}

// SearchHybrid retrieves candidates from both vector (pgvector) and full-text (tsvector)
// indexes, then combines them using Reciprocal Rank Fusion (RRF) with additive metadata boosts.
func (p *Postgres) SearchHybrid(ctx context.Context, query string, embedding []float32, topK int, cfg memory.ScoringConfig) ([]memory.ScoredMemory, error) {
	vec := pgvector.NewVector(embedding)

	// Decay constant: ln(2) / half_life_days
	decayLambda := math.Ln2 / cfg.RecencyHalfLifeDays

	rows, err := p.pool.Query(ctx, `
		WITH vector_candidates AS (
			SELECT id,
			       ROW_NUMBER() OVER (ORDER BY embedding <=> $1) AS vrank
			FROM memories
			WHERE embedding IS NOT NULL
			ORDER BY embedding <=> $1
			LIMIT $2
		),
		fts_query AS (
			SELECT to_tsquery('english',
				replace(plainto_tsquery('english', $4)::text, ' & ', ' | ')
			) AS q
		),
		fulltext_candidates AS (
			SELECT id,
			       ROW_NUMBER() OVER (ORDER BY ts_rank_cd(content_tsv, fts_query.q) DESC) AS frank
			FROM memories, fts_query
			WHERE content_tsv @@ fts_query.q
			LIMIT $2
		),
		combined AS (
			SELECT COALESCE(v.id, f.id) AS id,
			       COALESCE(v.vrank,  $2) AS vrank,
			       COALESCE(f.frank,  $2) AS frank
			FROM vector_candidates v
			FULL OUTER JOIN fulltext_candidates f ON v.id = f.id
		)
		SELECT m.id, m.content, m.content_hash, m.summary, m.source, m.session_id,
		       m.tags, m.importance, m.access_count, m.last_accessed_at, m.created_at, m.updated_at,
		       (
		           1.0 / ($5::double precision + c.vrank::double precision)
		         + 1.0 / ($5::double precision + c.frank::double precision)
		         + $6::double precision * (m.importance - 1)::double precision / 4.0
		         + $7::double precision * exp(-$8::double precision * EXTRACT(EPOCH FROM (NOW() - m.created_at)) / 86400.0)
		         + $9::double precision * LEAST(1.0, ln(1.0 + m.access_count::double precision) / ln(21.0))
		       ) AS score
		FROM combined c
		JOIN memories m ON m.id = c.id
		ORDER BY score DESC
		LIMIT $3
	`, vec, cfg.CandidateK, topK, query,
		cfg.RRFConstant, cfg.ImportanceWeight, cfg.RecencyWeight, decayLambda, cfg.AccessWeight,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []memory.ScoredMemory
	for rows.Next() {
		m := &memory.Memory{}
		var score float32
		if err := rows.Scan(
			&m.ID, &m.Content, &m.ContentHash, &m.Summary, &m.Source, &m.SessionID,
			&m.Tags, &m.Importance, &m.AccessCount, &m.LastAccessedAt, &m.CreatedAt, &m.UpdatedAt, &score,
		); err != nil {
			return nil, err
		}
		results = append(results, memory.ScoredMemory{Memory: m, Score: score})
	}
	return results, nil
}

// RecordAccess increments access_count and updates last_accessed_at for the given memory IDs.
func (p *Postgres) RecordAccess(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := p.pool.Exec(ctx, `
		UPDATE memories
		SET access_count = access_count + 1, last_accessed_at = NOW()
		WHERE id = ANY($1)
	`, ids)
	return err
}

// --- Graph / Entity operations ---

func (p *Postgres) UpsertEntity(ctx context.Context, name, entityType string) (uuid.UUID, error) {
	var id uuid.UUID
	err := p.pool.QueryRow(ctx, `
		INSERT INTO entities (name, entity_type)
		VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE SET entity_type = EXCLUDED.entity_type
		RETURNING id
	`, name, entityType).Scan(&id)
	return id, err
}

func (p *Postgres) UpsertRelation(ctx context.Context, fromID, toID uuid.UUID, relationship string, memoryID uuid.UUID, confidence float64) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO relations (from_entity, to_entity, relationship, memory_id, confidence)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (from_entity, to_entity, relationship)
		DO UPDATE SET confidence = GREATEST(relations.confidence, EXCLUDED.confidence),
		              memory_id  = EXCLUDED.memory_id
	`, fromID, toID, relationship, memoryID, confidence)
	return err
}

func (p *Postgres) LinkMemoryEntity(ctx context.Context, memoryID, entityID uuid.UUID) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO memory_entities (memory_id, entity_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING
	`, memoryID, entityID)
	return err
}

func (p *Postgres) GetGraph(ctx context.Context, entityName string, depth int) (*memory.Graph, error) {
	rows, err := p.pool.Query(ctx, `
		WITH RECURSIVE graph AS (
			SELECT e.id, e.name, e.entity_type, 0 AS depth
			FROM entities e WHERE e.name ILIKE $1
			UNION ALL
			SELECT e2.id, e2.name, e2.entity_type, g.depth + 1
			FROM graph g
			JOIN relations r ON r.from_entity = g.id OR r.to_entity = g.id
			JOIN entities e2 ON (r.from_entity = e2.id OR r.to_entity = e2.id) AND e2.id != g.id
			WHERE g.depth < $2
		) CYCLE id SET is_cycle USING path
		SELECT DISTINCT id, name, entity_type FROM graph WHERE NOT is_cycle
	`, "%"+entityName+"%", depth)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	g := &memory.Graph{Nodes: []memory.Entity{}, Edges: []memory.Relation{}}
	var nodeIDs []uuid.UUID
	for rows.Next() {
		var e memory.Entity
		if err := rows.Scan(&e.ID, &e.Name, &e.Type); err != nil {
			return nil, err
		}
		g.Nodes = append(g.Nodes, e)
		nodeIDs = append(nodeIDs, e.ID)
	}

	if len(nodeIDs) > 0 {
		erows, err := p.pool.Query(ctx, `
			SELECT r.id, fe.name, te.name, r.relationship, r.confidence
			FROM relations r
			JOIN entities fe ON fe.id = r.from_entity
			JOIN entities te ON te.id = r.to_entity
			WHERE r.from_entity = ANY($1) AND r.to_entity = ANY($1)
		`, nodeIDs)
		if err != nil {
			return nil, err
		}
		defer erows.Close()
		for erows.Next() {
			var rel memory.Relation
			if err := erows.Scan(&rel.ID, &rel.From, &rel.To, &rel.Relationship, &rel.Confidence); err != nil {
				return nil, err
			}
			g.Edges = append(g.Edges, rel)
		}
	}
	return g, nil
}

func (p *Postgres) GetFullGraph(ctx context.Context) (*memory.Graph, error) {
	g := &memory.Graph{Nodes: []memory.Entity{}, Edges: []memory.Relation{}}

	rows, err := p.pool.Query(ctx, `SELECT id, name, entity_type FROM entities ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodeIDs []uuid.UUID
	for rows.Next() {
		var e memory.Entity
		if err := rows.Scan(&e.ID, &e.Name, &e.Type); err != nil {
			return nil, err
		}
		g.Nodes = append(g.Nodes, e)
		nodeIDs = append(nodeIDs, e.ID)
	}

	if len(nodeIDs) > 0 {
		erows, err := p.pool.Query(ctx, `
			SELECT r.id, fe.name, te.name, r.relationship, r.confidence
			FROM relations r
			JOIN entities fe ON fe.id = r.from_entity
			JOIN entities te ON te.id = r.to_entity
		`)
		if err != nil {
			return nil, err
		}
		defer erows.Close()

		for erows.Next() {
			var rel memory.Relation
			if err := erows.Scan(&rel.ID, &rel.From, &rel.To, &rel.Relationship, &rel.Confidence); err != nil {
				return nil, err
			}
			g.Edges = append(g.Edges, rel)
		}
	}

	return g, nil
}

func (p *Postgres) GetGraphForMemories(ctx context.Context, memoryIDs []uuid.UUID) (*memory.Graph, error) {
	if len(memoryIDs) == 0 {
		return nil, nil
	}

	g := &memory.Graph{}

	// Get entities linked to these memories.
	eRows, err := p.pool.Query(ctx, `
		SELECT DISTINCT e.id, e.name, e.entity_type
		FROM memory_entities me
		JOIN entities e ON e.id = me.entity_id
		WHERE me.memory_id = ANY($1)
	`, memoryIDs)
	if err != nil {
		return nil, fmt.Errorf("get entities for memories: %w", err)
	}
	defer eRows.Close()

	entityIDs := map[uuid.UUID]bool{}
	for eRows.Next() {
		var e memory.Entity
		if err := eRows.Scan(&e.ID, &e.Name, &e.Type); err != nil {
			return nil, err
		}
		g.Nodes = append(g.Nodes, e)
		entityIDs[e.ID] = true
	}

	if len(g.Nodes) == 0 {
		return nil, nil
	}

	// Get relations between those entities.
	rRows, err := p.pool.Query(ctx, `
		SELECT r.id, e1.name, e2.name, r.relationship, r.confidence
		FROM relations r
		JOIN entities e1 ON e1.id = r.from_entity
		JOIN entities e2 ON e2.id = r.to_entity
		WHERE r.from_entity = ANY($1) AND r.to_entity = ANY($1)
	`, uuidKeys(entityIDs))
	if err != nil {
		return nil, fmt.Errorf("get relations for memories: %w", err)
	}
	defer rRows.Close()

	for rRows.Next() {
		var r memory.Relation
		if err := rRows.Scan(&r.ID, &r.From, &r.To, &r.Relationship, &r.Confidence); err != nil {
			return nil, err
		}
		g.Edges = append(g.Edges, r)
	}

	return g, nil
}

func uuidKeys(m map[uuid.UUID]bool) []uuid.UUID {
	keys := make([]uuid.UUID, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func (p *Postgres) GetMemoriesByEntity(ctx context.Context, entityID uuid.UUID) ([]*memory.Memory, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT m.id, m.content, m.content_hash, m.summary, m.source, m.session_id, m.tags, m.importance, m.access_count, m.last_accessed_at, m.created_at, m.updated_at
		FROM memories m
		JOIN memory_entities me ON me.memory_id = m.id
		WHERE me.entity_id = $1
		ORDER BY m.created_at DESC
	`, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var memories []*memory.Memory
	for rows.Next() {
		m := &memory.Memory{}
		if err := rows.Scan(&m.ID, &m.Content, &m.ContentHash, &m.Summary, &m.Source, &m.SessionID, &m.Tags, &m.Importance, &m.AccessCount, &m.LastAccessedAt, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		memories = append(memories, m)
	}
	return memories, nil
}

func (p *Postgres) GetEntityTypes(ctx context.Context) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT DISTINCT entity_type FROM entities WHERE entity_type != '' ORDER BY entity_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		types = append(types, t)
	}
	return types, nil
}

func (p *Postgres) GetRelatedMemories(ctx context.Context, memoryID uuid.UUID) ([]memory.RelatedMemory, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT
			m.id, m.content, m.content_hash, m.summary, m.source, m.session_id, m.tags, m.importance, m.access_count, m.last_accessed_at, m.created_at, m.updated_at,
			e.id AS entity_id, e.name AS entity_name, e.entity_type
		FROM memory_entities me1
		JOIN memory_entities me2 ON me2.entity_id = me1.entity_id AND me2.memory_id != me1.memory_id
		JOIN memories m ON m.id = me2.memory_id
		JOIN entities e ON e.id = me1.entity_id
		WHERE me1.memory_id = $1
		ORDER BY m.id, e.name
	`, memoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Group by memory ID, collecting all shared entities.
	type entry struct {
		m    *memory.Memory
		via  []memory.Entity
		seen map[uuid.UUID]bool
	}
	index := map[uuid.UUID]*entry{}
	var order []uuid.UUID

	for rows.Next() {
		m := &memory.Memory{}
		var ent memory.Entity
		if err := rows.Scan(&m.ID, &m.Content, &m.ContentHash, &m.Summary, &m.Source, &m.SessionID, &m.Tags, &m.Importance, &m.AccessCount, &m.LastAccessedAt, &m.CreatedAt, &m.UpdatedAt, &ent.ID, &ent.Name, &ent.Type); err != nil {
			return nil, err
		}
		if e, ok := index[m.ID]; ok {
			if !e.seen[ent.ID] {
				e.via = append(e.via, ent)
				e.seen[ent.ID] = true
			}
		} else {
			index[m.ID] = &entry{m: m, via: []memory.Entity{ent}, seen: map[uuid.UUID]bool{ent.ID: true}}
			order = append(order, m.ID)
		}
	}

	result := make([]memory.RelatedMemory, 0, len(order))
	for _, id := range order {
		e := index[id]
		result = append(result, memory.RelatedMemory{Memory: e.m, ViaEntities: e.via})
	}
	return result, nil
}

func (p *Postgres) UpdateSummary(ctx context.Context, id uuid.UUID, summary string) error {
	_, err := p.pool.Exec(ctx, `UPDATE memories SET summary = $2, updated_at = NOW() WHERE id = $1`, id, summary)
	return err
}

// --- helpers ---

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nilIfSliceEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
