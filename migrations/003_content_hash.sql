-- +goose Up

ALTER TABLE memories ADD COLUMN content_hash TEXT;

CREATE UNIQUE INDEX idx_memories_content_hash ON memories(content_hash);

-- Backfill existing rows with SHA-256 of normalized content.
UPDATE memories SET content_hash = encode(sha256(convert_to(lower(trim(content)), 'UTF8')), 'hex')
WHERE content_hash IS NULL;

-- +goose Down

DROP INDEX IF EXISTS idx_memories_content_hash;
ALTER TABLE memories DROP COLUMN IF EXISTS content_hash;
