-- PLT.HMR-0004: model, connection and error class of chat messages, and
-- retries. Nullable columns: no table rewrite.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE chat_messages
  ADD COLUMN model         TEXT,
  ADD COLUMN connection_id UUID REFERENCES llm_connections(id) ON DELETE SET NULL,
  ADD COLUMN error_class   TEXT,
  ADD COLUMN retry_of      UUID REFERENCES chat_messages(id) ON DELETE SET NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE chat_messages
  DROP COLUMN retry_of, DROP COLUMN error_class, DROP COLUMN connection_id, DROP COLUMN model;
-- +goose StatementEnd
