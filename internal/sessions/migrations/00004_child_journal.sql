-- +goose Up
CREATE TABLE child_bindings (
    child_id TEXT PRIMARY KEY NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    parent_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    lease TEXT,
    result_status TEXT,
    result_data BLOB,
    CHECK (child_id <> parent_id)
);
CREATE INDEX child_bindings_by_parent ON child_bindings(parent_id);

CREATE TABLE child_activities (
    child_id TEXT NOT NULL REFERENCES child_bindings(child_id) ON DELETE CASCADE,
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    kind TEXT NOT NULL,
    data BLOB,
    PRIMARY KEY (child_id, sequence)
) WITHOUT ROWID;

-- +goose Down
DROP TABLE child_activities;
DROP TABLE child_bindings;
