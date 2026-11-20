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
