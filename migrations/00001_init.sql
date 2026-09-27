-- +goose Up
-- +goose StatementBegin
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- ─── Enums ─────────────────────────────────────────────────────────
CREATE TYPE area AS ENUM ('product', 'design', 'arch', 'tech', 'qa');
CREATE TYPE app_role AS ENUM ('admin', 'approver', 'editor');
CREATE TYPE agent_tone AS ENUM ('business', 'friendly', 'concise', 'mentor');
CREATE TYPE feature_status AS ENUM ('in_progress', 'handed_off', 'deleted');
CREATE TYPE gate_status AS ENUM ('draft', 'in_review', 'approved');
CREATE TYPE gate_event_type AS ENUM (
  'created', 'edited', 'submitted', 'approved', 'reset', 'deleted'
);
CREATE TYPE chat_role AS ENUM ('user', 'agent');
CREATE TYPE chat_mode AS ENUM ('general', 'spec');
CREATE TYPE rule_file AS ENUM ('template', 'fix_template');
CREATE TYPE rule_change_status AS ENUM ('open', 'merged', 'withdrawn');
CREATE TYPE import_status AS ENUM ('validated', 'running', 'done', 'failed', 'cancelled');
CREATE TYPE import_item_status AS ENUM ('pending', 'skipped', 'imported', 'failed');

-- ─── Users ─────────────────────────────────────────────────────────
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

-- ─── Roles ─────────────────────────────────────────────────────────
CREATE TABLE user_roles (
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role    app_role NOT NULL,
  area    area NOT NULL,
  PRIMARY KEY (user_id, role, area)
);
CREATE INDEX ON user_roles (role, area);

-- ─── Dictionary ────────────────────────────────────────────────────
CREATE TABLE domains (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  key        TEXT NOT NULL UNIQUE CHECK (key ~ '^[A-Z][A-Z0-9]{1,9}$'),
  name       TEXT NOT NULL,
  approval_required BOOLEAN NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE systems (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  domain_id   UUID NOT NULL REFERENCES domains(id),
  key         TEXT NOT NULL CHECK (key ~ '^[A-Z][A-Z0-9]{1,9}$'),
  name        TEXT NOT NULL,
  last_number INTEGER NOT NULL DEFAULT 0,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (domain_id, key)
);

CREATE TABLE user_domains (
  user_id   UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  domain_id UUID NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
  PRIMARY KEY (user_id, domain_id)
);

-- ─── Features ──────────────────────────────────────────────────────
CREATE TABLE features (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  unique_id       TEXT NOT NULL UNIQUE,
  system_id       UUID NOT NULL REFERENCES systems(id),
  number          INTEGER NOT NULL,
  title           TEXT NOT NULL,
  branch_name     TEXT NOT NULL,
  pr_number       INTEGER NOT NULL,
  pr_url          TEXT NOT NULL,
  status          feature_status NOT NULL DEFAULT 'in_progress',
  parent_id       UUID REFERENCES features(id),
  created_by      UUID NOT NULL REFERENCES users(id),
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  handed_off_by   UUID REFERENCES users(id),
  handed_off_at   TIMESTAMPTZ,
  handed_off_without_approval BOOLEAN NOT NULL DEFAULT false,
  deleted_by      UUID REFERENCES users(id),
  deleted_at      TIMESTAMPTZ,
  branch_cleanup_pending BOOLEAN NOT NULL DEFAULT false,
  UNIQUE (system_id, number)
);
CREATE INDEX ON features (status, created_at DESC);
CREATE INDEX ON features (parent_id);
CREATE INDEX features_title_trgm ON features USING gin (title gin_trgm_ops);

CREATE TABLE feature_locks (
  feature_id UUID PRIMARY KEY REFERENCES features(id) ON DELETE CASCADE,
  locked_by  UUID NOT NULL REFERENCES users(id),
  locked_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL
);

-- ─── Gates ─────────────────────────────────────────────────────────
CREATE TABLE gates (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  feature_id      UUID NOT NULL REFERENCES features(id) ON DELETE CASCADE,
  area            area NOT NULL,
  status          gate_status NOT NULL DEFAULT 'draft',
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

-- ─── Chat ──────────────────────────────────────────────────────────
CREATE TABLE chat_messages (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role          chat_role NOT NULL,
  mode          chat_mode NOT NULL,
  feature_id    UUID REFERENCES features(id),
  area          area,
  content       TEXT NOT NULL,
  is_voice      BOOLEAN NOT NULL DEFAULT false,
  gate_event_id UUID REFERENCES gate_events(id),
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (mode = 'general' OR feature_id IS NOT NULL)
);
CREATE INDEX ON chat_messages (user_id, created_at DESC);

-- ─── Attachments ───────────────────────────────────────────────────
CREATE TABLE attachments (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  message_id UUID REFERENCES chat_messages(id) ON DELETE SET NULL,
  file_name  TEXT NOT NULL,
  mime_type  TEXT NOT NULL,
  size_bytes BIGINT NOT NULL,
  s3_key     TEXT NOT NULL UNIQUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON attachments (user_id, created_at DESC);
CREATE INDEX ON attachments (created_at);

-- ─── Rule changes ──────────────────────────────────────────────────
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

-- ─── Archive imports ───────────────────────────────────────────────
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

-- ─── Service tables ────────────────────────────────────────────────
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
INSERT INTO admin_settings (key, value) VALUES ('attachment_retention_days', '90');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS admin_settings, processed_webhook_events, import_items, imports,
  rule_changes, attachments, chat_messages, gate_events, gates, feature_locks, features,
  user_domains, systems, domains, user_roles, user_sessions, user_git_tokens, users CASCADE;
DROP TYPE IF EXISTS import_item_status, import_status, rule_change_status, rule_file, chat_mode,
  chat_role, gate_event_type, gate_status, feature_status, agent_tone, app_role, area;
-- +goose StatementEnd
