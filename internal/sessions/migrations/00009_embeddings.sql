-- +goose Up
CREATE TABLE message_embeddings (
    reference INTEGER PRIMARY KEY REFERENCES messages(rowid) ON DELETE CASCADE,
    vector BLOB NOT NULL
);
-- +goose StatementBegin
CREATE TRIGGER invalidate_message AFTER UPDATE OF body ON messages BEGIN
    DELETE FROM message_embeddings WHERE reference=old.rowid;
    INSERT INTO message_search(message_search,rowid,body) VALUES ('delete',old.rowid,old.body);
    INSERT INTO message_search(rowid,body) VALUES (new.rowid,new.body);
END;
-- +goose StatementEnd
-- +goose Down
DROP TRIGGER invalidate_message;
DROP TABLE message_embeddings;
