-- +goose Up
-- +goose StatementBegin
-- Hammurapi schema: HMR.CMN-0001 (MVP) + HMR.CMN-0002 (closed development cycle).
-- The MVP was never in production, so its migrations were rewritten in place.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- ─── Enums ─────────────────────────────────────────────────────────
CREATE TYPE area AS ENUM ('product', 'design', 'arch', 'tech', 'qa');
CREATE TYPE expert_kind AS ENUM ('product', 'technical');
CREATE TYPE agent_tone AS ENUM ('business', 'friendly', 'concise', 'mentor');
CREATE TYPE catalog_source AS ENUM ('manual', 'backstage');
CREATE TYPE autonomy_level AS ENUM ('plan', 'pr', 'autonomous');
CREATE TYPE issue_type AS ENUM ('idea', 'problem');
CREATE TYPE issue_source AS ENUM ('manual', 'hammurapi_analysis', 'jira', 'feedback', 'prod_errors');
CREATE TYPE issue_status AS ENUM ('new', 'discovery', 'verification', 'accepted', 'resolved', 'rejected', 'merged');
CREATE TYPE feature_phase AS ENUM ('spec', 'codegen', 'validation', 'in_release', 'released', 'rolled_back', 'deleted');
CREATE TYPE gate_status AS ENUM ('draft', 'in_review', 'approved');
CREATE TYPE gate_event_type AS ENUM ('created', 'edited', 'submitted', 'approved', 'reset', 'deleted', 'generated');
CREATE TYPE chat_role AS ENUM ('user', 'agent');
CREATE TYPE chat_mode AS ENUM ('general', 'spec');
CREATE TYPE rule_file AS ENUM ('template', 'fix_template');
CREATE TYPE rule_change_status AS ENUM ('open', 'merged', 'withdrawn');
CREATE TYPE import_status AS ENUM ('validated', 'running', 'done', 'failed', 'cancelled');
CREATE TYPE import_item_status AS ENUM ('pending', 'skipped', 'imported', 'failed');
CREATE TYPE task_type AS ENUM ('implement', 'address_review', 'update_pr', 'revert', 'ci_setup');
CREATE TYPE task_status AS ENUM ('queued', 'running', 'succeeded', 'failed', 'cancelled');
CREATE TYPE pr_kind AS ENUM ('spec', 'service', 'revert');
CREATE TYPE pr_state AS ENUM ('open', 'merged', 'closed');
CREATE TYPE pr_review AS ENUM ('none', 'required', 'changes_requested', 'approved', 'not_required');
CREATE TYPE release_status AS ENUM (
  'merging', 'deploying', 'enabling_flags', 'evaluating',
  'awaiting_confirmation', 'succeeded', 'rolling_back', 'rolled_back'
);
CREATE TYPE deploy_env AS ENUM ('stage', 'production');
CREATE TYPE deploy_status AS ENUM ('triggered', 'started', 'success', 'failure', 'timeout');
CREATE TYPE deploy_signal AS ENUM ('pipeline', 'tag', 'manual');
CREATE TYPE workflow_kind AS ENUM (
  'discovery', 'codegen', 'codegen_task', 'validation', 'release', 'rollback', 'catalog_sync', 'gate_generation'
);

