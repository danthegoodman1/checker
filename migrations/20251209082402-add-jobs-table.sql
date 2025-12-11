
-- +migrate Up
CREATE TYPE job_state AS ENUM ('pending', 'running', 'suspended', 'pending_retry', 'completed', 'failed');
CREATE TYPE job_operation AS ENUM ('checkpointing', 'terminating', 'exiting', 'restarting');

-- Job definitions table - stores registered job definitions for recovery
CREATE TABLE job_definitions (
    name TEXT NOT NULL,
    version TEXT NOT NULL,
    runtime_type TEXT NOT NULL,
    config JSONB NOT NULL,
    retry_policy JSONB,
    metadata JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (name, version)
);

CREATE TABLE jobs (
    id TEXT PRIMARY KEY,
    definition_name TEXT NOT NULL,
    definition_version TEXT NOT NULL,
    state job_state NOT NULL DEFAULT 'pending',

    -- Environment and params
    env JSONB NOT NULL DEFAULT '{}',
    params JSONB,

    -- Result (populated on completion)
    -- Note: stdout/stderr not stored here - will use S3 in the future
    result_exit_code INT,
    result_output JSONB,
    error TEXT,

    -- Timestamps
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,

    -- Checkpoint state
    checkpoint_count INT NOT NULL DEFAULT 0,
    last_checkpoint_at TIMESTAMPTZ,
    retry_count INT NOT NULL DEFAULT 0,
    resume_at TIMESTAMPTZ,  -- Used for both suspend wake and retry scheduling

    -- Checkpoint recovery data (needed to restore)
    checkpoint_path TEXT,
    runtime_type TEXT NOT NULL,
    runtime_config JSONB NOT NULL,  -- Denormalized from definition for restore

    -- Metadata
    metadata JSONB NOT NULL DEFAULT '{}',

    -- Current in-flight operation (NULL when not in a transient substate)
    current_operation job_operation
);

-- Index for polling jobs waiting to resume (suspended or pending_retry)
CREATE INDEX idx_jobs_resume_at ON jobs (resume_at) WHERE (state = 'suspended' OR state = 'pending_retry') AND resume_at IS NOT NULL;

-- Index for listing non-terminal jobs
CREATE INDEX idx_jobs_state ON jobs (state) WHERE state NOT IN ('completed', 'failed');

-- Index for keyset pagination (created_at DESC, id DESC)
CREATE INDEX idx_jobs_created_at_id ON jobs (created_at DESC, id DESC);

-- +migrate Down
DROP TABLE jobs;
DROP TABLE job_definitions;
DROP TYPE job_operation;
DROP TYPE job_state;
