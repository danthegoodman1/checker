package hypervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/danthegoodman1/checker/pg"
	"github.com/danthegoodman1/checker/query"
	"github.com/danthegoodman1/checker/runtime"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// Command types for the command channel
type commandType int

const (
	cmdStart commandType = iota
	cmdCheckpoint
	cmdKill
	cmdExit
	cmdGetState
	cmdTakeLock
	cmdReleaseLock
	cmdWaitForResult
	cmdProcessExited
	cmdRetry
	cmdWake
	cmdStop
)

// command represents a request to the job runner
type command struct {
	typ        commandType
	ctx        context.Context
	resultChan chan<- commandResult

	// Command-specific payloads
	suspendDuration time.Duration   // for checkpoint
	token           string          // for checkpoint (idempotency)
	exitCode        int             // for exit/processExited
	exitOutput      json.RawMessage // for exit
	exitError       error           // for processExited
	lockID          string          // for releaseLock
}

// commandResult is the result of a command
type commandResult struct {
	job    *Job
	lockID string
	err    error
}

// RetryDecision is returned by the failure callback to indicate whether to retry.
type RetryDecision struct {
	ShouldRetry bool
	RetryDelay  time.Duration
}

// JobRunner manages the lifecycle of a single job using a state machine.
type JobRunner struct {
	job            *Job
	definition     *JobDefinition
	rt             runtime.Runtime
	process        runtime.Process
	checkpoint     runtime.Checkpoint
	config         any
	apiHostAddress string
	logger         zerolog.Logger

	// Stdout and Stderr writers for process output
	stdout io.Writer
	stderr io.Writer

	// Command channel - all operations go through this
	cmdChan chan command

	// Done channel signals when the job has terminated
	doneChan chan struct{}

	// Result waiters - channels to notify when job completes
	resultWaiters []chan struct{}

	// Checkpoint lock tracking
	activeLocks map[string]time.Time
	lockCounter int64

	// Checkpoint tokens for idempotency
	checkpointTokens map[string]struct{}

	// Track pending checkpoint requests for collapsing
	pendingCheckpointRequests []chan<- commandResult

	// Track when a retry is pending (scheduled via timer)
	retryPending bool

	// Context for cancellation
	ctx    context.Context
	cancel context.CancelFunc

	// onFailure is called when the job fails to determine retry behavior.
	// It receives the job state and exit code, and returns a decision.
	onFailure func(job *Job, exitCode int) RetryDecision

	// Database pool for persisting state changes
	pool *pgxpool.Pool
}

func NewJobRunner(job *Job, definition *JobDefinition, rt runtime.Runtime, config any, apiHostAddress string, pool *pgxpool.Pool, stdout, stderr io.Writer) *JobRunner {
	ctx, cancel := context.WithCancel(context.Background())

	r := &JobRunner{
		job:              job,
		definition:       definition,
		rt:               rt,
		config:           config,
		apiHostAddress:   apiHostAddress,
		pool:             pool,
		stdout:           stdout,
		stderr:           stderr,
		logger:           logger.With().Str("job_id", job.ID).Logger(),
		cmdChan:          make(chan command, 16),
		doneChan:         make(chan struct{}),
		activeLocks:      make(map[string]time.Time),
		checkpointTokens: make(map[string]struct{}),
		ctx:              ctx,
		cancel:           cancel,
	}

	go r.commandLoop()

	return r
}

// commandLoop processes all commands sequentially
func (r *JobRunner) commandLoop() {
	defer close(r.doneChan)

	for {
		select {
		case cmd := <-r.cmdChan:
			r.handleCommand(cmd)

			// Check if we're in a terminal state after handling
			// Don't exit if a retry is pending (failure callback is executing)
			if r.job.IsTerminal() && !r.retryPending {
				r.notifyWaiters()
				r.drainRemainingCommands()
				return
			}

			// Exit if job is pending_retry with a delay - the resume poller will handle it
			// The runner should be evicted from memory so the poller can pick up the job
			if r.job.State == JobStatePendingRetry {
				r.notifyWaiters()
				r.drainRemainingCommands()
				return
			}

		case <-r.ctx.Done():
			r.drainRemainingCommands()
			return
		}
	}
}

