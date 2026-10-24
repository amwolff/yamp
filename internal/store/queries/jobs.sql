-- name: EnqueueJob :execrows
INSERT OR IGNORE INTO jobs (media_id, profile_id, state, priority, created_at)
VALUES (?, ?, 'queued', ?, ?);

-- AutoEnqueue queues each (media, profile) pair that the policy selects and
-- that has no rendition and no unfinished job. Failed and canceled jobs need a
-- manual retry.
-- name: AutoEnqueue :execrows
INSERT OR IGNORE INTO jobs (media_id, profile_id, state, priority, created_at)
SELECT m.id, p.id, 'queued', p.priority, ?
FROM media m, profiles p
WHERE m.status = 'ok'
    AND (p.auto_policy = 'always' OR (p.auto_policy = 'when-needed' AND m.playability != 'direct'))
    AND NOT EXISTS (SELECT 1 FROM renditions r WHERE r.media_id = m.id AND r.profile_id = p.id)
    AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.media_id = m.id AND j.profile_id = p.id AND j.state != 'done');

-- name: ClaimJob :one
UPDATE jobs SET state = 'running', started_at = ?, progress = 0, error = ''
WHERE id = (
    SELECT q.id FROM jobs q WHERE q.state = 'queued' ORDER BY q.priority DESC, q.id LIMIT 1
)
RETURNING id, media_id, profile_id;

-- name: UpdateJobProgress :exec
UPDATE jobs SET progress = ? WHERE id = ?;

-- name: FinishJob :exec
UPDATE jobs SET state = ?, error = ?, progress = ?, finished_at = ? WHERE id = ?;

-- name: CancelQueuedJob :execrows
UPDATE jobs SET state = 'canceled', finished_at = ? WHERE id = ? AND state = 'queued';

-- name: RequeueRunningJobs :exec
UPDATE jobs SET state = 'queued', progress = 0 WHERE state = 'running';

-- name: GetJob :one
SELECT * FROM jobs WHERE id = ?;

-- name: ListJobs :many
SELECT j.*, m.title AS media_title, m.pid AS media_pid, p.name AS profile_name
FROM jobs j
JOIN media m ON m.id = j.media_id
JOIN profiles p ON p.id = j.profile_id
ORDER BY CASE j.state WHEN 'running' THEN 0 WHEN 'queued' THEN 1 ELSE 2 END,
    CASE WHEN j.state = 'queued' THEN -j.priority ELSE 0 END,
    CASE WHEN j.state IN ('running', 'queued') THEN j.id ELSE -j.id END
LIMIT ?;

-- name: ListJobsForMedia :many
SELECT j.*, p.name AS profile_name
FROM jobs j
JOIN profiles p ON p.id = j.profile_id
WHERE j.media_id = ?
ORDER BY j.id DESC
LIMIT 10;

-- name: CountJobsByState :many
SELECT state, COUNT(*) AS n FROM jobs GROUP BY state;

-- name: DeleteFinishedJobs :exec
DELETE FROM jobs WHERE state IN ('done', 'canceled') AND finished_at < ?;
