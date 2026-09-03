package memory

import (
	"time"

	"github.com/google/uuid"
)

type Memory struct {
	ID             uuid.UUID  `json:"id"`
	Content        string     `json:"content"`
	ContentHash    string     `json:"content_hash,omitempty"`
	Summary        string     `json:"summary,omitempty"`
	Source         string     `json:"source,omitempty"` // which tool/agent stored this
	SessionID      string     `json:"session_id,omitempty"`
	Tags           []string   `json:"tags,omitempty"`
	Importance     int        `json:"importance"`
	AccessCount    int        `json:"access_count"`
	LastAccessedAt *time.Time `json:"last_accessed_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type Filter struct {
	Source string
	Tags   []string
	Limit  int
}

type ScoredMemory struct {
	Memory *Memory `json:"memory"`
	Score  float32 `json:"score"`
}

type RelatedMemory struct {
	Memory      *Memory  `json:"memory"`
	ViaEntities []Entity `json:"via_entities"`
}

type EntityGroup struct {
	Entity   Entity
	Memories []*Memory
}

type StoreResult struct {
	Memory *Memory `json:"memory"`
	Action string  `json:"action"`          // "created", "merged", "updated", "skipped"
	Score  float32 `json:"score,omitempty"` // similarity score if not created
}

// Graph types

type Entity struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Type string    `json:"type"`
}

type Relation struct {
	ID           uuid.UUID `json:"id"`
	From         string    `json:"from"`
	To           string    `json:"to"`
	Relationship string    `json:"relationship"`
	Confidence   float64   `json:"confidence"`
}

type Graph struct {
	Nodes []Entity   `json:"nodes"`
	Edges []Relation `json:"edges"`
}

// RecallResult is returned by RecallMemoryWithGraph, combining semantic search
// results with the subgraph of entities and relations connected to those memories.
type RecallResult struct {
	Memories []ScoredMemory `json:"memories"`
	Graph    *Graph         `json:"graph,omitempty"`
}

// Extraction types (returned by Anthropic)

type ExtractionResult struct {
	Summary   string              `json:"summary"`
	Relations []ExtractedRelation `json:"relations"`
}

type ExtractedRelation struct {
	FromEntity   string  `json:"from_entity"`
	FromType     string  `json:"from_type"`
	ToEntity     string  `json:"to_entity"`
	ToType       string  `json:"to_type"`
	Relationship string  `json:"relationship"`
	Confidence   float64 `json:"confidence"`
}

// FullTextMode selects how the lexical half of hybrid search turns the query
// string into a tsquery. It is the main lever on retrieval precision: the
// vector half supplies recall, so the full-text half is only useful when it is
// selective.
type FullTextMode string

const (
	// FullTextAll requires every term to be present (plainto_tsquery). Precise,
	// and contributes nothing when no memory contains all the terms — which is
	// the correct behaviour for a complement to vector search.
	FullTextAll FullTextMode = "all"

	// FullTextWebsearch behaves like FullTextAll but also honours quoted
	// phrases, OR, and -negation in the query string (websearch_to_tsquery).
	FullTextWebsearch FullTextMode = "websearch"

	// FullTextAny ORs every term together. Matches a large fraction of any
	// corpus, so it ranks documents that merely repeat a common term and
	// dilutes the vector signal through RRF. Kept for comparison.
	FullTextAny FullTextMode = "any"

	// FullTextOff disables the lexical half, leaving pure vector search.
	FullTextOff FullTextMode = "off"
)

// tsqueryExpr returns the SQL expression building the tsquery for this mode.
// The result is a fixed literal chosen from this closed set — never anything
// derived from user input — so it is safe to interpolate into the query.
func (m FullTextMode) tsqueryExpr() string {
	switch m {
	case FullTextAll:
		return `plainto_tsquery('english', $4)`
	case FullTextAny:
		return `to_tsquery('english', replace(plainto_tsquery('english', $4)::text, ' & ', ' | '))`
	case FullTextOff:
		// Always NULL, so nothing matches and only the vector half contributes.
		// Phrased via the parameter rather than a bare NULL::tsquery so Postgres
		// can still infer $4's type — it appears nowhere else in the query.
		return `NULLIF(plainto_tsquery('english', $4), plainto_tsquery('english', $4))`
	default:
		return `websearch_to_tsquery('english', $4)`
	}
}

// TsqueryExpr exposes tsqueryExpr to the storage layer.
func (m FullTextMode) TsqueryExpr() string { return m.tsqueryExpr() }

// ScoringConfig controls the hybrid relevance scoring weights.
// Retrieval uses Reciprocal Rank Fusion (RRF) over vector + full-text candidates,
// then adds metadata boosts for importance, recency, and access frequency.
type ScoringConfig struct {
	RRFConstant         int          // k constant in 1/(k+rank), typically 60
	CandidateK          int          // how many candidates to fetch from each source
	FullText            FullTextMode // how the lexical half builds its tsquery
	FullTextWeight      float64      // relative weight of the full-text half in the fusion
	ImportanceWeight    float64      // additive boost for importance (0-1 normalized)
	RecencyWeight       float64      // additive boost for recency decay
	AccessWeight        float64      // additive boost for access frequency
	RecencyHalfLifeDays float64
}

// DefaultScoringConfig holds the retrieval settings, tuned with cmd/rankeval
// against a copy of a real knowledge base (153 labelled queries).
//
// RRFConstant is 10 rather than the 60 from the original RRF paper. That 60 is
// calibrated for fusing many systems over large TREC corpora; here it made the
// fusion so flat that rank 1 scored barely more than rank 50 (1/61 vs 1/110),
// and the metadata boosts below — up to 0.013 combined — swamped RRF terms
// that topped out near 0.033. They were steering the ranking, not breaking
// ties. Sharpening k restores the intended balance: MRR 0.471 -> 0.664 and
// R@1 0.268 -> 0.542, and zeroing the boosts entirely now changes nothing.
//
// FullTextAny looks wrong at first glance — ORing every term matches a large
// slice of the corpus — but measurement disagrees: it beats requiring all
// terms. Natural questions rarely share every term with the memory that
// answers them, so AND semantics leave the lexical half silent and throw its
// contribution away.
//
// Against pure vector search the choice is a genuine trade-off rather than a
// win, and it changed when the embedder did. Under bge-m3, on 26 hand-written
// questions:
//
//	          MRR    R@1    R@5    R@10   missed
//	any      0.687  0.538  0.885  1.000  0
//	off      0.804  0.731  0.846  0.962  1
//
// FullTextOff ranks better — it puts the right memory first far more often —
// but it loses one answer completely, and recall_memory hands its whole result
// set to a model that reads all of it. A memory ranked fourth is still read; a
// memory that is not returned is invisible, and the tool answers "nothing
// found" to a question the corpus can answer. The rarer, worse failure wins the
// argument, so the lexical half stays.
//
// Re-measure with cmd/rankeval before changing any of this — and on
// hand-written questions, since generated ones rank both models and modes
// differently.
var DefaultScoringConfig = ScoringConfig{
	RRFConstant:         10,
	CandidateK:          100,
	FullText:            FullTextAny,
	FullTextWeight:      1.0,
	ImportanceWeight:    0.005,
	RecencyWeight:       0.005,
	AccessWeight:        0.003,
	RecencyHalfLifeDays: 90.0,
}

// EntityStat is a graph node with the numbers that say whether it earns its
// place: how many edges it carries, how many memories name it, and which other
// names resolve to it.
type EntityStat struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Relations int       `json:"relations"`
	Memories  int       `json:"memories"`
	Aliases   []string  `json:"aliases,omitempty"`
}
