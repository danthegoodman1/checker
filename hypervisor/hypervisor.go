package hypervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	goruntime "runtime"
	"sync"
	"time"

	"github.com/danthegoodman1/checker/gologger"
	"github.com/danthegoodman1/checker/http_server"
	"github.com/danthegoodman1/checker/runtime"
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
	}

	if cfg.IDGenerator != nil {
		h.generateID = cfg.IDGenerator
	} else {
		h.generateID = defaultIDGenerator()
	}

	h.callerHTTPAddress = cfg.CallerHTTPAddress
	h.runtimeHTTPAddress = cfg.RuntimeHTTPAddress

	h.callerHTTPServer = http_server.StartHTTPServer(h.callerHTTPAddress, "", h.RegisterCallerAPI)
	h.runtimeHTTPServer = http_server.StartHTTPServer(h.runtimeHTTPAddress, "", h.RegisterRuntimeAPI)

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
func (h *Hypervisor) RegisterJobDefinition(jd *JobDefinition) error {
	// Validate that we have a runtime for this job type
	h.runnersMu.RLock()
	_, hasRuntime := h.runtimes[jd.RuntimeType]
	h.runnersMu.RUnlock()

	if !hasRuntime {
		return fmt.Errorf("no runtime registered for type %q", jd.RuntimeType)
	}

	return h.definitions.Register(jd)
}

// Called via caller API
func (h *Hypervisor) UnregisterJobDefinition(name, version string) error {
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

	runner := NewJobRunner(job, jd, rt, jd.Config, h.runtimeHTTPAddress, opts.Stdout, opts.Stderr)

	// Set up retry callback
	runner.SetOnFailure(func(r *JobRunner, exitCode int) {
		job, _ := r.GetState(context.Background())
		maxRetries := 0
		var retryDelay time.Duration
		if jd.RetryPolicy != nil {
			maxRetries = jd.RetryPolicy.MaxRetries
			if jd.RetryPolicy.RetryDelay != "" {
				retryDelay, _ = time.ParseDuration(jd.RetryPolicy.RetryDelay)
			}
		}

		if job.RetryCount < maxRetries {
			r.logger.Debug().
				Int("exit_code", exitCode).
				Int("retry_count", job.RetryCount+1).
				Int("max_retries", maxRetries).
				Dur("retry_delay", retryDelay).
				Msg("retrying failed job")

			if retryDelay > 0 {
				time.Sleep(retryDelay)
			}
			if err := r.Retry(); err != nil {
				r.logger.Error().Err(err).Msg("failed to retry job")
				r.MarkDone()
			}
		} else {
			r.logger.Debug().
				Int("exit_code", exitCode).
				Int("retry_count", job.RetryCount).
				Msg("job failed, no retries remaining")
			r.MarkDone()
		}
	})

	h.runnersMu.Lock()
	h.runners[jobID] = runner
	h.runnersMu.Unlock()

	if err := runner.Start(); err != nil {
		h.runnersMu.Lock()
		delete(h.runners, jobID)
		h.runnersMu.Unlock()
		return "", fmt.Errorf("failed to start job: %w", err)
	}

	return job.ID, nil
}

// Called via caller API
func (h *Hypervisor) GetJob(ctx context.Context, id string) (*Job, error) {
	h.runnersMu.RLock()
	runner, exists := h.runners[id]
	h.runnersMu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("job %q not found", id)
	}

	return runner.GetState(ctx)
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

	return runner.Checkpoint(ctx, suspendDuration, token)
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

// ListJobs returns all active jobs.
// Called via caller API
func (h *Hypervisor) ListJobs(ctx context.Context) ([]*Job, error) {
	h.runnersMu.RLock()
	defer h.runnersMu.RUnlock()

	var jobs []*Job
	for _, runner := range h.runners {
		job, err := runner.GetState(ctx)
		if err != nil {
			continue
		}
		jobs = append(jobs, job)
	}

	return jobs, nil
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
