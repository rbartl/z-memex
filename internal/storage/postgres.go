package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
		SELECT id, content, COALESCE(content_hash, '') AS content_hash, summary, source, session_id, tags, importance, access_count, last_accessed_at, created_at, updated_at
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
		SELECT id, content, COALESCE(content_hash, '') AS content_hash, summary, source, session_id, tags, importance, access_count, last_accessed_at, created_at, updated_at
		FROM memories WHERE id = $1
	`, id)
	m := &memory.Memory{}
	return m, row.Scan(&m.ID, &m.Content, &m.ContentHash, &m.Summary, &m.Source, &m.SessionID, &m.Tags, &m.Importance, &m.AccessCount, &m.LastAccessedAt, &m.CreatedAt, &m.UpdatedAt)
}

func (p *Postgres) ListMemories(ctx context.Context, filter memory.Filter) ([]*memory.Memory, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, content, COALESCE(content_hash, '') AS content_hash, summary, source, session_id, tags, importance, access_count, last_accessed_at, created_at, updated_at
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
		SELECT id, content, COALESCE(content_hash, '') AS content_hash, summary, source, session_id, tags, importance, access_count, last_accessed_at, created_at, updated_at,
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

	rows, err := p.pool.Query(ctx, fmt.Sprintf(`
		WITH vector_candidates AS (
			SELECT id,
			       ROW_NUMBER() OVER (ORDER BY embedding <=> $1) AS vrank
			FROM memories
			WHERE embedding IS NOT NULL
			ORDER BY embedding <=> $1
			LIMIT $2
		),
		fts_query AS (
			SELECT %s AS q
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
		SELECT m.id, m.content, COALESCE(m.content_hash, '') AS content_hash, m.summary, m.source, m.session_id,
		       m.tags, m.importance, m.access_count, m.last_accessed_at, m.created_at, m.updated_at,
		       (
		           1.0 / ($5::double precision + c.vrank::double precision)
		         + $10::double precision / ($5::double precision + c.frank::double precision)
		         + $6::double precision * (m.importance - 1)::double precision / 4.0
		         + $7::double precision * exp(-$8::double precision * EXTRACT(EPOCH FROM (NOW() - m.created_at)) / 86400.0)
		         + $9::double precision * LEAST(1.0, ln(1.0 + m.access_count::double precision) / ln(21.0))
		       ) AS score
		FROM combined c
		JOIN memories m ON m.id = c.id
		ORDER BY score DESC
		LIMIT $3
	`, cfg.FullText.TsqueryExpr()), vec, cfg.CandidateK, topK, query,
		cfg.RRFConstant, cfg.ImportanceWeight, cfg.RecencyWeight, decayLambda, cfg.AccessWeight,
		cfg.FullTextWeight,
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
	norm := memory.NormalizeName(name)
	if norm == "" {
		return uuid.Nil, fmt.Errorf("entity name is empty")
	}

	// A curated alias wins over creating a node: once someone has said that
	// "Max" is "Maximilian Zollneritsch", every later extraction of "Max" has to
	// land on that entity instead of splitting the person again.
	var aliasID uuid.UUID
	err := p.pool.QueryRow(ctx, `
		SELECT entity_id FROM entity_aliases WHERE alias_norm = $1
	`, norm).Scan(&aliasID)
	if err == nil {
		return aliasID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("alias lookup failed, falling through to upsert", "name", name, "err", err)
	}

	// Identity is the normalised name; the first spelling seen stays as the
	// display name, so a later "ACME" resolves to the existing node rather
	// than renaming it.
	var id uuid.UUID
	err = p.pool.QueryRow(ctx, `
		INSERT INTO entities (name, name_norm, entity_type)
		VALUES ($1, $2, $3)
		ON CONFLICT (name_norm) DO UPDATE SET entity_type = EXCLUDED.entity_type
		RETURNING id
	`, name, norm, entityType).Scan(&id)
	return id, err
}

// ListEntities returns graph nodes with the counts that show whether each one
// earns its place, most connected first. A non-empty query filters by name.
func (p *Postgres) ListEntities(ctx context.Context, query string, limit int) ([]memory.EntityStat, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT e.id, e.name, e.entity_type,
		       (SELECT count(*) FROM relations r WHERE r.from_entity = e.id OR r.to_entity = e.id),
		       (SELECT count(*) FROM memory_entities me WHERE me.entity_id = e.id),
		       COALESCE((SELECT array_agg(a.alias_norm ORDER BY a.alias_norm)
		                 FROM entity_aliases a WHERE a.entity_id = e.id), '{}')
		FROM entities e
		WHERE $1 = '' OR e.name_norm LIKE '%' || $1 || '%'
		ORDER BY 4 DESC, 5 DESC, e.name
		LIMIT $2
	`, memory.NormalizeName(query), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []memory.EntityStat
	for rows.Next() {
		var e memory.EntityStat
		if err := rows.Scan(&e.ID, &e.Name, &e.Type, &e.Relations, &e.Memories, &e.Aliases); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MergeEntities folds loser into winner: every memory link and every edge is
// repointed, the loser's name (and anything already aliased to it) becomes an
// alias of the winner, and the loser is deleted.
//
// This is the runtime form of the merge migrations 007 and 009 run in bulk. It
// is not reversible -- which is why nothing calls it automatically.
func (p *Postgres) MergeEntities(ctx context.Context, loserID, winnerID uuid.UUID) error {
	if loserID == winnerID {
		return fmt.Errorf("cannot merge an entity into itself")
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	// The loser's name has to become an alias before the row goes, and the
	// aliases already pointing at it move across with it.
	if _, err := tx.Exec(ctx, `
		INSERT INTO entity_aliases (alias_norm, entity_id)
		SELECT e.name_norm, $2 FROM entities e WHERE e.id = $1
		ON CONFLICT (alias_norm) DO UPDATE SET entity_id = EXCLUDED.entity_id
	`, loserID, winnerID); err != nil {
		return fmt.Errorf("record alias: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE entity_aliases SET entity_id = $2 WHERE entity_id = $1
	`, loserID, winnerID); err != nil {
		return fmt.Errorf("repoint aliases: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_entities (memory_id, entity_id)
		SELECT me.memory_id, $2 FROM memory_entities me WHERE me.entity_id = $1
		ON CONFLICT DO NOTHING
	`, loserID, winnerID); err != nil {
		return fmt.Errorf("repoint memory links: %w", err)
	}

	// Edges are grouped before insert for the same reason migration 007 groups
	// them: two edges that differed only by which node they touched collapse
	// onto one key here, and ON CONFLICT DO UPDATE refuses to hit one row twice
	// in a statement. Symmetric predicates get their endpoints re-ordered, since
	// the merge can leave an edge pointing the wrong way round.
	if _, err := tx.Exec(ctx, `
		INSERT INTO relations (from_entity, to_entity, relationship, relationship_raw, memory_id, confidence)
		SELECT o.f, o.t, o.rel,
		       (array_agg(o.raw ORDER BY o.conf DESC))[1],
		       (array_agg(o.mem ORDER BY o.conf DESC))[1],
		       max(o.conf)
		FROM (
		    SELECT CASE WHEN x.rel = ANY($3) AND x.f > x.t THEN x.t ELSE x.f END AS f,
		           CASE WHEN x.rel = ANY($3) AND x.f > x.t THEN x.f ELSE x.t END AS t,
		           x.rel, x.raw, x.mem, x.conf
		    FROM (
		        SELECT CASE WHEN r.from_entity = $1 THEN $2 ELSE r.from_entity END AS f,
		               CASE WHEN r.to_entity   = $1 THEN $2 ELSE r.to_entity   END AS t,
		               r.relationship AS rel, r.relationship_raw AS raw,
		               r.memory_id AS mem, r.confidence AS conf
		        FROM relations r
		        WHERE r.from_entity = $1 OR r.to_entity = $1
		    ) x
		    WHERE x.f <> x.t
		) o
		GROUP BY o.f, o.t, o.rel
		ON CONFLICT (from_entity, to_entity, relationship)
		DO UPDATE SET confidence = GREATEST(relations.confidence, EXCLUDED.confidence)
	`, loserID, winnerID, memory.SymmetricRelationships()); err != nil {
		return fmt.Errorf("repoint relations: %w", err)
	}

	// The loser's own rows in both tables go with it, by ON DELETE CASCADE.
	if _, err := tx.Exec(ctx, `DELETE FROM entities WHERE id = $1`, loserID); err != nil {
		return fmt.Errorf("delete merged entity: %w", err)
	}

	return tx.Commit(ctx)
}

// UpsertRelation writes one edge. relationship must already be folded onto the
// closed vocabulary; rawRelationship keeps the extractor's own wording, which is
// the only record of what the model actually said once the fold has run.
func (p *Postgres) UpsertRelation(ctx context.Context, fromID, toID uuid.UUID, relationship, rawRelationship string, memoryID uuid.UUID, confidence float64) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO relations (from_entity, to_entity, relationship, relationship_raw, memory_id, confidence)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (from_entity, to_entity, relationship)
		DO UPDATE SET confidence = GREATEST(relations.confidence, EXCLUDED.confidence),
		              memory_id  = EXCLUDED.memory_id
	`, fromID, toID, relationship, rawRelationship, memoryID, confidence)
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

// weakEntityCondition matches an entity that has not earned a place in the
// graph: it is typed as the fallback, one relation is all it has, and only one
// memory ever named it. That describes the abstract noun phrases the extractor
// emits against instructions -- "layered approach", "bulk assign", "freed
// capacity" -- which made up over half the corpus's nodes and connect nothing.
//
// It is a filter rather than a delete: an entity that is genuinely new starts
// out looking exactly like this, and earns its way in as soon as a second
// memory or a second relation arrives.
const weakEntityCondition = `(
	e.entity_type = 'concept'
	AND (SELECT count(*) FROM relations r WHERE r.from_entity = e.id OR r.to_entity = e.id) <= 1
	AND (SELECT count(*) FROM memory_entities me WHERE me.entity_id = e.id) <= 1
)`

// GetFullGraph returns the whole graph. Weak entities are left out unless
// includeWeak is set; the graph page exposes that as a toggle so the hidden
// half stays reachable.
func (p *Postgres) GetFullGraph(ctx context.Context, includeWeak bool) (*memory.Graph, error) {
	g := &memory.Graph{Nodes: []memory.Entity{}, Edges: []memory.Relation{}}

	nodeQuery := `SELECT e.id, e.name, e.entity_type FROM entities e`
	if !includeWeak {
		nodeQuery += ` WHERE NOT ` + weakEntityCondition
	}
	nodeQuery += ` ORDER BY e.name`

	rows, err := p.pool.Query(ctx, nodeQuery)
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
		// Both endpoints must be present, or the client draws an edge to a node
		// it was never given. This held even before the filter existed: the
		// query used to fetch every relation regardless of the nodes returned.
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
		WHERE me.memory_id = ANY($1) AND NOT `+weakEntityCondition+`
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
		SELECT m.id, m.content, COALESCE(m.content_hash, '') AS content_hash, m.summary, m.source, m.session_id, m.tags, m.importance, m.access_count, m.last_accessed_at, m.created_at, m.updated_at
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

func (p *Postgres) GetRelatedMemories(ctx context.Context, memoryID uuid.UUID) ([]memory.RelatedMemory, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT
			m.id, m.content, COALESCE(m.content_hash, '') AS content_hash, m.summary, m.source, m.session_id, m.tags, m.importance, m.access_count, m.last_accessed_at, m.created_at, m.updated_at,
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

// EmbeddingDimension reports the width the embedding column is declared with.
// atttypmod carries it for pgvector, and a mismatch against the configured
// model is a misconfiguration worth refusing to start on rather than
// discovering one failed insert at a time.
func (p *Postgres) EmbeddingDimension(ctx context.Context) (int, error) {
	var dim int
	err := p.pool.QueryRow(ctx, `
		SELECT atttypmod
		FROM pg_attribute
		WHERE attrelid = 'memories'::regclass AND attname = 'embedding'
	`).Scan(&dim)
	return dim, err
}

// MemoriesMissingEmbedding returns memories that have no vector yet, oldest
// first. Rows land here after a migration that changes the embedding width, and
// after any store whose embedding call failed.
func (p *Postgres) MemoriesMissingEmbedding(ctx context.Context, limit int) ([]*memory.Memory, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id, content FROM memories
		WHERE embedding IS NULL
		ORDER BY created_at
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*memory.Memory
	for rows.Next() {
		m := &memory.Memory{}
		if err := rows.Scan(&m.ID, &m.Content); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetEmbedding stores a vector for one memory, leaving every other column
// alone -- unlike SaveMemory, which rewrites the row.
func (p *Postgres) SetEmbedding(ctx context.Context, id uuid.UUID, embedding []float32) error {
	_, err := p.pool.Exec(ctx, `UPDATE memories SET embedding = $2 WHERE id = $1`, id, pgvector.NewVector(embedding))
	return err
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
