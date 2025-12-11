package hypervisor

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	goruntime "runtime"
	"sync"
	"time"

	"github.com/danthegoodman1/checker/gologger"
	"github.com/danthegoodman1/checker/http_server"
	"github.com/danthegoodman1/checker/migrations"
	"github.com/danthegoodman1/checker/pg"
	"github.com/danthegoodman1/checker/query"
	"github.com/danthegoodman1/checker/runtime"
	"github.com/danthegoodman1/checker/utils"
	"github.com/jackc/pgx/v5/pgxpool"
)

var logger = gologger.NewLogger()

// Hypervisor is the main service that manages job definitions and job lifecycle.
type Hypervisor struct {
	definitions *JobDefinitionRegistry

	runtimes map[runtime.RuntimeType]runtime.Runtime

	runners   map[string]*JobRunner
	runnersMu sync.RWMutex

	// ID generator function
	generateID func() string

	// Context for managing lifecycle
	ctx    context.Context
	cancel context.CancelFunc

	callerHTTPAddress  string
	runtimeHTTPAddress string

	// HTTP Servers
	callerHTTPServer  *http_server.HTTPServer
	runtimeHTTPServer *http_server.HTTPServer

	// Database pool for job persistence
	pool *pgxpool.Pool

	// Wake poller interval
	wakePollerInterval time.Duration
}

// Config holds configuration options for creating a Hypervisor.
type Config struct {
	// IDGenerator is an optional custom ID generator function.
	// If nil, a default UUID-like generator is used.
	IDGenerator func() string

	// HTTP Addresses for the hypervisor API.
	// These are passed to the hypervisor so it can start HTTP servers.
	CallerHTTPAddress  string
	RuntimeHTTPAddress string

	// Pool is the database connection pool for job persistence.
	// Required for durable job storage.
	Pool *pgxpool.Pool

	// WakePollerInterval is how often to poll for suspended jobs to wake.
	// Defaults to 5 seconds if not set.
	WakePollerInterval time.Duration
}

// New creates a new Hypervisor instance.
func New(cfg Config) *Hypervisor {
	ctx, cancel := context.WithCancel(context.Background())

	h := &Hypervisor{
		definitions: NewJobDefinitionRegistry(),
		runtimes:    make(map[runtime.RuntimeType]runtime.Runtime),
		runners:     make(map[string]*JobRunner),
		ctx:         ctx,
		cancel:      cancel,
		pool:        cfg.Pool,
	}

	if cfg.IDGenerator != nil {
		h.generateID = cfg.IDGenerator
	} else {
		h.generateID = defaultIDGenerator()
	}

	h.callerHTTPAddress = cfg.CallerHTTPAddress
	h.runtimeHTTPAddress = cfg.RuntimeHTTPAddress

	// Set wake poller interval with default
	if cfg.WakePollerInterval > 0 {
		h.wakePollerInterval = cfg.WakePollerInterval
	} else {
		h.wakePollerInterval = 5 * time.Second
	}

	// Run migrations
	migrations.RunMigrations(utils.PG_DSN)

	h.callerHTTPServer = http_server.StartHTTPServer(h.callerHTTPAddress, "", h.RegisterCallerAPI)
	h.runtimeHTTPServer = http_server.StartHTTPServer(h.runtimeHTTPAddress, "", h.RegisterRuntimeAPI)

	// Start the resume poller for suspended and pending_retry jobs
	h.startResumePoller()

	return h
}

// defaultIDGenerator returns a simple ID generator.
// In production, you'd want to use UUIDs or similar.
func defaultIDGenerator() func() string {
	var counter int64
	var mu sync.Mutex
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		counter++
		return fmt.Sprintf("job-%d-%d", time.Now().UnixNano(), counter)
	}
}

// RegisterRuntime registers a runtime implementation.
// Only one runtime per type can be registered.
// Called via caller API
func (h *Hypervisor) RegisterRuntime(rt runtime.Runtime) error {
	h.runnersMu.Lock()
	defer h.runnersMu.Unlock()

	rtType := rt.Type()
	if _, exists := h.runtimes[rtType]; exists {
		return fmt.Errorf("runtime %q already registered", rtType)
	}

	h.runtimes[rtType] = rt
	return nil
}

// RegisterJobDefinition registers a job definition.
// Called via caller API
func (h *Hypervisor) RegisterJobDefinition(ctx context.Context, jd *JobDefinition) error {
	// Validate that we have a runtime for this job type
	h.runnersMu.RLock()
	_, hasRuntime := h.runtimes[jd.RuntimeType]
	h.runnersMu.RUnlock()

	if !hasRuntime {
		return fmt.Errorf("no runtime registered for type %q", jd.RuntimeType)
	}

	configJSON, err := json.Marshal(jd.Config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	var retryPolicyJSON []byte
	if jd.RetryPolicy != nil {
		retryPolicyJSON, err = json.Marshal(jd.RetryPolicy)
		if err != nil {
			return fmt.Errorf("failed to marshal retry policy: %w", err)
		}
	}

	metadataJSON, err := json.Marshal(jd.Metadata)
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	err = query.ReliableExecInTx(ctx, h.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		return q.UpsertJobDefinition(ctx, query.UpsertJobDefinitionParams{
			Name:        jd.Name,
			Version:     jd.Version,
			RuntimeType: string(jd.RuntimeType),
			Config:      configJSON,
			RetryPolicy: retryPolicyJSON,
			Metadata:    metadataJSON,
		})
	})
	if err != nil {
		return fmt.Errorf("failed to persist job definition: %w", err)
	}

	return h.definitions.Register(jd)
}

