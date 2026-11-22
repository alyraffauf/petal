-- +goose Up
CREATE TABLE project_directory (id INTEGER PRIMARY KEY CHECK (id = 1), cwd TEXT NOT NULL);

-- +goose Down
DROP TABLE project_directory;
