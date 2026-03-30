# Memex

A self-hosted MCP server that gives all your AI tools (Claude Desktop, Cursor, Claude Code, custom agents) a shared, persistent knowledge base. Every memory is embedded locally via Ollama, stored in PostgreSQL with pgvector, and connected automatically via a knowledge graph — all in a single database.

## Architecture

```
Your AI Clients  →  MCP Server (Go)  →  PostgreSQL + pgvector (vectors + graph + metadata)
                                      →  Ollama (embeddings, fully local)
                                      →  Anthropic API (relation extraction only)
```

**One database does everything.** pgvector handles semantic search alongside your relational data — no Qdrant, no separate vector store to manage.

## Features

### Smart Deduplication

Three-tier dedup prevents redundant memories without losing information:

| Tier | Condition | Action |
|------|-----------|--------|
| Exact match | SHA-256 content hash | Skip entirely — no embedding, no save |
| Merge zone | Cosine similarity 0.85–0.99 | LLM merges old + new content, preserving all unique facts |
| Near-identical | Cosine similarity ≥ 0.99 | Update in place |
| Distinct | Below 0.85 | Insert as new memory |

### Relevance Decay

Recall uses a hybrid scoring formula instead of pure cosine similarity:

```
final_score = 0.60 * cosine_similarity
            + 0.15 * importance              (normalized 1-5)
            + 0.15 * recency_decay           (exponential, 90-day half-life)
            + 0.10 * access_frequency         (log-scaled, saturates ~20 accesses)
```

- **Importance** (1-5): set at store time. 1 = trivia, 3 = normal, 5 = critical.
- **Recency**: newer memories score higher, decaying with a 90-day half-life.
- **Access frequency**: memories recalled more often stay relevant.

### Knowledge Graph

Every stored memory is automatically analyzed to extract entities and relationships. Cross-memory connections are discovered by comparing semantically similar memories. The graph is explorable via MCP tools and the web UI.

### Database Migrations

Schema changes are managed by [goose](https://github.com/pressly/goose) and run automatically on server startup. Migration files live in `migrations/`.

## Prerequisites

- Docker + Docker Compose
- An Anthropic API key (Haiku, for relationship extraction — ~$0.001 per memory)

## Setup

```bash
cp .env.example .env        # add ANTHROPIC_API_KEY and a strong MCP_AUTH_TOKEN
docker compose up -d        # starts Postgres+pgvector, Ollama, memex server
docker compose logs ollama-init -f   # wait for nomic-embed-text (~300MB, once)
curl http://localhost:3000/health    # → {"status":"ok"}
```

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
claude mcp add memex \
  --transport http \
  --url http://YOUR_SERVER_IP:3000/mcp \
  --header "Authorization: Bearer YOUR_MCP_AUTH_TOKEN"
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
| `recall_memory` | Semantic search with hybrid scoring (cosine + importance + recency + access frequency). |
| `get_context` | Formatted context block ready for injection into system prompts. |
| `list_memories` | List recent memories, filter by source/tags. |
| `delete_memory` | Remove a memory by ID. |
| `get_graph` | Explore the knowledge graph around any entity. |

## How It Works

1. `store_memory` called → content hashed for exact-dedup check → Ollama embeds locally → cosine similarity checked for merge/update/insert
2. Background goroutine → Anthropic Haiku extracts entities + relationships → graph grows in Postgres
3. Cross-connection: semantically similar memories are compared to discover shared entities
4. `recall_memory("backend preferences")` → pgvector ANN retrieval → re-ranked by hybrid scoring formula
5. `get_graph("Go")` → recursive CTE traverses the knowledge graph across all tools/sessions

## Web UI

Available at `http://localhost:3000/ui` (basic auth, credentials in `.env`).

- Browse and search memories with semantic search
- View memory details including importance, access count, and last accessed time
- Explore the interactive knowledge graph (D3.js)
- Delete memories

## Migrating to Qdrant Later

The `memory.Store` interface is the only seam. Implement it in `storage/qdrant.go`, swap in `main.go`. Data migration = `SELECT id, embedding FROM memories` → bulk upsert.
