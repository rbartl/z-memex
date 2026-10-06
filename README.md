# Memex

A self-hosted MCP server that gives all your AI tools (Claude Desktop, Cursor, Claude Code, custom agents) a shared, persistent knowledge base. Every memory is embedded locally via Ollama, stored in PostgreSQL with pgvector, and connected automatically via a knowledge graph — all in a single database.

## Architecture

```
Your AI Clients  →  MCP Server (Go)  →  PostgreSQL + pgvector (vectors + graph + metadata)
                                     →  Ollama (embeddings, extraction, chat — all local)
                                     →  Anthropic API (optional alternative for extraction)
```

**One database does everything.** pgvector handles semantic search alongside your relational data — no Qdrant, no separate vector store to manage.

**Everything runs locally by default.** Embeddings, relationship extraction, and the chat UI all talk to your own Ollama instance. The Anthropic API is opt-in, and only for extraction.

## Features

### Smart Deduplication

Three-tier dedup prevents redundant memories without losing information:

| Tier | Condition | Action |
|------|-----------|--------|
| Exact match | SHA-256 hash of normalized content | Skip entirely — no embedding, no save |
| Near-identical | Cosine similarity ≥ 0.99 | Update in place |
| Merge zone | Cosine similarity 0.85–0.99 | LLM merges old + new content, preserving all unique facts |
| Distinct | Below 0.85 | Insert as new memory |

### Hybrid Scoring

Recall uses **Reciprocal Rank Fusion (RRF)** over two retrieval paths — pgvector cosine similarity and PostgreSQL full-text search — with small additive metadata boosts:

```
score = 1/(k + vector_rank) + fulltext_weight / (k + fulltext_rank)
      + importance_weight * (importance - 1) / 4
      + recency_weight   * exp(-λ * age_in_days)
      + access_weight    * min(1, ln(1 + access_count) / ln(21))
```

`fulltext_weight` defaults to `1.0` — the sweep found no reliable gain from moving it, but it is there to be re-measured.

The metadata weights are intentionally small (`0.005`, `0.005`, `0.003`) so RRF dominates and metadata only breaks ties. The recency decay half-life is 90 days.

Whether that holds depends entirely on `k`. RRF's classic `k = 60` is calibrated for fusing many systems over large TREC corpora; at this scale it flattened the fusion so far that the top four results for a query differed by `0.002` total, while the metadata boosts contribute up to `0.013` — six times the entire relevance spread. Ranking was decided by importance and recency rather than by relevance. `k = 10` restores the intended balance, and zeroing the boosts entirely now changes nothing measurable.

The lexical half ORs its terms rather than requiring all of them. That matches a large share of the corpus and looks careless, but natural questions rarely share every term with the memory that answers them, so requiring all terms leaves full-text silent and discards its contribution.

