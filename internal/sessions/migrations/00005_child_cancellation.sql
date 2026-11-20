-- +goose Up
ALTER TABLE child_bindings ADD COLUMN cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1));

-- +goose Down
ALTER TABLE child_bindings DROP COLUMN cancel_requested;
