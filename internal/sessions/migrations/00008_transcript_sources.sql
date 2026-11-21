-- +goose Up
ALTER TABLE messages ADD COLUMN source_id TEXT;
CREATE INDEX messages_by_source ON messages(session_id, role, source_id) WHERE source_id IS NOT NULL;

-- +goose Down
DROP INDEX messages_by_source;
ALTER TABLE messages DROP COLUMN source_id;
