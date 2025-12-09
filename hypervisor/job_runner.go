package hypervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/danthegoodman1/checker/pg"
	"github.com/danthegoodman1/checker/query"
	"github.com/danthegoodman1/checker/runtime"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// JobRunner manages the lifecycle of a single job.
type JobRunner struct {
	job            *Job
	jobMu          sync.RWMutex // Protects job state
	definition     *JobDefinition
	rt             runtime.Runtime
	process        runtime.Process
	checkpoint     runtime.Checkpoint // Last checkpoint, used for restore
	config         any
	apiHostAddress string
	logger         zerolog.Logger

	// Stdout and Stderr writers for process output
	stdout io.Writer
	stderr io.Writer

	// Done channel signals when the job has terminated
	doneChan chan struct{}
	doneOnce sync.Once

	// Result waiters - channels to notify when job completes
	resultWaiters []chan struct{}
	waitersMu     sync.Mutex

	// Checkpoint lock - RWMutex for client lock API
	// Read locks are taken by client code (via TakeLock)
	// Write lock is taken during checkpoint
	checkpointMu sync.RWMutex

	// Track active locks for debugging/monitoring
	activeLocks   map[string]time.Time
	activeLocksMu sync.Mutex
	lockCounter   int64

	// Checkpoint tokens for idempotency (maps token -> true)
	// Stores tokens from both completed and collapsed checkpoint requests
	checkpointTokens   map[string]struct{}
	checkpointTokensMu sync.Mutex

	// Track if a checkpoint is currently in progress (for collapsing concurrent requests)
	checkpointInProgress   bool
	checkpointInProgressMu sync.Mutex

	// processReady is closed when the process field is set and ready to use.
	// This is used during restore to signal that the runner can handle requests.
	processReady     chan struct{}
	processReadyOnce sync.Once

	// Context for cancellation
	ctx    context.Context
	cancel context.CancelFunc

	// onFailure is called when the job fails, allowing retry logic
	onFailure func(runner *JobRunner, exitCode int)

	// Database pool for persisting state changes
	pool *pgxpool.Pool
}

func NewJobRunner(job *Job, definition *JobDefinition, rt runtime.Runtime, config any, apiHostAddress string, pool *pgxpool.Pool, stdout, stderr io.Writer) *JobRunner {
	ctx, cancel := context.WithCancel(context.Background())

	return &JobRunner{
		job:              job,
		definition:       definition,
		rt:               rt,
		config:           config,
		apiHostAddress:   apiHostAddress,
		pool:             pool,
		stdout:           stdout,
		stderr:           stderr,
		logger:           logger.With().Str("job_id", job.ID).Logger(),
		doneChan:         make(chan struct{}),
		activeLocks:      make(map[string]time.Time),
		checkpointTokens: make(map[string]struct{}),
		processReady:     make(chan struct{}),
		ctx:              ctx,
		cancel:           cancel,
	}
}

// signalProcessReady signals that the process field is set and ready to use.
// This is called after Start() or after restoring from checkpoint.
func (r *JobRunner) signalProcessReady() {
	r.processReadyOnce.Do(func() {
		close(r.processReady)
	})
}

// Cancel cancels the runner's context, which will unblock any pending requests
// waiting on processReady or other operations.
func (r *JobRunner) Cancel() {
	r.cancel()
}

