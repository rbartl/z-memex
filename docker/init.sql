CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS memories (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    content     TEXT NOT NULL,
    summary     TEXT,
    source      TEXT,          -- e.g. "claude-desktop", "cursor", "custom-agent"
    session_id  TEXT,
    tags        TEXT[],
    embedding   vector(768),   -- nomic-embed-text output size
    created_at  TIMESTAMPTZ DEFAULT NOW(),
    updated_at  TIMESTAMPTZ DEFAULT NOW()
);

-- HNSW index for approximate nearest-neighbor search.
-- Unlike IVFFlat, HNSW doesn't need training data — it builds incrementally as rows are inserted.
CREATE INDEX IF NOT EXISTS idx_memories_embedding
    ON memories USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);

CREATE TABLE IF NOT EXISTS entities (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name        TEXT NOT NULL UNIQUE,
    entity_type TEXT,          -- e.g. "project", "person", "technology", "concept"
    created_at  TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS relations (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    from_entity   UUID REFERENCES entities(id) ON DELETE CASCADE,
    to_entity     UUID REFERENCES entities(id) ON DELETE CASCADE,
    relationship  TEXT NOT NULL,   -- e.g. "uses", "prefers", "worked-on", "relates-to"
    memory_id     UUID REFERENCES memories(id) ON DELETE SET NULL,
    confidence    FLOAT DEFAULT 1.0,
    created_at    TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(from_entity, to_entity, relationship)
);

CREATE TABLE IF NOT EXISTS memory_entities (
    memory_id   UUID REFERENCES memories(id) ON DELETE CASCADE,
    entity_id   UUID REFERENCES entities(id) ON DELETE CASCADE,
    PRIMARY KEY (memory_id, entity_id)
);

CREATE INDEX IF NOT EXISTS idx_memories_tags    ON memories USING GIN(tags);
CREATE INDEX IF NOT EXISTS idx_memories_source  ON memories(source);
CREATE INDEX IF NOT EXISTS idx_memories_created ON memories(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_relations_from   ON relations(from_entity);
CREATE INDEX IF NOT EXISTS idx_relations_to     ON relations(to_entity);
