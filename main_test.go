package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danthegoodman1/checker/hypervisor"
	"github.com/danthegoodman1/checker/runtime"
	"github.com/danthegoodman1/checker/runtime/nodejs"
	"github.com/danthegoodman1/checker/runtime/podman"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var portCounter atomic.Int32

func init() {
	portCounter.Store(18080)
}

type testEnv struct {
	t          *testing.T
	h          *hypervisor.Hypervisor
	baseURL    string
	client     *http.Client
	workerPath string
}

func setupTestBase(t *testing.T) *testEnv {
	port := portCounter.Add(2)
	callerAddr := fmt.Sprintf("127.0.0.1:%d", port)
	runtimeAddr := fmt.Sprintf("127.0.0.1:%d", port+1)

	h := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  callerAddr,
		RuntimeHTTPAddress: runtimeAddr,
	})

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		assert.NoError(t, h.Shutdown(ctx))
	})

	time.Sleep(100 * time.Millisecond)

	cwd, err := os.Getwd()
	require.NoError(t, err)

	return &testEnv{
		t:          t,
		h:          h,
		baseURL:    fmt.Sprintf("http://%s", callerAddr),
		client:     &http.Client{Timeout: 60 * time.Second},
		workerPath: filepath.Join(cwd, "demo", "worker.js"),
	}
}

func setupTest(t *testing.T) *testEnv {
	env := setupTestBase(t)
	nodeRuntime := nodejs.NewRuntime()
	require.NoError(t, env.h.RegisterRuntime(nodeRuntime))
	return env
}

func (e *testEnv) registerWorker(retryPolicy *hypervisor.RetryPolicy) {
	e.registerWorkerWithConfig("test-worker", "1.0.0", runtime.RuntimeTypeNodeJS, map[string]any{
		"entry_point": e.workerPath,
		"work_dir":    filepath.Dir(e.workerPath),
	}, retryPolicy)
}

func (e *testEnv) registerWorkerWithConfig(name, version string, runtimeType runtime.RuntimeType, config map[string]any, retryPolicy *hypervisor.RetryPolicy) {
	registerReq := map[string]any{
		"name":         name,
		"version":      version,
		"runtime_type": runtimeType,
		"config":       config,
	}
	if retryPolicy != nil {
		registerReq["retry_policy"] = retryPolicy
	}
	registerBody, _ := json.Marshal(registerReq)
	resp, err := e.client.Post(e.baseURL+"/definitions", "application/json", bytes.NewReader(registerBody))
	require.NoError(e.t, err)
	resp.Body.Close()
	require.Equal(e.t, http.StatusCreated, resp.StatusCode)
}

type jobResult struct {
	ExitCode int             `json:"ExitCode"`
	Output   json.RawMessage `json:"Output"`
}

