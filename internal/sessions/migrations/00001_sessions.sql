-- +goose Up
CREATE TABLE sessions (
    id TEXT PRIMARY KEY NOT NULL,
    model TEXT NOT NULL,
    account TEXT NOT NULL,
    parent TEXT NOT NULL,
    title TEXT NOT NULL,
    opened TEXT NOT NULL,
    updated TEXT NOT NULL,
    selections TEXT NOT NULL
);
CREATE INDEX sessions_by_activity
    ON sessions (COALESCE(NULLIF(updated, ''), opened) DESC, id);

-- +goose Down
DROP TABLE sessions;
