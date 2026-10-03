-- FTR.HMR.CMN-0005: runs of the specification repository check. One queued row at
-- most: every request (schedule, "Check now", a new domain, a push) folds into it.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE spec_scan_runs (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  trigger      TEXT NOT NULL CHECK (trigger IN ('schedule', 'manual', 'catalog', 'push')),
  status       TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'succeeded', 'failed')),
  requested_by UUID REFERENCES users(id) ON DELETE SET NULL,
  not_before   TIMESTAMPTZ NOT NULL DEFAULT now(),
  commit_sha   TEXT,
  started_at   TIMESTAMPTZ,
  finished_at  TIMESTAMPTZ,
  found        INTEGER,
  indexed      INTEGER,
  issues       INTEGER,
  error        TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX spec_scan_one_queued ON spec_scan_runs ((true)) WHERE status = 'queued';
CREATE INDEX spec_scan_runs_created ON spec_scan_runs (created_at DESC);

INSERT INTO admin_settings (key, value) VALUES ('spec_scan', '{"interval":"1h"}')
ON CONFLICT (key) DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM admin_settings WHERE key = 'spec_scan';
DROP TABLE spec_scan_runs;
-- +goose StatementEnd
