-- +goose Up
-- Last ACP session id per user: lets another api pod restore the session via
-- session/load when the agent supports it (arch spec §9).
CREATE TABLE agent_sessions (
  user_id    UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  session_id TEXT NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE agent_sessions;