-- ─── Users and roles ───────────────────────────────────────────────
CREATE TABLE users (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  provider_uid  TEXT NOT NULL UNIQUE,
  username      TEXT NOT NULL UNIQUE,
  display_name  TEXT NOT NULL,
  avatar_url    TEXT,
  language      TEXT NOT NULL DEFAULT 'en' CHECK (language IN ('en','ru','de','es','zh-CN')),
  theme         TEXT NOT NULL DEFAULT 'light' CHECK (theme IN ('light','dark')),
  agent_name    TEXT NOT NULL,
  agent_tone    agent_tone NOT NULL,
  is_global_admin BOOLEAN NOT NULL DEFAULT false,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE user_git_tokens (
  user_id           UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  access_token_enc  BYTEA NOT NULL,
  refresh_token_enc BYTEA,
  expires_at        TIMESTAMPTZ NOT NULL,
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE user_sessions (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf_token   TEXT NOT NULL,
  expires_at   TIMESTAMPTZ NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON user_sessions (expires_at);

-- Area administrators edit the rules of their area (editor/approver roles are gone: HMR.CMN-0002 R39).
CREATE TABLE area_admins (
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  area    area NOT NULL,
  PRIMARY KEY (user_id, area)
);

-- ─── Catalog: domains, systems, services ───────────────────────────
CREATE TABLE domains (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  key        TEXT NOT NULL UNIQUE CHECK (key ~ '^[A-Z][A-Z0-9]{1,9}$'),
  name       TEXT NOT NULL,
  approval_required BOOLEAN NOT NULL DEFAULT true,
  source     catalog_source NOT NULL DEFAULT 'manual',
  catalog_name TEXT,
  catalog_ref  TEXT,
  deleted_in_catalog BOOLEAN NOT NULL DEFAULT false,
  last_issue_number INTEGER NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE systems (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  domain_id   UUID NOT NULL REFERENCES domains(id),
  key         TEXT NOT NULL CHECK (key ~ '^[A-Z][A-Z0-9]{1,9}$'),
  name        TEXT NOT NULL,
  last_number INTEGER NOT NULL DEFAULT 0,          -- features (FTR)
  last_release_number INTEGER NOT NULL DEFAULT 0,  -- releases (RLS)
  source      catalog_source NOT NULL DEFAULT 'manual',
  catalog_name TEXT,
  catalog_ref  TEXT,
  deleted_in_catalog BOOLEAN NOT NULL DEFAULT false,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (domain_id, key)
);

CREATE TABLE domain_experts (
  domain_id UUID NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
  user_id   UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind      expert_kind NOT NULL,
  PRIMARY KEY (domain_id, user_id, kind)
);
CREATE INDEX ON domain_experts (user_id, kind);

CREATE TABLE user_domains (
  user_id   UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  domain_id UUID NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
  PRIMARY KEY (user_id, domain_id)
);

CREATE TABLE services (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  key         TEXT NOT NULL UNIQUE,
  name        TEXT NOT NULL DEFAULT '',
  system_id   UUID REFERENCES systems(id),
  repo        TEXT NOT NULL,
  owner_ref   TEXT NOT NULL DEFAULT '',
  autonomy    autonomy_level NOT NULL DEFAULT 'pr',
  deploy_override JSONB,                           -- { "stage": {...}, "production": {...} }
  override_from_catalog BOOLEAN NOT NULL DEFAULT false,
  source      catalog_source NOT NULL DEFAULT 'manual',
  catalog_ref TEXT,
  deleted_in_catalog BOOLEAN NOT NULL DEFAULT false,
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE service_owners (
  service_id UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
  user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (service_id, user_id)
);

CREATE TABLE catalog_errors (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  kind        TEXT NOT NULL,
  name        TEXT NOT NULL,
  catalog_ref TEXT NOT NULL,
  reason      TEXT NOT NULL,
  seen_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (kind, name)
);

-- ─── Attachments (before issues reference them) ────────────────────
CREATE TABLE attachments (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  message_id UUID,
  file_name  TEXT NOT NULL,
  mime_type  TEXT NOT NULL,
  size_bytes BIGINT NOT NULL,
  s3_key     TEXT NOT NULL UNIQUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON attachments (user_id, created_at DESC);
CREATE INDEX ON attachments (created_at);

-- ─── Issues and Discovery ──────────────────────────────────────────
CREATE TABLE issues (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  key           TEXT NOT NULL UNIQUE,
  domain_id     UUID NOT NULL REFERENCES domains(id),
  number        INTEGER NOT NULL,
  type          issue_type NOT NULL,
  title         TEXT NOT NULL,
  description   TEXT NOT NULL,
  source        issue_source NOT NULL,
  source_ref    JSONB,
  status        issue_status NOT NULL DEFAULT 'new',
  author_id     UUID REFERENCES users(id),
  merged_into_id UUID REFERENCES issues(id),
  reject_reason TEXT,
  rolled_back_release_id UUID,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (domain_id, number)
);
CREATE INDEX ON issues (domain_id, status, created_at DESC);
CREATE INDEX issues_title_trgm ON issues USING gin (title gin_trgm_ops);

CREATE TABLE issue_aliases (
  alias_key TEXT PRIMARY KEY,
  issue_id  UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE
);

CREATE TABLE issue_attachments (
  issue_id      UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  attachment_id UUID NOT NULL REFERENCES attachments(id) ON DELETE CASCADE,
  PRIMARY KEY (issue_id, attachment_id)
);

CREATE TABLE discovery_docs (
  issue_id    UUID PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
  content     TEXT NOT NULL,
  value_text  TEXT,
  measure     JSONB,                             -- { source, query, target, window }
  measure_checked_at TIMESTAMPTZ,
  similar_items JSONB NOT NULL DEFAULT '[]',       -- [{ key, title, kind }]
  systems     JSONB NOT NULL DEFAULT '[]',       -- [ "FMS/CAR" ]
  services    JSONB NOT NULL DEFAULT '[]',       -- [ "booking" ]
  problem_feature_id UUID,
  revision    INTEGER NOT NULL DEFAULT 1,
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE discovery_revisions (
  issue_id  UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  revision  INTEGER NOT NULL,
  content   TEXT NOT NULL,
  is_agent  BOOLEAN NOT NULL,
  actor_id  UUID REFERENCES users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (issue_id, revision)
);

-- ─── Features (FTR) ────────────────────────────────────────────────
CREATE TABLE features (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  unique_id       TEXT NOT NULL UNIQUE,         -- FTR.FMS.CAR-0007
  system_id       UUID NOT NULL REFERENCES systems(id),
  number          INTEGER NOT NULL,
  title           TEXT NOT NULL,
  branch_name     TEXT NOT NULL,
  pr_number       INTEGER NOT NULL,             -- specification PR/MR
  pr_url          TEXT NOT NULL,
  phase           feature_phase NOT NULL DEFAULT 'spec',
  is_problem      BOOLEAN NOT NULL DEFAULT false,
  imported        BOOLEAN NOT NULL DEFAULT false,
  metric          JSONB,
  flag_key        TEXT,
  parent_id       UUID REFERENCES features(id),
  created_by      UUID NOT NULL REFERENCES users(id),
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_by      UUID REFERENCES users(id),
  deleted_at      TIMESTAMPTZ,
  branch_cleanup_pending BOOLEAN NOT NULL DEFAULT false,
  UNIQUE (system_id, number)
);
CREATE INDEX ON features (phase, created_at DESC);
CREATE INDEX ON features (parent_id);
CREATE INDEX features_title_trgm ON features USING gin (title gin_trgm_ops);

CREATE TABLE feature_issues (
  feature_id UUID NOT NULL REFERENCES features(id) ON DELETE CASCADE,
  issue_id   UUID NOT NULL REFERENCES issues(id),
  PRIMARY KEY (feature_id, issue_id)
);

CREATE TABLE feature_locks (
  feature_id UUID PRIMARY KEY REFERENCES features(id) ON DELETE CASCADE,
  locked_by  UUID NOT NULL REFERENCES users(id),
  locked_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE gates (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  feature_id      UUID NOT NULL REFERENCES features(id) ON DELETE CASCADE,
  area            area NOT NULL,
  status          gate_status NOT NULL DEFAULT 'draft',
  generated       BOOLEAN NOT NULL DEFAULT false,
  head_commit     TEXT NOT NULL,
  submitted_at    TIMESTAMPTZ,
  approved_commit TEXT,
  approved_by     UUID REFERENCES users(id),
  approved_at     TIMESTAMPTZ,
  created_by      UUID NOT NULL REFERENCES users(id),
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_by      UUID REFERENCES users(id),
  deleted_at      TIMESTAMPTZ
);
CREATE UNIQUE INDEX gates_active_area ON gates (feature_id, area) WHERE deleted_at IS NULL;
CREATE INDEX gates_in_review ON gates (area, submitted_at) WHERE status = 'in_review' AND deleted_at IS NULL;

CREATE TABLE gate_events (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  gate_id     UUID NOT NULL REFERENCES gates(id) ON DELETE CASCADE,
  event_type  gate_event_type NOT NULL,
  actor_id    UUID REFERENCES users(id),
  is_agent    BOOLEAN NOT NULL DEFAULT false,
  commit_sha  TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON gate_events (gate_id, created_at DESC);

CREATE TABLE feature_services (
  feature_id UUID NOT NULL REFERENCES features(id) ON DELETE CASCADE,
  service_id UUID NOT NULL REFERENCES services(id),
  PRIMARY KEY (feature_id, service_id)
);

CREATE TABLE requirements (
  feature_id UUID NOT NULL REFERENCES features(id) ON DELETE CASCADE,
  req_id     TEXT NOT NULL,
  text       TEXT NOT NULL,
  PRIMARY KEY (feature_id, req_id)
);

CREATE TABLE requirement_services (
  feature_id UUID NOT NULL,
  req_id     TEXT NOT NULL,
  service_id UUID NOT NULL REFERENCES services(id),
  PRIMARY KEY (feature_id, req_id, service_id),
  FOREIGN KEY (feature_id, req_id) REFERENCES requirements ON DELETE CASCADE
);

CREATE TABLE test_cases (
  feature_id UUID NOT NULL REFERENCES features(id) ON DELETE CASCADE,
  tc_id      TEXT NOT NULL,
  level      TEXT NOT NULL,
  req_ids    TEXT[] NOT NULL,
  title      TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (feature_id, tc_id)
);

-- ─── Code generation, PRs, validation ──────────────────────────────
CREATE TABLE agent_tasks (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  type        task_type NOT NULL,
  status      task_status NOT NULL DEFAULT 'queued',
  feature_id  UUID REFERENCES features(id),
  release_id  UUID,
  service_id  UUID NOT NULL REFERENCES services(id),
  workflow_run_id UUID NOT NULL,
  initiator_id UUID REFERENCES users(id),
  token_hash  BYTEA,
  executor_ref TEXT,
  input       JSONB NOT NULL,
  result      JSONB,
  progress    TEXT,
  error       TEXT,
  tokens_in   BIGINT NOT NULL DEFAULT 0,
  tokens_out  BIGINT NOT NULL DEFAULT 0,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  started_at  TIMESTAMPTZ,
  finished_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX agent_tasks_one_per_repo ON agent_tasks (service_id) WHERE status = 'running';
CREATE INDEX ON agent_tasks (feature_id);

CREATE TABLE pull_requests (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  repo        TEXT NOT NULL,
  number      INTEGER NOT NULL,
  url         TEXT NOT NULL,
  title       TEXT NOT NULL DEFAULT '',
  branch      TEXT NOT NULL DEFAULT '',
  kind        pr_kind NOT NULL,
  feature_id  UUID NOT NULL REFERENCES features(id),
  service_id  UUID REFERENCES services(id),
  by_agent    BOOLEAN NOT NULL,
  state       pr_state NOT NULL DEFAULT 'open',
  review      pr_review NOT NULL DEFAULT 'none',
  head_sha    TEXT NOT NULL,
  ci_status   TEXT,
  merge_sha   TEXT,
  merged_by   UUID REFERENCES users(id),
  merged_at   TIMESTAMPTZ,
  reverts_pr_id UUID REFERENCES pull_requests(id),
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (repo, number)
);
CREATE INDEX ON pull_requests (feature_id);

CREATE TABLE pr_requirements (
  pr_id  UUID NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
  req_id TEXT NOT NULL,
  PRIMARY KEY (pr_id, req_id)
);

CREATE TABLE ci_runs (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  repo         TEXT NOT NULL,
  sha          TEXT NOT NULL,
  branch       TEXT,
  environment  TEXT NOT NULL,
  pipeline_url TEXT,
  received_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON ci_runs (repo, sha);

CREATE TABLE test_results (
  ci_run_id UUID NOT NULL REFERENCES ci_runs(id) ON DELETE CASCADE,
  tc_id     TEXT NOT NULL,
  status    TEXT NOT NULL,
  duration_ms INTEGER,
  PRIMARY KEY (ci_run_id, tc_id)
);

CREATE TABLE discrepancies (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  feature_id  UUID NOT NULL REFERENCES features(id) ON DELETE CASCADE,
  req_id      TEXT NOT NULL,
  pr_id       UUID REFERENCES pull_requests(id),
  description TEXT NOT NULL,
  found_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  resolved_at TIMESTAMPTZ
);

CREATE TABLE validation_signatures (
  feature_id UUID NOT NULL REFERENCES features(id) ON DELETE CASCADE,
  side       expert_kind NOT NULL,
  user_id    UUID NOT NULL REFERENCES users(id),
  comment    TEXT,
  signed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (feature_id, side)
);

CREATE TABLE validation_returns (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  feature_id UUID NOT NULL REFERENCES features(id) ON DELETE CASCADE,
  target     TEXT NOT NULL CHECK (target IN ('code', 'spec')),
  comment    TEXT NOT NULL,
  user_id    UUID NOT NULL REFERENCES users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ─── Releases, deploys, flags, metrics ─────────────────────────────
CREATE TABLE releases (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  key         TEXT NOT NULL UNIQUE,
  system_id   UUID NOT NULL REFERENCES systems(id),
  number      INTEGER NOT NULL,
  feature_id  UUID NOT NULL UNIQUE REFERENCES features(id),
  status      release_status NOT NULL DEFAULT 'merging',
  plan        JSONB NOT NULL,                    -- { "order": [serviceKey, …] }
  merge_started_by UUID REFERENCES users(id),
  metric_result TEXT CHECK (metric_result IN ('achieved', 'not_achieved')),
  confirmed_by UUID REFERENCES users(id),
  confirmed_at TIMESTAMPTZ,
  rollback_reason TEXT,
  rolled_back_by UUID REFERENCES users(id),
  rolled_back_at TIMESTAMPTZ,
  blocked_reason TEXT,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (system_id, number)
);
CREATE INDEX ON releases (status, created_at DESC);

CREATE TABLE release_prs (
  release_id UUID NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
  pr_id      UUID NOT NULL REFERENCES pull_requests(id),
  position   SMALLINT NOT NULL,
  PRIMARY KEY (release_id, pr_id)
);

CREATE TABLE deploy_runs (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  environment deploy_env NOT NULL,
  service_id  UUID NOT NULL REFERENCES services(id),
  feature_id  UUID REFERENCES features(id),
  release_id  UUID REFERENCES releases(id),
  ref         TEXT NOT NULL,
  status      deploy_status NOT NULL DEFAULT 'triggered',
  signal      deploy_signal,
  version     TEXT,
  run_url     TEXT,
  error       TEXT,
  marked_by   UUID REFERENCES users(id),
  is_rollback BOOLEAN NOT NULL DEFAULT false,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at TIMESTAMPTZ
);
CREATE INDEX ON deploy_runs (release_id, service_id);
CREATE INDEX ON deploy_runs (feature_id, environment);

CREATE TABLE deploy_settings (
  environment deploy_env PRIMARY KEY,
  type        TEXT NOT NULL CHECK (type IN ('github-actions', 'gitlab-ci', 'webhook')),
  config      JSONB NOT NULL,                    -- workflow, ref, url, params, auth
  secret_refs TEXT[] NOT NULL,                   -- up to two active result secrets
  timeout_minutes INTEGER NOT NULL DEFAULT 60,
  updated_by  UUID REFERENCES users(id),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE flag_events (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  flag_key    TEXT NOT NULL,
  state       TEXT NOT NULL CHECK (state IN ('on', 'off')),
  environment TEXT NOT NULL,
  changed_at  TIMESTAMPTZ NOT NULL,
  actor       TEXT,
  source      TEXT NOT NULL CHECK (source IN ('webhook', 'manual')),
  received_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON flag_events (flag_key, changed_at);

CREATE TABLE metric_sources (
  name       TEXT PRIMARY KEY,
  type       TEXT NOT NULL CHECK (type IN ('clickhouse', 'prometheus')),
  endpoint   TEXT NOT NULL,
  username   TEXT,
  secret_ref TEXT NOT NULL DEFAULT '',
  limits     JSONB NOT NULL DEFAULT '{}'
);

CREATE TABLE metric_samples (
  release_id UUID NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
  at         TIMESTAMPTZ NOT NULL,
  value      DOUBLE PRECISION NOT NULL,
  PRIMARY KEY (release_id, at)
);

-- ─── Workflows (state machines), outbox, agent usage ───────────────
CREATE TABLE workflow_runs (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  kind         workflow_kind NOT NULL,
  subject_id   UUID NOT NULL,
  parent_id    UUID REFERENCES workflow_runs(id),
  state        TEXT NOT NULL,
  step         TEXT,
  context      JSONB NOT NULL DEFAULT '{}',
  attempt      INTEGER NOT NULL DEFAULT 0,
  next_run_at  TIMESTAMPTZ,
  locked_until TIMESTAMPTZ,
  locked_by    TEXT,
  last_error   TEXT,
  version      INTEGER NOT NULL DEFAULT 1,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX workflow_due ON workflow_runs (next_run_at)
  WHERE state NOT IN ('done', 'succeeded', 'failed', 'cancelled', 'rolled_back');
CREATE UNIQUE INDEX workflow_active_per_subject ON workflow_runs (kind, subject_id)
  WHERE state NOT IN ('done', 'succeeded', 'failed', 'cancelled', 'rolled_back');

CREATE TABLE workflow_events (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id       UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
  type         TEXT NOT NULL,
  payload      JSONB NOT NULL DEFAULT '{}',
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  processed_at TIMESTAMPTZ
);
CREATE INDEX ON workflow_events (run_id) WHERE processed_at IS NULL;

CREATE TABLE outbox (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id          UUID REFERENCES workflow_runs(id) ON DELETE CASCADE,
  effect          TEXT NOT NULL,
  payload         JSONB NOT NULL,
  idempotency_key TEXT NOT NULL UNIQUE,
  attempts        INTEGER NOT NULL DEFAULT 0,
  last_error      TEXT,
  next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  sent_at         TIMESTAMPTZ,
  failed_at       TIMESTAMPTZ
);
CREATE INDEX ON outbox (next_attempt_at) WHERE sent_at IS NULL AND failed_at IS NULL;

CREATE TABLE agent_usage (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  context     TEXT NOT NULL,
  issue_id    UUID REFERENCES issues(id) ON DELETE SET NULL,
  feature_id  UUID REFERENCES features(id) ON DELETE SET NULL,
  release_id  UUID REFERENCES releases(id) ON DELETE SET NULL,
  task_id     UUID REFERENCES agent_tasks(id) ON DELETE SET NULL,
  user_id     UUID REFERENCES users(id) ON DELETE SET NULL,
  model       TEXT,
  tokens_in   BIGINT NOT NULL,
  tokens_out  BIGINT NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- History of issues, features and releases beyond gate events: verification,
-- returns, signatures, release steps, rollback reasons.
CREATE TABLE activity (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  subject_type TEXT NOT NULL CHECK (subject_type IN ('issue', 'feature', 'release')),
  subject_id   UUID NOT NULL,
  type         TEXT NOT NULL,
  actor_id     UUID REFERENCES users(id),
  is_agent     BOOLEAN NOT NULL DEFAULT false,
  payload      JSONB NOT NULL DEFAULT '{}',
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON activity (subject_type, subject_id, created_at DESC);

-- ─── Chat ──────────────────────────────────────────────────────────
CREATE TABLE chat_messages (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role          chat_role NOT NULL,
  mode          chat_mode NOT NULL,
  context_type  TEXT CHECK (context_type IN ('issue', 'feature', 'release')),
  context_key   TEXT,
  area          area,
  content       TEXT NOT NULL,
  is_voice      BOOLEAN NOT NULL DEFAULT false,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (mode = 'general' OR context_key IS NOT NULL)
);
CREATE INDEX ON chat_messages (user_id, created_at DESC);
ALTER TABLE attachments ADD CONSTRAINT attachments_message FOREIGN KEY (message_id) REFERENCES chat_messages(id) ON DELETE SET NULL;

CREATE TABLE agent_sessions (
  user_id    UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  session_id TEXT NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ─── Rules, imports, service tables ────────────────────────────────
CREATE TABLE rule_changes (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  area        area NOT NULL,
  file        rule_file NOT NULL,
  branch_name TEXT NOT NULL,
  pr_number   INTEGER NOT NULL,
  pr_url      TEXT NOT NULL,
  comment     TEXT,
  status      rule_change_status NOT NULL DEFAULT 'open',
  author_id   UUID NOT NULL REFERENCES users(id),
  approved_by UUID REFERENCES users(id),
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  closed_at   TIMESTAMPTZ,
  CHECK (approved_by IS NULL OR approved_by <> author_id)
);
CREATE UNIQUE INDEX rule_changes_one_open ON rule_changes (area, file) WHERE status = 'open';

CREATE TABLE imports (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  s3_key      TEXT NOT NULL,
  status      import_status NOT NULL DEFAULT 'validated',
  preview     JSONB NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  started_at  TIMESTAMPTZ,
  finished_at TIMESTAMPTZ
);
CREATE INDEX ON imports (status, created_at);

CREATE TABLE import_items (
  import_id  UUID NOT NULL REFERENCES imports(id) ON DELETE CASCADE,
  archive_id TEXT NOT NULL,
  status     import_item_status NOT NULL DEFAULT 'pending',
  feature_id UUID REFERENCES features(id),
  error      TEXT,
  PRIMARY KEY (import_id, archive_id)
);

CREATE TABLE processed_webhook_events (
  provider_event_id TEXT PRIMARY KEY,
  processed_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE admin_settings (
  key        TEXT PRIMARY KEY,
  value      JSONB NOT NULL,
  updated_by UUID REFERENCES users(id),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO admin_settings (key, value) VALUES
  ('attachment_retention_days', '90'),
  ('feature_flags', '{"enabled": false, "secretRefs": []}'),
  ('stage', '{"enabled": false}'),
  ('catalog', '{"enabled": false, "catalogRepo": "", "catalogGlob": "**/catalog-info.yaml", "serviceFilePath": "catalog-info.yaml", "serviceRepos": []}');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP SCHEMA public CASCADE;
CREATE SCHEMA public;
-- +goose StatementEnd
