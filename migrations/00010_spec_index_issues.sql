-- FTR.HMR.CMN-0005: specifications of the default branch that could not be
-- indexed, and indexed or released ones whose folder disappeared.
-- +goose Up
-- +goose StatementBegin
CREATE TABLE spec_index_issues (
  id            UUID        DEFAULT gen_random_uuid() PRIMARY KEY,
  path          TEXT        NOT NULL, -- folder of the feature: specs/<d>/<s>/<id>/
  feature_key   TEXT       , -- folder name
  kind          TEXT        NOT NULL CHECK (kind IN ('old_format', 'bad_format', 'key_path_mismatch', 'missing_domain', 'missing_system', 'missing_parent', 'deleted')),
  details       JSONB       DEFAULT '{}' NOT NULL,
  first_seen_at TIMESTAMPTZ DEFAULT now() NOT NULL,
  last_seen_at  TIMESTAMPTZ DEFAULT now() NOT NULL,
  resolved_at   TIMESTAMPTZ
);

CREATE UNIQUE INDEX spec_index_issues_open
  ON spec_index_issues(path, kind) WHERE resolved_at IS NULL;

CREATE INDEX spec_index_issues_kind
  ON spec_index_issues(kind) WHERE resolved_at IS NULL;

-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
DROP TABLE spec_index_issues;


-- +goose StatementEnd