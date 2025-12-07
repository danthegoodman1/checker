package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danthegoodman1/checker/hypervisor"
	"github.com/danthegoodman1/checker/runtime"
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

func setupTest(t *testing.T) *testEnv {
	port := portCounter.Add(2)
	callerAddr := fmt.Sprintf("127.0.0.1:%d", port)
	runtimeAddr := fmt.Sprintf("127.0.0.1:%d", port+1)

	h := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  callerAddr,
		RuntimeHTTPAddress: runtimeAddr,
	})

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		assert.NoError(t, h.Shutdown(ctx))
	})

	nodeRuntime := nodejs.NewRuntime()
	require.NoError(t, h.RegisterRuntime(nodeRuntime))

	time.Sleep(100 * time.Millisecond)

	cwd, err := os.Getwd()
	require.NoError(t, err)

	env := &testEnv{
		t:          t,
		h:          h,
		baseURL:    fmt.Sprintf("http://%s", callerAddr),
		client:     &http.Client{Timeout: 30 * time.Second},
		workerPath: filepath.Join(cwd, "demo", "worker.js"),
	}

	env.registerWorker()
	return env
}

func (e *testEnv) registerWorker() {
	cwd, _ := os.Getwd()
	registerReq := map[string]any{
		"name":         "test-worker",
		"version":      "1.0.0",
		"runtime_type": runtime.RuntimeTypeNodeJS,
		"config": map[string]any{
			"entry_point": e.workerPath,
			"work_dir":    filepath.Join(cwd, "demo"),
		},
	}
	registerBody, _ := json.Marshal(registerReq)
	resp, err := e.client.Post(e.baseURL+"/definitions", "application/json", bytes.NewReader(registerBody))
	require.NoError(e.t, err)
	resp.Body.Close()
	require.Equal(e.t, http.StatusCreated, resp.StatusCode)
}

func (e *testEnv) spawnJob(params map[string]any) string {
	spawnReq := map[string]any{
		"definition_name":    "test-worker",
		"definition_version": "1.0.0",
		"params":             params,
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

	jobID := env.spawnJob(map[string]any{"test": true})
	t.Logf("Spawned job: %s", jobID)

	result := env.waitForResult(jobID)
	assert.Equal(t, 0, result.ExitCode)
	t.Logf("Job completed with exit code: %d, output: %s", result.ExitCode, string(result.Output))

	job := env.getJob(jobID)
	assert.Equal(t, float64(1), job["CheckpointCount"])
	t.Logf("Job checkpoint count: %v", job["CheckpointCount"])
}

func TestWorkerCrashBeforeCheckpoint(t *testing.T) {
	env := setupTest(t)

	jobID := env.spawnJob(map[string]any{"crash": "before_checkpoint"})
	t.Logf("Spawned crashing job: %s", jobID)

	result := env.waitForResult(jobID)
	assert.NotEqual(t, 0, result.ExitCode, "expected non-zero exit code for crash")
	t.Logf("Crashed job exit code: %d", result.ExitCode)

	job := env.getJob(jobID)
	assert.Equal(t, float64(0), job["CheckpointCount"], "expected 0 checkpoints before crash")
}

func TestWorkerCrashAfterCheckpoint(t *testing.T) {
	env := setupTest(t)

	jobID := env.spawnJob(map[string]any{"crash": "after_checkpoint"})
	t.Logf("Spawned crashing job: %s", jobID)

	result := env.waitForResult(jobID)
	assert.NotEqual(t, 0, result.ExitCode, "expected non-zero exit code for crash")
	t.Logf("Crashed job exit code: %d", result.ExitCode)

	job := env.getJob(jobID)
	assert.Equal(t, float64(1), job["CheckpointCount"], "expected 1 checkpoint before crash")
}
