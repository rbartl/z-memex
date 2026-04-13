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

// ScoringConfig controls the hybrid relevance scoring weights.
// Retrieval uses Reciprocal Rank Fusion (RRF) over vector + full-text candidates,
// then adds metadata boosts for importance, recency, and access frequency.
type ScoringConfig struct {
	RRFConstant         int     // k constant in 1/(k+rank), typically 60
	CandidateK          int     // how many candidates to fetch from each source
	ImportanceWeight    float64 // additive boost for importance (0-1 normalized)
	RecencyWeight       float64 // additive boost for recency decay
	AccessWeight        float64 // additive boost for access frequency
	RecencyHalfLifeDays float64
}

var DefaultScoringConfig = ScoringConfig{
	RRFConstant:         60,
	CandidateK:          100,
	ImportanceWeight:    0.005,
	RecencyWeight:       0.005,
	AccessWeight:        0.003,
	RecencyHalfLifeDays: 90.0,
}
