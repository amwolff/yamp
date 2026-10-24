-- name: LibraryTotals :one
SELECT COUNT(*) AS videos,
    CAST(COALESCE(SUM(duration_ms), 0) AS INTEGER) AS duration_ms,
    CAST(COALESCE(SUM(size), 0) AS INTEGER) AS size
FROM media WHERE status = 'ok';

-- name: PlayTotals :one
SELECT CAST(COALESCE(SUM(counted), 0) AS INTEGER) AS views,
    CAST(COALESCE(SUM(watched_ms), 0) AS INTEGER) AS watched_ms
FROM play_events;

-- name: TopViewed :many
SELECT m.pid, m.title, COUNT(*) AS views
FROM play_events e
JOIN media m ON m.id = e.media_id
WHERE e.counted = 1 AND m.status = 'ok'
GROUP BY m.id
ORDER BY views DESC, m.title
LIMIT ?;

-- name: TopRated :many
SELECT m.pid, m.title, CAST(AVG(r.stars) AS REAL) AS stars, COUNT(*) AS votes
FROM ratings r
JOIN media m ON m.id = r.media_id
WHERE m.status = 'ok'
GROUP BY m.id
ORDER BY stars DESC, votes DESC, m.title
LIMIT ?;
