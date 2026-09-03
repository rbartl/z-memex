-- +goose Up

-- The embedding model changes from nomic-embed-text (768) to bge-m3 (1024).
--
-- Measured with cmd/rankeval against a copy of the live corpus, on 26
-- hand-written natural questions -- the set that matters, since LLM-generated
-- questions echo their source memory's wording and rank models wrongly:
--
--   model                 MRR(any)  MRR(off)  R@1   misses  floor leakage
--   bge-m3                0.687     0.804     0.73  0       8%
--   mxbai-embed-large     0.632     0.767     0.65  2       8%
--   nomic-embed-text      0.647     0.727     0.65  3       75%
--
-- "floor leakage" is the share of deliberately unanswerable queries that still
-- outscore the threshold keeping 90% of real answers: 75% under the old model,
-- 8% under bge-m3. That is the difference between a knowledge base that can
-- tell "no match" from "a match" and one that cannot.
--
-- Vectors from two models are not comparable, so every embedding is discarded
-- rather than converted. The server re-embeds the corpus at startup before it
-- serves anything (Service.BackfillEmbeddings), which for a base of this size
-- is a slow boot rather than an outage.

DROP INDEX IF EXISTS idx_memories_embedding;

UPDATE memories SET embedding = NULL;

ALTER TABLE memories ALTER COLUMN embedding TYPE vector(1024);

CREATE INDEX idx_memories_embedding
    ON memories USING hnsw (embedding vector_cosine_ops);

-- +goose Down

-- Equally destructive in reverse: the 768-dimension vectors are gone, and
-- coming back means re-embedding with whatever model EMBED_MODEL then names.
DROP INDEX IF EXISTS idx_memories_embedding;
UPDATE memories SET embedding = NULL;
ALTER TABLE memories ALTER COLUMN embedding TYPE vector(768);
CREATE INDEX idx_memories_embedding
    ON memories USING hnsw (embedding vector_cosine_ops);