// drainRemainingCommands handles any remaining commands after shutdown
func (r *JobRunner) drainRemainingCommands() {
	for {
		select {
		case cmd := <-r.cmdChan:
			if cmd.resultChan != nil {
				cmd.resultChan <- commandResult{err: fmt.Errorf("job runner stopped")}
			}
		default:
			return
		}
	}
}

// handleCommand processes a single command
func (r *JobRunner) handleCommand(cmd command) {
	switch cmd.typ {
	case cmdStart:
		r.handleStart(cmd)
	case cmdCheckpoint:
		r.handleCheckpoint(cmd)
	case cmdKill:
		r.handleKill(cmd)
	case cmdExit:
		r.handleExit(cmd)
	case cmdGetState:
		r.handleGetState(cmd)
	case cmdTakeLock:
		r.handleTakeLock(cmd)
	case cmdReleaseLock:
		r.handleReleaseLock(cmd)
	case cmdWaitForResult:
		r.handleWaitForResult(cmd)
	case cmdProcessExited:
		r.handleProcessExited(cmd)
	case cmdRetry:
		r.handleRetry(cmd)
	case cmdWake:
		r.handleWake(cmd)
	case cmdStop:
		r.handleStop(cmd)
	}
}

func (r *JobRunner) handleStart(cmd command) {
	// Can only start from Pending state
	if r.job.State != JobStatePending {
		if cmd.resultChan != nil {
			cmd.resultChan <- commandResult{err: fmt.Errorf("cannot start job in state %s", r.job.State)}
		}
		return
	}

	process, err := r.rt.Start(r.ctx, runtime.StartOptions{
		ExecutionID:    r.job.ID,
		Env:            r.job.Env,
		Config:         r.config,
		APIHostAddress: r.apiHostAddress,
		Stdout:         r.stdout,
		Stderr:         r.stderr,
	})
	if err != nil {
		r.job.State = JobStateFailed
		r.job.Error = fmt.Sprintf("failed to start: %v", err)
		r.persistJobCompleted(query.JobStateFailed, nil, nil, r.job.Error)
		if cmd.resultChan != nil {
			cmd.resultChan <- commandResult{err: err}
		}
		return
	}

	r.process = process
	now := time.Now()
	r.job.StartedAt = &now
	r.job.State = JobStateRunning

	if dbErr := query.ReliableExecInTx(r.ctx, r.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		return q.UpdateJobStarted(ctx, query.UpdateJobStartedParams{
			ID:        r.job.ID,
			StartedAt: sql.NullTime{Time: now, Valid: true},
		})
	}); dbErr != nil {
		r.logger.Error().Err(dbErr).Msg("failed to persist job started state to DB")
	}

	go r.waitForProcessExit()

	if cmd.resultChan != nil {
		cmd.resultChan <- commandResult{job: r.job.Clone()}
	}
}

// waitForProcessExit waits for the process to exit and sends a command
func (r *JobRunner) waitForProcessExit() {
	exitCode, err := r.process.Wait(r.ctx)

	r.cmdChan <- command{
		typ:       cmdProcessExited,
		exitCode:  exitCode,
		exitError: err,
	}
}

