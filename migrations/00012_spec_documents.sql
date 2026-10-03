-- FTR.HMR.CMN-0005: the index of specification documents of the default branch for
-- the navigator, search and the agent's spec_* tools.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE spec_documents (
  path          TEXT PRIMARY KEY,
  feature_key   TEXT NOT NULL,
  domain        TEXT NOT NULL,
  system        TEXT NOT NULL,
  area          TEXT NOT NULL,
  blob_sha      TEXT NOT NULL,
  commit_sha    TEXT NOT NULL,
  title         TEXT,
  toc           JSONB NOT NULL DEFAULT '[]',
  markdown      TEXT NOT NULL,
  search_text   TEXT NOT NULL,
  search_vector TSVECTOR NOT NULL,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX spec_documents_fts ON spec_documents USING GIN (search_vector);
CREATE INDEX spec_documents_feature ON spec_documents (feature_key, area);
CREATE INDEX spec_documents_system ON spec_documents (domain, system);
CREATE INDEX spec_documents_key_prefix ON spec_documents (lower(feature_key) text_pattern_ops);

CREATE TABLE spec_files (
  path        TEXT PRIMARY KEY,
  feature_key TEXT NOT NULL,
  area        TEXT NOT NULL,
  name        TEXT NOT NULL,
  blob_sha    TEXT NOT NULL,
  size_bytes  BIGINT NOT NULL,
  mime_type   TEXT NOT NULL,
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX spec_files_feature ON spec_files (feature_key, area);
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
BEGIN
  CREATE EXTENSION IF NOT EXISTS pg_trgm;
EXCEPTION WHEN insufficient_privilege THEN
  RAISE NOTICE 'pg_trgm is not available: ID search uses the prefix index';
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm') THEN
    EXECUTE 'CREATE INDEX spec_documents_key_trgm ON spec_documents USING GIN (feature_key gin_trgm_ops)';
  END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE spec_files;
DROP TABLE spec_documents;
-- pg_trgm stays: other objects may use it
-- +goose StatementEnd
