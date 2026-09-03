package main

import (
	"context"
	"fmt"
	"log"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/zollneritsch/memex/internal/embeddings"
	"gitlab.com/zollneritsch/memex/internal/storage"
)

// --- swapping the embedding model ---

// reembed rewrites every memory's vector with a different model and resizes the
// column to that model's dimension. Embedders disagree on width — 768 for
// nomic-embed-text, 1024 for qwen3-embedding — and vectors from two models are
// not comparable anyway, so the whole column is replaced in one pass.
//
// This is schema surgery. Point it at a copy of the database, never the real
// one: it drops every stored embedding before it writes the new ones, so an
// interrupted run leaves the source column empty.
func reembed(ctx context.Context, dbURL string, embedder *embeddings.Ollama, model string) error {
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	probe, err := embedder.Embed(ctx, "dimension probe")
	if err != nil {
		return fmt.Errorf("probe %s (is it pulled?): %w", model, err)
	}
	dim := len(probe)
	if dim == 0 {
		return fmt.Errorf("%s returned a zero-length embedding", model)
	}
	fmt.Printf("re-embedding with %s (%d dimensions)\n", model, dim)

	// The index is built for the old width and the column cannot be resized
	// while any vector still occupies it.
	if _, err := pool.Exec(ctx, `DROP INDEX IF EXISTS idx_memories_embedding`); err != nil {
		return fmt.Errorf("drop index: %w", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE memories SET embedding = NULL`); err != nil {
		return fmt.Errorf("clear embeddings: %w", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE memories ALTER COLUMN embedding TYPE vector(%d)`, dim)); err != nil {
		return fmt.Errorf("resize column: %w", err)
	}

	rows, err := pool.Query(ctx, `SELECT id, content FROM memories ORDER BY created_at`)
	if err != nil {
		return err
	}
	type row struct {
		id      string
		content string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.content); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	rows.Close()

	for i, r := range all {
		v, err := embedder.Embed(ctx, r.content)
		if err != nil {
			return fmt.Errorf("embed memory %s: %w", r.id, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE memories SET embedding = $2 WHERE id = $1`, r.id, vectorLiteral(v)); err != nil {
			return fmt.Errorf("store embedding %s: %w", r.id, err)
		}
		if (i+1)%25 == 0 || i+1 == len(all) {
			fmt.Printf("  %d/%d\n", i+1, len(all))
		}
	}

	if _, err := pool.Exec(ctx, `CREATE INDEX idx_memories_embedding ON memories USING hnsw (embedding vector_cosine_ops)`); err != nil {
		return fmt.Errorf("recreate index: %w", err)
	}
	return nil
}

// vectorLiteral renders a vector for pgvector without pulling the typed helper
// into this tool; the driver sends it as text and Postgres casts it.
func vectorLiteral(v []float32) string {
	b := make([]byte, 0, len(v)*8+2)
	b = append(b, '[')
	for i, f := range v {
		if i > 0 {
			b = append(b, ',')
		}
		b = fmt.Appendf(b, "%g", f)
	}
	b = append(b, ']')
	return string(b)
}

// --- separation ---

// defaultDistractors are questions this knowledge base cannot answer. They read
// like plausible personal-memory queries so the embedder is not tipped off by
// register or phrasing alone.
var defaultDistractors = []string{
	"the migratory patterns of antarctic penguins",
	"recipe for sourdough starter hydration",
	"quantum basket weaving championship on mars",
	"what medication was i prescribed for my knee",
	"which violin teacher did i book for tuesday",
	"how deep should i plant tulip bulbs in november",
	"the score of the 1974 world cup final",
	"my scuba diving certification renewal date",
	"how do i descale the espresso machine",
	"which airline lost my luggage in lisbon",
	"the neighbour's dog breed and its name",
	"chord progression for the bridge of that song",
}

// separation answers the question a score floor depends on: does the true
// answer for a real query score higher than the best hit for a query with no
// answer at all? If the two overlap, no threshold can tell them apart, and
// recall_memory has no honest way to return fewer results.
//
// It reports cosine similarity from the vector half only. The fused score
// cannot serve here: its RRF term is 1/(k+rank), so the top result scores the
// same whatever the query.
type separation struct {
	model         string
	answerable    []float64 // cosine of the memory that should answer each case
	distractorTop []float64 // best cosine anything reached for an unanswerable query
}

func measureSeparation(ctx context.Context, store *storage.Postgres, embedder *embeddings.Ollama, model string, cases []Case, distractors []string, corpus int) separation {
	s := separation{model: model}

	for _, c := range cases {
		v, err := embedder.Embed(ctx, c.Query)
		if err != nil {
			log.Fatalf("embed %q: %v", c.Query, err)
		}
		// Ask for the whole corpus so the target is always found, whatever its
		// rank — a floor cares about its score, not its position.
		hits, err := store.SearchSimilar(ctx, v, corpus, -1)
		if err != nil {
			log.Fatalf("similar %q: %v", c.Query, err)
		}
		for _, h := range hits {
			if h.Memory.ID == c.target {
				s.answerable = append(s.answerable, float64(h.Score))
				break
			}
		}
	}

	for _, q := range distractors {
		v, err := embedder.Embed(ctx, q)
		if err != nil {
			log.Fatalf("embed %q: %v", q, err)
		}
		hits, err := store.SearchSimilar(ctx, v, 1, -1)
		if err != nil {
			log.Fatalf("similar %q: %v", q, err)
		}
		if len(hits) > 0 {
			s.distractorTop = append(s.distractorTop, float64(hits[0].Score))
		}
	}
	return s
}

// floorAt is the threshold that keeps `keep` of the true answers — the most
// permissive floor worth considering.
func (s separation) floorAt(keep float64) float64 {
	return percentile(s.answerable, 1-keep)
}

// survivors is the share of unanswerable queries whose best hit still clears
// that floor. This is the decision statistic: at 0 a floor works perfectly, at
// 1 it rejects nothing, and anywhere near the top it is not worth having.
func (s separation) survivors(floor float64) float64 {
	if len(s.distractorTop) == 0 {
		return 0
	}
	var n int
	for _, d := range s.distractorTop {
		if d >= floor {
			n++
		}
	}
	return float64(n) / float64(len(s.distractorTop))
}

func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	i := int(p * float64(len(sorted)-1))
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var t float64
	for _, x := range xs {
		t += x
	}
	return t / float64(len(xs))
}

func (s separation) report() {
	floor := s.floorAt(0.90)
	fmt.Printf("\nseparation (%s)\n", s.model)
	fmt.Printf("  answers    n=%-3d mean %.3f  median %.3f  p10 %.3f\n",
		len(s.answerable), mean(s.answerable), percentile(s.answerable, 0.5), percentile(s.answerable, 0.10))
	fmt.Printf("  no-answer  n=%-3d mean %.3f  median %.3f  max %.3f\n",
		len(s.distractorTop), mean(s.distractorTop), percentile(s.distractorTop, 0.5), percentile(s.distractorTop, 1))
	fmt.Printf("  gap (median answer - median no-answer): %+.3f\n", percentile(s.answerable, 0.5)-percentile(s.distractorTop, 0.5))
	fmt.Printf("  floor keeping 90%% of answers: %.3f — %.0f%% of unanswerable queries still clear it\n",
		floor, 100*s.survivors(floor))
}
