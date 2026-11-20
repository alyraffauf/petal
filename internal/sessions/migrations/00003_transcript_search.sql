-- +goose Up
CREATE TABLE messages (
    rowid INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    role TEXT NOT NULL,
    body TEXT NOT NULL
);
CREATE INDEX messages_by_session ON messages(session_id, rowid);
CREATE VIRTUAL TABLE message_search USING fts5(body, content='messages', content_rowid='rowid');
-- +goose StatementBegin
CREATE TRIGGER index_message AFTER INSERT ON messages BEGIN
    INSERT INTO message_search(rowid, body) VALUES (new.rowid, new.body);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER remove_message AFTER DELETE ON messages BEGIN
    INSERT INTO message_search(message_search, rowid, body) VALUES ('delete', old.rowid, old.body);
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER remove_message;
DROP TRIGGER index_message;
DROP TABLE message_search;
DROP TABLE messages;