// handleProcessExited handles the process exit event
func (r *JobRunner) handleProcessExited(cmd command) {
	if r.job.IsTerminal() {
		return
	}

	// If suspended, this is expected - don't update state
	if r.job.State == JobStateSuspended {
		return
	}

	now := time.Now()
	r.job.CompletedAt = &now

	if cmd.exitError != nil {
		r.job.State = JobStateFailed
		r.job.Error = cmd.exitError.Error()
	} else if cmd.exitCode != 0 {
		r.job.State = JobStateFailed
		r.job.Error = fmt.Sprintf("exit code %d", cmd.exitCode)
		r.job.Result = &JobResult{ExitCode: cmd.exitCode}
	} else {
		r.job.State = JobStateCompleted
		if r.job.Result == nil {
			r.job.Result = &JobResult{ExitCode: 0}
		}
	}

	failed := r.job.State == JobStateFailed

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cleanupCancel()
	if cleanupErr := r.process.Cleanup(cleanupCtx); cleanupErr != nil {
		r.logger.Error().Err(cleanupErr).Msg("failed to cleanup process")
	}

	if failed && r.onFailure != nil {
		decision := r.onFailure(r.job.Clone(), cmd.exitCode)
		if decision.ShouldRetry {
			if decision.RetryDelay > 0 {
				// Persist to DB as pending_retry - the resume poller will handle it
				resumeAt := time.Now().Add(decision.RetryDelay)
				r.job.State = JobStatePendingRetry
				r.job.ResumeAt = &resumeAt
				r.persistJobPendingRetry(resumeAt)
				r.logger.Info().
					Time("resume_at", resumeAt).
					Dur("retry_delay", decision.RetryDelay).
					Msg("job scheduled for retry")
			} else {
				// Retry immediately via command (processed after this handler returns)
				r.retryPending = true
				go func() {
					select {
					case r.cmdChan <- command{typ: cmdRetry, ctx: r.ctx}:
					case <-r.doneChan:
						// Runner already stopped
					}
				}()
			}
			return
		}
		r.persistJobCompleted(query.JobStateFailed, r.job.CompletedAt, r.job.Result, r.job.Error)
		return
	}

	var dbState query.JobState
	if r.job.State == JobStateCompleted {
		dbState = query.JobStateCompleted
	} else {
		dbState = query.JobStateFailed
	}
	r.persistJobCompleted(dbState, r.job.CompletedAt, r.job.Result, r.job.Error)
}

// handleCheckpoint handles a checkpoint request
func (r *JobRunner) handleCheckpoint(cmd command) {
	if cmd.token == "" {
		if cmd.resultChan != nil {
			cmd.resultChan <- commandResult{err: fmt.Errorf("checkpoint token is required")}
		}
		return
	}

	// Check for idempotent replay
	if _, exists := r.checkpointTokens[cmd.token]; exists {
		r.logger.Debug().Str("token", cmd.token).Msg("checkpoint token already used, returning success (idempotent replay)")
		if cmd.resultChan != nil {
			cmd.resultChan <- commandResult{job: r.job.Clone()}
		}
		return
	}

	if r.job.IsTerminal() {
		if cmd.resultChan != nil {
			cmd.resultChan <- commandResult{err: fmt.Errorf("job has terminated")}
		}
		return
	}

	// If already checkpointing, collapse this request
	if r.job.CurrentOperation == JobOpCheckpointing {
		r.logger.Debug().Str("token", cmd.token).Msg("checkpoint already in progress, collapsing request")
		r.pendingCheckpointRequests = append(r.pendingCheckpointRequests, cmd.resultChan)
		return
	}

	if len(r.activeLocks) > 0 {
		if cmd.resultChan != nil {
			cmd.resultChan <- commandResult{err: fmt.Errorf("cannot checkpoint: %d active locks", len(r.activeLocks))}
		}
		return
	}

	// Can only checkpoint from Running state
	if r.job.State != JobStateRunning {
		if cmd.resultChan != nil {
			cmd.resultChan <- commandResult{err: fmt.Errorf("cannot checkpoint job in state %s", r.job.State)}
		}
		return
	}

	r.job.CurrentOperation = JobOpCheckpointing
	r.persistJobOperation(JobOpCheckpointing)
	keepRunning := cmd.suspendDuration == 0

	// Update state before stopping container
	r.job.CheckpointCount++
	now := time.Now()
	r.job.LastCheckpointAt = &now
	if !keepRunning {
		resumeAt := now.Add(cmd.suspendDuration)
		r.job.ResumeAt = &resumeAt
	}

	checkpoint, err := r.process.Checkpoint(cmd.ctx, keepRunning)
	if err != nil {
		r.job.CheckpointCount--
		r.job.CurrentOperation = ""
		r.job.ResumeAt = nil

		checkpointErr := fmt.Errorf("checkpoint failed: %w", err)
		if cmd.resultChan != nil {
			cmd.resultChan <- commandResult{err: checkpointErr}
		}
		// Notify collapsed requests
		for _, ch := range r.pendingCheckpointRequests {
			if ch != nil {
				ch <- commandResult{err: checkpointErr}
			}
		}
		r.pendingCheckpointRequests = nil
		return
	}

	r.checkpoint = checkpoint
	r.checkpointTokens[cmd.token] = struct{}{}
	r.job.CurrentOperation = ""

	if !keepRunning {
		r.job.State = JobStateSuspended
	}

	dbState := query.JobStateRunning
	if !keepRunning {
		dbState = query.JobStateSuspended
	}
	if dbErr := r.persistJobCheckpointedWithError(dbState, *r.job.LastCheckpointAt, r.job.ResumeAt, checkpoint.Path()); dbErr != nil {
		r.logger.Error().Err(dbErr).Msg("CRITICAL: checkpoint succeeded but DB persist failed")
	}

	if cmd.resultChan != nil {
		cmd.resultChan <- commandResult{job: r.job.Clone()}
	}

	for _, ch := range r.pendingCheckpointRequests {
		if ch != nil {
			r.checkpointTokens[cmd.token] = struct{}{} // They share the token
			ch <- commandResult{job: r.job.Clone()}
		}
	}
	r.pendingCheckpointRequests = nil
}

