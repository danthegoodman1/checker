package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/danthegoodman1/checker/hypervisor"
	"github.com/danthegoodman1/checker/migrations"
	"github.com/danthegoodman1/checker/pg"
	"github.com/danthegoodman1/checker/query"
	"github.com/danthegoodman1/checker/runtime"
	"github.com/danthegoodman1/checker/runtime/podman"
	"github.com/danthegoodman1/checker/utils"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCrashRecovery verifies that jobs suspended at checkpoint time
// are properly recovered when the hypervisor restarts.
//
// This test:
// 1. Starts a hypervisor with database persistence
// 2. Spawns a job that checkpoints with a suspend duration
// 3. Waits for the job to be suspended
// 4. Shuts down the hypervisor (simulating crash)
// 5. Starts a NEW hypervisor and calls RecoverState()
// 6. Verifies the job wakes up and completes successfully
//
// Run with: go test -v -run TestCrashRecovery
// Requires: PG_DSN environment variable and Linux (for Podman)
func TestCrashRecoveryHypervisor(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("Crash recovery test requires Linux (Podman)")
	}

	if utils.PG_DSN == "" {
		t.Skip("PG_DSN environment variable not set")
	}

	// Run migrations
	_, err := migrations.RunMigrations(utils.PG_DSN)
	require.NoError(t, err)

	// Connect to database
	pool, err := pgxpool.New(context.Background(), utils.PG_DSN)
	require.NoError(t, err)
	defer pool.Close()

	// Build the test image
	cwd, err := os.Getwd()
	require.NoError(t, err)
	demoDir := filepath.Join(cwd, "demo")

	cmd := exec.Command("podman", "build", "-t", "checker-checkpoint-restore-test:latest",
		"-f", filepath.Join(demoDir, "Dockerfile.checkpoint_restore"), demoDir)
	buildOutput, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to build test image: %v\n%s", err, buildOutput)
	}

	// Get unique ports for this test
	port := portCounter.Add(2)
	callerAddr := fmt.Sprintf("127.0.0.1:%d", port)
	runtimeAddr := fmt.Sprintf("0.0.0.0:%d", port+1)

	// Phase 1: Start hypervisor, spawn job, wait for suspend, then "crash"
	t.Log("=== Phase 1: Starting hypervisor and spawning job ===")

	h1 := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  callerAddr,
		RuntimeHTTPAddress: runtimeAddr,
		Pool:               pool,
	})

	podmanRuntime, err := podman.NewRuntime()
	require.NoError(t, err)
	defer podmanRuntime.Close()

	require.NoError(t, h1.RegisterRuntime(podmanRuntime))

	// Register the job definition
	jd := &hypervisor.JobDefinition{
		Name:        "crash-recovery-test",
		Version:     "1.0.0",
		RuntimeType: runtime.RuntimeTypePodman,
		Config: &podman.Config{
			Image:   "checker-checkpoint-restore-test:latest",
			Network: "host",
		},
	}
	require.NoError(t, h1.RegisterJobDefinition(jd))

	// Spawn job that will checkpoint and suspend
	ctx := context.Background()
	jobID, err := h1.Spawn(ctx, hypervisor.SpawnOptions{
		DefinitionName:    "crash-recovery-test",
		DefinitionVersion: "1.0.0",
		Params:            json.RawMessage(`{"number": 5, "suspend_duration": "5s"}`),
		Stdout:            &testLogWriter{t: t, prefix: "[stdout]"},
		Stderr:            &testLogWriter{t: t, prefix: "[stderr]"},
	})
	require.NoError(t, err)
	t.Logf("Spawned job: %s", jobID)

	// Wait for job to be suspended (poll the database)
	t.Log("Waiting for job to be suspended...")
	var suspended bool
	for i := 0; i < 60; i++ { // Wait up to 30 seconds
		var dbJob query.Job
		err := query.ReliableExec(ctx, pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
			var err error
			dbJob, err = q.GetJob(ctx, jobID)
			return err
		})
		require.NoError(t, err)

		if dbJob.State == query.JobStateSuspended {
			suspended = true
			t.Logf("Job is now suspended (checkpoint_count=%d, suspend_until=%v)",
				dbJob.CheckpointCount, dbJob.SuspendUntil.Time)
			break
		}
		if dbJob.State == query.JobStateFailed {
			t.Fatalf("Job failed with error: %s", dbJob.Error.String)
		}
		t.Logf("Job state: %s, waiting...", dbJob.State)
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, suspended, "Job did not reach suspended state")

	// Simulate crash by shutting down hypervisor
	t.Log("=== Phase 2: Simulating crash (shutting down hypervisor) ===")
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	require.NoError(t, h1.Shutdown(shutdownCtx))
	cancel()

	// Small delay to ensure everything is cleaned up
	time.Sleep(500 * time.Millisecond)

	// Phase 3: Start NEW hypervisor and recover
	t.Log("=== Phase 3: Starting new hypervisor and recovering state ===")

	// IMPORTANT: Reuse the same ports so CHECKER_API_URL in the restored container
	// still points to a valid address. CRIU preserves environment variables.
	h2 := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  callerAddr,
		RuntimeHTTPAddress: runtimeAddr,
		Pool:               pool,
	})
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h2.Shutdown(shutdownCtx)
	}()

	// Create new podman runtime for h2
	podmanRuntime2, err := podman.NewRuntime()
	require.NoError(t, err)
	defer podmanRuntime2.Close()

	require.NoError(t, h2.RegisterRuntime(podmanRuntime2))

	// Recover state - this should load job definitions and schedule wake for suspended jobs
	t.Log("Calling RecoverState()...")
	require.NoError(t, h2.RecoverState(ctx))

	// The job should be scheduled to wake. Let's modify the suspend_until to be now
	// so we don't have to wait 30 seconds
	t.Log("Updating suspend_until to trigger immediate wake...")
	_, err = pool.Exec(ctx, "UPDATE jobs SET suspend_until = NOW() WHERE id = $1", jobID)
	require.NoError(t, err)

	// Wait for job to complete
	t.Log("Waiting for job to complete...")
	var completed bool
	var finalState query.JobState
	for i := 0; i < 120; i++ { // Wait up to 60 seconds
		var dbJob query.Job
		err := query.ReliableExec(ctx, pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
			var err error
			dbJob, err = q.GetJob(ctx, jobID)
			return err
		})
		require.NoError(t, err)

		finalState = dbJob.State
		if dbJob.State == query.JobStateCompleted || dbJob.State == query.JobStateFailed {
			completed = true
			t.Logf("Job finished with state: %s", dbJob.State)
			if dbJob.ResultOutput != nil {
				t.Logf("Job output: %s", string(dbJob.ResultOutput))
			}
			if dbJob.Error.Valid {
				t.Logf("Job error: %s", dbJob.Error.String)
			}
			break
		}
		t.Logf("Job state: %s, waiting...", dbJob.State)
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, completed, "Job did not complete")
	assert.Equal(t, query.JobStateCompleted, finalState, "Job should have completed successfully")

	// Verify the result
	var dbJob query.Job
	err = query.ReliableExec(ctx, pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		var err error
		dbJob, err = q.GetJob(ctx, jobID)
		return err
	})
	require.NoError(t, err)

	assert.Equal(t, int32(0), dbJob.ResultExitCode.Int32, "Exit code should be 0")

	var output struct {
		Result struct {
			Step  int `json:"step"`
			Value int `json:"value"`
		} `json:"result"`
		PreCheckpointRuns int `json:"pre_checkpoint_runs"`
	}
	require.NoError(t, json.Unmarshal(dbJob.ResultOutput, &output))
	assert.Equal(t, 12, output.Result.Value, "Expected (5 + 1) * 2 = 12")
	assert.Equal(t, 1, output.PreCheckpointRuns, "With CRIU, pre-checkpoint code should only run once")

	t.Log("=== Crash recovery test PASSED ===")

	// Cleanup: delete the test job and definition from DB
	_, _ = pool.Exec(ctx, "DELETE FROM jobs WHERE id = $1", jobID)
	_, _ = pool.Exec(ctx, "DELETE FROM job_definitions WHERE name = $1", "crash-recovery-test")
}

