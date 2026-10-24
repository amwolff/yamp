-- name: ListProfiles :many
SELECT * FROM profiles ORDER BY priority DESC, name;

-- name: GetProfile :one
SELECT * FROM profiles WHERE id = ?;

-- name: CountProfiles :one
SELECT COUNT(*) FROM profiles;

-- name: CreateProfile :one
INSERT INTO profiles (name, params_json, params_hash, auto_policy, priority, builtin)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: UpdateProfile :exec
UPDATE profiles SET name = ?, params_json = ?, params_hash = ?, auto_policy = ?, priority = ?
WHERE id = ?;

-- name: DeleteProfile :exec
DELETE FROM profiles WHERE id = ?;