// Called via caller API
func (h *Hypervisor) UnregisterJobDefinition(ctx context.Context, name, version string) error {
	// Delete from database first
	err := query.ReliableExecInTx(ctx, h.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		return q.DeleteJobDefinition(ctx, query.DeleteJobDefinitionParams{
			Name:    name,
			Version: version,
		})
	})
	if err != nil {
		return fmt.Errorf("failed to delete job definition from database: %w", err)
	}

	return h.definitions.Unregister(name, version)
}

// Called via caller API
func (h *Hypervisor) GetJobDefinition(name, version string) (*JobDefinition, error) {
	return h.definitions.Get(name, version)
}

// Called via caller API
func (h *Hypervisor) ListJobDefinitions() []*JobDefinition {
	return h.definitions.List()
}

type SpawnOptions struct {
	DefinitionName    string
	DefinitionVersion string

	// Params are the input parameters for the job.
	Params json.RawMessage

	// Env holds additional environment variables for the job.
	// Hypervisor-set env vars (CHECKER_JOB_ID, etc.) take precedence and cannot be overridden.
	Env map[string]string

	Metadata map[string]string

	// Stdout is where process stdout will be written. If nil, stdout is discarded.
	Stdout io.Writer

	// Stderr is where process stderr will be written. If nil, stderr is discarded.
	Stderr io.Writer
}

// Called via caller API
func (h *Hypervisor) Spawn(ctx context.Context, opts SpawnOptions) (string, error) {
	jd, err := h.definitions.Get(opts.DefinitionName, opts.DefinitionVersion)
	if err != nil {
		return "", fmt.Errorf("failed to get job definition: %w", err)
	}

	h.runnersMu.RLock()
	rt, exists := h.runtimes[jd.RuntimeType]
	h.runnersMu.RUnlock()

	if !exists {
		return "", fmt.Errorf("runtime %q not found", jd.RuntimeType)
	}

	jobID := h.generateID()

	// Build environment for the job
	// Start with spawn options, then apply hypervisor defaults (which take precedence)
	env := make(map[string]string)
	for k, v := range opts.Env {
		env[k] = v
	}
	// Hypervisor defaults (cannot be overridden)
	env["CHECKER_JOB_ID"] = jobID
	env["CHECKER_JOB_DEFINITION_NAME"] = jd.Name
	env["CHECKER_JOB_DEFINITION_VERSION"] = jd.Version
	env["CHECKER_JOB_SPAWNED_AT"] = fmt.Sprintf("%d", time.Now().Unix())
	env["CHECKER_ARCH"] = goruntime.GOARCH // amd64, arm64
	env["CHECKER_OS"] = goruntime.GOOS     // linux, darwin, windows

	job := &Job{
		ID:                jobID,
		DefinitionName:    jd.Name,
		DefinitionVersion: jd.Version,
		State:             JobStatePending,
		Env:               env,
		Params:            opts.Params,
		CreatedAt:         time.Now(),
		Metadata:          opts.Metadata,
	}

	// Merge job definition metadata with job metadata (job takes precedence)
	if job.Metadata == nil {
		job.Metadata = make(map[string]string)
	}
	for k, v := range jd.Metadata {
		if _, exists := job.Metadata[k]; !exists {
			job.Metadata[k] = v
		}
	}

	runtimeConfigJSON, err := json.Marshal(jd.Config)
	if err != nil {
		return "", fmt.Errorf("failed to marshal runtime config: %w", err)
	}

	envJSON, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("failed to marshal env: %w", err)
	}

	metadataJSON, err := json.Marshal(job.Metadata)
	if err != nil {
		return "", fmt.Errorf("failed to marshal metadata: %w", err)
	}

	err = query.ReliableExecInTx(ctx, h.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		return q.InsertJob(ctx, query.InsertJobParams{
			ID:                jobID,
			DefinitionName:    jd.Name,
			DefinitionVersion: jd.Version,
			State:             query.JobStatePending,
			Env:               envJSON,
			Params:            opts.Params,
			CreatedAt:         job.CreatedAt,
			RuntimeType:       string(jd.RuntimeType),
			RuntimeConfig:     runtimeConfigJSON,
			Metadata:          metadataJSON,
		})
	})
	if err != nil {
		return "", fmt.Errorf("failed to persist job to database: %w", err)
	}

	runner := NewJobRunner(job, jd, rt, jd.Config, h.runtimeHTTPAddress, h.pool, opts.Stdout, opts.Stderr)
	h.setupRetryCallback(runner, jd)

	h.runnersMu.Lock()
	h.runners[jobID] = runner
	h.runnersMu.Unlock()

	if err := runner.Start(); err != nil {
		h.runnersMu.Lock()
		delete(h.runners, jobID)
		h.runnersMu.Unlock()
		return "", fmt.Errorf("failed to start job: %w", err)
	}

	// Start a goroutine to evict the job when it completes
	go func() {
		<-runner.Done()
		job, err := runner.GetState(h.ctx)
		if err != nil {
			logger.Error().Err(err).Str("job_id", jobID).Msg("failed to get job state for eviction")
			return
		}
		h.maybeEvictJob(jobID, job)
	}()

	return job.ID, nil
}