// TestCrashRecoveryPendingJob verifies that pending jobs (not yet started)
// are properly recovered when the hypervisor restarts.
func TestCrashRecoveryPendingJob(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("Crash recovery test requires Linux (Podman)")
	}

	if utils.PG_DSN == "" {
		t.Skip("PG_DSN environment variable not set")
	}

	// Run migrations
	_, err := migrations.RunMigrations(utils.PG_DSN)
	require.NoError(t, err)

	pool, err := pgxpool.New(context.Background(), utils.PG_DSN)
	require.NoError(t, err)
	defer pool.Close()

	// Build test image
	cwd, err := os.Getwd()
	require.NoError(t, err)
	demoDir := filepath.Join(cwd, "demo")

	cmd := exec.Command("podman", "build", "-t", "checker-checkpoint-restore-test:latest",
		"-f", filepath.Join(demoDir, "Dockerfile.checkpoint_restore"), demoDir)
	buildOutput, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to build test image: %v\n%s", err, buildOutput)
	}

	ctx := context.Background()

	// Insert a pending job directly into the database (simulating a job that was
	// inserted but the hypervisor crashed before it could start)
	jobID := fmt.Sprintf("test-pending-%d", time.Now().UnixNano())
	jdName := "crash-recovery-pending-test"
	jdVersion := "1.0.0"

	// Allocate port first so we can include it in env
	port := portCounter.Add(2)
	runtimeAddr := fmt.Sprintf("0.0.0.0:%d", port+1)

	// First, insert the job definition
	configJSON, _ := json.Marshal(&podman.Config{
		Image:   "checker-checkpoint-restore-test:latest",
		Network: "host",
	})
	_, err = pool.Exec(ctx, `
		INSERT INTO job_definitions (name, version, runtime_type, config, metadata)
		VALUES ($1, $2, $3, $4, '{}')
		ON CONFLICT (name, version) DO NOTHING
	`, jdName, jdVersion, runtime.RuntimeTypePodman, configJSON)
	require.NoError(t, err)

	// Insert the pending job with proper env (normally set by Spawn)
	params, _ := json.Marshal(map[string]any{"number": 7, "skip_checkpoint": true})
	env, _ := json.Marshal(map[string]string{
		"CHECKER_JOB_ID":                 jobID,
		"CHECKER_JOB_DEFINITION_NAME":    jdName,
		"CHECKER_JOB_DEFINITION_VERSION": jdVersion,
		"CHECKER_JOB_SPAWNED_AT":         fmt.Sprintf("%d", time.Now().Unix()),
		"CHECKER_ARCH":                   goruntime.GOARCH,
		"CHECKER_OS":                     goruntime.GOOS,
	})
	_, err = pool.Exec(ctx, `
		INSERT INTO jobs (id, definition_name, definition_version, state, env, params, 
		                  runtime_type, runtime_config, metadata, created_at)
		VALUES ($1, $2, $3, 'pending', $4, $5, $6, $7, '{}', NOW())
	`, jobID, jdName, jdVersion, env, params, runtime.RuntimeTypePodman, configJSON)
	require.NoError(t, err)

	t.Logf("Inserted pending job: %s", jobID)

	// Start hypervisor and recover
	h := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  fmt.Sprintf("127.0.0.1:%d", port),
		RuntimeHTTPAddress: runtimeAddr,
		Pool:               pool,
	})
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h.Shutdown(shutdownCtx)
	}()

	podmanRuntime, err := podman.NewRuntime()
	require.NoError(t, err)
	defer podmanRuntime.Close()

	require.NoError(t, h.RegisterRuntime(podmanRuntime))

	t.Log("Calling RecoverState() to recover pending job...")
	require.NoError(t, h.RecoverState(ctx))

	// Wait for job to complete
	t.Log("Waiting for pending job to complete...")
	var completed bool
	var finalState query.JobState
	for i := 0; i < 120; i++ {
		var dbJob query.Job
		err := query.ReliableExec(ctx, pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
			var err error
			dbJob, err = q.GetJob(ctx, jobID)
			return err
		})
		require.NoError(t, err)

		finalState = dbJob.State
		if dbJob.State == query.JobStateCompleted || dbJob.State == query.JobStateFailed {
			completed = true
			t.Logf("Job finished with state: %s", dbJob.State)
			break
		}
		t.Logf("Job state: %s, waiting...", dbJob.State)
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, completed, "Job did not complete")
	assert.Equal(t, query.JobStateCompleted, finalState, "Job should have completed successfully")

	// Verify result: (7 + 1) * 2 = 16
	var dbJob query.Job
	err = query.ReliableExec(ctx, pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		var err error
		dbJob, err = q.GetJob(ctx, jobID)
		return err
	})
	require.NoError(t, err)

	var output struct {
		Result struct {
			Value int `json:"value"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(dbJob.ResultOutput, &output))
	assert.Equal(t, 16, output.Result.Value, "Expected (7 + 1) * 2 = 16")

	t.Log("=== Pending job recovery test PASSED ===")

	// Cleanup
	_, _ = pool.Exec(ctx, "DELETE FROM jobs WHERE id = $1", jobID)
	_, _ = pool.Exec(ctx, "DELETE FROM job_definitions WHERE name = $1", jdName)
}

// TestCrashRecoveryFullServerCrash simulates a complete server crash where both
// the hypervisor process AND the container are killed (e.g., power failure, OOM kill).
// The checkpoint file and database state should be sufficient to recover.
//
// This test:
// 1. Starts a hypervisor and spawns a job
// 2. Waits for the job to checkpoint and suspend
// 3. Force kills the container (simulating it died with the server)
// 4. Does NOT gracefully shutdown the hypervisor (simulating crash)
// 5. Starts a NEW hypervisor and recovers
// 6. Verifies the job completes successfully from checkpoint
func TestCrashRecoveryFullServerCrash(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("Crash recovery test requires Linux (Podman)")
	}

	if utils.PG_DSN == "" {
		t.Skip("PG_DSN environment variable not set")
	}

	// Run migrations
	_, err := migrations.RunMigrations(utils.PG_DSN)
	require.NoError(t, err)

	pool, err := pgxpool.New(context.Background(), utils.PG_DSN)
	require.NoError(t, err)
	defer pool.Close()

	// Build the test image
	cwd, err := os.Getwd()
	require.NoError(t, err)
	demoDir := filepath.Join(cwd, "demo")

	cmd := exec.Command("podman", "build", "-t", "checker-checkpoint-restore-test:latest",
		"-f", filepath.Join(demoDir, "Dockerfile.checkpoint_restore"), demoDir)
	buildOutput, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to build test image: %v\n%s", err, buildOutput)
	}

	port := portCounter.Add(2)
	callerAddr := fmt.Sprintf("127.0.0.1:%d", port)
	runtimeAddr := fmt.Sprintf("0.0.0.0:%d", port+1)

	ctx := context.Background()

	// Phase 1: Start hypervisor, spawn job, wait for suspend
	t.Log("=== Phase 1: Starting hypervisor and spawning job ===")

	h1 := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  callerAddr,
		RuntimeHTTPAddress: runtimeAddr,
		Pool:               pool,
	})

	podmanRuntime1, err := podman.NewRuntime()
	require.NoError(t, err)
	require.NoError(t, h1.RegisterRuntime(podmanRuntime1))

	jd := &hypervisor.JobDefinition{
		Name:        "full-crash-test",
		Version:     "1.0.0",
		RuntimeType: runtime.RuntimeTypePodman,
		Config: &podman.Config{
			Image:   "checker-checkpoint-restore-test:latest",
			Network: "host",
		},
	}
	require.NoError(t, h1.RegisterJobDefinition(jd))

	jobID, err := h1.Spawn(ctx, hypervisor.SpawnOptions{
		DefinitionName:    "full-crash-test",
		DefinitionVersion: "1.0.0",
		Params:            json.RawMessage(`{"number": 3, "suspend_duration": "5s"}`),
	})
	require.NoError(t, err)
	t.Logf("Spawned job: %s", jobID)

	// Wait for job to be suspended
	t.Log("Waiting for job to be suspended...")
	var suspended bool
	for i := 0; i < 60; i++ {
		var dbJob query.Job
		err := query.ReliableExec(ctx, pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
			var err error
			dbJob, err = q.GetJob(ctx, jobID)
			return err
		})
		require.NoError(t, err)

		if dbJob.State == query.JobStateSuspended {
			suspended = true
			t.Logf("Job is now suspended")
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, suspended, "Job did not reach suspended state")

	// Phase 2: Simulate full server crash
	t.Log("=== Phase 2: Simulating full server crash ===")

	// Force kill ANY checker containers (simulating they died with the server)
	containerName := fmt.Sprintf("checker-%s", jobID)
	killCmd := exec.Command("podman", "rm", "-f", containerName)
	killOutput, _ := killCmd.CombinedOutput()
	t.Logf("Force killed container %s: %s", containerName, string(killOutput))

	// Simulate crash - forcibly close without graceful shutdown
	h1.DevCrash()
	podmanRuntime1.Close()
	t.Log("Hypervisor 'crashed' (DevCrash called)")

	// Phase 3: Start NEW hypervisor and recover
	t.Log("=== Phase 3: Starting new hypervisor and recovering ===")

	// IMPORTANT: Reuse the same ports so CHECKER_API_URL in the restored container
	// still points to a valid address. CRIU preserves environment variables.
	h2 := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  callerAddr,
		RuntimeHTTPAddress: runtimeAddr,
		Pool:               pool,
	})
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h2.Shutdown(shutdownCtx)
	}()

	podmanRuntime2, err := podman.NewRuntime()
	require.NoError(t, err)
	defer podmanRuntime2.Close()

	require.NoError(t, h2.RegisterRuntime(podmanRuntime2))

	t.Log("Calling RecoverState()...")
	require.NoError(t, h2.RecoverState(ctx))

	// Update suspend_until to trigger immediate wake
	t.Log("Triggering immediate wake...")
	_, err = pool.Exec(ctx, "UPDATE jobs SET suspend_until = NOW() WHERE id = $1", jobID)
	require.NoError(t, err)

	// Wait for job to complete
	t.Log("Waiting for job to complete...")
	var completed bool
	var finalState query.JobState
	for i := 0; i < 120; i++ {
		var dbJob query.Job
		err := query.ReliableExec(ctx, pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
			var err error
			dbJob, err = q.GetJob(ctx, jobID)
			return err
		})
		require.NoError(t, err)

		finalState = dbJob.State
		if dbJob.State == query.JobStateCompleted || dbJob.State == query.JobStateFailed {
			completed = true
			t.Logf("Job finished with state: %s", dbJob.State)
			if dbJob.ResultOutput != nil {
				t.Logf("Job output: %s", string(dbJob.ResultOutput))
			}
			if dbJob.Error.Valid {
				t.Logf("Job error: %s", dbJob.Error.String)
			}
			break
		}
		t.Logf("Job state: %s, waiting...", dbJob.State)
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, completed, "Job did not complete")
	assert.Equal(t, query.JobStateCompleted, finalState, "Job should have completed successfully")

	// Verify the result: (3 + 1) * 2 = 8
	var dbJob query.Job
	err = query.ReliableExec(ctx, pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
		var err error
		dbJob, err = q.GetJob(ctx, jobID)
		return err
	})
	require.NoError(t, err)

	assert.Equal(t, int32(0), dbJob.ResultExitCode.Int32, "Exit code should be 0")

	var output struct {
		Result struct {
			Value int `json:"value"`
		} `json:"result"`
		PreCheckpointRuns int `json:"pre_checkpoint_runs"`
	}
	require.NoError(t, json.Unmarshal(dbJob.ResultOutput, &output))
	assert.Equal(t, 8, output.Result.Value, "Expected (3 + 1) * 2 = 8")
	assert.Equal(t, 1, output.PreCheckpointRuns, "With CRIU, pre-checkpoint code should only run once")

	t.Log("=== Full server crash recovery test PASSED ===")

	// Cleanup
	_, _ = pool.Exec(ctx, "DELETE FROM jobs WHERE id = $1", jobID)
	_, _ = pool.Exec(ctx, "DELETE FROM job_definitions WHERE name = $1", "full-crash-test")
}