// handleWake handles waking from suspended state
func (r *JobRunner) handleWake(cmd command) {
	// Can only wake from Suspended state
	if r.job.State != JobStateSuspended {
		r.logger.Debug().Str("state", string(r.job.State)).Msg("job not suspended, skipping wake")
		if cmd.resultChan != nil {
			cmd.resultChan <- commandResult{err: fmt.Errorf("job not suspended")}
		}
		return
	}

	r.job.State = JobStateRunning
	r.job.ResumeAt = nil

	r.persistJobState(query.JobStateRunning)

	stdout := r.stdout
	stderr := r.stderr
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}

	process, err := r.rt.Restore(r.ctx, runtime.RestoreOptions{
		Checkpoint: r.checkpoint,
		Stdout:     stdout,
		Stderr:     stderr,
	})
	if err != nil {
		r.logger.Error().Err(err).Msg("failed to restore from checkpoint")
		r.job.State = JobStateFailed
		r.job.Error = fmt.Sprintf("failed to restore: %v", err)
		now := time.Now()
		r.job.CompletedAt = &now
		r.persistJobCompleted(query.JobStateFailed, r.job.CompletedAt, nil, r.job.Error)
		if cmd.resultChan != nil {
			cmd.resultChan <- commandResult{err: err}
		}
		return
	}

	r.process = process
	r.logger.Debug().Msg("job restored from checkpoint")

	go r.waitForProcessExit()

	if cmd.resultChan != nil {
		cmd.resultChan <- commandResult{job: r.job.Clone()}
	}
}

// handleKill handles a kill request
func (r *JobRunner) handleKill(cmd command) {
	// Cannot kill already terminal jobs
	if r.job.IsTerminal() {
		if cmd.resultChan != nil {
			cmd.resultChan <- commandResult{err: fmt.Errorf("job has already terminated")}
		}
		return
	}

	// Can kill from Running or Suspended states
	if r.job.State != JobStateRunning && r.job.State != JobStateSuspended {
		if cmd.resultChan != nil {
			cmd.resultChan <- commandResult{err: fmt.Errorf("cannot kill job in state %s", r.job.State)}
		}
		return
	}

	r.job.CurrentOperation = JobOpTerminating
	r.persistJobOperation(JobOpTerminating)

	// For suspended jobs, we don't have a running process to kill
	if r.process != nil {
		if err := r.process.Kill(cmd.ctx); err != nil {
			if cmd.resultChan != nil {
				cmd.resultChan <- commandResult{err: fmt.Errorf("kill failed: %w", err)}
			}
			return
		}
	}

	r.job.State = JobStateFailed
	r.job.Error = "killed"
	now := time.Now()
	r.job.CompletedAt = &now

	r.persistJobCompleted(query.JobStateFailed, &now, nil, "killed")

	if cmd.resultChan != nil {
		cmd.resultChan <- commandResult{job: r.job.Clone()}
	}
}

