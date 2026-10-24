-- name: ListMediaForScan :many
SELECT id, path, size, mtime_ns, fingerprint, status, has_thumb FROM media;

-- name: GetMediaByFingerprint :one
SELECT id, path, status FROM media WHERE fingerprint = ?;

-- name: InsertMedia :one
INSERT INTO media (
    path, root, rel_dir, title, size, mtime_ns, fingerprint, pid,
    duration_ms, container, width, height, video_codec, audio_codec,
    mime, codecs, hdr, playability, probe_json, added_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?,
    ?, ?, ?, ?, ?, ?,
    ?, ?, ?, ?, ?, ?, ?
)
RETURNING id;

-- name: UpdateMediaContent :exec
UPDATE media SET
    path = ?, root = ?, rel_dir = ?, title = ?, size = ?, mtime_ns = ?,
    fingerprint = ?, pid = ?, duration_ms = ?, container = ?, width = ?,
    height = ?, video_codec = ?, audio_codec = ?, mime = ?, codecs = ?,
    hdr = ?, playability = ?, probe_json = ?, has_thumb = 0, status = 'ok',
    updated_at = ?
WHERE id = ?;

-- name: UpdateMediaLocation :exec
UPDATE media SET
    path = ?, root = ?, rel_dir = ?, title = ?, size = ?, mtime_ns = ?,
    status = 'ok', updated_at = ?
WHERE id = ?;

-- name: SetMediaStatus :exec
UPDATE media SET status = ?, updated_at = ? WHERE id = ?;

-- name: SetMediaThumb :exec
UPDATE media SET has_thumb = ? WHERE id = ?;

-- name: GetMedia :one
SELECT * FROM media WHERE id = ?;

-- name: GetMediaByPID :one
SELECT * FROM media WHERE pid = ? AND status = 'ok';

-- name: ListMediaFingerprints :many
SELECT id, fingerprint FROM media;

-- name: UpdateMediaPID :exec
UPDATE media SET pid = ? WHERE id = ?;

-- name: ListDirs :many
SELECT DISTINCT root, rel_dir FROM media WHERE status = 'ok';

-- name: CountMediaByPlayability :many
SELECT playability, COUNT(*) AS n FROM media WHERE status = 'ok'
GROUP BY playability ORDER BY playability;

-- name: CountMissingMedia :one
SELECT COUNT(*) FROM media WHERE status = 'missing';

-- name: InsertSubtitle :exec
INSERT INTO subtitles (media_id, stream_index, lang, label, path) VALUES (?, ?, ?, ?, ?);

-- name: ListSubtitles :many
SELECT * FROM subtitles WHERE media_id = ? ORDER BY stream_index;

-- name: GetSubtitle :one
SELECT * FROM subtitles WHERE id = ? AND media_id = ?;

-- name: DeleteSubtitles :exec
DELETE FROM subtitles WHERE media_id = ?;
