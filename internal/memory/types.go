package memory

import (
	"time"

	"github.com/google/uuid"
)

type Memory struct {
	ID        uuid.UUID `json:"id"`
	Content   string    `json:"content"`
	Summary   string    `json:"summary,omitempty"`
	Source    string    `json:"source,omitempty"` // which tool/agent stored this
	SessionID string    `json:"session_id,omitempty"`
	Tags      []string  `json:"tags,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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
	Memory    *Memory `json:"memory"`
	Duplicate bool    `json:"duplicate"`
	Score     float32 `json:"score,omitempty"` // similarity score if duplicate
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