// handleExit handles the job calling the exit API
func (r *JobRunner) handleExit(cmd command) {
	// Cannot exit already terminal jobs
	if r.job.IsTerminal() {
		if cmd.resultChan != nil {
			cmd.resultChan <- commandResult{err: fmt.Errorf("job has already terminated")}
		}
		return
	}

	// Can only exit from Running state
	if r.job.State != JobStateRunning {
		if cmd.resultChan != nil {
			cmd.resultChan <- commandResult{err: fmt.Errorf("cannot exit job in state %s", r.job.State)}
		}
		return
	}

	r.job.CurrentOperation = JobOpExiting

	var errorMsg string
	if cmd.exitCode == 0 {
		r.job.State = JobStateCompleted
	} else {
		r.job.State = JobStateFailed
		errorMsg = fmt.Sprintf("exit code %d", cmd.exitCode)
		r.job.Error = errorMsg
	}

	r.job.Result = &JobResult{
		ExitCode: cmd.exitCode,
		Output:   cmd.exitOutput,
	}
	now := time.Now()
	r.job.CompletedAt = &now

	var dbState query.JobState
	if r.job.State == JobStateCompleted {
		dbState = query.JobStateCompleted
	} else {
		dbState = query.JobStateFailed
	}
	r.persistJobCompleted(dbState, &now, r.job.Result, errorMsg)

	if cmd.resultChan != nil {
		cmd.resultChan <- commandResult{job: r.job.Clone()}
	}
}

// handleGetState returns the current job state
func (r *JobRunner) handleGetState(cmd command) {
	if cmd.resultChan != nil {
		cmd.resultChan <- commandResult{job: r.job.Clone()}
	}
}

// handleTakeLock handles taking a checkpoint lock
func (r *JobRunner) handleTakeLock(cmd command) {
	if r.job.IsTerminal() {
		if cmd.resultChan != nil {
			cmd.resultChan <- commandResult{err: fmt.Errorf("job has terminated")}
		}
		return
	}

	r.lockCounter++
	lockID := fmt.Sprintf("lock-%d", r.lockCounter)
	r.activeLocks[lockID] = time.Now()

	if cmd.resultChan != nil {
		cmd.resultChan <- commandResult{lockID: lockID}
	}
}

// handleReleaseLock handles releasing a checkpoint lock
func (r *JobRunner) handleReleaseLock(cmd command) {
	if _, exists := r.activeLocks[cmd.lockID]; !exists {
		if cmd.resultChan != nil {
			cmd.resultChan <- commandResult{err: fmt.Errorf("lock %q not found or already released", cmd.lockID)}
		}
		return
	}

	delete(r.activeLocks, cmd.lockID)

	if cmd.resultChan != nil {
		cmd.resultChan <- commandResult{}
	}
}

// handleWaitForResult registers a waiter for job completion
func (r *JobRunner) handleWaitForResult(cmd command) {
	if r.job.IsTerminal() {
		if cmd.resultChan != nil {
			cmd.resultChan <- commandResult{job: r.job.Clone()}
		}
		return
	}

	waitChan := make(chan struct{})
	r.resultWaiters = append(r.resultWaiters, waitChan)

	if cmd.resultChan != nil {
		// Return the wait channel through the result
		// The caller will wait on this channel
		go func() {
			<-waitChan
			cmd.resultChan <- commandResult{job: r.job.Clone()}
		}()
	}
}

