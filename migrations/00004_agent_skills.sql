-- FTR.HMR.CMN-0004: snapshots of the agent skills from the specification
-- repository and the audit log of the Agent section (including skill PRs).

-- +goose Up
-- +goose StatementBegin
CREATE TABLE agent_skill_snapshots (
  hash        TEXT PRIMARY KEY,                 -- sha256:<hex> of the tar.gz
  commit_sha  TEXT NOT NULL,
  s3_key      TEXT NOT NULL,
  skills      JSONB NOT NULL,                   -- [{name, description, scenarios[]}]
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON agent_skill_snapshots (created_at DESC);

CREATE TABLE agent_config_changes (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  object_type   TEXT NOT NULL CHECK (object_type IN ('connection', 'scenario_models', 'skill', 'mcp_server')),
  object_ref    TEXT NOT NULL,
  action        TEXT NOT NULL,                  -- create | update | delete | replace_key | enable | disable | propose | approve | withdraw
  summary       TEXT NOT NULL,                  -- never secret values
  actor_id      UUID REFERENCES users(id) ON DELETE SET NULL,   -- NULL: the system (bootstrap)
  pr_url        TEXT,
  pr_number     INTEGER,
  branch_name   TEXT,
  pr_state      TEXT CHECK (pr_state IN ('open', 'merged', 'closed')),
  details       JSONB NOT NULL DEFAULT '{}',    -- skill changes: {kind, scenarios, description}
  self_approved BOOLEAN NOT NULL DEFAULT false,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON agent_config_changes (created_at DESC);
-- One open change per skill.
CREATE UNIQUE INDEX agent_skill_open_change ON agent_config_changes (object_ref)
  WHERE object_type = 'skill' AND pr_state = 'open';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE agent_config_changes;
DROP TABLE agent_skill_snapshots;
-- +goose StatementEnd
