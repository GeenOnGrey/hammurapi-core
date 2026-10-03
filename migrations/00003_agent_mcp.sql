-- PLT.HMR-0004: MCP servers of the agent (streamable HTTP with header auth).

-- +goose Up
-- +goose StatementBegin
CREATE TYPE mcp_exposure AS ENUM ('direct', 'deferred');

CREATE TABLE mcp_servers (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name          TEXT NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9][a-z0-9-]{1,40}$' AND name <> 'hammurapi'),
  url           TEXT NOT NULL,
  headers_enc   BYTEA,                          -- AES-GCM of JSON {name: value}
  header_meta   JSONB NOT NULL DEFAULT '[]',    -- [{name, last4}]
  exposure      mcp_exposure NOT NULL DEFAULT 'deferred',
  scenarios     agent_scenario[] NOT NULL DEFAULT '{}',
  enabled       BOOLEAN NOT NULL DEFAULT true,
  status        TEXT NOT NULL DEFAULT 'unknown',
  status_reason TEXT,
  tools_count   INTEGER,
  status_at     TIMESTAMPTZ,
  created_by    UUID REFERENCES users(id) ON DELETE SET NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE mcp_servers;
DROP TYPE mcp_exposure;
-- +goose StatementEnd
