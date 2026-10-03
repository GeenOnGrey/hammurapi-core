-- PLT.HMR-0004: usage by scenario, connection and model. Correct whether
-- agent_usage already exists (PLT.HMR-0002) or not.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS agent_usage (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  context     TEXT NOT NULL,
  tokens_in   BIGINT NOT NULL,
  tokens_out  BIGINT NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE agent_usage
  ADD COLUMN IF NOT EXISTS scenario        agent_scenario,
  ADD COLUMN IF NOT EXISTS connection_id   UUID REFERENCES llm_connections(id) ON DELETE SET NULL,
  ADD COLUMN IF NOT EXISTS model           TEXT,
  ADD COLUMN IF NOT EXISTS cache_read      BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS cache_write     BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS cost_usd        NUMERIC(12,6) NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS user_id         UUID REFERENCES users(id) ON DELETE SET NULL,
  ADD COLUMN IF NOT EXISTS chat_session_id UUID;
CREATE INDEX IF NOT EXISTS agent_usage_created ON agent_usage (created_at);
CREATE INDEX IF NOT EXISTS agent_usage_scenario ON agent_usage (scenario, created_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS agent_usage_scenario;
DROP INDEX IF EXISTS agent_usage_created;
-- model and user_id existed before this migration (PLT.HMR-0002) and stay;
-- the table itself stays too.
ALTER TABLE agent_usage
  DROP COLUMN IF EXISTS chat_session_id,
  DROP COLUMN IF EXISTS cost_usd, DROP COLUMN IF EXISTS cache_write, DROP COLUMN IF EXISTS cache_read,
  DROP COLUMN IF EXISTS connection_id, DROP COLUMN IF EXISTS scenario;
-- +goose StatementEnd
