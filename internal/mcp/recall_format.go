package mcp

import (
	"fmt"
	"sort"
	"strings"

	"gitlab.com/zollneritsch/memex/internal/memory"
)

// recallGraphEdgeLimit caps how many relationships a recall reports. Entities
// are shared across the corpus, so the subgraph around five memories routinely
// reaches 70 entities and 170 edges -- an order of magnitude more text than the
// memories themselves, most of it unrelated to why the memory matched. The
// highest-confidence edges carry nearly all of the useful context.
const recallGraphEdgeLimit = 25

// formatRecall renders a recall result as compact text.
//
// It used to be json.MarshalIndent of the whole result. For a five-memory
// recall that was ~2KB of content wrapped in ~29KB of graph: every entity and
// every relation as an indented JSON object, each carrying a UUID that no tool
// in this server accepts as input. What a reader needs from an edge is the two
// names and the verb between them, which is one line.
func formatRecall(query string, result *memory.RecallResult) string {
	if result == nil || len(result.Memories) == 0 {
		return fmt.Sprintf("No memories matched %q.", query)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d memories for %q, best first.\n", len(result.Memories), query)

	for i, sm := range result.Memories {
		m := sm.Memory
		if m == nil {
			continue
		}

		// The header carries what a caller can act on: the score that explains
		// the ranking, and the id that delete_memory takes. content_hash,
		// updated_at, last_accessed_at and session_id are dropped -- they are
		// internal bookkeeping no client has ever needed.
		fmt.Fprintf(&b, "\n[%d] score %.2f · id %s · importance %d", i+1, sm.Score, m.ID, m.Importance)
		if m.Source != "" {
			fmt.Fprintf(&b, " · via %s", m.Source)
		}
		fmt.Fprintf(&b, " · %s", m.CreatedAt.Format("2006-01-02"))
		if len(m.Tags) > 0 {
			fmt.Fprintf(&b, " · %s", strings.Join(m.Tags, ", "))
		}
		b.WriteString("\n")
		b.WriteString(strings.TrimSpace(m.Content))
		b.WriteString("\n")
	}

	b.WriteString(formatGraph(result.Graph))
	return b.String()
}

// formatGraph renders relationships as one line each. The node list is dropped
// with them: every node worth mentioning is named by an edge, and a node that
// no edge mentions says nothing about why these memories matched.
func formatGraph(g *memory.Graph) string {
	if g == nil || len(g.Edges) == 0 {
		return ""
	}

	edges := make([]memory.Relation, len(g.Edges))
	copy(edges, g.Edges)

	// Highest confidence first, then alphabetically so the same result renders
	// the same way twice.
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].Confidence != edges[j].Confidence {
			return edges[i].Confidence > edges[j].Confidence
		}
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})

	shown := edges
	if len(shown) > recallGraphEdgeLimit {
		shown = shown[:recallGraphEdgeLimit]
	}

	var b strings.Builder
	if len(edges) > len(shown) {
		fmt.Fprintf(&b, "\nHow these connect (%d strongest of %d):\n", len(shown), len(edges))
	} else {
		fmt.Fprintf(&b, "\nHow these connect (%d):\n", len(shown))
	}
	for _, e := range shown {
		fmt.Fprintf(&b, "%s --%s--> %s\n", e.From, e.Relationship, e.To)
	}
	return b.String()
}