func (r *JobRunner) Start() error {
	process, err := r.rt.Start(r.ctx, runtime.StartOptions{
		ExecutionID:    r.job.ID,
		Env:            r.job.Env,
		Config:         r.config,
		APIHostAddress: r.apiHostAddress,
		Stdout:         r.stdout,
		Stderr:         r.stderr,
	})
	if err != nil {
		r.jobMu.Lock()
		r.job.State = JobStateFailed
		r.job.Error = fmt.Sprintf("failed to start: %v", err)
		r.jobMu.Unlock()

		r.persistJobCompleted(query.JobStateFailed, nil, nil, fmt.Sprintf("failed to start: %v", err))
		return err
	}

	r.process = process
	r.signalProcessReady()

	now := time.Now()

	if dbErr := query.ReliableExecInTx(r.ctx, r.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		return q.UpdateJobStarted(ctx, query.UpdateJobStartedParams{
			ID:        r.job.ID,
			StartedAt: sql.NullTime{Time: now, Valid: true},
		})
	}); dbErr != nil {
		r.logger.Error().Err(dbErr).Msg("failed to persist job started state to DB")
	}

	r.jobMu.Lock()
	r.job.StartedAt = &now
	r.job.State = JobStateRunning
	r.jobMu.Unlock()

	go r.waitForExit()

	return nil
}

// Retry restarts the job with incremented retry count.
// If a checkpoint exists, restores from checkpoint instead of starting from scratch.
func (r *JobRunner) Retry() error {
	r.jobMu.Lock()
	r.job.RetryCount++
	retryCount := r.job.RetryCount
	r.job.State = JobStatePending
	r.job.Error = ""
	r.job.Result = nil
	r.job.CompletedAt = nil
	hasCheckpoint := r.checkpoint != nil
	r.jobMu.Unlock()

	r.persistJobRetryCount(retryCount)

	// If we have a checkpoint, restore from it instead of starting from scratch
	if hasCheckpoint {
		r.logger.Info().Int("retry_count", retryCount).Msg("retrying job from checkpoint")
		return r.restoreFromCheckpoint()
	}

	r.logger.Info().Int("retry_count", retryCount).Msg("retrying job from start (no checkpoint)")
	return r.Start()
}

// restoreFromCheckpoint restores the job from its checkpoint.
func (r *JobRunner) restoreFromCheckpoint() error {
	// Create a fresh context for this restore attempt - the old context may have been
	// affected by the previous process's failure
	ctx, cancel := context.WithCancel(context.Background())
	r.ctx = ctx
	r.cancel = cancel

	r.jobMu.Lock()
	r.job.State = JobStateRunning
	r.job.SuspendUntil = nil
	r.jobMu.Unlock()

	process, err := r.rt.Restore(r.ctx, runtime.RestoreOptions{
		Checkpoint: r.checkpoint,
		Stdout:     r.stdout,
		Stderr:     r.stderr,
	})
	if err != nil {
		r.logger.Error().Err(err).Msg("failed to restore from checkpoint")
		r.jobMu.Lock()
		r.job.State = JobStateFailed
		r.job.Error = fmt.Sprintf("failed to restore: %v", err)
		now := time.Now()
		r.job.CompletedAt = &now
		r.jobMu.Unlock()
		return err
	}

	r.process = process
	r.logger.Debug().Msg("job restored from checkpoint")

	// Persist running state
	now := time.Now()
	if dbErr := query.ReliableExec(r.ctx, r.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		return q.UpdateJobStarted(ctx, query.UpdateJobStartedParams{
			ID:        r.job.ID,
			StartedAt: sql.NullTime{Time: now, Valid: true},
		})
	}); dbErr != nil {
		r.logger.Error().Err(dbErr).Msg("failed to persist job started state to DB")
	}

	go r.waitForExit()

	return nil
}

// SetOnFailure sets the callback for when the job fails.
func (r *JobRunner) SetOnFailure(fn func(runner *JobRunner, exitCode int)) {
	r.onFailure = fn
}

// MarkDone closes the done channel and notifies waiters.
func (r *JobRunner) MarkDone() {
	r.doneOnce.Do(func() {
		close(r.doneChan)
		r.notifyWaiters()
	})
}

