-- +goose Up

-- Entity names come from an LLM, which capitalises the same thing differently
-- from call to call. entities_name_key is UNIQUE on name, and Postgres compares
-- text case-sensitively, so "Acme" and "ACME" became two nodes by
-- construction -- as did three spellings of "Platform Team". Every fact
-- about the thing was then split across those nodes, and a traversal arriving
-- at one variant could not see anything attached to the other.
--
-- Identity moves to name_norm (lowercased, whitespace-collapsed). name stays as
-- the display spelling. The expression here must match memory.NormalizeName.

ALTER TABLE entities ADD COLUMN name_norm text;

UPDATE entities SET name_norm = lower(btrim(regexp_replace(name, '\s+', ' ', 'g')));

-- Elect one surviving row per normalised name: the variant carrying the most
-- memory links, since that is the spelling most of the corpus already uses.
-- created_at then id break ties so the choice is deterministic on re-runs.
CREATE TEMP TABLE entity_dupes AS
WITH ranked AS (
    SELECT e.id,
           ROW_NUMBER() OVER w  AS rn,
           first_value(e.id) OVER w AS canonical_id
    FROM entities e
    WINDOW w AS (
        PARTITION BY e.name_norm
        ORDER BY (SELECT count(*) FROM memory_entities me WHERE me.entity_id = e.id) DESC,
                 e.created_at,
                 e.id
    )
)
SELECT id AS dup_id, canonical_id FROM ranked WHERE rn > 1;

-- Repoint the memory links onto the survivor. The losers' own rows are removed
-- by ON DELETE CASCADE when the duplicate entities go, so only the additions
-- are needed here.
INSERT INTO memory_entities (memory_id, entity_id)
SELECT me.memory_id, d.canonical_id
FROM memory_entities me
JOIN entity_dupes d ON me.entity_id = d.dup_id
ON CONFLICT DO NOTHING;

-- Repoint relations the same way. Two relations that differed only by the
-- spelling of an endpoint collapse onto one key here, so they are grouped
-- before the insert: ON CONFLICT DO UPDATE refuses to touch the same row twice
-- within a single statement. The surviving row keeps the highest confidence and
-- the memory that claimed it.
--
-- A relation whose two ends collapse onto the same node is self-referential and
-- carries no information, so it is dropped rather than recreated.
INSERT INTO relations (from_entity, to_entity, relationship, memory_id, confidence)
SELECT x.from_entity,
       x.to_entity,
       x.relationship,
       (array_agg(x.memory_id ORDER BY x.confidence DESC))[1],
       max(x.confidence)
FROM (
    SELECT COALESCE(df.canonical_id, r.from_entity) AS from_entity,
           COALESCE(dt.canonical_id, r.to_entity)   AS to_entity,
           r.relationship,
           r.memory_id,
           r.confidence
    FROM relations r
    LEFT JOIN entity_dupes df ON r.from_entity = df.dup_id
    LEFT JOIN entity_dupes dt ON r.to_entity   = dt.dup_id
    WHERE df.dup_id IS NOT NULL OR dt.dup_id IS NOT NULL
) x
WHERE x.from_entity <> x.to_entity
GROUP BY x.from_entity, x.to_entity, x.relationship
ON CONFLICT (from_entity, to_entity, relationship)
DO UPDATE SET confidence = GREATEST(relations.confidence, EXCLUDED.confidence);

DELETE FROM entities WHERE id IN (SELECT dup_id FROM entity_dupes);

DROP TABLE entity_dupes;

ALTER TABLE entities ALTER COLUMN name_norm SET NOT NULL;

-- name is display-only from here on; keeping a second uniqueness rule over it
-- would suggest it still carries identity.
ALTER TABLE entities DROP CONSTRAINT IF EXISTS entities_name_key;
CREATE UNIQUE INDEX entities_name_norm_key ON entities (name_norm);

-- Tags carry the same split ("acme" 30 rows, "ACME" 6). Sorting is a
-- side effect of de-duplicating in SQL and makes the stored order deterministic.
UPDATE memories
SET tags = COALESCE((
        SELECT array_agg(DISTINCT s.t ORDER BY s.t)
        FROM (SELECT lower(btrim(regexp_replace(x, '\s+', ' ', 'g'))) AS t
              FROM unnest(tags) AS x) s
        WHERE s.t <> ''
    ), '{}')
WHERE tags IS NOT NULL;

-- +goose Down

-- Merging is not reversible: rows folded into a survivor cannot be split back
-- out. Down only removes the new identity column, restoring the case-sensitive
-- constraint over whatever names remain.
DROP INDEX IF EXISTS entities_name_norm_key;
ALTER TABLE entities DROP COLUMN name_norm;
ALTER TABLE entities ADD CONSTRAINT entities_name_key UNIQUE (name);
