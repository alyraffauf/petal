-- +goose Up
ALTER TABLE sessions ADD COLUMN placement TEXT NOT NULL DEFAULT 'null';

-- +goose Down
ALTER TABLE sessions DROP COLUMN placement;
