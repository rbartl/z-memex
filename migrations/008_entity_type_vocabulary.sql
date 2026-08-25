-- +goose Up

-- The extraction prompt used to end with "if none fit, invent a short lowercase
-- singular noun", and validEntityType only checked that a type was slug-shaped.
-- So the corpus accumulated 43 types: a useful core, then a long tail of
-- "substance", "layout", "alias", "timezone", "cms", "case" -- labels that
-- appear once or twice and classify nothing.
--
-- The vocabulary is now closed (memory.EntityTypes) and the prompt names it
-- explicitly. This folds the historical values onto it. Types not named below
-- fall to 'concept', which is the intended catch-all rather than a failure.
--
-- The CHECK at the bottom must stay in step with memory.EntityTypes; a mismatch
-- fails inserts loudly, which is the right failure mode for a closed list.

UPDATE entities
SET entity_type = CASE
    WHEN entity_type IS NULL THEN 'concept'
    WHEN lower(btrim(entity_type)) IN (
        'person','organization','team','role','project','product',
        'technology','tool','file','event','location','metric','concept'
    ) THEN lower(btrim(entity_type))
    ELSE COALESCE(
        (SELECT m.canon
         FROM (VALUES
             -- documents and artifacts
             ('asset','file'), ('script','file'), ('directory','file'),
             ('document','file'), ('report','file'), ('template','file'),
             -- the technical stack
             ('platform','technology'), ('model','technology'),
             ('library','technology'), ('language','technology'),
             ('component','technology'), ('class','technology'),
             ('function','technology'), ('endpoint','technology'),
             ('database','technology'), ('cms','technology'),
             ('server','technology'), ('framework','technology'),
             -- things that do work for you
             ('agent','tool'), ('software','tool'), ('bot','tool'),
             -- people and groups
             ('department','team'), ('group','team'),
             ('company','organization'), ('client','organization'),
             -- efforts and outputs
             ('initiative','project'), ('program','project'),
             ('application','product'), ('app','product'),
             ('service','product'), ('system','product')
         ) AS m(alias, canon)
         WHERE m.alias = lower(btrim(entity_type))),
        'concept'
    )
END;

ALTER TABLE entities ALTER COLUMN entity_type SET DEFAULT 'concept';
ALTER TABLE entities ALTER COLUMN entity_type SET NOT NULL;

ALTER TABLE entities ADD CONSTRAINT entities_entity_type_check CHECK (
    entity_type IN (
        'person','organization','team','role','project','product',
        'technology','tool','file','event','location','metric','concept'
    )
);

-- +goose Down

-- The original per-entity labels are not recoverable; only the constraints come
-- off, leaving the folded values in place.
ALTER TABLE entities DROP CONSTRAINT IF EXISTS entities_entity_type_check;
ALTER TABLE entities ALTER COLUMN entity_type DROP NOT NULL;
ALTER TABLE entities ALTER COLUMN entity_type DROP DEFAULT;
