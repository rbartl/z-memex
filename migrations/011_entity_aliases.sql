-- +goose Up

-- Migrations 007 and 009 fold spellings of one name onto one node. They cannot
-- fold aliases, because an alias is not a spelling: "Max", "Maximilian",
-- "Maximilian Zollneritsch" and "User" share no characters to normalise, and
-- the corpus carried all four as separate people holding 91, 43, 40 and 36
-- edges. Nothing about the strings says they are one person -- only someone who
-- knows the domain can say that.
--
-- So identity gets a second, curated half. entity_aliases maps a normalised
-- name onto the entity that owns it; UpsertEntity consults it after failing to
-- match entities.name_norm, which makes every later extraction of an alias land
-- on the right node without another merge.
--
-- Rows arrive from a human merging two entities in the UI. Nothing writes here
-- automatically: auto-merging on name similarity is how two real people with
-- similar names become one.

CREATE TABLE IF NOT EXISTS entity_aliases (
    -- The alias is the key: one spelling can only ever mean one entity.
    alias_norm text PRIMARY KEY,
    entity_id  uuid NOT NULL REFERENCES entities(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_entity_aliases_entity ON entity_aliases(entity_id);

-- +goose Down

DROP TABLE IF EXISTS entity_aliases;
