package hypervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/danthegoodman1/checker/runtime"
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
	apiHostAddress string         // Host:port for the runtime API
	logger         zerolog.Logger // Per-runner logger with job context

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

	// Context for cancellation
	ctx    context.Context
	cancel context.CancelFunc

	// onFailure is called when the job fails, allowing retry logic
	onFailure func(runner *JobRunner, exitCode int)
}

// NewJobRunner creates a new actor for a job.
func NewJobRunner(job *Job, definition *JobDefinition, rt runtime.Runtime, config any, apiHostAddress string) *JobRunner {
	ctx, cancel := context.WithCancel(context.Background())

	return &JobRunner{
		job:            job,
		definition:     definition,
		rt:             rt,
		config:         config,
		apiHostAddress: apiHostAddress,
		logger:         logger.With().Str("job_id", job.ID).Logger(),
		doneChan:       make(chan struct{}),
		activeLocks:    make(map[string]time.Time),
		ctx:            ctx,
		cancel:         cancel,
	}
}

func (r *JobRunner) Start() error {
	process, err := r.rt.Start(r.ctx, runtime.StartOptions{
		ExecutionID:    r.job.ID,
		Env:            r.job.Env,
		Config:         r.config,
		APIHostAddress: r.apiHostAddress,
	})
	if err != nil {
		r.jobMu.Lock()
		r.job.State = JobStateFailed
		r.job.Error = fmt.Sprintf("failed to start: %v", err)
		r.jobMu.Unlock()
		return err
	}

	r.process = process

	r.jobMu.Lock()
	now := time.Now()
	r.job.StartedAt = &now
	r.job.State = JobStateRunning
	r.jobMu.Unlock()

	// Start a goroutine to wait for process exit
	go r.waitForExit()

	return nil
}

// Retry restarts the job with incremented retry count.
func (r *JobRunner) Retry() error {
	r.jobMu.Lock()
	r.job.RetryCount++
	r.job.State = JobStatePending
	r.job.Error = ""
	r.job.Result = nil
	r.job.CompletedAt = nil
	r.jobMu.Unlock()

	return r.Start()
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
	if !r.job.IsTerminal() {
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
	r.jobMu.Unlock()

	// Call failure callback before marking done (allows retry)
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

		// Restore from checkpoint
		process, err := r.rt.Restore(r.ctx, r.checkpoint)
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

		// Start monitoring the restored process
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

// GetCheckpointGracePeriod returns the grace period from the runtime.
func (r *JobRunner) GetCheckpointGracePeriod() int64 {
	return r.rt.CheckpointGracePeriodMs()
}

// Checkpoint requests a checkpoint of the job.
func (r *JobRunner) Checkpoint(ctx context.Context, suspendDuration time.Duration) (*Job, error) {
	// Check if already terminal
	if _, err := func() (*Job, error) {
		r.jobMu.RLock()
		defer r.jobMu.RUnlock()
		if r.job.IsTerminal() {
			return nil, fmt.Errorf("job has terminated")
		}
		return nil, nil
	}(); err != nil {
		return nil, err
	}

	// Acquire write lock - blocks until all read locks are released
	r.checkpointMu.Lock()
	defer r.checkpointMu.Unlock()

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
	} else {
		r.job.State = JobStateCheckpointed
	}
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

	// Schedule the wake timer now that the checkpoint is stored
	r.jobMu.Lock()
	defer r.jobMu.Unlock()
	if !keepRunning && r.job.SuspendUntil != nil {
		r.scheduleSuspendWake(*r.job.SuspendUntil)
	}
	return r.job.Clone(), nil
}

func (r *JobRunner) Kill(ctx context.Context) (*Job, error) {
	// Check if already terminal
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

	r.MarkDone()

	return result, nil
}

// This is called by the job via the runtime API.
func (r *JobRunner) Exit(ctx context.Context, exitCode int, output json.RawMessage) error {
	r.jobMu.Lock()
	defer r.jobMu.Unlock()

	if r.job.IsTerminal() {
		return fmt.Errorf("job has already terminated")
	}

	if exitCode == 0 {
		r.job.State = JobStateCompleted
	} else {
		r.job.State = JobStateFailed
		r.job.Error = fmt.Sprintf("exit code %d", exitCode)
	}
	r.job.Result = &JobResult{
		ExitCode: exitCode,
		Output:   output,
	}
	now := time.Now()
	r.job.CompletedAt = &now

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