// handleRetry handles a retry request
func (r *JobRunner) handleRetry(cmd command) {
	// Clear retry pending flag - we're handling the retry now
	r.retryPending = false

	// Can only retry from Failed or PendingRetry state
	if r.job.State != JobStateFailed && r.job.State != JobStatePendingRetry {
		if cmd.resultChan != nil {
			cmd.resultChan <- commandResult{err: fmt.Errorf("can only retry failed jobs, current state: %s", r.job.State)}
		}
		return
	}

	r.job.CurrentOperation = JobOpRestarting
	r.persistJobOperation(JobOpRestarting)
	r.job.RetryCount++
	r.job.Error = ""
	r.job.Result = nil
	r.job.CompletedAt = nil

	r.persistJobRetryCount(r.job.RetryCount)

	hasCheckpoint := r.checkpoint != nil

	if hasCheckpoint {
		r.logger.Info().Int("retry_count", r.job.RetryCount).Msg("retrying job from checkpoint")

		stdout := r.stdout
		stderr := r.stderr
		if stdout == nil {
			stdout = os.Stdout
		}
		if stderr == nil {
			stderr = os.Stderr
		}

		process, err := r.rt.Restore(r.ctx, runtime.RestoreOptions{
			Checkpoint: r.checkpoint,
			Stdout:     stdout,
			Stderr:     stderr,
		})
		if err != nil {
			r.logger.Error().Err(err).Msg("failed to restore from checkpoint during retry")
			r.job.State = JobStateFailed
			r.job.Error = fmt.Sprintf("failed to restore: %v", err)
			now := time.Now()
			r.job.CompletedAt = &now
			if cmd.resultChan != nil {
				cmd.resultChan <- commandResult{err: err}
			}
			return
		}

		r.process = process
		r.job.State = JobStateRunning
		r.job.ResumeAt = nil

		now := time.Now()
		if dbErr := query.ReliableExecInTx(r.ctx, r.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
			return q.UpdateJobStarted(ctx, query.UpdateJobStartedParams{
				ID:        r.job.ID,
				StartedAt: sql.NullTime{Time: now, Valid: true},
			})
		}); dbErr != nil {
			r.logger.Error().Err(dbErr).Msg("failed to persist job started state to DB")
		}

		go r.waitForProcessExit()
	} else {
		r.logger.Info().Int("retry_count", r.job.RetryCount).Msg("retrying job from start (no checkpoint)")

		process, err := r.rt.Start(r.ctx, runtime.StartOptions{
			ExecutionID:    r.job.ID,
			Env:            r.job.Env,
			Config:         r.config,
			APIHostAddress: r.apiHostAddress,
			Stdout:         r.stdout,
			Stderr:         r.stderr,
		})
		if err != nil {
			r.job.State = JobStateFailed
			r.job.Error = fmt.Sprintf("failed to start: %v", err)
			r.persistJobCompleted(query.JobStateFailed, nil, nil, r.job.Error)
			if cmd.resultChan != nil {
				cmd.resultChan <- commandResult{err: err}
			}
			return
		}

		r.process = process
		r.job.State = JobStateRunning
		now := time.Now()
		r.job.StartedAt = &now

		if dbErr := query.ReliableExecInTx(r.ctx, r.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
			return q.UpdateJobStarted(ctx, query.UpdateJobStartedParams{
				ID:        r.job.ID,
				StartedAt: sql.NullTime{Time: now, Valid: true},
			})
		}); dbErr != nil {
			r.logger.Error().Err(dbErr).Msg("failed to persist job started state to DB")
		}

		go r.waitForProcessExit()
	}

	if cmd.resultChan != nil {
		cmd.resultChan <- commandResult{job: r.job.Clone()}
	}
}

// handleStop handles stopping the runner
func (r *JobRunner) handleStop(cmd command) {
	r.cancel()
	if cmd.resultChan != nil {
		cmd.resultChan <- commandResult{}
	}
}

// notifyWaiters signals all result waiters that the job has completed
func (r *JobRunner) notifyWaiters() {
	for _, ch := range r.resultWaiters {
		close(ch)
	}
	r.resultWaiters = nil
}

// ============================================================================
// Public API - thin wrappers that send commands and wait for results
// ============================================================================

// sendCommand sends a command and waits for the result.
// Returns an error result if the runner stops before the command completes.
func (r *JobRunner) sendCommand(cmd command) commandResult {
	resultChan := make(chan commandResult, 1)
	cmd.resultChan = resultChan

	select {
	case r.cmdChan <- cmd:
	case <-r.doneChan:
		return commandResult{err: fmt.Errorf("job runner stopped")}
	}

	select {
	case result := <-resultChan:
		return result
	case <-r.doneChan:
		return commandResult{err: fmt.Errorf("job runner stopped")}
	}
}

