package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitlab.com/zollneritsch/memex/internal/memory"
)

// realisticResult mirrors the shape the live corpus produces: a handful of
// memories, and the subgraph around them fanning out to dozens of entities and
// well over a hundred edges.
func realisticResult(memories, nodes, edges int) *memory.RecallResult {
	r := &memory.RecallResult{Graph: &memory.Graph{}}
	for i := 0; i < memories; i++ {
		r.Memories = append(r.Memories, memory.ScoredMemory{
			Score: float32(90-i) / 100,
			Memory: &memory.Memory{
				ID:          uuid.New(),
				Content:     strings.Repeat("a fact worth remembering. ", 16),
				ContentHash: strings.Repeat("ab", 32),
				Summary:     "a one sentence summary of the fact",
				Source:      "claude-code",
				SessionID:   uuid.NewString(),
				Tags:        []string{"architecture", "decision"},
				Importance:  4,
				AccessCount: 7,
				CreatedAt:   time.Now(),
				UpdatedAt:   time.Now(),
			},
		})
	}
	names := make([]string, nodes)
	for i := 0; i < nodes; i++ {
		names[i] = fmt.Sprintf("Entity Number %d", i)
		r.Graph.Nodes = append(r.Graph.Nodes, memory.Entity{
			ID: uuid.New(), Name: names[i], Type: "concept",
		})
	}
	for i := 0; i < edges; i++ {
		r.Graph.Edges = append(r.Graph.Edges, memory.Relation{
			ID:           uuid.New(),
			From:         names[i%nodes],
			To:           names[(i*7+3)%nodes],
			Relationship: "relates-to",
			Confidence:   float64(i%100) / 100,
		})
	}
	return r
}

// The reason this format exists: the JSON dump was mostly graph scaffolding.
func TestFormatRecallIsAFractionOfTheJSONDump(t *testing.T) {
	result := realisticResult(5, 72, 167)

	asJSON, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	asText := formatRecall("some query", result)

	ratio := float64(len(asText)) / float64(len(asJSON))
	t.Logf("json %d bytes, text %d bytes (%.0f%%)", len(asJSON), len(asText), ratio*100)
	if ratio > 0.25 {
		t.Errorf("text form is %.0f%% of the JSON dump, want well under 25%%", ratio*100)
	}
}

// Internal bookkeeping has no reader on the other end of the tool call.
func TestFormatRecallOmitsInternalFields(t *testing.T) {
	result := realisticResult(2, 4, 4)
	out := formatRecall("q", result)

	if strings.Contains(out, result.Memories[0].Memory.ContentHash) {
		t.Error("content_hash leaked into the response")
	}
	if strings.Contains(out, result.Memories[0].Memory.SessionID) {
		t.Error("session_id leaked into the response")
	}
	for _, n := range result.Graph.Nodes {
		if strings.Contains(out, n.ID.String()) {
			t.Error("entity UUID leaked into the response; no tool accepts one")
			break
		}
	}
	// The memory id stays: delete_memory takes it.
	if !strings.Contains(out, result.Memories[0].Memory.ID.String()) {
		t.Error("memory id missing; delete_memory needs it")
	}
}

func TestFormatRecallCapsAndRanksEdges(t *testing.T) {
	result := realisticResult(1, 50, 200)
	out := formatRecall("q", result)

	lines := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "-->") {
			lines++
		}
	}
	if lines != recallGraphEdgeLimit {
		t.Errorf("rendered %d edges, want the cap of %d", lines, recallGraphEdgeLimit)
	}
	if !strings.Contains(out, "strongest of 200") {
		t.Errorf("truncation not disclosed: %s", out)
	}

	// Same input, same output — the ordering must not depend on map iteration.
	if out != formatRecall("q", result) {
		t.Error("format is not deterministic")
	}
}

func TestFormatRecallHandlesNoResults(t *testing.T) {
	for _, r := range []*memory.RecallResult{nil, {}} {
		out := formatRecall("nothing here", r)
		if !strings.Contains(out, "No memories matched") {
			t.Errorf("empty result rendered as %q", out)
		}
	}
}

// A recall whose memories have no extracted relations yet must not print an
// empty "How these connect" heading.
func TestFormatRecallOmitsEmptyGraph(t *testing.T) {
	result := realisticResult(1, 1, 0)
	if strings.Contains(formatRecall("q", result), "How these connect") {
		t.Error("empty graph still produced a heading")
	}
}
