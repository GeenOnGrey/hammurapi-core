-- PLT.HMR-0004: LLM connections and the model of every agent scenario.
-- Expand only: new types and tables; nothing existing changes.

-- +goose Up
-- +goose StatementBegin
CREATE TYPE llm_connection_type AS ENUM ('deepseek');
CREATE TYPE llm_connection_status AS ENUM ('unknown', 'ok', 'insufficient_balance', 'auth', 'unavailable', 'disabled');
CREATE TYPE agent_scenario AS ENUM ('chat', 'issue_analysis', 'gate_generation', 'conformance_check',
                                    'codegen', 'review_update', 'rollback_revert');

CREATE TABLE llm_connections (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name          TEXT NOT NULL UNIQUE,
  type          llm_connection_type NOT NULL,
  base_url      TEXT NOT NULL,
  models        JSONB NOT NULL,                 -- [agent.ModelDef]
  api_key_enc   BYTEA NOT NULL,                 -- AES-GCM with TOKEN_ENCRYPTION_KEY
  api_key_last4 TEXT NOT NULL,
  enabled       BOOLEAN NOT NULL DEFAULT true,
  status        llm_connection_status NOT NULL DEFAULT 'unknown',
  status_reason TEXT,
  status_at     TIMESTAMPTZ,
  created_by    UUID REFERENCES users(id) ON DELETE SET NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A scenario without a row uses the default model.
CREATE TABLE agent_scenario_models (
  scenario      agent_scenario PRIMARY KEY,
  connection_id UUID NOT NULL REFERENCES llm_connections(id) ON DELETE RESTRICT,
  model         TEXT NOT NULL,
  thinking      TEXT NOT NULL DEFAULT 'off',
  updated_by    UUID REFERENCES users(id) ON DELETE SET NULL,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE agent_default_model (
  id            BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),   -- one row
  connection_id UUID NOT NULL REFERENCES llm_connections(id) ON DELETE RESTRICT,
  model         TEXT NOT NULL,
  thinking      TEXT NOT NULL DEFAULT 'off',
  updated_by    UUID REFERENCES users(id) ON DELETE SET NULL,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE agent_default_model;
DROP TABLE agent_scenario_models;
DROP TABLE llm_connections;
DROP TYPE agent_scenario;
DROP TYPE llm_connection_status;
DROP TYPE llm_connection_type;
-- +goose StatementEnd
