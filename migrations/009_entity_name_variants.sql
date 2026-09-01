-- +goose Up

-- Migration 007 made identity case-insensitive, which fixed "Acme" beside
-- "ACME". Three further spelling habits survived it, and each one splits a
-- node the same way:
--
--   * a trailing gloss    -- "Go" beside "Go (Golang)", "TLS" beside "TLS (Transport Layer Security)"
--   * separator style     -- "acme-invoice-sync" beside "Acme Invoice Sync"
--   * stray quoting       -- "'Sprint Review'" beside "Sprint Review"
--
-- name_norm is recomputed with those folded in. The expression must match
-- memory.NormalizeName; note what it deliberately does NOT do — only a TRAILING
-- parenthetical goes, and only hyphen/underscore/slash count as separators, so
-- "C++" and "C#" keep their distinct identities instead of both becoming "c".
--
-- The unique index comes off first: the update collides by design, and the
-- merge below is what resolves the collisions.

DROP INDEX IF EXISTS entities_name_norm_key;

UPDATE entities
SET name_norm = btrim(
    regexp_replace(
        regexp_replace(
            -- A name that is nothing but a parenthetical keeps it; stripping
            -- would leave no identity at all.
            CASE
                WHEN btrim(regexp_replace(lower(btrim(name)), '\s*\([^()]*\)\s*$', '', 'g')) = ''
                    THEN lower(btrim(name))
                ELSE regexp_replace(lower(btrim(name)), '\s*\([^()]*\)\s*$', '', 'g')
            END,
        '[-_/]+', ' ', 'g'),
    '\s+', ' ', 'g'),
' ''"`');

-- From here this is migration 007's merge, re-run against the new key: elect the
-- variant carrying the most memory links, repoint everything at it, drop the
-- losers. Kept verbatim in shape so the two read as the same operation.
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

INSERT INTO memory_entities (memory_id, entity_id)
SELECT me.memory_id, d.canonical_id
FROM memory_entities me
JOIN entity_dupes d ON me.entity_id = d.dup_id
ON CONFLICT DO NOTHING;

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

CREATE UNIQUE INDEX entities_name_norm_key ON entities (name_norm);

-- +goose Down

-- Merging is not reversible. Down restores the 007 key expression over whatever
-- names remain, so the index still matches the column it guards.
DROP INDEX IF EXISTS entities_name_norm_key;
UPDATE entities SET name_norm = lower(btrim(regexp_replace(name, '\s+', ' ', 'g')));
CREATE UNIQUE INDEX entities_name_norm_key ON entities (name_norm);
