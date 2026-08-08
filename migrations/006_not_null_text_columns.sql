-- +goose Up

-- summary, source and session_id are scanned into plain Go strings, so a single
-- NULL in any of them fails every read path — list, get, and both search
-- queries. SaveMemory already writes empty strings rather than NULL, so this
-- constraint matches how the data is actually produced; it just stops a row
-- inserted by hand or by a future code path from wedging reads.
--
-- content_hash deliberately stays nullable: it carries a UNIQUE index, and NULL
-- is what allows several rows to coexist without a hash (empty strings would
-- collide). Read queries COALESCE it instead.

UPDATE memories SET summary    = '' WHERE summary    IS NULL;
UPDATE memories SET source     = '' WHERE source     IS NULL;
UPDATE memories SET session_id = '' WHERE session_id IS NULL;

ALTER TABLE memories
    ALTER COLUMN summary    SET DEFAULT '',
    ALTER COLUMN summary    SET NOT NULL,
    ALTER COLUMN source     SET DEFAULT '',
    ALTER COLUMN source     SET NOT NULL,
    ALTER COLUMN session_id SET DEFAULT '',
    ALTER COLUMN session_id SET NOT NULL;

-- +goose Down

ALTER TABLE memories
    ALTER COLUMN summary    DROP NOT NULL,
    ALTER COLUMN summary    DROP DEFAULT,
    ALTER COLUMN source     DROP NOT NULL,
    ALTER COLUMN source     DROP DEFAULT,
    ALTER COLUMN session_id DROP NOT NULL,
    ALTER COLUMN session_id DROP DEFAULT;
