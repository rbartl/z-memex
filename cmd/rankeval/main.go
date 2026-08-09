// Command rankeval measures retrieval quality so ranking changes can be
// compared rather than eyeballed.
//
// It runs a labelled set of questions through each FullTextMode against a real
// database and reports MRR and recall at several cutoffs. Point it at a copy of
// production data, never production itself — evaluation issues many queries and
// RecordAccess would skew the access-count boost being measured.
//
//	# clone production into a throwaway instance first, then:
//	go run ./cmd/rankeval -generate -out eval.json   # build a labelled set
//	go run ./cmd/rankeval -set eval.json             # compare ranking modes
//
// The generated file contains real memory text, so keep it out of version
// control (it is gitignored by default).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"gitlab.com/zollneritsch/memex/internal/embeddings"
	"gitlab.com/zollneritsch/memex/internal/memory"
	"gitlab.com/zollneritsch/memex/internal/storage"
)

// Case is one labelled question. Target names the memory that should be
// retrieved, either directly by ID or by a distinctive substring of its
// content, which survives re-cloning the database.
type Case struct {
	Query    string `json:"query"`
	MemoryID string `json:"memory_id,omitempty"`
	Match    string `json:"match,omitempty"`

	// Origin records whether a case was written by hand or generated, so the
	// two can be reported separately — generated questions echo the source
	// wording and flatter lexical matching.
	Origin string `json:"origin,omitempty"`

	target uuid.UUID
}

func main() {
	var (
		dbURL     = flag.String("db", "postgres://memex:memex@localhost:55432/memex?sslmode=disable", "database to evaluate against (use a copy, not production)")
		ollamaURL = flag.String("ollama", "http://localhost:11434", "Ollama endpoint")
		genModel  = flag.String("model", "qwen3.5:9b", "model used to generate questions")
		setPath   = flag.String("set", "eval.json", "labelled question set")
		outPath   = flag.String("out", "eval.json", "where -generate writes its output")
		generate  = flag.Bool("generate", false, "generate a question set from the memories in -db")
		limit     = flag.Int("limit", 0, "generate at most this many cases (0 = all memories)")
		topK      = flag.Int("topk", 10, "how many results to retrieve per query")
		sweep     = flag.Bool("sweep", false, "sweep the fusion balance instead of comparing tsquery modes")
		baseline  = flag.String("baseline", "", "variant to diff against (default: first)")
		candidate = flag.String("candidate", "", "variant to diff (default: best MRR)")
	)
	flag.Parse()

	ctx := context.Background()

	store, err := storage.NewPostgres(ctx, *dbURL)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer store.Close()

	if *generate {
		if err := generateSet(ctx, store, *ollamaURL, *genModel, *outPath, *limit); err != nil {
			log.Fatalf("generate: %v", err)
		}
		return
	}

	cases, err := loadSet(ctx, store, *setPath)
	if err != nil {
		log.Fatalf("load %s: %v", *setPath, err)
	}
	fmt.Printf("%d labelled queries against %s\n\n", len(cases), *dbURL)

	variants := modeVariants()
	if *sweep {
		variants = sweepVariants()
	}

	base, cand := *baseline, *candidate
	if base == "" {
		base = variants[0].name
	}

	embedder := embeddings.NewOllama(*ollamaURL, "nomic-embed-text")
	evaluate(ctx, store, embedder, cases, *topK, variants, base, cand)
}

// --- evaluation ---

// variant is one named scoring configuration under test.
type variant struct {
	name string
	cfg  memory.ScoringConfig
}

// modeVariants compares the ways of building the tsquery, holding everything
// else at its default.
func modeVariants() []variant {
	var out []variant
	for _, mode := range []memory.FullTextMode{
		memory.FullTextAny, // current behaviour
		memory.FullTextAll,
		memory.FullTextWebsearch,
		memory.FullTextOff,
	} {
		cfg := memory.DefaultScoringConfig
		cfg.FullText = mode
		out = append(out, variant{name: string(mode), cfg: cfg})
	}
	return out
}

// sweepVariants explores the fusion balance: how much the lexical half counts
// relative to the vector half, and how sharply RRF prefers top ranks.
func sweepVariants() []variant {
	var out []variant
	for _, k := range []int{5, 10, 20, 60} {
		for _, w := range []float64{0.5, 0.75, 1.0, 1.5} {
			cfg := memory.DefaultScoringConfig
			cfg.RRFConstant = k
			cfg.FullTextWeight = w
			out = append(out, variant{name: fmt.Sprintf("k=%-2d w=%.2f", k, w), cfg: cfg})
		}
	}

	// The metadata boosts are meant to be tiebreakers. Their size only makes
	// sense relative to the RRF terms, which k scales, so check they are not
	// steering the ranking at whichever k wins.
	for _, k := range []int{10, 60} {
		bare := memory.DefaultScoringConfig
		bare.RRFConstant = k
		bare.ImportanceWeight, bare.RecencyWeight, bare.AccessWeight = 0, 0, 0
		out = append(out, variant{name: fmt.Sprintf("k=%-2d no-meta", k), cfg: bare})
	}
	return out
}

