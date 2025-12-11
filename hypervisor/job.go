package hypervisor

import (
	"encoding/json"
	"time"
)

// JobState represents the current state of a job.
type JobState string

const (
	// JobStatePending means the job has been created but not yet started.
	// Persisted to DB.
	JobStatePending JobState = "pending"

	// JobStateRunning means the job is currently running.
	// Persisted to DB.
	JobStateRunning JobState = "running"

	// JobStateRestarting means the job is transitioning from failed to running (retry).
	// Transient state, not persisted to DB.
	JobStateRestarting JobState = "restarting"

	// JobStateCheckpointing means a checkpoint operation is in progress.
	// Transient state, not persisted to DB.
	JobStateCheckpointing JobState = "checkpointing"

	// JobStateSuspended means the job is suspended (checkpointed + scheduled for later wake).
	// Persisted to DB.
	JobStateSuspended JobState = "suspended"

	// JobStateExiting means the process has signaled exit, cleanup is in progress.
	// Transient state, not persisted to DB.
	JobStateExiting JobState = "exiting"

	// JobStateTerminating means kill was requested, waiting for process to stop.
	// Transient state, not persisted to DB.
	JobStateTerminating JobState = "terminating"

	// JobStateCompleted means the job finished successfully.
	// Persisted to DB (terminal state).
	JobStateCompleted JobState = "completed"

	// JobStateFailed means the job failed or crashed.
	// Persisted to DB (terminal state).
	JobStateFailed JobState = "failed"
)

// Job represents a running or completed instance of a job definition.
type Job struct {
	// ID is the unique identifier for this job.
	ID string

	// DefinitionName is the name of the job definition this job is running.
	DefinitionName string

	// DefinitionVersion is the version of the job definition this job is running.
	DefinitionVersion string

	// State is the current state of the job.
	State JobState

	// Env holds the environment variables for the job process.
	// This is set by the hypervisor and includes job metadata.
	Env map[string]string

	// Params are the input parameters passed when spawning the job.
	Params json.RawMessage

	// Result holds the result data once the job completes.
	Result *JobResult

	// Error holds error information if the job failed.
	Error string

	// CreatedAt is when the job was created.
	CreatedAt time.Time

	// StartedAt is when the job actually started running.
	StartedAt *time.Time

	// CompletedAt is when the job finished (success or failure).
	CompletedAt *time.Time

	// CheckpointCount tracks how many times this job has been checkpointed.
	CheckpointCount int

	// LastCheckpointAt is when the last checkpoint occurred.
	LastCheckpointAt *time.Time

	// RetryCount tracks how many times this job has been retried.
	RetryCount int

	// SuspendUntil is set when the job is suspended and should wake at this time.
	SuspendUntil *time.Time

	// Metadata holds arbitrary key-value metadata for filtering and identification.
	Metadata map[string]string
}

// JobResult holds the result of a completed job.
type JobResult struct {
	// ExitCode is the exit code of the process.
	ExitCode int

	// Output is the return value set by the job via runtime_exit().
	Output json.RawMessage

	// Stdout captures stdout if configured.
	Stdout string

	// Stderr captures stderr if configured.
	Stderr string
}

// Clone creates a deep copy of the job for safe reading.
func (j *Job) Clone() *Job {
	clone := *j

	// Deep copy slices and maps
	if j.Params != nil {
		clone.Params = make(json.RawMessage, len(j.Params))
		copy(clone.Params, j.Params)
	}

	if j.Env != nil {
		clone.Env = make(map[string]string, len(j.Env))
		for k, v := range j.Env {
			clone.Env[k] = v
		}
	}

	if j.Metadata != nil {
		clone.Metadata = make(map[string]string, len(j.Metadata))
		for k, v := range j.Metadata {
			clone.Metadata[k] = v
		}
	}

	if j.Result != nil {
		resultCopy := *j.Result
		if j.Result.Output != nil {
			resultCopy.Output = make(json.RawMessage, len(j.Result.Output))
			copy(resultCopy.Output, j.Result.Output)
		}
		clone.Result = &resultCopy
	}

	// Deep copy time pointers
	if j.StartedAt != nil {
		t := *j.StartedAt
		clone.StartedAt = &t
	}
	if j.CompletedAt != nil {
		t := *j.CompletedAt
		clone.CompletedAt = &t
	}
	if j.LastCheckpointAt != nil {
		t := *j.LastCheckpointAt
		clone.LastCheckpointAt = &t
	}
	if j.SuspendUntil != nil {
		t := *j.SuspendUntil
		clone.SuspendUntil = &t
	}

	return &clone
}

// IsTerminal returns true if the job is in a terminal state (completed or failed).
func (j *Job) IsTerminal() bool {
	return j.State == JobStateCompleted || j.State == JobStateFailed
}

// IsRunning returns true if the job is actively running (including transient active states).
func (j *Job) IsRunning() bool {
	switch j.State {
	case JobStateRunning, JobStateCheckpointing, JobStateExiting, JobStateTerminating:
		return true
	default:
		return false
	}
}

// IsTransient returns true if the job is in a transient state (not persisted to DB).
func (j *Job) IsTransient() bool {
	switch j.State {
	case JobStateRestarting, JobStateCheckpointing, JobStateExiting, JobStateTerminating:
		return true
	default:
		return false
	}
}

// PersistableState returns the DB-persistable state for a job.
// Transient states are mapped to their logical DB equivalent.
func (j *Job) PersistableState() JobState {
	switch j.State {
	case JobStateRestarting:
		return JobStatePending
	case JobStateCheckpointing, JobStateExiting, JobStateTerminating:
		return JobStateRunning
	default:
		return j.State
	}
}
