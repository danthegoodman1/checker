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

func TestRunJSWorker(t *testing.T) {
	h := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  "127.0.0.1:18080",
		RuntimeHTTPAddress: "127.0.0.1:18081",
	})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		assert.NoError(t, h.Shutdown(ctx))
	}()

	nodeRuntime := nodejs.NewRuntime()
	require.NoError(t, h.RegisterRuntime(nodeRuntime))

	cwd, err := os.Getwd()
	require.NoError(t, err)
	workerPath := filepath.Join(cwd, "demo", "worker.js")

	_, err = os.Stat(workerPath)
	require.NoError(t, err, "worker.js not found at %s", workerPath)

	demoJobDef := &hypervisor.JobDefinition{
		Name:        "test-worker",
		Version:     "1.0.0",
		RuntimeType: runtime.RuntimeTypeNodeJS,
		Config: &nodejs.Config{
			EntryPoint: workerPath,
			WorkDir:    filepath.Join(cwd, "demo"),
			Env:        map[string]string{"NODE_ENV": "test"},
		},
	}
	require.NoError(t, h.RegisterJobDefinition(demoJobDef))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	jobID, err := h.Spawn(ctx, hypervisor.SpawnOptions{
		DefinitionName:    "test-worker",
		DefinitionVersion: hypervisor.VersionLatest,
		Params:            json.RawMessage(`{"test": true}`),
	})
	require.NoError(t, err)
	t.Logf("Spawned job: %s", jobID)

	result, err := h.GetResult(ctx, jobID, true)
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	t.Logf("Job completed with exit code: %d", result.ExitCode)
}

func TestRunJSWorkerWithNonZeroExit(t *testing.T) {
	tmpDir := t.TempDir()
	workerPath := filepath.Join(tmpDir, "failing-worker.js")
	workerCode := `
console.log("This worker will exit with code 1");
process.exit(1);
`
	require.NoError(t, os.WriteFile(workerPath, []byte(workerCode), 0644))

	h := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  "127.0.0.1:18082",
		RuntimeHTTPAddress: "127.0.0.1:18083",
	})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		assert.NoError(t, h.Shutdown(ctx))
	}()

	nodeRuntime := nodejs.NewRuntime()
	require.NoError(t, h.RegisterRuntime(nodeRuntime))

	jobDef := &hypervisor.JobDefinition{
		Name:        "failing-worker",
		Version:     "1.0.0",
		RuntimeType: runtime.RuntimeTypeNodeJS,
		Config: &nodejs.Config{
			EntryPoint: workerPath,
			WorkDir:    tmpDir,
		},
	}
	require.NoError(t, h.RegisterJobDefinition(jobDef))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	jobID, err := h.Spawn(ctx, hypervisor.SpawnOptions{
		DefinitionName:    "failing-worker",
		DefinitionVersion: hypervisor.VersionLatest,
	})
	require.NoError(t, err)

	result, err := h.GetResult(ctx, jobID, true)
	require.NoError(t, err)
	assert.Equal(t, 1, result.ExitCode)
	t.Logf("Job correctly exited with code: %d", result.ExitCode)
}

func TestRunJSWorkerViaHTTPAPI(t *testing.T) {
	callerAddr := "127.0.0.1:18084"
	runtimeAddr := "127.0.0.1:18085"

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
		"name":         "test-worker-http",
		"version":      "1.0.0",
		"runtime_type": runtime.RuntimeTypeNodeJS,
		"config": map[string]any{
			"entry_point": workerPath,
			"work_dir":    filepath.Join(cwd, "demo"),
			"env":         map[string]string{"NODE_ENV": "test"},
		},
	}
	registerBody, _ := json.Marshal(registerReq)

	resp, err := client.Post(baseURL+"/definitions", "application/json", bytes.NewReader(registerBody))
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "register failed: %s", string(body))

	// List definitions
	resp, err = client.Get(baseURL + "/definitions")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var definitions []any
	json.NewDecoder(resp.Body).Decode(&definitions)
	resp.Body.Close()
	assert.Len(t, definitions, 1)

	// Spawn job via HTTP
	spawnReq := map[string]any{
		"definition_name":    "test-worker-http",
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

	// Get job status
	resp, err = client.Get(fmt.Sprintf("%s/jobs/%s", baseURL, spawnResp.JobID))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var job map[string]any
	json.NewDecoder(resp.Body).Decode(&job)
	resp.Body.Close()
	t.Logf("Job state: %v", job["State"])

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

	// List jobs
	resp, err = client.Get(baseURL + "/jobs")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var jobs []any
	json.NewDecoder(resp.Body).Decode(&jobs)
	resp.Body.Close()
	assert.Len(t, jobs, 1)
}
