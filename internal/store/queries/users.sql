-- name: CountUsers :one
SELECT COUNT(*) FROM users;

-- name: CreateUser :one
INSERT INTO users (username, pw_hash, created_at) VALUES (?, ?, ?)
RETURNING id;

-- name: GetUser :one
SELECT * FROM users WHERE id = ?;

-- name: GetUserByUsername :one
SELECT * FROM users WHERE username = ?;

-- name: ListUsers :many
SELECT * FROM users ORDER BY username;

-- name: UpdatePassword :exec
UPDATE users SET pw_hash = ? WHERE id = ?;

-- name: SetDiscreet :exec
UPDATE users SET discreet = ? WHERE id = ?;

-- name: DeleteUser :exec
DELETE FROM users WHERE id = ?;

-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?);

-- name: GetSession :one
SELECT s.expires_at, u.id, u.username, u.discreet
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = ?;

-- name: ExtendSession :exec
UPDATE sessions SET expires_at = ? WHERE token_hash = ?;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = ?;

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expires_at < ?;

-- name: DeleteOtherSessions :exec
DELETE FROM sessions WHERE user_id = ? AND token_hash != ?;
