-- +goose Up

ALTER TABLE memories ADD COLUMN IF NOT EXISTS content_tsv tsvector;

UPDATE memories
SET content_tsv = to_tsvector('english', coalesce(content, '') || ' ' || coalesce(summary, ''))
WHERE content_tsv IS NULL;

CREATE INDEX IF NOT EXISTS idx_memories_content_tsv ON memories USING GIN(content_tsv);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION memories_tsv_trigger() RETURNS trigger AS $$
BEGIN
  NEW.content_tsv := to_tsvector('english', coalesce(NEW.content, '') || ' ' || coalesce(NEW.summary, ''));
  RETURN NEW;
END
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS tsvector_update ON memories;
CREATE TRIGGER tsvector_update BEFORE INSERT OR UPDATE ON memories
FOR EACH ROW EXECUTE FUNCTION memories_tsv_trigger();

-- +goose Down

DROP TRIGGER IF EXISTS tsvector_update ON memories;
DROP FUNCTION IF EXISTS memories_tsv_trigger();
DROP INDEX IF EXISTS idx_memories_content_tsv;
ALTER TABLE memories DROP COLUMN IF EXISTS content_tsv;