// sendCommandWithCtx is like sendCommand but also handles context cancellation.
func (r *JobRunner) sendCommandWithCtx(ctx context.Context, cmd command) commandResult {
	resultChan := make(chan commandResult, 1)
	cmd.resultChan = resultChan

	select {
	case r.cmdChan <- cmd:
	case <-r.doneChan:
		return commandResult{err: fmt.Errorf("job runner stopped")}
	case <-ctx.Done():
		return commandResult{err: ctx.Err()}
	}

	select {
	case result := <-resultChan:
		return result
	case <-r.doneChan:
		return commandResult{err: fmt.Errorf("job runner stopped")}
	case <-ctx.Done():
		return commandResult{err: ctx.Err()}
	}
}

// sendCommandGraceful sends a command with context support, but returns
// gracefully (with current job state) if the runner is already done.
func (r *JobRunner) sendCommandGraceful(ctx context.Context, cmd command) commandResult {
	resultChan := make(chan commandResult, 1)
	cmd.resultChan = resultChan

	select {
	case r.cmdChan <- cmd:
	case <-r.doneChan:
		return commandResult{job: r.job.Clone()}
	case <-ctx.Done():
		return commandResult{err: ctx.Err()}
	}

	select {
	case result := <-resultChan:
		return result
	case <-r.doneChan:
		return commandResult{job: r.job.Clone()}
	case <-ctx.Done():
		return commandResult{err: ctx.Err()}
	}
}

func (r *JobRunner) Start() error {
	return r.sendCommand(command{typ: cmdStart, ctx: r.ctx}).err
}

// Retry restarts the job with incremented retry count.
func (r *JobRunner) Retry() error {
	return r.sendCommand(command{typ: cmdRetry, ctx: r.ctx}).err
}

func (r *JobRunner) GetState(ctx context.Context) (*Job, error) {
	result := r.sendCommandGraceful(ctx, command{typ: cmdGetState, ctx: ctx})
	return result.job, result.err
}

func (r *JobRunner) Checkpoint(ctx context.Context, suspendDuration time.Duration, token string) (*Job, error) {
	result := r.sendCommandWithCtx(ctx, command{
		typ:             cmdCheckpoint,
		ctx:             ctx,
		suspendDuration: suspendDuration,
		token:           token,
	})
	return result.job, result.err
}

func (r *JobRunner) Kill(ctx context.Context) (*Job, error) {
	result := r.sendCommandGraceful(ctx, command{typ: cmdKill, ctx: ctx})
	return result.job, result.err
}

// Exit marks the job as completed with a result (called by runtime API).
func (r *JobRunner) Exit(ctx context.Context, exitCode int, output json.RawMessage) error {
	result := r.sendCommandGraceful(ctx, command{
		typ:        cmdExit,
		ctx:        ctx,
		exitCode:   exitCode,
		exitOutput: output,
	})
	return result.err
}

// TakeLock takes a checkpoint lock, preventing checkpointing until released.
func (r *JobRunner) TakeLock(ctx context.Context) (string, error) {
	result := r.sendCommandWithCtx(ctx, command{typ: cmdTakeLock, ctx: ctx})
	return result.lockID, result.err
}

// ReleaseLock releases a previously taken checkpoint lock.
func (r *JobRunner) ReleaseLock(ctx context.Context, lockID string) error {
	result := r.sendCommandGraceful(ctx, command{typ: cmdReleaseLock, ctx: ctx, lockID: lockID})
	return result.err
}

// WaitForResult returns a channel that will be closed when the job completes.
func (r *JobRunner) WaitForResult() <-chan struct{} {
	select {
	case <-r.doneChan:
		ch := make(chan struct{})
		close(ch)
		return ch
	default:
	}

	waitChan := make(chan struct{})
	resultChan := make(chan commandResult, 1)

	select {
	case r.cmdChan <- command{typ: cmdWaitForResult, resultChan: resultChan}:
	case <-r.doneChan:
		close(waitChan)
		return waitChan
	}

	go func() {
		select {
		case <-resultChan:
		case <-r.doneChan:
		}
		close(waitChan)
	}()

	return waitChan
}