type result struct {
	name   string
	mrr    float64
	recall map[int]float64
	misses int
}

func evaluate(ctx context.Context, store *storage.Postgres, embedder *embeddings.Ollama, cases []Case, topK int, variants []variant, baseline, candidate string) {
	cutoffs := []int{1, 3, 5, 10}

	// Embed once and reuse across variants: the vector half is identical, so
	// this keeps the comparison to the variables under test.
	vectors := make([][]float32, len(cases))
	for i, c := range cases {
		v, err := embedder.Embed(ctx, c.Query)
		if err != nil {
			log.Fatalf("embed %q: %v", c.Query, err)
		}
		vectors[i] = v
	}

	results := make([]result, 0, len(variants))
	rankByName := make(map[string][]int, len(variants))

	for _, v := range variants {
		cfg := v.cfg

		res := result{name: v.name, recall: map[int]float64{}}
		ranks := make([]int, len(cases))

		for i, c := range cases {
			hits, err := store.SearchHybrid(ctx, c.Query, vectors[i], topK, cfg)
			if err != nil {
				log.Fatalf("search (%s) %q: %v", v.name, c.Query, err)
			}

			rank := 0 // 0 means "not retrieved"
			for pos, h := range hits {
				if h.Memory.ID == c.target {
					rank = pos + 1
					break
				}
			}
			ranks[i] = rank

			if rank == 0 {
				res.misses++
				continue
			}
			res.mrr += 1 / float64(rank)
			for _, k := range cutoffs {
				if rank <= k {
					res.recall[k]++
				}
			}
		}

		n := float64(len(cases))
		res.mrr /= n
		for _, k := range cutoffs {
			res.recall[k] /= n
		}
		results = append(results, res)
		rankByName[v.name] = ranks
	}

	report(results, cutoffs)
	reportByOrigin(cases, variants, rankByName)

	// Default the diff to whichever variant scored best, so a sweep always
	// shows what adopting its winner would actually change.
	if candidate == "" {
		best := results[0]
		for _, r := range results[1:] {
			if r.mrr > best.mrr {
				best = r
			}
		}
		candidate = best.name
	}
	if candidate != baseline {
		reportRegressions(cases, rankByName, baseline, candidate)
	}
}

func report(results []result, cutoffs []int) {
	fmt.Printf("%-14s %8s", "variant", "MRR")
	for _, k := range cutoffs {
		fmt.Printf(" %8s", fmt.Sprintf("R@%d", k))
	}
	fmt.Printf(" %8s\n", "missed")
	fmt.Println(strings.Repeat("-", 14+9*(len(cutoffs)+2)))

	best := 0
	for i, r := range results {
		if r.mrr > results[best].mrr {
			best = i
		}
	}

	for i, r := range results {
		marker := "  "
		if i == best {
			marker = " *"
		}
		fmt.Printf("%-14s %8.3f", r.name+marker, r.mrr)
		for _, k := range cutoffs {
			fmt.Printf(" %8.3f", r.recall[k])
		}
		fmt.Printf(" %8d\n", r.misses)
	}
	fmt.Println("\n* best MRR")
}

// reportByOrigin splits the headline numbers by how each case was written.
// Generated questions echo the wording of their source memory and so flatter
// lexical matching; handwritten ones are the more trustworthy signal.
func reportByOrigin(cases []Case, variants []variant, rankByName map[string][]int) {
	origins := map[string]bool{}
	for _, c := range cases {
		origins[c.Origin] = true
	}
	if len(origins) < 2 {
		return
	}

	names := make([]string, 0, len(origins))
	for o := range origins {
		names = append(names, o)
	}
	sort.Strings(names)

	fmt.Println("\nby origin (MRR):")
	fmt.Printf("%-14s", "variant")
	for _, o := range names {
		fmt.Printf(" %14s", o)
	}
	fmt.Println()

	for _, v := range variants {
		fmt.Printf("%-14s", v.name)
		for _, o := range names {
			var sum, n float64
			for i, c := range cases {
				if c.Origin != o {
					continue
				}
				n++
				if r := rankByName[v.name][i]; r > 0 {
					sum += 1 / float64(r)
				}
			}
			if n == 0 {
				fmt.Printf(" %14s", "-")
				continue
			}
			fmt.Printf(" %14.3f", sum/n)
		}
		fmt.Println()
	}
}

