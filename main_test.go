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
	"testing"
	"time"

	"github.com/danthegoodman1/checker/hypervisor"
	"github.com/danthegoodman1/checker/runtime"
	"github.com/danthegoodman1/checker/runtime/nodejs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunJSWorkerViaHTTPAPI(t *testing.T) {
	callerAddr := "127.0.0.1:18080"
	runtimeAddr := "127.0.0.1:18081"

	h := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  callerAddr,
		RuntimeHTTPAddress: runtimeAddr,
	})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		assert.NoError(t, h.Shutdown(ctx))
	}()

	nodeRuntime := nodejs.NewRuntime()
	require.NoError(t, h.RegisterRuntime(nodeRuntime))

	time.Sleep(100 * time.Millisecond)

	baseURL := fmt.Sprintf("http://%s", callerAddr)
	client := &http.Client{Timeout: 30 * time.Second}

	cwd, err := os.Getwd()
	require.NoError(t, err)
	workerPath := filepath.Join(cwd, "demo", "worker.js")

	// Register job definition via HTTP
	registerReq := map[string]any{
		"name":         "test-worker",
		"version":      "1.0.0",
		"runtime_type": runtime.RuntimeTypeNodeJS,
		"config": map[string]any{
			"entry_point": workerPath,
			"work_dir":    filepath.Join(cwd, "demo"),
		},
	}
	registerBody, _ := json.Marshal(registerReq)

	resp, err := client.Post(baseURL+"/definitions", "application/json", bytes.NewReader(registerBody))
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "register failed: %s", string(body))

	// Spawn job via HTTP
	spawnReq := map[string]any{
		"definition_name":    "test-worker",
		"definition_version": "1.0.0",
		"params":             map[string]any{"test": true},
	}
	spawnBody, _ := json.Marshal(spawnReq)

	resp, err = client.Post(baseURL+"/jobs", "application/json", bytes.NewReader(spawnBody))
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "spawn failed: %s", string(body))

	var spawnResp struct {
		JobID string `json:"job_id"`
	}
	require.NoError(t, json.Unmarshal(body, &spawnResp))
	t.Logf("Spawned job: %s", spawnResp.JobID)

	// Wait for result
	resp, err = client.Get(fmt.Sprintf("%s/jobs/%s/result?wait=true", baseURL, spawnResp.JobID))
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "get result failed: %s", string(body))

	var result struct {
		ExitCode int `json:"ExitCode"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	assert.Equal(t, 0, result.ExitCode)
	t.Logf("Job completed with exit code: %d", result.ExitCode)

	// Verify job was checkpointed by checking the job state
	resp, err = client.Get(fmt.Sprintf("%s/jobs/%s", baseURL, spawnResp.JobID))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var job struct {
		CheckpointCount int `json:"CheckpointCount"`
	}
	json.NewDecoder(resp.Body).Decode(&job)
	resp.Body.Close()
	assert.Equal(t, 1, job.CheckpointCount, "expected 1 checkpoint")
	t.Logf("Job checkpoint count: %d", job.CheckpointCount)
}
