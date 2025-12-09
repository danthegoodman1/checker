-- name: InsertJob :exec
INSERT INTO jobs (
    id, definition_name, definition_version, state, env, params,
    created_at, runtime_type, runtime_config, metadata
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: UpdateJobState :exec
UPDATE jobs SET state = $2 WHERE id = $1;

-- name: UpdateJobStarted :exec
UPDATE jobs SET state = 'running', started_at = $2 WHERE id = $1;

-- name: UpdateJobCheckpointed :exec
UPDATE jobs SET
    state = $2,
    checkpoint_count = checkpoint_count + 1,
    last_checkpoint_at = $3,
    suspend_until = $4,
    checkpoint_path = $5
WHERE id = $1;

-- name: UpdateJobCompleted :exec
UPDATE jobs SET
    state = $2,
    completed_at = $3,
    result_exit_code = $4,
    result_output = $5,
    error = $6
WHERE id = $1;

-- name: UpdateJobRetryCount :exec
UPDATE jobs SET retry_count = $2 WHERE id = $1;

-- name: GetJob :one
SELECT * FROM jobs WHERE id = $1;

-- name: GetSuspendedJobsToWake :many
SELECT * FROM jobs
WHERE state = 'suspended' AND suspend_until IS NOT NULL AND suspend_until <= $1
ORDER BY suspend_until ASC
LIMIT $2;

-- name: GetNonTerminalJobs :many
SELECT * FROM jobs WHERE state NOT IN ('completed', 'failed');

-- name: ListJobs :many
SELECT * FROM jobs
WHERE (sqlc.narg('cursor_created_at')::TIMESTAMPTZ IS NULL OR
       (created_at, id) < (sqlc.narg('cursor_created_at')::TIMESTAMPTZ, sqlc.narg('cursor_id')::TEXT))
ORDER BY created_at DESC, id DESC
LIMIT $1;
-- TODO: use k-sortable ids?