// reportRegressions lists the queries a candidate handles differently from the
// baseline, so a headline win can be checked for hidden losses.
func reportRegressions(cases []Case, rankByName map[string][]int, baseline, candidate string) {
	before, after := rankByName[baseline], rankByName[candidate]
	if before == nil || after == nil {
		return
	}

	type change struct {
		query       string
		from, to    int
		improvement int
	}
	var worse, better []change

	for i, c := range cases {
		b, a := before[i], after[i]
		if b == a {
			continue
		}
		// Treat "not retrieved" as worse than any rank.
		norm := func(r int) int {
			if r == 0 {
				return 1 << 20
			}
			return r
		}
		ch := change{query: c.Query, from: b, to: a, improvement: norm(b) - norm(a)}
		if ch.improvement < 0 {
			worse = append(worse, ch)
		} else {
			better = append(better, ch)
		}
	}

	fmt.Printf("\n%s -> %s: %d improved, %d regressed\n", baseline, candidate, len(better), len(worse))
	if len(worse) == 0 {
		return
	}
	sort.Slice(worse, func(i, j int) bool { return worse[i].improvement < worse[j].improvement })
	fmt.Println("worst regressions:")
	for i, c := range worse {
		if i == 5 {
			break
		}
		fmt.Printf("  rank %s -> %s  %q\n", rankStr(c.from), rankStr(c.to), truncate(c.query, 70))
	}
}

func rankStr(r int) string {
	if r == 0 {
		return "miss"
	}
	return fmt.Sprintf("%d", r)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// --- labelled set handling ---

func loadSet(ctx context.Context, store *storage.Postgres, path string) ([]Case, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cases []Case
	if err := json.Unmarshal(raw, &cases); err != nil {
		return nil, err
	}

	all, err := store.ListMemories(ctx, memory.Filter{Limit: 500})
	if err != nil {
		return nil, fmt.Errorf("list memories: %w", err)
	}

	out := make([]Case, 0, len(cases))
	for _, c := range cases {
		switch {
		case c.MemoryID != "":
			id, err := uuid.Parse(c.MemoryID)
			if err != nil {
				return nil, fmt.Errorf("case %q: %w", c.Query, err)
			}
			c.target = id
		case c.Match != "":
			var found []*memory.Memory
			for _, m := range all {
				if strings.Contains(strings.ToLower(m.Content), strings.ToLower(c.Match)) {
					found = append(found, m)
				}
			}
			if len(found) != 1 {
				return nil, fmt.Errorf("case %q: match %q hit %d memories, want exactly 1",
					c.Query, c.Match, len(found))
			}
			c.target = found[0].ID
		default:
			return nil, fmt.Errorf("case %q: needs memory_id or match", c.Query)
		}
		if c.Origin == "" {
			c.Origin = "generated"
		}
		out = append(out, c)
	}
	return out, nil
}

const questionPrompt = `You are helping test a personal knowledge base's search.

Below is one memory stored about a user. Write the single most natural question
that person would ask to retrieve this memory later.

Rules:
- Write it as the user asking about their own life ("what bike do i ride?").
- Keep it short, as someone would actually type it.
- Do not copy long phrases from the memory. Use everyday wording where you can,
  though real names and product names are fine.
- Reply with ONLY the question.

Memory:
%s`

func generateSet(ctx context.Context, store *storage.Postgres, ollamaURL, model, outPath string, limit int) error {
	memories, err := store.ListMemories(ctx, memory.Filter{Limit: 500})
	if err != nil {
		return err
	}
	if limit > 0 && len(memories) > limit {
		memories = memories[:limit]
	}

	cases := make([]Case, 0, len(memories))
	for i, m := range memories {
		q, err := ask(ctx, ollamaURL, model, fmt.Sprintf(questionPrompt, m.Content))
		if err != nil {
			log.Printf("  [%d/%d] skipped: %v", i+1, len(memories), err)
			continue
		}
		q = firstLine(q)
		if q == "" {
			continue
		}
		cases = append(cases, Case{Query: q, MemoryID: m.ID.String(), Origin: "generated"})
		log.Printf("  [%d/%d] %s", i+1, len(memories), truncate(q, 70))
	}

	blob, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(outPath, blob, 0o600); err != nil {
		return err
	}
	log.Printf("wrote %d cases to %s", len(cases), outPath)
	return nil
}

func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return strings.Trim(line, `"`)
		}
	}
	return ""
}

func ask(ctx context.Context, baseURL, model, prompt string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":  model,
		"stream": false,
		"think":  false,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	})
	if err != nil {
		return "", err
	}

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("ollama status %d: %s", resp.StatusCode, msg)
	}

	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Message.Content, nil
}
