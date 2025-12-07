package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/danthegoodman1/checker/hypervisor"
	"github.com/danthegoodman1/checker/runtime"
	"github.com/danthegoodman1/checker/runtime/nodejs"
)

func main() {
	// Create the hypervisor
	h := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  "0.0.0.0:8080",
		RuntimeHTTPAddress: "0.0.0.0:8081",
	})

	// Register the NodeJS runtime
	// The nodejs.NewRuntime() call is platform-specific:
	// - On macOS (darwin): returns a runtime with no-op checkpoint/restore
	// - On Linux: returns a runtime with CRIU-based checkpoint/restore (stubbed for now)
	nodeRuntime := nodejs.NewRuntime()
	if err := h.RegisterRuntime(nodeRuntime); err != nil {
		fmt.Fprintf(os.Stderr, "failed to register nodejs runtime: %v\n", err)
		os.Exit(1)
	}

	// Get the directory where the binary is running from
	// so we can find the demo/worker.js file
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get working directory: %v\n", err)
		os.Exit(1)
	}
	workerPath := filepath.Join(cwd, "demo", "worker.js")

	// Register a job definition for the demo worker
	demoJobDef := &hypervisor.JobDefinition{
		Name:        "demo-worker",
		Version:     "1.0.0",
		RuntimeType: runtime.RuntimeTypeNodeJS,
		Config: &nodejs.Config{
			EntryPoint: workerPath,
			WorkDir:    filepath.Join(cwd, "demo"),
			Env: map[string]string{
				"NODE_ENV": "development",
			},
		},
		Metadata: map[string]string{
			"team": "platform",
		},
	}

	if err := h.RegisterJobDefinition(demoJobDef); err != nil {
		fmt.Fprintf(os.Stderr, "failed to register job definition: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Checker hypervisor initialized")
	fmt.Println("Registered runtimes: nodejs")
	fmt.Printf("Registered job definitions: %s@%s\n", demoJobDef.Name, demoJobDef.Version)
	fmt.Println()

	// Run the demo job and wait for result
	if err := runDemoJob(h); err != nil {
		fmt.Fprintf(os.Stderr, "demo job failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\nShutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := h.Shutdown(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "shutdown error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Shutdown complete")
}

// runDemoJob spawns the demo worker job and waits for its result.
func runDemoJob(h *hypervisor.Hypervisor) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Println("--- Running Demo Job ---")

	// Spawn the job
	jobID, err := h.Spawn(ctx, hypervisor.SpawnOptions{
		DefinitionName:    "demo-worker",
		DefinitionVersion: hypervisor.VersionLatest,
		Params:            json.RawMessage(`{"task": "demo", "count": 42}`),
		Metadata: map[string]string{
			"request_id": "demo-12345",
		},
	})
	if err != nil {
		return fmt.Errorf("failed to spawn job: %w", err)
	}

	fmt.Printf("Spawned job: %s\n", jobID)
	fmt.Println("Waiting for job to complete...")

	// Wait for the result
	result, err := h.GetResult(ctx, jobID, true)
	if err != nil {
		return fmt.Errorf("failed to get result: %w", err)
	}

	fmt.Printf("\nJob completed with exit code: %d\n", result.ExitCode)
	if result.Output != nil {
		fmt.Printf("Job output: %s\n", string(result.Output))
	}

	fmt.Println("--- Demo Job Complete ---")
	return nil
}
