-- name: GetSession :one
SELECT * FROM sessions WHERE id = ?;

-- name: PutSession :exec
INSERT INTO sessions (id, model, account, parent, title, opened, updated, selections)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    model = excluded.model, account = excluded.account, parent = excluded.parent,
    title = excluded.title, opened = excluded.opened, updated = excluded.updated,
    selections = excluded.selections;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id = ?;

-- name: ListSessions :many
SELECT * FROM sessions WHERE model IN (sqlc.slice('models'))
ORDER BY COALESCE(NULLIF(updated, ''), opened) DESC, id;

-- name: LatestSession :one
SELECT * FROM sessions WHERE model = ?
ORDER BY COALESCE(NULLIF(updated, ''), opened) DESC, id LIMIT 1;

-- name: AppendMessage :exec
INSERT INTO messages (session_id, role, body) VALUES (?, ?, ?);

-- name: ReadTranscript :many
SELECT rowid, role, body FROM messages WHERE session_id = ? ORDER BY rowid;

-- name: TranscriptTail :many
SELECT role, body FROM (
    SELECT rowid, role, body FROM messages WHERE session_id = ?
    AND role IN ('user', 'assistant', 'tool', 'system') ORDER BY rowid DESC LIMIT ?
) ORDER BY rowid;
