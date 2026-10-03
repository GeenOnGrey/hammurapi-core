-- HMR.CMN-0004: Pi sessions in the agent operator — a user's chat or a runner
-- task — with the key of the saved Pi session file. The MVP table
-- agent_sessions (ACP session ids) stays until the contract migration of the
-- next release.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE pi_sessions (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id        UUID REFERENCES users(id) ON DELETE CASCADE,       -- the user's chat (one chat per user)
  task_id        UUID REFERENCES agent_tasks(id) ON DELETE CASCADE, -- a runner task
  scenario       agent_scenario NOT NULL,
  operator_id    TEXT,                          -- the operator's sessionId while the process lives
  snapshot_key   TEXT,                          -- the Pi session file in object storage
  model          TEXT NOT NULL,
  thinking       TEXT NOT NULL DEFAULT 'off',
  connection_id  UUID REFERENCES llm_connections(id) ON DELETE SET NULL,
  resumes        INTEGER NOT NULL DEFAULT 0,    -- runner sessions reopened after an operator failure
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_active_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  closed_at      TIMESTAMPTZ,
  CHECK ((user_id IS NOT NULL) <> (task_id IS NOT NULL))
);
CREATE UNIQUE INDEX pi_sessions_active_chat ON pi_sessions (user_id) WHERE closed_at IS NULL AND user_id IS NOT NULL;
CREATE INDEX pi_sessions_task ON pi_sessions (task_id) WHERE task_id IS NOT NULL;
CREATE INDEX pi_sessions_idle ON pi_sessions (last_active_at) WHERE closed_at IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE pi_sessions;
-- +goose StatementEnd