// Called via caller API
func (h *Hypervisor) GetJob(ctx context.Context, id string) (*Job, error) {
	// Try memory first
	h.runnersMu.RLock()
	runner, exists := h.runners[id]
	h.runnersMu.RUnlock()

	if exists {
		return runner.GetState(ctx)
	}

	// Fall back to DB
	return h.loadJobFromDB(ctx, id)
}

// Called via caller API
func (h *Hypervisor) KillJob(ctx context.Context, id string) error {
	h.runnersMu.RLock()
	runner, exists := h.runners[id]
	h.runnersMu.RUnlock()

	if !exists {
		return fmt.Errorf("job %q not found", id)
	}

	_, err := runner.Kill(ctx)
	return err
}

// GetResult retrieves the result of a job, optionally waiting for completion.
// Called via caller API
func (h *Hypervisor) GetResult(ctx context.Context, id string, wait bool) (*JobResult, error) {
	h.runnersMu.RLock()
	runner, exists := h.runners[id]
	h.runnersMu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("job %q not found", id)
	}

	if wait {
		// Wait for the job to complete
		select {
		case <-runner.WaitForResult():
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	job, err := runner.GetState(ctx)
	if err != nil {
		return nil, err
	}

	if !job.IsTerminal() {
		return nil, fmt.Errorf("job %q has not completed", id)
	}

	return job.Result, nil
}

// CheckpointJob checkpoints a job, optionally suspending it for a given duration.
// Will block until all currently held locks are released.
// Called via runtime API
func (h *Hypervisor) CheckpointJob(ctx context.Context, jobID string, suspendDuration time.Duration, token string) (*Job, error) {
	h.runnersMu.RLock()
	runner, exists := h.runners[jobID]
	h.runnersMu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("job %q not found", jobID)
	}

	job, err := runner.Checkpoint(ctx, suspendDuration, token)
	if err != nil {
		return nil, err
	}

	// Check if job should be evicted (long sleep duration)
	h.maybeEvictJob(jobID, job)

	return job, nil
}

// TakeJobLock takes a checkpoint lock for a job, blocking checkpointing until the lock is released.
// Called via runtime API
func (h *Hypervisor) TakeJobLock(ctx context.Context, jobID string) (string, error) {
	h.runnersMu.RLock()
	runner, exists := h.runners[jobID]
	h.runnersMu.RUnlock()

	if !exists {
		return "", fmt.Errorf("job %q not found", jobID)
	}

	return runner.TakeLock(ctx)
}

// ReleaseJobLock releases a checkpoint lock for a job.
// Called via runtime API
func (h *Hypervisor) ReleaseJobLock(ctx context.Context, jobID string, lockID string) error {
	h.runnersMu.RLock()
	runner, exists := h.runners[jobID]
	h.runnersMu.RUnlock()

	if !exists {
		return fmt.Errorf("job %q not found", jobID)
	}

	return runner.ReleaseLock(ctx, lockID)
}

// Exit marks a job as completed with a result.
// Called via runtime API
func (h *Hypervisor) Exit(ctx context.Context, jobID string, exitCode int, output json.RawMessage) error {
	h.runnersMu.RLock()
	runner, exists := h.runners[jobID]
	h.runnersMu.RUnlock()

	if !exists {
		return fmt.Errorf("job %q not found", jobID)
	}

	return runner.Exit(ctx, exitCode, output)
}

// GetParams retrieves the input parameters for a job.
// Called via runtime API
func (h *Hypervisor) GetParams(ctx context.Context, jobID string) (json.RawMessage, error) {
	h.runnersMu.RLock()
	runner, exists := h.runners[jobID]
	h.runnersMu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("job %q not found", jobID)
	}

	job, err := runner.GetState(ctx)
	if err != nil {
		return nil, err
	}

	return job.Params, nil
}

// JobMetadata contains metadata about a job that the runtime can access.
type JobMetadata struct {
	JobID              string `json:"job_id"`
	DefinitionName     string `json:"definition_name"`
	DefinitionVersion  string `json:"definition_version"`
	RetryCount         int    `json:"retry_count"`
	LastCheckpointAtMs *int64 `json:"last_checkpoint_at_ms"`
}

