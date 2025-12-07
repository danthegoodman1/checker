package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danthegoodman1/checker/hypervisor"
	"github.com/danthegoodman1/checker/runtime"
	"github.com/danthegoodman1/checker/runtime/nodejs"
)

func TestRunJSWorker(t *testing.T) {
	// Create the hypervisor
	h := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  "0.0.0.0:8080",
		RuntimeHTTPAddress: "0.0.0.0:8081",
	})

	// Register the NodeJS runtime
	nodeRuntime := nodejs.NewRuntime()
	if err := h.RegisterRuntime(nodeRuntime); err != nil {
		t.Fatalf("failed to register nodejs runtime: %v", err)
	}

	// Get working directory to find the demo worker
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	workerPath := filepath.Join(cwd, "demo", "worker.js")

	// Verify the worker file exists
	if _, err := os.Stat(workerPath); os.IsNotExist(err) {
		t.Fatalf("worker.js not found at %s", workerPath)
	}

	// Register a job definition for the demo worker
	demoJobDef := &hypervisor.JobDefinition{
		Name:        "test-worker",
		Version:     "1.0.0",
		RuntimeType: runtime.RuntimeTypeNodeJS,
		Config: &nodejs.Config{
			EntryPoint: workerPath,
			WorkDir:    filepath.Join(cwd, "demo"),
			Env: map[string]string{
				"NODE_ENV": "test",
			},
		},
	}

	if err := h.RegisterJobDefinition(demoJobDef); err != nil {
		t.Fatalf("failed to register job definition: %v", err)
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Spawn the job
	jobID, err := h.Spawn(ctx, hypervisor.SpawnOptions{
		DefinitionName:    "test-worker",
		DefinitionVersion: hypervisor.VersionLatest,
		Params:            json.RawMessage(`{"test": true}`),
	})
	if err != nil {
		t.Fatalf("failed to spawn job: %v", err)
	}

	t.Logf("Spawned job: %s", jobID)

	// Wait for the result
	result, err := h.GetResult(ctx, jobID, true)
	if err != nil {
		t.Fatalf("failed to get result: %v", err)
	}

	// Verify the result
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}

	t.Logf("Job completed with exit code: %d", result.ExitCode)

	// Shutdown gracefully
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := h.Shutdown(shutdownCtx); err != nil {
		t.Errorf("shutdown error: %v", err)
	}
}

func TestRunJSWorkerWithNonZeroExit(t *testing.T) {
	// Create a temporary JS file that exits with non-zero code
	tmpDir := t.TempDir()
	workerPath := filepath.Join(tmpDir, "failing-worker.js")
	workerCode := `
console.log("This worker will exit with code 1");
process.exit(1);
`
	if err := os.WriteFile(workerPath, []byte(workerCode), 0644); err != nil {
		t.Fatalf("failed to write temp worker: %v", err)
	}

	// Create the hypervisor
	h := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  "0.0.0.0:8080",
		RuntimeHTTPAddress: "0.0.0.0:8081",
	})

	// Register the NodeJS runtime
	nodeRuntime := nodejs.NewRuntime()
	if err := h.RegisterRuntime(nodeRuntime); err != nil {
		t.Fatalf("failed to register nodejs runtime: %v", err)
	}

	// Register a job definition
	jobDef := &hypervisor.JobDefinition{
		Name:        "failing-worker",
		Version:     "1.0.0",
		RuntimeType: runtime.RuntimeTypeNodeJS,
		Config: &nodejs.Config{
			EntryPoint: workerPath,
			WorkDir:    tmpDir,
		},
	}

	if err := h.RegisterJobDefinition(jobDef); err != nil {
		t.Fatalf("failed to register job definition: %v", err)
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Spawn the job
	jobID, err := h.Spawn(ctx, hypervisor.SpawnOptions{
		DefinitionName:    "failing-worker",
		DefinitionVersion: hypervisor.VersionLatest,
	})
	if err != nil {
		t.Fatalf("failed to spawn job: %v", err)
	}

	// Wait for the result
	result, err := h.GetResult(ctx, jobID, true)
	if err != nil {
		t.Fatalf("failed to get result: %v", err)
	}

	// Verify the result is exit code 1
	if result.ExitCode != 1 {
		t.Errorf("expected exit code 1, got %d", result.ExitCode)
	}

	t.Logf("Job correctly exited with code: %d", result.ExitCode)

	// Shutdown gracefully
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := h.Shutdown(shutdownCtx); err != nil {
		t.Errorf("shutdown error: %v", err)
	}
}