Both choices are measured, not assumed — see [Tuning retrieval](#tuning-retrieval).

Vector search uses an **HNSW** index. Because an HNSW scan returns at most `hnsw.ef_search` rows, the server raises that per connection to match the candidate pool the hybrid query requests — otherwise the vector half of the ranking would be silently starved.

### Knowledge Graph

Every stored memory is automatically analyzed to extract entities and relationships. Cross-memory connections are discovered by comparing semantically similar memories. The graph is explorable via MCP tools and the web UI.

Entity identity is the **normalized** name, not the literal one. Names come from an LLM, which spells the same thing differently between calls, so one node used to split four ways. `NormalizeName` folds all of it: case (`Acme` / `ACME`), a trailing gloss (`Go` / `Go (Golang)`), separator style (`acme-invoice-sync` / `Acme Invoice Sync`) and stray quoting (`'Sprint Review'`). It deliberately stops there — folding every punctuation mark would collapse `C++` and `C#` onto `c`. The first spelling seen is kept for display. Tags are folded the same way, since clients choose them freely. Migrations `007` and `009` backfill this and merge the duplicates they find, keeping the variant that carried the most memory links.

**Aliases** are the half of identity normalization cannot reach. `Max`, `Maximilian`, `Maximilian Zollneritsch` and `User` were one person holding 91, 40, 43 and 36 edges across four nodes; no amount of string folding says they are the same, only someone who knows the domain can. Merging two entities in the UI moves every edge and memory link onto the survivor and records the loser's name in `entity_aliases`, so later extractions of that name resolve to the survivor instead of splitting it again. Nothing writes aliases automatically — auto-merging on name similarity is how two real people with similar names become one.

Relationship predicates come from a **closed vocabulary** (`memory.Relationships`), for the same reason entity types do and with a worse symptom: "a short verb phrase" produced 503 distinct labels over 1272 edges, 341 used exactly once, with `holds`, `holds role` and `holds-role` sitting on one pair of nodes as three separate edges. Each predicate has one canonical direction, and the inverse spellings a model reaches for (`employs`, `used-by`, `led-by`) fold onto it with the endpoints swapped — which is what collapses the 111 facts that were stored in both directions at once. Symmetric predicates get a fixed endpoint order for the same reason. The model's own wording survives in `relations.relationship_raw`, so folding loses nothing. Migration `010` applies all of it to the existing corpus: 503 labels down to 28, self-edges gone, duplicate reciprocal edges gone.

Entity types come from a **closed vocabulary** (`memory.EntityTypes`): `person`, `organization`, `team`, `role`, `project`, `product`, `technology`, `tool`, `file`, `event`, `location`, `metric`, `concept`. The prompt is offered every type except `concept` (`memory.PromptEntityTypes`): naming the catch-all made it the cheap answer, and 35% of the corpus landed there. `concept` remains the internal fallback for a type the model coins anyway. The prompt used to end with "if none fit, invent a short lowercase singular noun", and validation only checked that a type looked like a slug, so the corpus reached 43 types — a useful core followed by `substance`, `timezone`, `cms`, `alias`. Migration `008` folds the historical values onto the list and adds a CHECK constraint, so adding a type now means writing a migration. A type outside the vocabulary is mapped to `concept` and logged rather than dropping the relation, since a mislabelled edge is a smaller loss than a missing one.

The prompt also now states what qualifies as an entity — a specific named thing that could plausibly appear in an unrelated note — and explicitly rejects the abstract noun phrases ("reduced overhead", "the attached spreadsheet") that made up much of the graph.

The entities already extracted under the old prompt are **hidden rather than deleted**. An entity is *weak* when it is typed `concept`, carries at most one edge, and is named by at most one memory — which described 169 of 828 nodes, the "layered approach" and "bulk assign" end of the corpus. The graph leaves those out by default; the Graph page has a *Show unconnected concepts* toggle that brings them back. Filtering rather than deleting matters because a genuinely new entity starts out looking exactly like a weak one, and earns its way in as soon as a second memory or a second relation arrives.

### Database Migrations

Schema changes are managed by [goose](https://github.com/pressly/goose) and run automatically on server startup. Migration files live in `migrations/`.

## Prerequisites

- Docker + Docker Compose
- **[Ollama](https://ollama.com) running on the host** (the server reaches it at `host.docker.internal:11434`), with the models below pulled
- An Anthropic API key — **only** if you set `EXTRACTOR=anthropic`

```bash
ollama pull bge-m3             # embeddings (EMBED_MODEL)
ollama pull gemma4:26b         # relationship extraction (OLLAMA_EXTRACT_MODEL)
ollama pull llama3.2           # chat UI (OLLAMA_CHAT_MODEL)
```

All three models are configurable. The embedding model is the constrained one: the schema declares a fixed vector width (1024 since migration `012`), so `EMBED_MODEL` may only name a model of that width — the server checks at startup and refuses to run on a mismatch rather than failing one insert at a time. Changing width means a migration; the corpus is then re-embedded automatically on the next boot.

`bge-m3` was chosen by measurement, not reputation. `cmd/rankeval` compared five local models against a copy of a real corpus on 26 hand-written questions: bge-m3 led on MRR (0.687 `any` / 0.804 `off`) and R@1 (0.73), missed nothing, and cut the share of unanswerable queries that outscore a 90%-recall threshold from 75% to 8%. `mxbai-embed-large` came close but has a 512-token context and rejects longer memories outright. Judge on hand-written questions only: on LLM-generated ones the models rank almost in reverse, because a generated question echoes the wording of the memory that answers it.

## Setup

```bash
cp .env.example .env        # set a strong MCP_AUTH_TOKEN and UI_PASS
docker compose up -d        # starts Postgres+pgvector and the memex server
docker compose logs -f memex         # watch migrations apply on first boot
curl http://localhost:3000/health    # → {"status":"ok"}
```

Postgres is published on `5433` for psql access; the server listens on `3000`.

## Configuration

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `POSTGRES_URL` | yes | set by compose | Postgres connection string |
| `OLLAMA_URL` | yes | set by compose | Ollama endpoint |
| `MCP_AUTH_TOKEN` | yes | — | Bearer token for `/mcp`. Compose refuses to start without it |
| `UI_USER` | yes | `admin` | Web UI login user |
| `UI_PASS` | yes | `password` | Web UI login password — change this |
| `EXTRACTOR` | no | `ollama` | `ollama` or `anthropic` |
| `EMBED_MODEL` | no | `bge-m3` | Embedding model. Must produce the width the schema declares (1024 since migration `012`) |
| `OLLAMA_EXTRACT_MODEL` | no | `gemma4:26b` | Model used for entity/relationship extraction |
| `OLLAMA_CHAT_MODEL` | no | `llama3.2` | Model backing the chat UI |
| `ANTHROPIC_API_KEY` | only if `EXTRACTOR=anthropic` | — | Extraction runs on Haiku, billed per memory |
| `EXTRACTION_WORKERS` | no | `1` | Background extraction jobs run concurrently. Raise only if the extractor backend can serve parallel calls |

Storing a memory returns as soon as the memory is saved; entity extraction and
cross-connection then run on a background queue. Each job is several LLM calls,
so the default of one worker keeps a single local Ollama from being swamped when
a client stores a batch of memories at once. The queue is unbounded and FIFO —
a burst waits its turn instead of being dropped, and the backlog shows up as the
`pending` field on the queue's debug log lines — and as a badge in the UI nav,
which polls `/ui/queue` every five seconds and shows how many jobs are still
outstanding. With `EXTRACTOR=anthropic`, or an
Ollama serving parallel requests, a higher `EXTRACTION_WORKERS` drains the
backlog faster.

## Connecting Clients

### Claude Desktop

`~/Library/Application Support/Claude/claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "memex": {
      "url": "http://YOUR_SERVER_IP:3000/mcp",
      "headers": { "Authorization": "Bearer YOUR_MCP_AUTH_TOKEN" }
    }
  }
}
```

### Claude Code (CLI)

```bash
claude mcp add memex --transport http --url http://YOUR_SERVER_IP:3000/mcp --header "Authorization: Bearer YOUR_MCP_AUTH_TOKEN"
```

### Cursor / VS Code / any MCP client

```json
{ "transport": "http", "url": "http://YOUR_SERVER_IP:3000/mcp",
  "headers": { "Authorization": "Bearer YOUR_MCP_AUTH_TOKEN" } }
```

## Available MCP Tools

| Tool | Description |
|---|---|
| `store_memory` | Store knowledge with optional importance (1-5). Auto-deduplicates and extracts entities + relationships in background. |
| `recall_memory` | Semantic search with hybrid RRF scoring. Returns ranked memories plus an aggregated knowledge graph of entities and relationships across the result set. `top_k` defaults to 5, capped at 100. |
| `manage_memories` | List and browse stored memories for housekeeping. Filter by source or tags. `limit` defaults to 20, capped at 500. |
| `delete_memory` | Remove a memory by ID. |

## How It Works

1. `store_memory` called → content hashed for exact-dedup check → Ollama embeds locally → cosine similarity checked for merge/update/insert
2. Background goroutine → the configured LLM extracts entities + relationships → graph grows in Postgres
3. Cross-connection: semantically similar memories are compared to discover shared entities
4. `recall_memory("backend preferences")` → RRF over pgvector ANN + full-text search → metadata boosts applied → ranked results returned with aggregated knowledge graph

Each memory records the client that stored it. On `initialize` the server assigns an `Mcp-Session-Id` and remembers that session's client name, so memories from Claude Code and Cursor stay distinguishable when both are connected. Clients that don't echo the session header are attributed by User-Agent instead.

## Web UI

Available at `http://localhost:3000/ui`. Unauthenticated visits land on a sign-in form; the credentials are `UI_USER` / `UI_PASS`.

- **Memories** — browse and search with semantic search, or open a memory for detail (importance, access count, last accessed, related memories grouped by shared entity)
- **Graph** — explore the interactive knowledge graph (D3.js), filtered by entity. Weak single-mention nodes are hidden unless *Show unconnected concepts* is ticked
- **Entities** — every node with its edge and memory counts, and the merge flow for folding two names for one thing into a single entity
- **Chat** — RAG chat against your knowledge base, streaming token-by-token with the retrieved memories cited as sources. Models that emit reasoning tokens show their thinking separately.

Each chat turn first asks the model to rewrite your question into a search query, so follow-ups resolve pronouns against the earlier conversation ("what does *he* work on?"). That rewrite is only ever an optimization: if the model returns nothing usable, your own words are used instead, and retrieval always runs. It used to be able to answer "no lookup needed" and skip retrieval altogether, which meant one uncooperative response silently hid the entire knowledge base from the model.

Note the rewrite can work against the lexical half of the search by trading your specific wording for generic terms. Sources retrieved in earlier turns are carried forward and merged, capped at the highest-scoring 12.
- **Setup** — client connection instructions

## Security

The MCP endpoint requires a bearer token; the web UI requires a login session. Both are rate-limited per client IP, and request bodies are capped at 1 MiB. `/health` and the landing page are deliberately open.

The UI login is a normal HTML form (`/ui/login`) that trades the credentials for an opaque 256-bit session token in an `HttpOnly`, `SameSite=Lax` cookie scoped to `/ui`, so it is never sent to the MCP endpoints. `Secure` is set when the request arrives over TLS (directly or through a proxy that sets `X-Forwarded-Proto`). Login posts get their own tight rate limit (10/min per IP) on top of the UI-wide budget, and `SameSite=Lax` is what stands in for CSRF tokens on the delete and chat endpoints.

Sessions live in the server's memory, so a restart signs you out — the tradeoff for keeping session state off the database on a single-user deployment. Each session clears two deadlines: an idle window of 7 days that slides forward on use, and an absolute lifetime of 30 days fixed at login and never extended, so a stolen cookie cannot be kept alive indefinitely by polling. **Signing out ends every session, not just the one in the browser you clicked from** — one person owns them all, so that is also the recovery path if you think a cookie has leaked. Signing in from a second device is additive and does not disturb the first.

Signed-in responses are sent `Cache-Control: no-store` so memory contents do not survive in the browser's history or disk cache after sign-out. Vendored static assets are the one exception (`private, max-age=3600`): they are public library code that happens to sit behind the gate.

The rate limiter identifies clients by the connection's peer address, which is correct when memex is exposed directly (as `docker-compose.yml` does) and cannot be spoofed. **If you put memex behind a reverse proxy, load balancer, or CDN, change it** — otherwise every request arrives wearing the proxy's address and shares one bucket, so one busy client locks out everyone else. See `RateLimitByClientIP` in `internal/httpmw`.

Chat markdown is rendered client-side and sanitized with DOMPurify, since model output is steered by knowledge-base content.

## Development

```bash
go test ./...          # unit tests
golangci-lint run ./...
npm install && npm run build   # re-vendor browser assets into web/static/vendor
```

The server resolves `migrations/` and `web/` relative to the working directory, so run it from the repository root.

The entity-merge tests in `internal/storage` write to a real database and are skipped unless `MEMEX_TEST_POSTGRES` names one. Point it at a throwaway copy, never the live database — a copy of the real corpus is the useful thing to test a migration against:

```bash
docker exec memex-postgres-1 psql -U memex -d postgres -c "CREATE DATABASE memex_test TEMPLATE memex"
```

### Continuous integration

`.gitlab-ci.yml` runs on every branch and merge request:

| Job | What it protects |
|---|---|
| `format` | `gofmt` cleanliness |
| `lint` | `golangci-lint run`, plus `config verify` — an unknown key makes `run` skip a setting silently rather than fail |
| `vulncheck` | `govulncheck`, non-blocking: advisories usually land against the Go image rather than this code |
| `assets` | `web/static/vendor` still matches `package-lock.json`, which has silently drifted before |
| `test` | `go test -race` with coverage |
| `smoke` | Migrations apply to a real pgvector database, templates parse, and the HTTP surface comes up behind its auth |
| `container` | The image still builds |

`smoke` is the one that catches what unit tests cannot — it boots the actual binary against a Postgres service and checks `/health`, the UI (rendered, and rejecting anonymous requests), and an MCP `tools/list`. No Ollama is needed, because the server only contacts it lazily.

`container` needs a Docker-capable runner; GitLab.com's shared runners qualify. On branches and merge requests it builds and discards the image. On `main`, `container:push` publishes `registry.gitlab.com/zollneritsch/memex:latest` plus a tag for the commit SHA.

### Releasing

Releases are cut by pushing a version tag. Versions follow [Semantic Versioning](https://semver.org/), and tags carry a `v` prefix (`v0.1.0`, or `v0.2.0-rc.1` for a pre-release).

There is no changelog to write. [git-cliff](https://git-cliff.org) builds the release notes from the commit messages since the previous release, so they depend on commits following [Conventional Commits](https://www.conventionalcommits.org/):

| Commit | Appears under |
|---|---|
| `feat:` | Features |
| `fix:` | Bug fixes |
| `perf:` | Performance |
| `feat!:`, or a `BREAKING CHANGE:` footer | Marked **Breaking** |
| no type, or `polish:` | Other changes |
| `chore:` `ci:` `docs:` `test:` `style:` `build:` `refactor:` | Left out |

The grouping lives in [cliff.toml](cliff.toml). A scope is shown in bold, as in `feat(graph): ...`.

1. On an up-to-date `main`, preview the notes (needs `brew install git-cliff`). The script fails if no commit since the previous release would appear in them.
   ```bash
   scripts/release-notes.sh v0.1.0
   ```
2. Tag the commit and push the tag:
   ```bash
   git tag -a v0.1.0 -m "memex v0.1.0"
   ```
   ```bash
   git push origin v0.1.0
   ```

The tag pipeline runs the full CI, plus three release jobs:

| Job | What it does |
|---|---|
| `release-notes` | Generates the notes. Runs first, so a tag with nothing to release fails before anything is published |
| `container:push` | Publishes `registry.gitlab.com/zollneritsch/memex:v0.1.0` (and the commit SHA tag). `:latest` keeps tracking `main` |
| `release` | Creates the [GitLab release](https://gitlab.com/zollneritsch/memex/-/releases) with the notes, a `docker pull` command, and a link to the container registry. The releases page is the changelog |

If the pipeline fails, fix the problem on `main`, then delete the tag (`git push origin :v0.1.0`) and tag again. Once a release is published, don't move its tag; release a new patch version instead.

### Tuning retrieval

`cmd/rankeval` measures retrieval quality so ranking changes can be compared instead of eyeballed. It runs a labelled set of questions through each scoring variant and reports MRR and recall at several cutoffs.

Point it at a **copy** of your data, never the live database — evaluation issues hundreds of queries, and `RecordAccess` would skew the very access-count boost being measured.

```bash
# clone production into a throwaway instance
docker run -d --rm --name memex-eval -e POSTGRES_USER=memex -e POSTGRES_PASSWORD=memex \
  -e POSTGRES_DB=memex -p 55432:5432 pgvector/pgvector:pg16
docker exec memex-postgres-1 pg_dump -U memex -d memex --no-owner \
  | docker exec -i memex-eval psql -U memex -d memex

go run ./cmd/rankeval -generate -out eval.json   # label a question per memory
go run ./cmd/rankeval -set eval.json             # compare tsquery modes
go run ./cmd/rankeval -set eval.json -sweep      # sweep the fusion balance
```

`-generate` asks a local model to write the question each memory answers. Those questions echo the wording of their source and so flatter lexical matching, which is why the report also breaks results down by origin: add hand-written cases (`{"query": "...", "match": "distinctive substring", "origin": "handwritten"}`) and trust those more. `match` resolves to a memory ID at load time and must hit exactly one memory, so a set survives re-cloning the database.

The generated file contains real memory text and is gitignored.

## Migrating to Qdrant Later

The `memory.Store` interface is the only seam. Implement it in `storage/qdrant.go`, swap in `main.go`. Data migration = `SELECT id, embedding FROM memories` → bulk upsert.

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