// waitForExit waits for the process to exit and updates state accordingly.
func (r *JobRunner) waitForExit() {
	exitCode, err := r.process.Wait(r.ctx)

	r.jobMu.Lock()
	// If suspended, the process exit is expected - don't mark as failed.
	// The wake timer is scheduled by the Checkpoint method after storing the checkpoint.
	if r.job.State == JobStateSuspended {
		r.jobMu.Unlock()
		return
	}

	// Only update if not already terminal (could have been killed or exited via API)
	var shouldPersist bool
	if !r.job.IsTerminal() {
		shouldPersist = true
		now := time.Now()
		r.job.CompletedAt = &now

		if err != nil {
			r.job.State = JobStateFailed
			r.job.Error = err.Error()
		} else if exitCode != 0 {
			r.job.State = JobStateFailed
			r.job.Error = fmt.Sprintf("exit code %d", exitCode)
			r.job.Result = &JobResult{ExitCode: exitCode}
		} else {
			r.job.State = JobStateCompleted
			if r.job.Result == nil {
				r.job.Result = &JobResult{ExitCode: 0}
			}
		}
	}
	failed := r.job.State == JobStateFailed
	completedAt := r.job.CompletedAt
	result := r.job.Result
	errorMsg := r.job.Error
	state := r.job.State
	r.jobMu.Unlock()

	if shouldPersist {
		var dbState query.JobState
		if state == JobStateCompleted {
			dbState = query.JobStateCompleted
		} else {
			dbState = query.JobStateFailed
		}
		r.persistJobCompleted(dbState, completedAt, result, errorMsg)
	}

	if cleanupErr := r.process.Cleanup(context.Background()); cleanupErr != nil {
		r.logger.Error().Err(cleanupErr).Msg("failed to cleanup process")
	}

	if failed && r.onFailure != nil {
		r.onFailure(r, exitCode)
		return // onFailure decides whether to markDone
	}

	r.MarkDone()
}

// scheduleSuspendWake schedules a job restore when the suspend duration expires.
func (r *JobRunner) scheduleSuspendWake(wakeTime time.Time) {
	delay := time.Until(wakeTime)
	if delay < 0 {
		delay = 0
	}

	r.logger.Debug().
		Time("wake_time", wakeTime).
		Dur("delay", delay).
		Msg("scheduling suspend wake")

	time.AfterFunc(delay, func() {
		// Check if context is cancelled (e.g., hypervisor shutdown or crash)
		select {
		case <-r.ctx.Done():
			r.logger.Debug().Msg("suspend wake timer fired but context cancelled, marking done")
			r.MarkDone()
			return
		default:
		}

		r.logger.Debug().Msg("suspend wake timer fired, restoring job")

		r.jobMu.Lock()
		// Check if still suspended (could have been killed while waiting)
		if r.job.State != JobStateSuspended {
			r.logger.Debug().
				Str("state", string(r.job.State)).
				Msg("job no longer suspended, skipping restore")
			r.jobMu.Unlock()
			return
		}
		r.job.State = JobStateRunning
		r.job.SuspendUntil = nil
		r.jobMu.Unlock()

		process, err := r.rt.Restore(r.ctx, runtime.RestoreOptions{
			Checkpoint: r.checkpoint,
			Stdout:     r.stdout,
			Stderr:     r.stderr,
		})
		if err != nil {
			r.logger.Error().Err(err).Msg("failed to restore from checkpoint")
			r.jobMu.Lock()
			r.job.State = JobStateFailed
			r.job.Error = fmt.Sprintf("failed to restore: %v", err)
			now := time.Now()
			r.job.CompletedAt = &now
			r.jobMu.Unlock()
			r.MarkDone()
			return
		}

		r.process = process
		r.logger.Debug().Msg("job restored from checkpoint")

		go r.waitForExit()
	})
}

// notifyWaiters signals all result waiters that the job has completed.
func (r *JobRunner) notifyWaiters() {
	r.waitersMu.Lock()
	defer r.waitersMu.Unlock()

	for _, ch := range r.resultWaiters {
		close(ch)
	}
	r.resultWaiters = nil
}

// GetState returns the current job state.
func (r *JobRunner) GetState(ctx context.Context) (*Job, error) {
	r.jobMu.RLock()
	defer r.jobMu.RUnlock()
	return r.job.Clone(), nil
}

