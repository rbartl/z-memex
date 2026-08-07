-- +goose Up

-- Replace the ivfflat vector index with HNSW.
--
-- ivfflat partitions vectors into `lists` centroids at build time, so the index
-- is only as good as the data present when it was built. Migration 001 creates
-- it on an empty table, which yields centroids that never reflect any real
-- data, and nothing ever rebuilds it as memories accumulate.
--
-- HNSW builds its graph incrementally as rows are inserted, so it has no
-- cold-start problem and needs no periodic REINDEX. It also gives better recall
-- at these dataset sizes. Requires pgvector >= 0.5.0.
--
-- Note that an HNSW index scan returns at most `hnsw.ef_search` rows; the
-- application raises that per connection to match the candidate pool that
-- hybrid search requests (see storage.NewPostgres).

DROP INDEX IF EXISTS idx_memories_embedding;

CREATE INDEX idx_memories_embedding
    ON memories USING hnsw (embedding vector_cosine_ops);

-- +goose Down

DROP INDEX IF EXISTS idx_memories_embedding;

CREATE INDEX idx_memories_embedding
    ON memories USING ivfflat (embedding vector_cosine_ops)
    WITH (lists = 100);
