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
score = 1/(k + vector_rank) + 1/(k + fulltext_rank)
      + importance_weight * (importance - 1) / 4
      + recency_weight   * exp(-λ * age_in_days)
      + access_weight    * min(1, ln(1 + access_count) / ln(21))
```

Default weights are intentionally small (`0.005`, `0.005`, `0.003`) so RRF dominates and metadata acts as a tiebreaker. The recency decay half-life is 90 days.

Vector search uses an **HNSW** index. Because an HNSW scan returns at most `hnsw.ef_search` rows, the server raises that per connection to match the candidate pool the hybrid query requests — otherwise the vector half of the ranking would be silently starved.

### Knowledge Graph

Every stored memory is automatically analyzed to extract entities and relationships. Cross-memory connections are discovered by comparing semantically similar memories. The graph is explorable via MCP tools and the web UI.

### Database Migrations

Schema changes are managed by [goose](https://github.com/pressly/goose) and run automatically on server startup. Migration files live in `migrations/`.

## Prerequisites

- Docker + Docker Compose
- **[Ollama](https://ollama.com) running on the host** (the server reaches it at `host.docker.internal:11434`), with the models below pulled
- An Anthropic API key — **only** if you set `EXTRACTOR=anthropic`

```bash
ollama pull nomic-embed-text   # embeddings — required, model name is fixed
ollama pull gemma4:26b         # relationship extraction (OLLAMA_EXTRACT_MODEL)
ollama pull llama3.2           # chat UI (OLLAMA_CHAT_MODEL)
```

The extraction and chat models are configurable — pull whatever you prefer and set the matching variable. Only `nomic-embed-text` is hardcoded, because the schema fixes the embedding dimension at 768.

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
| `UI_USER` | yes | `admin` | Web UI basic-auth user |
| `UI_PASS` | yes | `password` | Web UI basic-auth password — change this |
| `EXTRACTOR` | no | `ollama` | `ollama` or `anthropic` |
| `OLLAMA_EXTRACT_MODEL` | no | `gemma4:26b` | Model used for entity/relationship extraction |
| `OLLAMA_CHAT_MODEL` | no | `llama3.2` | Model backing the chat UI |
| `ANTHROPIC_API_KEY` | only if `EXTRACTOR=anthropic` | — | Extraction runs on Haiku, billed per memory |

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

Available at `http://localhost:3000/ui` (basic auth, credentials from `UI_USER` / `UI_PASS`).

- **Memories** — browse and search with semantic search, or open a memory for detail (importance, access count, last accessed, related memories grouped by shared entity)
- **Graph** — explore the interactive knowledge graph (D3.js), filtered by entity
- **Chat** — RAG chat against your knowledge base, streaming token-by-token with the retrieved memories cited as sources. Models that emit reasoning tokens show their thinking separately.
- **Setup** — client connection instructions

## Security

The MCP endpoint requires a bearer token; the web UI requires basic auth. Both are rate-limited per client IP, and request bodies are capped at 1 MiB. `/health` is deliberately open for uptime monitors.

The rate limiter identifies clients by the connection's peer address, which is correct when memex is exposed directly (as `docker-compose.yml` does) and cannot be spoofed. **If you put memex behind a reverse proxy, load balancer, or CDN, change it** — otherwise every request arrives wearing the proxy's address and shares one bucket, so one busy client locks out everyone else. See `RateLimitByClientIP` in `internal/httpmw`.

Chat markdown is rendered client-side and sanitized with DOMPurify, since model output is steered by knowledge-base content.

## Development

```bash
go test ./...          # unit tests
golangci-lint run ./...
npm install && npm run build   # re-vendor browser assets into web/static/vendor
```

The server resolves `migrations/` and `web/` relative to the working directory, so run it from the repository root.

## Migrating to Qdrant Later

The `memory.Store` interface is the only seam. Implement it in `storage/qdrant.go`, swap in `main.go`. Data migration = `SELECT id, embedding FROM memories` → bulk upsert.
