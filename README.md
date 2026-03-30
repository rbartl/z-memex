# memex — Personal AI Knowledge MCP Server

A self-hosted MCP server that gives all your AI tools (Claude Desktop, Cursor, Claude Code, custom agents) a shared, persistent knowledge base. Every memory is embedded locally via Ollama, stored in PostgreSQL with pgvector, and connected automatically via a knowledge graph — all in a single database.

## Architecture

```
Your AI Clients  →  MCP Server (Go)  →  PostgreSQL + pgvector (vectors + graph + metadata)
                                      →  Ollama (embeddings, fully local)
                                      →  Anthropic API (relation extraction only)
```

**One database does everything.** pgvector handles semantic search alongside your relational data — no Qdrant, no separate vector store to manage.

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
| `store_memory` | Store knowledge. Auto-extracts entities + relationships in background. |
| `recall_memory` | Semantic search via pgvector cosine similarity. |
| `get_context` | Formatted context block ready for injection into system prompts. |
| `list_memories` | List recent memories, filter by source/tags. |
| `delete_memory` | Remove a memory by ID. |
| `get_graph` | Explore the knowledge graph around any entity. |

## How Automatic Connections Work

1. `store_memory` called → Ollama embeds locally → saved to `memories.embedding vector(768)`
2. Background goroutine → Anthropic Haiku extracts entities + relationships
3. Graph grows in Postgres: `Max --prefers--> Go`, `schriebl --uses--> WordPress`
4. `recall_memory("backend preferences")` → pgvector `<=>` cosine search, single SQL query
5. `get_graph("Go")` → recursive CTE traverses the graph across all tools/sessions

## Backups

```bash
# Everything in one shot — memories, graph, embeddings
docker exec memex-postgres-1 pg_dump -U memex memex | gzip > backup-$(date +%F).sql.gz

# Restore
gunzip -c backup.sql.gz | docker exec -i memex-postgres-1 psql -U memex memex
```

## Migrating to Qdrant Later

The `memory.Store` interface is the only seam. Implement it in `storage/qdrant.go`, swap
in `main.go`. Data migration = `SELECT id, embedding FROM memories` → bulk upsert. ~2-3h work.