// Checkpoint requests a checkpoint of the job.
// The token parameter is an idempotency key - if it matches a previously seen
// checkpoint token, the request is a no-op (used for retry after restore).
func (r *JobRunner) Checkpoint(ctx context.Context, suspendDuration time.Duration, token string) (*Job, error) {
	// Token is required for idempotency
	if token == "" {
		return nil, fmt.Errorf("checkpoint token is required")
	}

	// Wait for process to be ready (handles race during restore where runner
	// is in map but process hasn't been set yet)
	select {
	case <-r.processReady:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.ctx.Done():
		// Runner context cancelled (e.g., restore failed)
		return nil, fmt.Errorf("job runner cancelled")
	}

	// Check for idempotent replay (retry after restore)
	r.checkpointTokensMu.Lock()
	if _, exists := r.checkpointTokens[token]; exists {
		r.checkpointTokensMu.Unlock()
		r.logger.Debug().Str("token", token).Msg("checkpoint token already used, returning success (idempotent replay)")
		r.jobMu.RLock()
		job := r.job.Clone()
		r.jobMu.RUnlock()
		return job, nil
	}
	r.checkpointTokensMu.Unlock()

	// Check if already terminal
	r.jobMu.RLock()
	isTerminal := r.job.IsTerminal()
	r.jobMu.RUnlock()
	if isTerminal {
		return nil, fmt.Errorf("job has terminated")
	}

	// Check if a checkpoint is already in progress - if so, collapse this request
	// but still store the token so retries work.
	r.checkpointInProgressMu.Lock()
	if r.checkpointInProgress {
		r.checkpointTokensMu.Lock()
		r.checkpointTokens[token] = struct{}{}
		r.checkpointTokensMu.Unlock()
		r.checkpointInProgressMu.Unlock()
		r.logger.Debug().Str("token", token).Msg("checkpoint already in progress, collapsing request")
		r.jobMu.RLock()
		job := r.job.Clone()
		r.jobMu.RUnlock()
		return job, nil
	}
	r.checkpointInProgress = true
	r.checkpointInProgressMu.Unlock()

	// Acquire write lock - this waits for all read locks (from TakeLock) to be released
	r.checkpointMu.Lock()
	defer r.checkpointMu.Unlock()
	defer func() {
		r.checkpointInProgressMu.Lock()
		r.checkpointInProgress = false
		r.checkpointInProgressMu.Unlock()
	}()

	keepRunning := suspendDuration == 0

	// Update job state BEFORE stopping the container.
	// This prevents a race where waitForExit sees the container exit
	// but the state hasn't been updated yet.
	r.jobMu.Lock()
	r.job.CheckpointCount++
	now := time.Now()
	r.job.LastCheckpointAt = &now
	if !keepRunning {
		r.job.State = JobStateSuspended
		suspendUntil := now.Add(suspendDuration)
		r.job.SuspendUntil = &suspendUntil
	}
	// When keepRunning=true, state stays as 'running' - we just record the checkpoint
	r.jobMu.Unlock()

	// Perform the checkpoint (this may stop the container on darwin)
	checkpoint, err := r.process.Checkpoint(ctx, keepRunning)
	if err != nil {
		// Revert state on failure
		r.jobMu.Lock()
		r.job.CheckpointCount--
		r.job.State = JobStateRunning
		r.job.SuspendUntil = nil
		r.jobMu.Unlock()
		return nil, fmt.Errorf("checkpoint failed: %w", err)
	}
	r.checkpoint = checkpoint

	// Store the token for idempotency on retry
	r.checkpointTokensMu.Lock()
	r.checkpointTokens[token] = struct{}{}
	r.checkpointTokensMu.Unlock()

	// Persist checkpoint state to DB
	r.jobMu.RLock()
	dbState := query.JobStateRunning
	if !keepRunning {
		dbState = query.JobStateSuspended
	}
	lastCheckpointAt := *r.job.LastCheckpointAt
	suspendUntilCopy := r.job.SuspendUntil
	r.jobMu.RUnlock()
	r.persistJobCheckpointed(dbState, lastCheckpointAt, suspendUntilCopy, checkpoint.Path())

	// Schedule the wake timer now that the checkpoint is stored
	r.jobMu.Lock()
	defer r.jobMu.Unlock()
	if !keepRunning && r.job.SuspendUntil != nil {
		r.scheduleSuspendWake(*r.job.SuspendUntil)
	}
	return r.job.Clone(), nil
}