func (e *testEnv) waitForResult(jobID string) jobResult {
	resp, err := e.client.Get(fmt.Sprintf("%s/jobs/%s/result?wait=true", e.baseURL, jobID))
	require.NoError(e.t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(e.t, http.StatusOK, resp.StatusCode, "get result failed: %s", string(body))

	var result jobResult
	require.NoError(e.t, json.Unmarshal(body, &result))
	return result
}

func (e *testEnv) getJob(jobID string) map[string]any {
	resp, err := e.client.Get(fmt.Sprintf("%s/jobs/%s", e.baseURL, jobID))
	require.NoError(e.t, err)
	require.Equal(e.t, http.StatusOK, resp.StatusCode)
	var job map[string]any
	json.NewDecoder(resp.Body).Decode(&job)
	resp.Body.Close()
	return job
}

// testLogWriter writes to t.Log with a prefix
type testLogWriter struct {
	t      *testing.T
	prefix string
}

func (w *testLogWriter) Write(p []byte) (n int, err error) {
	w.t.Logf("%s %s", w.prefix, string(p))
	return len(p), nil
}

// spawnJobWithLogs spawns a job directly via the hypervisor with log writers.
// This allows capturing logs from the very start of the process.
func (e *testEnv) spawnJobWithLogs(definitionName string, params map[string]any) string {
	paramsJSON, _ := json.Marshal(params)

	jobID, err := e.h.Spawn(context.Background(), hypervisor.SpawnOptions{
		DefinitionName:    definitionName,
		DefinitionVersion: "1.0.0",
		Params:            paramsJSON,
		Stdout:            &testLogWriter{t: e.t, prefix: "[stdout]"},
		Stderr:            &testLogWriter{t: e.t, prefix: "[stderr]"},
	})
	require.NoError(e.t, err)
	return jobID
}

func TestRunJSWorkerViaHTTPAPI(t *testing.T) {
	env := setupTest(t)
	env.registerWorker(nil)

	inputNumber := 5
	// Worker adds 1 then doubles: (5 + 1) * 2 = 12
	expectedResult := (inputNumber + 1) * 2

	jobID := env.spawnJobWithLogs("test-worker", map[string]any{"number": inputNumber})
	t.Logf("Spawned job: %s", jobID)

	result := env.waitForResult(jobID)
	assert.Equal(t, 0, result.ExitCode)
	t.Logf("Job completed with exit code: %d, output: %s", result.ExitCode, string(result.Output))

	var output struct {
		Result struct {
			Step  int `json:"step"`
			Value int `json:"value"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(result.Output, &output))
	assert.Equal(t, expectedResult, output.Result.Value, "expected (input + 1) * 2")
	t.Logf("Input: %d, Output: %d", inputNumber, output.Result.Value)

	job := env.getJob(jobID)
	assert.Equal(t, float64(1), job["CheckpointCount"])
	t.Logf("Job checkpoint count: %v", job["CheckpointCount"])
}

func TestWorkerCrashNoRetry(t *testing.T) {
	env := setupTest(t)
	env.registerWorker(nil) // No retry policy

	jobID := env.spawnJobWithLogs("test-worker", map[string]any{"crash": "before_checkpoint"})
	t.Logf("Spawned crashing job: %s", jobID)

	result := env.waitForResult(jobID)
	assert.NotEqual(t, 0, result.ExitCode, "expected non-zero exit code for crash")
	t.Logf("Crashed job exit code: %d", result.ExitCode)

	job := env.getJob(jobID)
	assert.Equal(t, float64(0), job["CheckpointCount"], "expected 0 checkpoints")
	assert.Equal(t, float64(0), job["RetryCount"], "expected 0 retries")
}

func TestWorkerCrashWithRetry(t *testing.T) {
	env := setupTest(t)
	env.registerWorker(&hypervisor.RetryPolicy{MaxRetries: 1})

	jobID := env.spawnJobWithLogs("test-worker", map[string]any{"crash": "before_checkpoint"})
	t.Logf("Spawned crashing job with retry: %s", jobID)

	result := env.waitForResult(jobID)
	assert.Equal(t, 0, result.ExitCode, "expected success after retry")
	t.Logf("Job completed with exit code: %d, output: %s", result.ExitCode, string(result.Output))

	job := env.getJob(jobID)
	assert.Equal(t, float64(1), job["RetryCount"], "expected 1 retry")
	assert.Equal(t, float64(1), job["CheckpointCount"], "expected 1 checkpoint after successful retry")
}

func TestWorkerCrashAfterCheckpointWithRetry(t *testing.T) {
	env := setupTest(t)
	env.registerWorker(&hypervisor.RetryPolicy{MaxRetries: 1})

	jobID := env.spawnJobWithLogs("test-worker", map[string]any{"crash": "after_checkpoint"})
	t.Logf("Spawned crashing job with retry: %s", jobID)

	result := env.waitForResult(jobID)
	assert.Equal(t, 0, result.ExitCode, "expected success after retry")
	t.Logf("Job completed with exit code: %d, output: %s", result.ExitCode, string(result.Output))

	job := env.getJob(jobID)
	assert.Equal(t, float64(1), job["RetryCount"], "expected 1 retry")
	// First run: checkpoint then crash. Second run: checkpoint then success = 2 checkpoints total
	assert.Equal(t, float64(2), job["CheckpointCount"], "expected 2 checkpoints (1 per attempt)")
}

func TestWorkerCrashExhaustsRetries(t *testing.T) {
	env := setupTest(t)
	env.registerWorker(&hypervisor.RetryPolicy{MaxRetries: 2})

	jobID := env.spawnJobWithLogs("test-worker", map[string]any{"crash": "always"})
	t.Logf("Spawned always-crashing job: %s", jobID)

	result := env.waitForResult(jobID)
	assert.Equal(t, 1, result.ExitCode, "expected exit code 1 after exhausting retries")
	t.Logf("Job failed with exit code: %d", result.ExitCode)

	job := env.getJob(jobID)
	assert.Equal(t, float64(2), job["RetryCount"], "expected 2 retries (exhausted)")
	assert.Equal(t, "failed", job["State"], "expected failed state")
}

func setupPodmanTest(t *testing.T) *testEnv {
	if goruntime.GOOS != "linux" {
		t.Skip("Podman checkpoint/restore only supported on Linux")
	}

	// On Linux, containers access the host via host networking,
	// so the runtime API must bind to 0.0.0.0 to be reachable from containers.
	port := portCounter.Add(2)
	callerAddr := fmt.Sprintf("127.0.0.1:%d", port)
	runtimeAddr := fmt.Sprintf("0.0.0.0:%d", port+1)

	h := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  callerAddr,
		RuntimeHTTPAddress: runtimeAddr,
	})

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		assert.NoError(t, h.Shutdown(ctx))
	})

	time.Sleep(100 * time.Millisecond)

	cwd, err := os.Getwd()
	require.NoError(t, err)

	env := &testEnv{
		t:          t,
		h:          h,
		baseURL:    fmt.Sprintf("http://%s", callerAddr),
		client:     &http.Client{Timeout: 60 * time.Second},
		workerPath: filepath.Join(cwd, "demo", "worker.js"),
	}

	podmanRuntime, err := podman.NewRuntime()
	if err != nil {
		t.Skipf("Skipping Podman test: %v", err)
	}
	t.Cleanup(func() {
		podmanRuntime.Close()
	})
	require.NoError(t, env.h.RegisterRuntime(podmanRuntime))
	return env
}

// TestPodmanCheckpointRestore tests checkpoint with suspend_duration using Podman.
// Podman exports checkpoints to portable tar files, enabling cross-node restore.
// This test verifies that CRIU properly preserves in-memory state.
// Also tests the checkpoint_dir config parameter.
func TestPodmanCheckpointRestore(t *testing.T) {
	env := setupPodmanTest(t)

	// Build the checkpoint restore test image using podman
	cwd, err := os.Getwd()
	require.NoError(t, err)
	demoDir := filepath.Join(cwd, "demo")

	cmd := exec.Command("podman", "build", "-t", "checker-checkpoint-restore-test:latest", "-f", filepath.Join(demoDir, "Dockerfile.checkpoint_restore"), demoDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to build checkpoint restore test image: %v\n%s", err, output)
	}

	// Register the checkpoint restore test worker
	// Use host network to avoid network namespace issues with checkpoint/restore
	env.registerWorkerWithConfig("test-podman-checkpoint-restore", "1.0.0", runtime.RuntimeTypePodman, map[string]any{
		"image":   "checker-checkpoint-restore-test:latest",
		"network": "host",
	}, nil)

	inputNumber := 10
	expectedResult := (inputNumber + 1) * 2

	// Worker will checkpoint with 4s suspend only if within 3 seconds of spawn time.
	// After the 4s suspend + restore, we'll be past the 3s window so checkpoint is skipped.
	jobID := env.spawnJobWithLogs("test-podman-checkpoint-restore", map[string]any{
		"number":                 inputNumber,
		"checkpoint_within_secs": 3, // Only checkpoint within 3s of spawn
		"suspend_duration":       "4s",
	})
	t.Logf("Spawned Podman checkpoint/restore job: %s", jobID)

	result := env.waitForResult(jobID)
	assert.Equal(t, 0, result.ExitCode)
	t.Logf("Podman job completed with exit code: %d, output: %s", result.ExitCode, string(result.Output))

	var restoreOutput struct {
		Result struct {
			Step  int `json:"step"`
			Value int `json:"value"`
		} `json:"result"`
		CheckpointSkipped bool `json:"checkpoint_skipped"`
		PreCheckpointRuns int  `json:"pre_checkpoint_runs"`
	}
	require.NoError(t, json.Unmarshal(result.Output, &restoreOutput))
	assert.Equal(t, expectedResult, restoreOutput.Result.Value, "expected (input + 1) * 2")
	t.Logf("Input: %d, Output: %d, CheckpointSkipped: %v, PreCheckpointRuns: %d",
		inputNumber, restoreOutput.Result.Value, restoreOutput.CheckpointSkipped, restoreOutput.PreCheckpointRuns)

	// With real CRIU (Podman on Linux): in-memory state is preserved across checkpoint/restore,
	// so code before checkpoint only executes once
	assert.Equal(t, 1, restoreOutput.PreCheckpointRuns,
		"with real CRIU checkpoint, pre-checkpoint code should only run once (state preserved)")

	job := env.getJob(jobID)
	assert.Equal(t, float64(1), job["CheckpointCount"])
	t.Logf("Podman job checkpoint count: %v", job["CheckpointCount"])
}

// TestPodmanCheckpointLock tests that the checkpoint lock prevents checkpointing
// until the lock is released. The worker takes a lock, schedules a release after
// a delay, and immediately tries to checkpoint. The checkpoint should be blocked
// until the lock is released.
func TestPodmanCheckpointLock(t *testing.T) {
	env := setupPodmanTest(t)

	// Build the checkpoint lock test image
	cwd, err := os.Getwd()
	require.NoError(t, err)
	demoDir := filepath.Join(cwd, "demo")

	cmd := exec.Command("podman", "build", "-t", "checker-checkpoint-lock-test:latest", "-f", filepath.Join(demoDir, "Dockerfile.checkpoint_lock"), demoDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to build checkpoint lock test image: %v\n%s", err, output)
	}

	// Register the checkpoint lock test worker with host networking
	env.registerWorkerWithConfig("test-podman-checkpoint-lock", "1.0.0", runtime.RuntimeTypePodman, map[string]any{
		"image":   "checker-checkpoint-lock-test:latest",
		"network": "host",
	}, nil)

	lockHoldMs := 2000 // Hold lock for 2 seconds

	jobID := env.spawnJobWithLogs("test-podman-checkpoint-lock", map[string]any{
		"lock_hold_ms": lockHoldMs,
	})
	t.Logf("Spawned Podman checkpoint lock test job: %s", jobID)

	result := env.waitForResult(jobID)
	assert.Equal(t, 0, result.ExitCode)
	t.Logf("Podman job completed with exit code: %d, output: %s", result.ExitCode, string(result.Output))

	var lockTestOutput struct {
		LockHoldMs           int  `json:"lock_hold_ms"`
		CheckpointDurationMs int  `json:"checkpoint_duration_ms"`
		WasBlocked           bool `json:"was_blocked"`
	}
	require.NoError(t, json.Unmarshal(result.Output, &lockTestOutput))

	t.Logf("Checkpoint lock test results: lock_hold_ms=%d, checkpoint_duration_ms=%d, was_blocked=%v",
		lockTestOutput.LockHoldMs,
		lockTestOutput.CheckpointDurationMs,
		lockTestOutput.WasBlocked)

	// Verify the checkpoint was actually blocked by the lock
	assert.True(t, lockTestOutput.WasBlocked,
		"checkpoint should have been blocked by lock (duration=%dms, expected >=%dms)",
		lockTestOutput.CheckpointDurationMs,
		lockHoldMs)

	// Verify the checkpoint duration was at least the lock hold time
	assert.GreaterOrEqual(t, lockTestOutput.CheckpointDurationMs, lockHoldMs,
		"checkpoint duration should be >= %dms (lock hold time)", lockHoldMs)

	job := env.getJob(jobID)
	assert.Equal(t, float64(1), job["CheckpointCount"])
	t.Logf("Podman job checkpoint count: %v", job["CheckpointCount"])
}

// TestPodmanCheckpointIdempotency tests that duplicate checkpoint tokens are handled
// idempotently - the checkpoint count should only increment for unique tokens.
func TestPodmanCheckpointIdempotency(t *testing.T) {
	env := setupPodmanTest(t)

	// Build the checkpoint idempotency test image
	cwd, err := os.Getwd()
	require.NoError(t, err)
	demoDir := filepath.Join(cwd, "demo")

	cmd := exec.Command("podman", "build", "-t", "checker-checkpoint-idempotency-test:latest", "-f", filepath.Join(demoDir, "Dockerfile.checkpoint_idempotency"), demoDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to build checkpoint idempotency test image: %v\n%s", err, output)
	}

	// Register the checkpoint idempotency test worker with host networking
	env.registerWorkerWithConfig("test-podman-checkpoint-idempotency", "1.0.0", runtime.RuntimeTypePodman, map[string]any{
		"image":   "checker-checkpoint-idempotency-test:latest",
		"network": "host",
	}, nil)

	jobID := env.spawnJobWithLogs("test-podman-checkpoint-idempotency", map[string]any{})
	t.Logf("Spawned Podman checkpoint idempotency test job: %s", jobID)

	result := env.waitForResult(jobID)
	assert.Equal(t, 0, result.ExitCode)
	t.Logf("Podman job completed with exit code: %d, output: %s", result.ExitCode, string(result.Output))

	var idempotencyOutput struct {
		Success          bool  `json:"success"`
		CheckpointCounts []int `json:"checkpoint_counts"`
		Expected         []int `json:"expected"`
	}
	require.NoError(t, json.Unmarshal(result.Output, &idempotencyOutput))

	t.Logf("Checkpoint idempotency test results: success=%v, counts=%v, expected=%v",
		idempotencyOutput.Success,
		idempotencyOutput.CheckpointCounts,
		idempotencyOutput.Expected)

	assert.True(t, idempotencyOutput.Success, "idempotency test should pass")
	assert.Equal(t, []int{1, 1, 2, 2}, idempotencyOutput.CheckpointCounts,
		"checkpoint counts should be [1, 1, 2, 2] - duplicates should not increment")

	job := env.getJob(jobID)
	assert.Equal(t, float64(2), job["CheckpointCount"])
	t.Logf("Podman job checkpoint count: %v", job["CheckpointCount"])
}
