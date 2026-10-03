-- FTR.HMR.CMN-0005: features found in the repository are indexed as implemented
-- (phase "indexed", source "repository"). Such a feature has no author, so
-- created_by becomes optional. Without a transaction: a value added to an
-- enum cannot be used in the transaction that adds it.

-- +goose NO TRANSACTION
-- +goose Up
ALTER TYPE feature_phase ADD VALUE IF NOT EXISTS 'indexed';

-- +goose StatementBegin
ALTER TABLE features
  ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'hammurapi' CHECK (source IN ('hammurapi', 'repository')),
  ADD COLUMN IF NOT EXISTS indexed_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS repo_deleted_at TIMESTAMPTZ,
  ALTER COLUMN created_by DROP NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE features
  DROP COLUMN IF EXISTS repo_deleted_at,
  DROP COLUMN IF EXISTS indexed_at,
  DROP COLUMN IF EXISTS source;
-- created_by stays optional (indexed rows may exist); the 'indexed' enum value
-- stays as well: PostgreSQL does not remove enum values.
-- +goose StatementEnd
