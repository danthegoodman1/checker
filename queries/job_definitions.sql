-- name: UpsertJobDefinition :exec
INSERT INTO job_definitions (name, version, runtime_type, config, retry_policy, metadata, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
ON CONFLICT (name, version) DO UPDATE SET
    runtime_type = EXCLUDED.runtime_type,
    config = EXCLUDED.config,
    retry_policy = EXCLUDED.retry_policy,
    metadata = EXCLUDED.metadata,
    updated_at = NOW();

-- name: GetJobDefinition :one
SELECT * FROM job_definitions WHERE name = $1 AND version = $2;

-- name: ListJobDefinitions :many
SELECT * FROM job_definitions ORDER BY name, version;

-- name: DeleteJobDefinition :exec
DELETE FROM job_definitions WHERE name = $1 AND version = $2;