func (r *JobRunner) Kill(ctx context.Context) (*Job, error) {
	r.jobMu.RLock()
	if r.job.IsTerminal() {
		r.jobMu.RUnlock()
		return nil, fmt.Errorf("job has already terminated")
	}
	r.jobMu.RUnlock()

	if err := r.process.Kill(ctx); err != nil {
		return nil, fmt.Errorf("kill failed: %w", err)
	}

	r.jobMu.Lock()
	r.job.State = JobStateFailed
	r.job.Error = "killed"
	now := time.Now()
	r.job.CompletedAt = &now
	result := r.job.Clone()
	r.jobMu.Unlock()

	r.persistJobCompleted(query.JobStateFailed, &now, nil, "killed")

	r.MarkDone()

	return result, nil
}

// This is called by the job via the runtime API.
func (r *JobRunner) Exit(ctx context.Context, exitCode int, output json.RawMessage) error {
	r.jobMu.Lock()

	if r.job.IsTerminal() {
		r.jobMu.Unlock()
		return fmt.Errorf("job has already terminated")
	}

	var errorMsg string
	if exitCode == 0 {
		r.job.State = JobStateCompleted
	} else {
		r.job.State = JobStateFailed
		errorMsg = fmt.Sprintf("exit code %d", exitCode)
		r.job.Error = errorMsg
	}
	r.job.Result = &JobResult{
		ExitCode: exitCode,
		Output:   output,
	}
	now := time.Now()
	r.job.CompletedAt = &now

	state := r.job.State
	result := r.job.Result
	r.jobMu.Unlock()

	// Persist exit state to DB
	var dbState query.JobState
	if state == JobStateCompleted {
		dbState = query.JobStateCompleted
	} else {
		dbState = query.JobStateFailed
	}
	r.persistJobCompleted(dbState, &now, result, errorMsg)

	// Don't call MarkDone here - waitForExit will handle it when process actually exits
	return nil
}

// TakeLock takes a checkpoint lock, preventing checkpointing until released.
// Returns a lock ID that must be passed to ReleaseLock.
func (r *JobRunner) TakeLock(ctx context.Context) (string, error) {
	// Check if terminated
	select {
	case <-r.doneChan:
		return "", fmt.Errorf("job has terminated")
	default:
	}

	// Take a read lock - allows multiple concurrent locks but blocks checkpoint
	r.checkpointMu.RLock()

	r.activeLocksMu.Lock()
	r.lockCounter++
	lockID := fmt.Sprintf("lock-%d", r.lockCounter)
	r.activeLocks[lockID] = time.Now()
	r.activeLocksMu.Unlock()

	return lockID, nil
}

// ReleaseLock releases a previously taken checkpoint lock.
func (r *JobRunner) ReleaseLock(ctx context.Context, lockID string) error {
	r.activeLocksMu.Lock()
	if _, exists := r.activeLocks[lockID]; !exists {
		r.activeLocksMu.Unlock()
		return fmt.Errorf("lock %q not found or already released", lockID)
	}
	delete(r.activeLocks, lockID)
	r.activeLocksMu.Unlock()

	// Release the read lock
	r.checkpointMu.RUnlock()

	return nil
}

