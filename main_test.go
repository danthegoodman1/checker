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
	"sync/atomic"
	"testing"
	"time"

	"github.com/danthegoodman1/checker/hypervisor"
	"github.com/danthegoodman1/checker/runtime"
	"github.com/danthegoodman1/checker/runtime/docker"
	"github.com/danthegoodman1/checker/runtime/nodejs"
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

func setupDockerTest(t *testing.T) *testEnv {
	env := setupTestBase(t)

	// Build the Docker image from the demo directory
	cwd, err := os.Getwd()
	require.NoError(t, err)
	demoDir := filepath.Join(cwd, "demo")

	cmd := exec.Command("docker", "build", "-t", "checker-worker-test:latest", demoDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("Skipping Docker test: failed to build image: %v\n%s", err, output)
	}

	dockerRuntime, err := docker.NewRuntime()
	if err != nil {
		t.Skipf("Skipping Docker test: %v", err)
	}
	t.Cleanup(func() {
		dockerRuntime.Close()
	})
	require.NoError(t, env.h.RegisterRuntime(dockerRuntime))
	return env
}

func (e *testEnv) registerWorker(retryPolicy *hypervisor.RetryPolicy) {
	e.registerWorkerWithConfig("test-worker", "1.0.0", runtime.RuntimeTypeNodeJS, map[string]any{
		"entry_point": e.workerPath,
		"work_dir":    filepath.Dir(e.workerPath),
	}, retryPolicy)
}

func (e *testEnv) registerDockerWorker(retryPolicy *hypervisor.RetryPolicy) {
	e.registerWorkerWithConfig("test-docker-worker", "1.0.0", runtime.RuntimeTypeDocker, map[string]any{
		"image": "checker-worker-test:latest",
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

func (e *testEnv) spawnJob(definitionName string, params map[string]any) string {
	return e.spawnJobWithEnv(definitionName, params, nil)
}

func (e *testEnv) spawnJobWithEnv(definitionName string, params map[string]any, env map[string]string) string {
	spawnReq := map[string]any{
		"definition_name":    definitionName,
		"definition_version": "1.0.0",
		"params":             params,
	}
	if env != nil {
		spawnReq["env"] = env
	}
	spawnBody, _ := json.Marshal(spawnReq)

	resp, err := e.client.Post(e.baseURL+"/jobs", "application/json", bytes.NewReader(spawnBody))
	require.NoError(e.t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(e.t, http.StatusCreated, resp.StatusCode, "spawn failed: %s", string(body))

	var spawnResp struct {
		JobID string `json:"job_id"`
	}
	require.NoError(e.t, json.Unmarshal(body, &spawnResp))
	return spawnResp.JobID
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

func TestRunJSWorkerViaHTTPAPI(t *testing.T) {
	env := setupTest(t)
	env.registerWorker(nil)

	inputNumber := 5
	// Worker adds 1 then doubles: (5 + 1) * 2 = 12
	expectedResult := (inputNumber + 1) * 2

	jobID := env.spawnJob("test-worker", map[string]any{"number": inputNumber})
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

	jobID := env.spawnJob("test-worker", map[string]any{"crash": "before_checkpoint"})
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

	jobID := env.spawnJob("test-worker", map[string]any{"crash": "before_checkpoint"})
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

	jobID := env.spawnJob("test-worker", map[string]any{"crash": "after_checkpoint"})
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

	jobID := env.spawnJob("test-worker", map[string]any{"crash": "always"})
	t.Logf("Spawned always-crashing job: %s", jobID)

	result := env.waitForResult(jobID)
	assert.Equal(t, 1, result.ExitCode, "expected exit code 1 after exhausting retries")
	t.Logf("Job failed with exit code: %d", result.ExitCode)

	job := env.getJob(jobID)
	assert.Equal(t, float64(2), job["RetryCount"], "expected 2 retries (exhausted)")
	assert.Equal(t, "failed", job["State"], "expected failed state")
}

// TestDockerWorker tests running a worker inside a Docker container.
// Requires: docker build -t checker-worker-test:latest ./demo
func TestDockerWorker(t *testing.T) {
	env := setupDockerTest(t)
	env.registerDockerWorker(nil)

	inputNumber := 7
	// Worker adds 1 then doubles: (7 + 1) * 2 = 16
	expectedResult := (inputNumber + 1) * 2

	jobID := env.spawnJob("test-docker-worker", map[string]any{"number": inputNumber})
	t.Logf("Spawned Docker job: %s", jobID)

	result := env.waitForResult(jobID)
	assert.Equal(t, 0, result.ExitCode)
	t.Logf("Docker job completed with exit code: %d, output: %s", result.ExitCode, string(result.Output))

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
	t.Logf("Docker job checkpoint count: %v", job["CheckpointCount"])
}

// TestDockerCheckpointRestore tests checkpoint with suspend_duration, then restore after delay.
// On macOS, checkpoint just stops/starts the container. To avoid infinite loops,
// we use the CHECKER_JOB_SPAWNED_AT env var (set by hypervisor) combined with
// checkpoint_suspend_within_secs param. Worker only checkpoints if within that window.
// After restore, enough time has passed so checkpoint is skipped.
func TestDockerCheckpointRestore(t *testing.T) {
	env := setupDockerTest(t)
	env.registerDockerWorker(nil)

	inputNumber := 10
	expectedResult := (inputNumber + 1) * 2

	// Worker will checkpoint with 4s suspend only if within 3 seconds of spawn time.
	// After the 4s suspend + restore, we'll be past the 3s window so checkpoint is skipped.
	jobID := env.spawnJob("test-docker-worker", map[string]any{
		"number":                         inputNumber,
		"checkpoint_suspend_within_secs": 3, // Only checkpoint within 3s of spawn
		"checkpoint_suspend_duration":    "4s",
	})
	t.Logf("Spawned Docker checkpoint/restore job: %s", jobID)

	result := env.waitForResult(jobID)
	assert.Equal(t, 0, result.ExitCode)
	t.Logf("Docker job completed with exit code: %d, output: %s", result.ExitCode, string(result.Output))

	var output struct {
		Result struct {
			Step  int `json:"step"`
			Value int `json:"value"`
		} `json:"result"`
		CheckpointSkipped bool `json:"checkpoint_skipped"`
	}
	require.NoError(t, json.Unmarshal(result.Output, &output))
	assert.Equal(t, expectedResult, output.Result.Value, "expected (input + 1) * 2")
	t.Logf("Input: %d, Output: %d, CheckpointSkipped: %v", inputNumber, output.Result.Value, output.CheckpointSkipped)

	job := env.getJob(jobID)
	// On macOS: first run checkpoints (stops container), suspend timer restores it,
	// second run skips checkpoint -> 1 checkpoint total
	// On Linux with real CRIU: checkpoint preserves state, restore continues -> 1 checkpoint
	assert.Equal(t, float64(1), job["CheckpointCount"])
	t.Logf("Docker job checkpoint count: %v", job["CheckpointCount"])
}
