-- name: InsertRendition :one
INSERT INTO renditions (
    media_id, profile_id, params_hash, path, size, mime, codecs, width, height, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: ListRenditionsForMedia :many
SELECT r.*, p.name AS profile_name, p.priority AS profile_priority,
    p.params_hash != r.params_hash AS stale
FROM renditions r
JOIN profiles p ON p.id = r.profile_id
WHERE r.media_id = ?
ORDER BY stale, p.priority DESC, r.id DESC;

-- name: GetRendition :one
SELECT * FROM renditions WHERE id = ? AND media_id = ?;

-- name: ListReplacedRenditions :many
SELECT id, path FROM renditions WHERE media_id = ? AND profile_id = ? AND id != ?;

-- name: DeleteRendition :exec
DELETE FROM renditions WHERE id = ?;

-- name: ListAllRenditions :many
SELECT id, media_id, profile_id, path, size FROM renditions;

-- name: ListRenditionPathsForProfile :many
SELECT path FROM renditions WHERE profile_id = ?;

-- name: ListRenditionPathsForMedia :many
SELECT path FROM renditions WHERE media_id = ?;

-- name: DeleteRenditionsForMedia :exec
DELETE FROM renditions WHERE media_id = ?;

-- name: ListStaleMediaForProfile :many
SELECT DISTINCT r.media_id
FROM renditions r
JOIN profiles p ON p.id = r.profile_id
WHERE r.profile_id = ? AND r.params_hash != p.params_hash;

-- name: CountStaleRenditions :many
SELECT r.profile_id, COUNT(*) AS n
FROM renditions r
JOIN profiles p ON p.id = r.profile_id
WHERE r.params_hash != p.params_hash
GROUP BY r.profile_id;

-- name: SumRenditionSize :one
SELECT CAST(COALESCE(SUM(size), 0) AS INTEGER) FROM renditions;
