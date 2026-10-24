-- name: GetPlayback :one
SELECT * FROM playback WHERE user_id = ? AND media_id = ?;

-- name: UpsertPlayback :exec
INSERT INTO playback (user_id, media_id, position_ms, completed, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (user_id, media_id) DO UPDATE SET
    position_ms = excluded.position_ms,
    completed = excluded.completed,
    updated_at = excluded.updated_at;

-- name: AddPlayEvent :one
INSERT INTO play_events (play_key, user_id, media_id, started_at, updated_at, watched_ms)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (play_key) DO UPDATE SET
    watched_ms = play_events.watched_ms + excluded.watched_ms,
    updated_at = excluded.updated_at
WHERE play_events.user_id = excluded.user_id AND play_events.media_id = excluded.media_id
RETURNING id, watched_ms, counted;

-- name: MarkPlayEventCounted :exec
UPDATE play_events SET counted = 1 WHERE id = ?;

-- name: CountViews :one
SELECT COUNT(*) FROM play_events WHERE media_id = ? AND counted = 1;

-- name: GetRating :one
SELECT stars FROM ratings WHERE user_id = ? AND media_id = ?;

-- name: UpsertRating :exec
INSERT INTO ratings (user_id, media_id, stars, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT (user_id, media_id) DO UPDATE SET stars = excluded.stars, updated_at = excluded.updated_at;

-- name: DeleteRating :exec
DELETE FROM ratings WHERE user_id = ? AND media_id = ?;
