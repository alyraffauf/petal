-- +goose Up
ALTER TABLE message_embeddings ADD COLUMN digest TEXT;

-- +goose Down
ALTER TABLE message_embeddings DROP COLUMN digest;
