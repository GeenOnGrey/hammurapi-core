-- FTR.HMR.CMN-0005: requirements of product specifications and references to
-- features and requirements, extracted from the documents of the index.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE spec_requirements (
  feature_key TEXT NOT NULL,
  req_id      TEXT NOT NULL,                    -- R3
  path        TEXT NOT NULL REFERENCES spec_documents(path) ON DELETE CASCADE,
  section     TEXT,
  text        TEXT NOT NULL,
  criteria    JSONB NOT NULL DEFAULT '[]',
  PRIMARY KEY (feature_key, req_id)
);

CREATE TABLE spec_references (
  id          BIGSERIAL PRIMARY KEY,
  source_path TEXT NOT NULL REFERENCES spec_documents(path) ON DELETE CASCADE,
  source_key  TEXT NOT NULL,
  area        TEXT NOT NULL,
  section     TEXT,
  target_key  TEXT NOT NULL,                    -- FTR.FMS.CAR-0002
  target_req  TEXT,                             -- R3 or NULL
  snippet     TEXT NOT NULL
);
CREATE INDEX spec_references_target ON spec_references (target_key, target_req);
CREATE INDEX spec_references_source ON spec_references (source_path);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE spec_references;
DROP TABLE spec_requirements;
-- +goose StatementEnd
