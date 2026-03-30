-- +goose Up

ALTER TABLE memories ADD COLUMN importance INTEGER NOT NULL DEFAULT 3;
ALTER TABLE memories ADD COLUMN access_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE memories ADD COLUMN last_accessed_at TIMESTAMPTZ;

-- +goose Down

ALTER TABLE memories DROP COLUMN IF EXISTS last_accessed_at;
ALTER TABLE memories DROP COLUMN IF EXISTS access_count;
ALTER TABLE memories DROP COLUMN IF EXISTS importance;