// Done returns a channel that is closed when the runner terminates.
func (r *JobRunner) Done() <-chan struct{} {
	return r.doneChan
}

// Stop gracefully stops the runner.
func (r *JobRunner) Stop() {
	select {
	case r.cmdChan <- command{typ: cmdStop}:
	case <-r.doneChan:
	}
}

func (r *JobRunner) Cancel() {
	r.cancel()
}

// SetOnFailure sets the callback for when the job fails.
func (r *JobRunner) SetOnFailure(fn func(job *Job, exitCode int) RetryDecision) {
	r.onFailure = fn
}

// SetCheckpoint sets the checkpoint for restoration (used during recovery).
func (r *JobRunner) SetCheckpoint(checkpoint runtime.Checkpoint) {
	r.checkpoint = checkpoint
}

// ============================================================================
// Database persistence helpers
// ============================================================================

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

	if dbErr := query.ReliableExecInTx(r.ctx, r.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
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

func (r *JobRunner) persistJobCheckpointedWithError(state query.JobState, lastCheckpointAt time.Time, resumeAt *time.Time, checkpointPath string) error {
	var resumeAtSQL sql.NullTime
	if resumeAt != nil {
		resumeAtSQL = sql.NullTime{Time: *resumeAt, Valid: true}
	}

	if dbErr := query.ReliableExecInTx(r.ctx, r.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		return q.UpdateJobCheckpointed(ctx, query.UpdateJobCheckpointedParams{
			ID:               r.job.ID,
			State:            state,
			LastCheckpointAt: sql.NullTime{Time: lastCheckpointAt, Valid: true},
			ResumeAt:         resumeAtSQL,
			CheckpointPath:   sql.NullString{String: checkpointPath, Valid: checkpointPath != ""},
		})
	}); dbErr != nil {
		r.logger.Error().Err(dbErr).Msg("failed to persist job checkpoint state to DB")
		return dbErr
	}
	return nil
}

func (r *JobRunner) persistJobRetryCount(retryCount int) {
	if dbErr := query.ReliableExecInTx(r.ctx, r.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		return q.UpdateJobRetryCount(ctx, query.UpdateJobRetryCountParams{
			ID:         r.job.ID,
			RetryCount: int32(retryCount),
		})
	}); dbErr != nil {
		r.logger.Error().Err(dbErr).Msg("failed to persist job retry count to DB")
	}
}

func (r *JobRunner) persistJobPendingRetry(resumeAt time.Time) {
	if dbErr := query.ReliableExecInTx(r.ctx, r.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		return q.UpdateJobPendingRetry(ctx, query.UpdateJobPendingRetryParams{
			ID:       r.job.ID,
			ResumeAt: sql.NullTime{Time: resumeAt, Valid: true},
		})
	}); dbErr != nil {
		r.logger.Error().Err(dbErr).Msg("failed to persist job pending_retry state to DB")
	}
}

func (r *JobRunner) persistJobState(state query.JobState) {
	if dbErr := query.ReliableExecInTx(r.ctx, r.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		return q.UpdateJobState(ctx, query.UpdateJobStateParams{
			ID:    r.job.ID,
			State: state,
		})
	}); dbErr != nil {
		r.logger.Error().Err(dbErr).Msg("failed to persist job state to DB")
	}
}

func (r *JobRunner) persistJobOperation(op JobOperation) {
	if dbErr := query.ReliableExecInTx(r.ctx, r.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		return q.UpdateJobOperation(ctx, query.UpdateJobOperationParams{
			ID:               r.job.ID,
			CurrentOperation: query.NullJobOperation{JobOperation: query.JobOperation(op), Valid: true},
		})
	}); dbErr != nil {
		r.logger.Error().Err(dbErr).Msg("failed to persist job operation to DB")
	}
}
