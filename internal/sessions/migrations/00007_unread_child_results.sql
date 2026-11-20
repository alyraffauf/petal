-- +goose Up
ALTER TABLE child_bindings ADD COLUMN result_consumed INTEGER NOT NULL DEFAULT 0 CHECK (result_consumed IN (0, 1));

-- +goose Down
ALTER TABLE child_bindings DROP COLUMN result_consumed;