// GetJobMetadata retrieves metadata about a job for the runtime.
// Called via runtime API
func (h *Hypervisor) GetJobMetadata(ctx context.Context, jobID string) (*JobMetadata, error) {
	h.runnersMu.RLock()
	runner, exists := h.runners[jobID]
	h.runnersMu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("job %q not found", jobID)
	}

	job, err := runner.GetState(ctx)
	if err != nil {
		return nil, err
	}

	meta := &JobMetadata{
		JobID:             job.ID,
		DefinitionName:    job.DefinitionName,
		DefinitionVersion: job.DefinitionVersion,
		RetryCount:        job.RetryCount,
	}
	if job.LastCheckpointAt != nil {
		ms := job.LastCheckpointAt.UnixMilli()
		meta.LastCheckpointAtMs = &ms
	}
	return meta, nil
}

// listJobsCursor is the cursor structure for keyset pagination.
type listJobsCursor struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

func encodeListJobsCursor(createdAt time.Time, id string) string {
	cursor := listJobsCursor{CreatedAt: createdAt, ID: id}
	data, _ := json.Marshal(cursor)
	return base64.URLEncoding.EncodeToString(data)
}

func decodeListJobsCursor(cursor string) (*listJobsCursor, error) {
	data, err := base64.URLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor encoding: %w", err)
	}
	var c listJobsCursor
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("invalid cursor format: %w", err)
	}
	return &c, nil
}

// ListJobs returns jobs from the database with keyset pagination.
// Called via caller API
func (h *Hypervisor) ListJobs(ctx context.Context, limit int32, cursor string) ([]*Job, *string, error) {
	var params query.ListJobsParams
	params.Limit = limit

	// Decode cursor if provided
	if cursor != "" {
		c, err := decodeListJobsCursor(cursor)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid cursor: %w", err)
		}
		params.CursorCreatedAt = sql.NullTime{Time: c.CreatedAt, Valid: true}
		params.CursorID = sql.NullString{String: c.ID, Valid: true}
	}

	var dbJobs []query.Job
	err := query.ReliableExec(ctx, h.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		var err error
		dbJobs, err = q.ListJobs(ctx, params)
		return err
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list jobs from database: %w", err)
	}

	jobs := make([]*Job, 0, len(dbJobs))
	for _, dbJob := range dbJobs {
		job := dbJobToJob(dbJob)
		jobs = append(jobs, job)
	}

	// Generate next cursor if we got a full page
	var nextCursor *string
	if len(dbJobs) == int(limit) {
		lastJob := dbJobs[len(dbJobs)-1]
		encoded := encodeListJobsCursor(lastJob.CreatedAt, lastJob.ID)
		nextCursor = &encoded
	}

	return jobs, nextCursor, nil
}

