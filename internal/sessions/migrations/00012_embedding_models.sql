-- +goose Up
DROP TRIGGER invalidate_message;
ALTER TABLE message_embeddings RENAME TO old_message_embeddings;
CREATE TABLE message_embeddings (
    reference INTEGER NOT NULL REFERENCES messages(rowid) ON DELETE CASCADE,
    vector BLOB NOT NULL,
    digest TEXT NOT NULL,
    PRIMARY KEY (reference, digest)
);
INSERT INTO message_embeddings (reference, vector, digest)
SELECT reference, vector, digest FROM old_message_embeddings WHERE digest IS NOT NULL;
DROP TABLE old_message_embeddings;
-- +goose StatementBegin
CREATE TRIGGER invalidate_message AFTER UPDATE OF body ON messages BEGIN
    DELETE FROM message_embeddings WHERE reference=old.rowid;
    INSERT INTO message_search(message_search,rowid,body) VALUES ('delete',old.rowid,old.body);
    INSERT INTO message_search(rowid,body) VALUES (new.rowid,new.body);
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER invalidate_message;
ALTER TABLE message_embeddings RENAME TO old_message_embeddings;
CREATE TABLE message_embeddings (
    reference INTEGER PRIMARY KEY REFERENCES messages(rowid) ON DELETE CASCADE,
    vector BLOB NOT NULL,
    digest TEXT
);
INSERT OR REPLACE INTO message_embeddings (reference, vector, digest)
SELECT reference, vector, digest FROM old_message_embeddings ORDER BY rowid;
DROP TABLE old_message_embeddings;
-- +goose StatementBegin
CREATE TRIGGER invalidate_message AFTER UPDATE OF body ON messages BEGIN
    DELETE FROM message_embeddings WHERE reference=old.rowid;
    INSERT INTO message_search(message_search,rowid,body) VALUES ('delete',old.rowid,old.body);
    INSERT INTO message_search(rowid,body) VALUES (new.rowid,new.body);
END;
-- +goose StatementEnd