// WaitForResult returns a channel that will be closed when the job completes.
func (r *JobRunner) WaitForResult() <-chan struct{} {
	// Check if already done
	select {
	case <-r.doneChan:
		// Already done, return a closed channel
		ch := make(chan struct{})
		close(ch)
		return ch
	default:
	}

	r.waitersMu.Lock()
	defer r.waitersMu.Unlock()

	// Double-check after acquiring lock
	select {
	case <-r.doneChan:
		ch := make(chan struct{})
		close(ch)
		return ch
	default:
	}

	ch := make(chan struct{})
	r.resultWaiters = append(r.resultWaiters, ch)
	return ch
}

// Done returns a channel that is closed when the actor terminates.
func (r *JobRunner) Done() <-chan struct{} {
	return r.doneChan
}

// Stop gracefully stops the actor.
func (r *JobRunner) Stop() {
	r.cancel()
}

// persistJobCompleted persists the job completion state to the database.
func (r *JobRunner) persistJobCompleted(state query.JobState, completedAt *time.Time, result *JobResult, errorMsg string) {
	var resultExitCode pgtype.Int4
	var resultOutput []byte
	var errStr sql.NullString

	if result != nil {
		resultExitCode = pgtype.Int4{Int32: int32(result.ExitCode), Valid: true}
		if result.Output != nil {
			resultOutput = result.Output
		}
	}

	if errorMsg != "" {
		errStr = sql.NullString{String: errorMsg, Valid: true}
	}

	var completedAtSQL sql.NullTime
	if completedAt != nil {
		completedAtSQL = sql.NullTime{Time: *completedAt, Valid: true}
	}

	if dbErr := query.ReliableExecInTx(context.Background(), r.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		return q.UpdateJobCompleted(ctx, query.UpdateJobCompletedParams{
			ID:             r.job.ID,
			State:          state,
			CompletedAt:    completedAtSQL,
			ResultExitCode: resultExitCode,
			ResultOutput:   resultOutput,
			Error:          errStr,
		})
	}); dbErr != nil {
		r.logger.Error().Err(dbErr).Msg("failed to persist job completed state to DB")
	}
}

// persistJobCheckpointed persists the job checkpoint state to the database.
func (r *JobRunner) persistJobCheckpointed(state query.JobState, lastCheckpointAt time.Time, suspendUntil *time.Time, checkpointPath string) {
	var suspendUntilSQL sql.NullTime
	if suspendUntil != nil {
		suspendUntilSQL = sql.NullTime{Time: *suspendUntil, Valid: true}
	}

	if dbErr := query.ReliableExecInTx(context.Background(), r.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		return q.UpdateJobCheckpointed(ctx, query.UpdateJobCheckpointedParams{
			ID:               r.job.ID,
			State:            state,
			LastCheckpointAt: sql.NullTime{Time: lastCheckpointAt, Valid: true},
			SuspendUntil:     suspendUntilSQL,
			CheckpointPath:   sql.NullString{String: checkpointPath, Valid: checkpointPath != ""},
		})
	}); dbErr != nil {
		r.logger.Error().Err(dbErr).Msg("failed to persist job checkpoint state to DB")
	}
}

// persistJobRetryCount persists the job retry count to the database.
func (r *JobRunner) persistJobRetryCount(retryCount int) {
	if dbErr := query.ReliableExecInTx(context.Background(), r.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		return q.UpdateJobRetryCount(ctx, query.UpdateJobRetryCountParams{
			ID:         r.job.ID,
			RetryCount: int32(retryCount),
		})
	}); dbErr != nil {
		r.logger.Error().Err(dbErr).Msg("failed to persist job retry count to DB")
	}
}

// persistJobState persists just the job state to the database.
func (r *JobRunner) persistJobState(state query.JobState) {
	if dbErr := query.ReliableExecInTx(context.Background(), r.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		return q.UpdateJobState(ctx, query.UpdateJobStateParams{
			ID:    r.job.ID,
			State: state,
		})
	}); dbErr != nil {
		r.logger.Error().Err(dbErr).Msg("failed to persist job state to DB")
	}
}