// Shutdown gracefully shuts down the hypervisor.
func (h *Hypervisor) Shutdown(ctx context.Context) error {
	h.cancel()

	// Stop HTTP servers
	h.callerHTTPServer.Shutdown(ctx)
	h.runtimeHTTPServer.Shutdown(ctx)

	// Stop all actors
	h.runnersMu.RLock()
	runners := make([]*JobRunner, 0, len(h.runners))
	for _, runner := range h.runners {
		runners = append(runners, runner)
	}
	h.runnersMu.RUnlock()

	for _, runner := range runners {
		runner.Stop()
	}

	// Wait for all actors to finish (with timeout from context)
	for _, runner := range runners {
		select {
		case <-runner.Done():
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

// DevCrash forcibly closes the hypervisor without graceful shutdown.
// This simulates a crash scenario for testing - it releases ports immediately
// but doesn't wait for jobs to complete or clean up properly.
func (h *Hypervisor) DevCrash() {
	h.cancel()

	// Cancel all runner contexts to stop any pending timers (e.g., scheduleSuspendWake)
	h.runnersMu.RLock()
	for _, runner := range h.runners {
		runner.Cancel()
	}
	h.runnersMu.RUnlock()

	h.callerHTTPServer.Close()
	h.runtimeHTTPServer.Close()
}

// setupRetryCallback configures the retry callback for a job runner based on job definition policy.
func (h *Hypervisor) setupRetryCallback(runner *JobRunner, jd *JobDefinition) {
	maxRetries := 0
	var retryDelay time.Duration
	if jd.RetryPolicy != nil {
		maxRetries = jd.RetryPolicy.MaxRetries
		if jd.RetryPolicy.RetryDelay != "" {
			retryDelay, _ = time.ParseDuration(jd.RetryPolicy.RetryDelay)
		}
	}
	runner.SetOnFailure(func(job *Job, exitCode int) RetryDecision {
		if job.RetryCount < maxRetries {
			logger.Debug().
				Str("job_id", job.ID).
				Int("exit_code", exitCode).
				Int("retry_count", job.RetryCount+1).
				Int("max_retries", maxRetries).
				Dur("retry_delay", retryDelay).
				Msg("retrying failed job")
			return RetryDecision{ShouldRetry: true, RetryDelay: retryDelay}
		}
		logger.Debug().
			Str("job_id", job.ID).
			Int("exit_code", exitCode).
			Int("retry_count", job.RetryCount).
			Msg("job failed, no retries remaining")
		return RetryDecision{ShouldRetry: false}
	})
}

// maybeEvictJob evicts a job from memory if it's in a terminal state, pending retry, or sleeping for more than 10 seconds.
// This is safe because the DB is the source of truth - jobs can be reloaded when needed.
func (h *Hypervisor) maybeEvictJob(jobID string, job *Job) {
	if job.IsTerminal() {
		// Evict terminal jobs immediately (DB is source of truth)
		h.runnersMu.Lock()
		delete(h.runners, jobID)
		h.runnersMu.Unlock()
		logger.Debug().Str("job_id", jobID).Str("state", string(job.State)).Msg("evicted terminal job from memory")
		return
	}

	if job.State == JobStatePendingRetry {
		// Evict pending_retry jobs so the resume poller can pick them up
		h.runnersMu.Lock()
		delete(h.runners, jobID)
		h.runnersMu.Unlock()
		logger.Debug().
			Str("job_id", jobID).
			Str("state", string(job.State)).
			Msg("evicted pending_retry job from memory")
		return
	}

	if job.State == JobStateSuspended && job.ResumeAt != nil {
		if time.Until(*job.ResumeAt) > 10*time.Second {
			// Evict long-sleeping jobs - the resume poller will reload them
			h.runnersMu.Lock()
			delete(h.runners, jobID)
			h.runnersMu.Unlock()
			logger.Debug().
				Str("job_id", jobID).
				Time("resume_at", *job.ResumeAt).
				Msg("evicted long-sleeping job from memory")
		}
	}
}

// loadJobFromDB loads a job from the database by ID.
func (h *Hypervisor) loadJobFromDB(ctx context.Context, id string) (*Job, error) {
	var dbJob query.Job
	err := query.ReliableExec(ctx, h.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		var err error
		dbJob, err = q.GetJob(ctx, id)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("job %q not found", id)
	}

	return dbJobToJob(dbJob), nil
}

// dbJobToJob converts a database job record to a Job struct.
func dbJobToJob(dbJob query.Job) *Job {
	job := &Job{
		ID:                dbJob.ID,
		DefinitionName:    dbJob.DefinitionName,
		DefinitionVersion: dbJob.DefinitionVersion,
		State:             JobState(dbJob.State),
		CreatedAt:         dbJob.CreatedAt,
		CheckpointCount:   int(dbJob.CheckpointCount),
		RetryCount:        int(dbJob.RetryCount),
	}

	// Parse env from JSON
	if len(dbJob.Env) > 0 {
		var env map[string]string
		if err := json.Unmarshal(dbJob.Env, &env); err == nil {
			job.Env = env
		}
	}

	if len(dbJob.Params) > 0 {
		job.Params = dbJob.Params
	}

	if len(dbJob.Metadata) > 0 {
		var metadata map[string]string
		if err := json.Unmarshal(dbJob.Metadata, &metadata); err == nil {
			job.Metadata = metadata
		}
	}

	if dbJob.StartedAt.Valid {
		job.StartedAt = &dbJob.StartedAt.Time
	}
	if dbJob.CompletedAt.Valid {
		job.CompletedAt = &dbJob.CompletedAt.Time
	}
	if dbJob.LastCheckpointAt.Valid {
		job.LastCheckpointAt = &dbJob.LastCheckpointAt.Time
	}
	if dbJob.ResumeAt.Valid {
		job.ResumeAt = &dbJob.ResumeAt.Time
	}

	if dbJob.Error.Valid {
		job.Error = dbJob.Error.String
	}

	if dbJob.ResultExitCode.Valid {
		job.Result = &JobResult{
			ExitCode: int(dbJob.ResultExitCode.Int32),
		}
		if len(dbJob.ResultOutput) > 0 {
			job.Result.Output = dbJob.ResultOutput
		}
	}

	return job
}

// RecoverState recovers job definitions and jobs from the database.
// This should be called after all runtimes have been registered.
func (h *Hypervisor) RecoverState(ctx context.Context) error {
	// Recover job definitions first
	if err := h.recoverJobDefinitions(ctx); err != nil {
		return fmt.Errorf("failed to recover job definitions: %w", err)
	}

	// Recover jobs
	if err := h.recoverJobs(ctx); err != nil {
		return fmt.Errorf("failed to recover jobs: %w", err)
	}

	return nil
}

// recoverJobs recovers non-terminal jobs from the database.
func (h *Hypervisor) recoverJobs(ctx context.Context) error {
	var dbJobs []query.Job
	err := query.ReliableExec(ctx, h.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		var err error
		dbJobs, err = q.GetNonTerminalJobs(ctx)
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to get non-terminal jobs from database: %w", err)
	}

	logger.Info().Int("count", len(dbJobs)).Msg("recovering non-terminal jobs")

	for _, dbJob := range dbJobs {
		switch query.JobState(dbJob.State) {
		case query.JobStateRunning:
			// Job was running when we crashed - mark as failed and potentially retry
			if err := h.recoverRunningJob(ctx, dbJob); err != nil {
				logger.Error().
					Err(err).
					Str("job_id", dbJob.ID).
					Msg("failed to recover running job")
			}

		case query.JobStateSuspended, query.JobStatePendingRetry:
			// For crash recovery, let the resume poller handle suspended and pending_retry jobs.
			logger.Info().
				Str("job_id", dbJob.ID).
				Time("resume_at", dbJob.ResumeAt.Time).
				Msg("job will be handled by resume poller")

		case query.JobStatePending:
			// Job was pending - restart it
			if err := h.restartPendingJob(ctx, dbJob); err != nil {
				logger.Error().
					Err(err).
					Str("job_id", dbJob.ID).
					Msg("failed to restart pending job")
			}
		}
	}

	return nil
}

// recoverRunningJob handles a job that was in the 'running' state when we crashed.
// According to the plan, we mark it as failed and auto-retry based on the retry policy.
func (h *Hypervisor) recoverRunningJob(ctx context.Context, dbJob query.Job) error {
	logger.Info().Str("job_id", dbJob.ID).Msg("recovering running job (was interrupted)")

	// Get the job definition to check retry policy
	jd, err := h.definitions.Get(dbJob.DefinitionName, dbJob.DefinitionVersion)
	if err != nil {
		// No definition - just mark as failed
		return h.markJobAsFailed(ctx, dbJob.ID, "interrupted: job definition not found")
	}

	// Check if we should retry
	maxRetries := 0
	if jd.RetryPolicy != nil {
		maxRetries = jd.RetryPolicy.MaxRetries
	}

	if int(dbJob.RetryCount) < maxRetries {
		// Retry the job
		logger.Info().
			Str("job_id", dbJob.ID).
			Int("retry_count", int(dbJob.RetryCount)+1).
			Int("max_retries", maxRetries).
			Msg("auto-retrying interrupted job")
		return h.restartPendingJob(ctx, dbJob)
	}

	// No retries left - mark as failed
	return h.markJobAsFailed(ctx, dbJob.ID, "interrupted: no retries remaining")
}

// restartPendingJob restarts a pending job.
func (h *Hypervisor) restartPendingJob(ctx context.Context, dbJob query.Job) error {
	logger.Info().Str("job_id", dbJob.ID).Msg("restarting pending job")

	jd, err := h.definitions.Get(dbJob.DefinitionName, dbJob.DefinitionVersion)
	if err != nil {
		return h.markJobAsFailed(ctx, dbJob.ID, fmt.Sprintf("job definition not found: %v", err))
	}

	h.runnersMu.RLock()
	rt, exists := h.runtimes[jd.RuntimeType]
	h.runnersMu.RUnlock()
	if !exists {
		return h.markJobAsFailed(ctx, dbJob.ID, fmt.Sprintf("runtime %q not found", jd.RuntimeType))
	}

	job := dbJobToJob(dbJob)

	runner := NewJobRunner(job, jd, rt, jd.Config, h.runtimeHTTPAddress, h.pool, nil, nil)
	h.setupRetryCallback(runner, jd)

	h.runnersMu.Lock()
	h.runners[dbJob.ID] = runner
	h.runnersMu.Unlock()

	if err := runner.Start(); err != nil {
		h.runnersMu.Lock()
		delete(h.runners, dbJob.ID)
		h.runnersMu.Unlock()
		return fmt.Errorf("failed to start job: %w", err)
	}

	// Start eviction goroutine
	go func() {
		<-runner.Done()
		job, err := runner.GetState(h.ctx)
		if err != nil {
			return
		}
		h.maybeEvictJob(dbJob.ID, job)
	}()

	return nil
}

// markJobAsFailed marks a job as failed in the database.
func (h *Hypervisor) markJobAsFailed(ctx context.Context, jobID string, errorMsg string) error {
	now := time.Now()
	return query.ReliableExecInTx(ctx, h.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		return q.UpdateJobCompleted(ctx, query.UpdateJobCompletedParams{
			ID:          jobID,
			State:       query.JobStateFailed,
			CompletedAt: utils.SQLNullTime(now),
			Error:       utils.SQLNullString(errorMsg),
		})
	})
}

// recoverJobDefinitions loads job definitions from the database and registers them.
func (h *Hypervisor) recoverJobDefinitions(ctx context.Context) error {
	var dbDefs []query.JobDefinition
	err := query.ReliableExec(ctx, h.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		var err error
		dbDefs, err = q.ListJobDefinitions(ctx)
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to list job definitions from database: %w", err)
	}

	for _, dbDef := range dbDefs {
		// Check if we have a runtime for this definition
		rtType := runtime.RuntimeType(dbDef.RuntimeType)
		h.runnersMu.RLock()
		rt, hasRuntime := h.runtimes[rtType]
		h.runnersMu.RUnlock()

		if !hasRuntime {
			logger.Warn().
				Str("name", dbDef.Name).
				Str("version", dbDef.Version).
				Str("runtime_type", dbDef.RuntimeType).
				Msg("skipping job definition recovery: runtime not registered")
			continue
		}

		config, err := rt.ParseConfig(dbDef.Config)
		if err != nil {
			logger.Error().
				Err(err).
				Str("name", dbDef.Name).
				Str("version", dbDef.Version).
				Msg("failed to parse job definition config")
			continue
		}

		var retryPolicy *RetryPolicy
		if len(dbDef.RetryPolicy) > 0 {
			retryPolicy = &RetryPolicy{}
			if err := json.Unmarshal(dbDef.RetryPolicy, retryPolicy); err != nil {
				logger.Error().
					Err(err).
					Str("name", dbDef.Name).
					Str("version", dbDef.Version).
					Msg("failed to parse job definition retry policy")
				continue
			}
		}

		var metadata map[string]string
		if len(dbDef.Metadata) > 0 {
			if err := json.Unmarshal(dbDef.Metadata, &metadata); err != nil {
				logger.Error().
					Err(err).
					Str("name", dbDef.Name).
					Str("version", dbDef.Version).
					Msg("failed to parse job definition metadata")
				continue
			}
		}

		jd := &JobDefinition{
			Name:        dbDef.Name,
			Version:     dbDef.Version,
			RuntimeType: rtType,
			Config:      config,
			RetryPolicy: retryPolicy,
			Metadata:    metadata,
		}
		if err := h.definitions.Register(jd); err != nil {
			logger.Error().
				Err(err).
				Str("name", dbDef.Name).
				Str("version", dbDef.Version).
				Msg("failed to register recovered job definition")
			continue
		}

		logger.Info().
			Str("name", dbDef.Name).
			Str("version", dbDef.Version).
			Msg("recovered job definition from database")
	}

	return nil
}

// startResumePoller starts a background goroutine that polls for jobs to resume (suspended or pending_retry).
func (h *Hypervisor) startResumePoller() {
	ticker := time.NewTicker(h.wakePollerInterval)
	go func() {
		for {
			select {
			case <-ticker.C:
				h.resumeReadyJobs()
			case <-h.ctx.Done():
				ticker.Stop()
				return
			}
		}
	}()
	logger.Info().Msg("started resume poller")
}

// resumeReadyJobs queries for suspended and pending_retry jobs that are ready to resume.
func (h *Hypervisor) resumeReadyJobs() {
	var dbJobs []query.Job
	err := query.ReliableExec(h.ctx, h.pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		var err error
		dbJobs, err = q.GetJobsToResume(ctx, query.GetJobsToResumeParams{
			ResumeAt: utils.SQLNullTime(time.Now()),
			Limit:    100, // Process in batches
		})
		return err
	})
	if err != nil {
		logger.Error().Err(err).Msg("failed to query jobs to resume")
		return
	}

	for _, dbJob := range dbJobs {
		h.runnersMu.RLock()
		_, exists := h.runners[dbJob.ID]
		h.runnersMu.RUnlock()
		if exists {
			// Already being handled in memory
			continue
		}

		switch dbJob.State {
		case query.JobStateSuspended:
			if err := h.wakeJob(h.ctx, dbJob); err != nil {
				logger.Error().
					Err(err).
					Str("job_id", dbJob.ID).
					Msg("failed to wake suspended job")
			}
		case query.JobStatePendingRetry:
			if err := h.retryJob(h.ctx, dbJob); err != nil {
				logger.Error().
					Err(err).
					Str("job_id", dbJob.ID).
					Msg("failed to retry pending job")
			}
		}
	}
}

// wakeJob restores a suspended job from its checkpoint.
func (h *Hypervisor) wakeJob(ctx context.Context, dbJob query.Job) error {
	logger.Info().Str("job_id", dbJob.ID).Msg("waking suspended job")

	jd, err := h.definitions.Get(dbJob.DefinitionName, dbJob.DefinitionVersion)
	if err != nil {
		return fmt.Errorf("failed to get job definition: %w", err)
	}

	h.runnersMu.RLock()
	rt, exists := h.runtimes[jd.RuntimeType]
	h.runnersMu.RUnlock()
	if !exists {
		return fmt.Errorf("runtime %q not found", jd.RuntimeType)
	}

	job := dbJobToJob(dbJob)

	if !dbJob.CheckpointPath.Valid || dbJob.CheckpointPath.String == "" {
		return fmt.Errorf("no checkpoint path for suspended job")
	}

	// Verify checkpoint file exists before attempting restore
	checkpointPath := dbJob.CheckpointPath.String
	if fileInfo, statErr := os.Stat(checkpointPath); statErr != nil {
		logger.Error().
			Err(statErr).
			Str("checkpoint_path", checkpointPath).
			Str("job_id", dbJob.ID).
			Msg("checkpoint file does not exist")
		return fmt.Errorf("checkpoint file does not exist at %s: %w", checkpointPath, statErr)
	} else {
		logger.Info().
			Str("checkpoint_path", checkpointPath).
			Str("job_id", dbJob.ID).
			Int64("file_size", fileInfo.Size()).
			Msg("checkpoint file verified")
	}

	checkpoint, err := rt.ReconstructCheckpoint(
		dbJob.CheckpointPath.String,
		dbJob.ID,
		job.Env,
		jd.Config,
		h.runtimeHTTPAddress,
	)
	if err != nil {
		return fmt.Errorf("failed to reconstruct checkpoint: %w", err)
	}

	runner := NewJobRunner(job, jd, rt, jd.Config, h.runtimeHTTPAddress, h.pool, nil, nil)
	runner.SetCheckpoint(checkpoint)
	h.setupRetryCallback(runner, jd)

	// Add to runners map BEFORE sending wake command
	h.runnersMu.Lock()
	h.runners[dbJob.ID] = runner
	h.runnersMu.Unlock()

	// Send wake command to restore from checkpoint via the command loop
	resultChan := make(chan commandResult, 1)
	runner.cmdChan <- command{typ: cmdWake, ctx: ctx, resultChan: resultChan}

	select {
	case result := <-resultChan:
		if result.err != nil {
			// Cancel runner and remove from map
			runner.Cancel()
			h.runnersMu.Lock()
			delete(h.runners, dbJob.ID)
			h.runnersMu.Unlock()
			return fmt.Errorf("failed to wake job: %w", result.err)
		}
	case <-ctx.Done():
		runner.Cancel()
		h.runnersMu.Lock()
		delete(h.runners, dbJob.ID)
		h.runnersMu.Unlock()
		return ctx.Err()
	}

	// Start eviction goroutine
	go func() {
		<-runner.Done()
		job, err := runner.GetState(h.ctx)
		if err != nil {
			logger.Error().Err(err).Str("job_id", dbJob.ID).Msg("failed to get job state for eviction")
			return
		}
		h.maybeEvictJob(dbJob.ID, job)
	}()

	logger.Info().Str("job_id", dbJob.ID).Msg("successfully woke suspended job")
	return nil
}

// retryJob retries a pending_retry job from its checkpoint (if available) or from scratch.
func (h *Hypervisor) retryJob(ctx context.Context, dbJob query.Job) error {
	logger.Info().Str("job_id", dbJob.ID).Int("retry_count", int(dbJob.RetryCount)).Msg("retrying job from poller")

	jd, err := h.definitions.Get(dbJob.DefinitionName, dbJob.DefinitionVersion)
	if err != nil {
		return h.markJobAsFailed(ctx, dbJob.ID, fmt.Sprintf("job definition not found: %v", err))
	}

	h.runnersMu.RLock()
	rt, exists := h.runtimes[jd.RuntimeType]
	h.runnersMu.RUnlock()
	if !exists {
		return h.markJobAsFailed(ctx, dbJob.ID, fmt.Sprintf("runtime %q not found", jd.RuntimeType))
	}

	job := dbJobToJob(dbJob)
	// Mark job as failed so handleRetry can transition it properly
	job.State = JobStateFailed

	runner := NewJobRunner(job, jd, rt, jd.Config, h.runtimeHTTPAddress, h.pool, nil, nil)
	h.setupRetryCallback(runner, jd)

	// If checkpoint exists, set it for restoration
	if dbJob.CheckpointPath.Valid && dbJob.CheckpointPath.String != "" {
		checkpoint, err := rt.ReconstructCheckpoint(
			dbJob.CheckpointPath.String,
			dbJob.ID,
			job.Env,
			jd.Config,
			h.runtimeHTTPAddress,
		)
		if err != nil {
			logger.Warn().Err(err).Str("job_id", dbJob.ID).Msg("failed to reconstruct checkpoint, will retry from scratch")
		} else {
			runner.SetCheckpoint(checkpoint)
		}
	}

	h.runnersMu.Lock()
	h.runners[dbJob.ID] = runner
	h.runnersMu.Unlock()

	// Send retry command
	resultChan := make(chan commandResult, 1)
	runner.cmdChan <- command{typ: cmdRetry, ctx: ctx, resultChan: resultChan}

	select {
	case result := <-resultChan:
		if result.err != nil {
			runner.Cancel()
			h.runnersMu.Lock()
			delete(h.runners, dbJob.ID)
			h.runnersMu.Unlock()
			return fmt.Errorf("failed to retry job: %w", result.err)
		}
	case <-ctx.Done():
		runner.Cancel()
		h.runnersMu.Lock()
		delete(h.runners, dbJob.ID)
		h.runnersMu.Unlock()
		return ctx.Err()
	}

	// Start eviction goroutine
	go func() {
		<-runner.Done()
		job, err := runner.GetState(h.ctx)
		if err != nil {
			logger.Error().Err(err).Str("job_id", dbJob.ID).Msg("failed to get job state for eviction")
			return
		}
		h.maybeEvictJob(dbJob.ID, job)
	}()

	logger.Info().Str("job_id", dbJob.ID).Msg("successfully retried job")
	return nil
}
