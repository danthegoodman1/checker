package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/danthegoodman1/checker/hypervisor"
	"github.com/danthegoodman1/checker/migrations"
	"github.com/danthegoodman1/checker/runtime"
	"github.com/danthegoodman1/checker/runtime/cloudhv"
	"github.com/danthegoodman1/checker/utils"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Environment variables for Cloud Hypervisor tests:
// - CHV_FIRMWARE_PATH: Path to hypervisor-fw firmware (required)
//   Download from Cloud Hypervisor releases:
//   curl -fLo hypervisor-fw "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v50.0/hypervisor-fw"
//   The firmware includes a built-in kernel with virtio drivers.
// - CHV_BRIDGE_NAME: Name of the bridge for TAP networking (default: chvbr0)
// - PG_DSN: PostgreSQL connection string (required for hypervisor tests)
//
// Before running tests, set up the network bridge (IPv6).
// IMPORTANT: DAD must be disabled BEFORE adding the IPv6 address, otherwise the
// address gets stuck in "tentative" state and cannot be bound to.
//
//   sudo ip link add chvbr0 type bridge
//   sudo ip link set chvbr0 up
//   sudo sysctl -w net.ipv6.conf.chvbr0.accept_dad=0
//   sudo sysctl -w net.ipv6.conf.chvbr0.dad_transmits=0
//   sudo ip -6 addr add fdcd::1/16 dev chvbr0
//   echo 1 | sudo tee /proc/sys/net/ipv6/conf/all/forwarding
//   sudo ip6tables -t nat -A POSTROUTING -s fdcd::/16 ! -o chvbr0 -j MASQUERADE
//   sudo ip6tables -A FORWARD -i chvbr0 -j ACCEPT
//   sudo ip6tables -A FORWARD -o chvbr0 -m state --state RELATED,ESTABLISHED -j ACCEPT
//
// Run with: CHV_FIRMWARE_PATH=/path/to/hypervisor-fw PG_DSN=postgres://... go test -v -run TestCloudHypervisor

func getCloudHypervisorTestConfig(t *testing.T) (firmwarePath, bridgeName string) {
	t.Helper()

	if goruntime.GOOS != "linux" {
		t.Skip("Cloud Hypervisor requires Linux")
	}

	firmwarePath = os.Getenv("CHV_FIRMWARE_PATH")
	if firmwarePath == "" {
		t.Skip("CHV_FIRMWARE_PATH not set. Download hypervisor-fw from Cloud Hypervisor releases:\n" +
			"curl -fLo hypervisor-fw 'https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v50.0/hypervisor-fw'")
	}

	if _, err := exec.LookPath("cloud-hypervisor"); err != nil {
		t.Skip("cloud-hypervisor not found in PATH")
	}

	bridgeName = os.Getenv("CHV_BRIDGE_NAME")
	if bridgeName == "" {
		bridgeName = "chvbr0"
	}

	// Verify bridge exists
	cmd := exec.Command("ip", "link", "show", bridgeName)
	if err := cmd.Run(); err != nil {
		t.Skipf("Bridge %s not found. Set up with:\n"+
			"  sudo ip link add %s type bridge\n"+
			"  sudo ip link set %s up\n"+
			"  sudo sysctl -w net.ipv6.conf.%s.accept_dad=0\n"+
			"  sudo sysctl -w net.ipv6.conf.%s.dad_transmits=0\n"+
			"  sudo ip -6 addr add fdcd::1/16 dev %s\n"+
			"  echo 1 | sudo tee /proc/sys/net/ipv6/conf/all/forwarding\n"+
			"  sudo ip6tables -t nat -A POSTROUTING -s fdcd::/16 ! -o %s -j MASQUERADE\n"+
			"  sudo ip6tables -A FORWARD -i %s -j ACCEPT\n"+
			"  sudo ip6tables -A FORWARD -o %s -m state --state RELATED,ESTABLISHED -j ACCEPT",
			bridgeName, bridgeName, bridgeName, bridgeName, bridgeName, bridgeName, bridgeName, bridgeName, bridgeName)
	}

	// Verify bridge has IPv6 address fdcd::1
	cmd = exec.Command("ip", "-6", "addr", "show", "dev", bridgeName)
	output, err := cmd.Output()
	if err != nil || !strings.Contains(string(output), "fdcd::1") {
		t.Skipf("Bridge %s does not have IPv6 address fdcd::1. Set up with:\n"+
			"  sudo sysctl -w net.ipv6.conf.%s.accept_dad=0\n"+
			"  sudo sysctl -w net.ipv6.conf.%s.dad_transmits=0\n"+
			"  sudo ip -6 addr add fdcd::1/16 dev %s",
			bridgeName, bridgeName, bridgeName, bridgeName)
	}

	return firmwarePath, bridgeName
}

// buildCloudHypervisorRootfs builds a rootfs raw image from the checkpoint_restore Dockerfile.
// Returns the path to the rootfs file.
// Network configuration (IPv6) is injected at runtime by the Cloud Hypervisor runtime.
func buildCloudHypervisorRootfs(t *testing.T) string {
	t.Helper()

	// Check dependencies
	for _, cmd := range []string{"buildah", "skopeo", "umoci", "mkfs.ext4", "jq"} {
		if _, err := exec.LookPath(cmd); err != nil {
			t.Skipf("%s not found, skipping test", cmd)
		}
	}

	cwd, err := os.Getwd()
	require.NoError(t, err)

	rootfsPath := filepath.Join(t.TempDir(), "rootfs.raw")

	// Use the build-chv-rootfs.sh script (run with bash to avoid permission issues)
	// No network config needed - IPv6 is configured at runtime from env vars
	scriptPath := filepath.Join(cwd, "scripts", "build-chv-rootfs.sh")
	dockerfilePath := filepath.Join(cwd, "demo", "Dockerfile.checkpoint_restore")

	cmd := exec.Command("bash", scriptPath, dockerfilePath, rootfsPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to build rootfs: %v\n%s", err, output)
	}
	t.Logf("Built rootfs: %s", rootfsPath)

	return rootfsPath
}

// TestCloudHypervisorHypervisorIntegration tests the full hypervisor flow with Cloud Hypervisor:
// spawn job -> checkpoint -> restore -> complete
func TestCloudHypervisorHypervisorIntegration(t *testing.T) {
	firmwarePath, bridgeName := getCloudHypervisorTestConfig(t)

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

	// Build rootfs (IPv6 networking is configured at runtime)
	rootfsPath := buildCloudHypervisorRootfs(t)

	// Get unique ports for this test - use IPv6 gateway address
	port := portCounter.Add(2)
	callerAddr := fmt.Sprintf("[%s]:%d", cloudhv.IPv6Gateway, port)    // Use bridge IP so VM can reach it
	runtimeAddr := fmt.Sprintf("[%s]:%d", cloudhv.IPv6Gateway, port+1) // Use bridge IP

	ctx := context.Background()

	t.Log("=== Starting hypervisor ===")
	h := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  callerAddr,
		RuntimeHTTPAddress: runtimeAddr,
		Pool:               pool,
		WakePollerInterval: 500 * time.Millisecond,
	})
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h.Shutdown(shutdownCtx)
	}()

	chvRuntime, err := cloudhv.NewRuntime()
	require.NoError(t, err)
	defer chvRuntime.Close()

	require.NoError(t, h.RegisterRuntime(chvRuntime))

	// Register job definition
	jd := &hypervisor.JobDefinition{
		Name:        "chv-integration-test",
		Version:     "1.0.0",
		RuntimeType: runtime.RuntimeTypeCloudHypervisor,
		Config: &cloudhv.Config{
			FirmwarePath: firmwarePath,
			RootfsPath:   rootfsPath,
			VcpuCount:    1,
			MemSizeMib:   512,
			Network: &cloudhv.NetworkConfig{
				BridgeName: bridgeName,
			},
		},
	}
	require.NoError(t, h.RegisterJobDefinition(ctx, jd))

	// Spawn job
	t.Log("=== Spawning job ===")
	jobID, err := h.Spawn(ctx, hypervisor.SpawnOptions{
		DefinitionName:    "chv-integration-test",
		DefinitionVersion: "1.0.0",
		Params:            json.RawMessage(`{"number": 5, "suspend_duration": "500ms"}`),
		Stdout:            &testLogWriter{t: t, prefix: "[chv-stdout]"},
		Stderr:            &testLogWriter{t: t, prefix: "[chv-stderr]"},
	})
	require.NoError(t, err)
	t.Logf("Spawned job: %s", jobID)

	// Wait for job to be suspended
	t.Log("Waiting for job to be suspended...")
	var suspended bool
	for i := 0; i < 120; i++ { // Wait up to 60 seconds (Cloud Hypervisor boot can be slower)
		job, err := h.GetJob(ctx, jobID)
		require.NoError(t, err)

		if job.State == hypervisor.JobStateSuspended {
			suspended = true
			t.Logf("Job is now suspended (checkpoint_count=%d)", job.CheckpointCount)
			break
		}
		if job.State == hypervisor.JobStateFailed {
			t.Fatalf("Job failed with error: %s", job.Error)
		}
		if i%10 == 0 {
			t.Logf("Job state: %s, waiting...", job.State)
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, suspended, "Job did not reach suspended state")

	// Wait for job to complete (wake poller will restore it after suspend_duration)
	t.Log("Waiting for job to complete...")
	var completed bool
	var finalState hypervisor.JobState
	for i := 0; i < 120; i++ {
		job, err := h.GetJob(ctx, jobID)
		require.NoError(t, err)

		finalState = job.State
		if job.State == hypervisor.JobStateCompleted || job.State == hypervisor.JobStateFailed {
			completed = true
			t.Logf("Job finished with state: %s", job.State)
			if job.Result != nil && job.Result.Output != nil {
				t.Logf("Job output: %s", string(job.Result.Output))
			}
			if job.Error != "" {
				t.Logf("Job error: %s", job.Error)
			}
			break
		}
		if i%10 == 0 {
			t.Logf("Job state: %s, waiting...", job.State)
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, completed, "Job did not complete")
	assert.Equal(t, hypervisor.JobStateCompleted, finalState, "Job should have completed successfully")

	// Verify result
	job, err := h.GetJob(ctx, jobID)
	require.NoError(t, err)

	require.NotNil(t, job.Result, "Job result should not be nil")
	assert.Equal(t, 0, job.Result.ExitCode, "Exit code should be 0")

	var output struct {
		Result struct {
			Step  int `json:"step"`
			Value int `json:"value"`
		} `json:"result"`
		PreCheckpointRuns int `json:"pre_checkpoint_runs"`
	}
	require.NoError(t, json.Unmarshal(job.Result.Output, &output))
	assert.Equal(t, 12, output.Result.Value, "Expected (5 + 1) * 2 = 12")
	assert.Equal(t, 1, output.PreCheckpointRuns, "Pre-checkpoint code should only run once")

	t.Log("=== Cloud Hypervisor hypervisor integration test PASSED ===")

	// Cleanup
	_, _ = pool.Exec(ctx, "DELETE FROM jobs WHERE id = $1", jobID)
	_, _ = pool.Exec(ctx, "DELETE FROM job_definitions WHERE name = $1", "chv-integration-test")
}

// TestCloudHypervisorHypervisorCrashRecovery tests that Cloud Hypervisor jobs can be recovered after hypervisor crash.
func TestCloudHypervisorHypervisorCrashRecovery(t *testing.T) {
	firmwarePath, bridgeName := getCloudHypervisorTestConfig(t)

	if utils.PG_DSN == "" {
		t.Skip("PG_DSN environment variable not set")
	}

	// Run migrations
	_, err := migrations.RunMigrations(utils.PG_DSN)
	require.NoError(t, err)

	pool, err := pgxpool.New(context.Background(), utils.PG_DSN)
	require.NoError(t, err)
	defer pool.Close()

	// Build rootfs (IPv6 networking is configured at runtime)
	rootfsPath := buildCloudHypervisorRootfs(t)

	port := portCounter.Add(2)
	callerAddr := fmt.Sprintf("[%s]:%d", cloudhv.IPv6Gateway, port)
	runtimeAddr := fmt.Sprintf("[%s]:%d", cloudhv.IPv6Gateway, port+1)

	ctx := context.Background()

	// Phase 1: Start hypervisor, spawn job, wait for suspend, then "crash"
	t.Log("=== Phase 1: Starting hypervisor and spawning job ===")

	h1 := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  callerAddr,
		RuntimeHTTPAddress: runtimeAddr,
		Pool:               pool,
		WakePollerInterval: 500 * time.Millisecond,
	})

	chvRuntime1, err := cloudhv.NewRuntime()
	require.NoError(t, err)
	require.NoError(t, h1.RegisterRuntime(chvRuntime1))

	jd := &hypervisor.JobDefinition{
		Name:        "chv-crash-recovery-test",
		Version:     "1.0.0",
		RuntimeType: runtime.RuntimeTypeCloudHypervisor,
		Config: &cloudhv.Config{
			FirmwarePath: firmwarePath,
			RootfsPath:   rootfsPath,
			VcpuCount:    1,
			MemSizeMib:   512,
			Network: &cloudhv.NetworkConfig{
				BridgeName: bridgeName,
			},
		},
	}
	require.NoError(t, h1.RegisterJobDefinition(ctx, jd))

	jobID, err := h1.Spawn(ctx, hypervisor.SpawnOptions{
		DefinitionName:    "chv-crash-recovery-test",
		DefinitionVersion: "1.0.0",
		Params:            json.RawMessage(`{"number": 7, "suspend_duration": "1s"}`),
		Stdout:            &testLogWriter{t: t, prefix: "[chv-stdout]"},
		Stderr:            &testLogWriter{t: t, prefix: "[chv-stderr]"},
	})
	require.NoError(t, err)
	t.Logf("Spawned job: %s", jobID)

	// Wait for job to be suspended
	t.Log("Waiting for job to be suspended...")
	var suspended bool
	for i := 0; i < 120; i++ {
		job, err := h1.GetJob(ctx, jobID)
		require.NoError(t, err)

		if job.State == hypervisor.JobStateSuspended {
			suspended = true
			t.Logf("Job is now suspended")
			break
		}
		if job.State == hypervisor.JobStateFailed {
			t.Fatalf("Job failed with error: %s", job.Error)
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, suspended, "Job did not reach suspended state")

	// Simulate crash
	t.Log("=== Phase 2: Simulating crash (DevCrash) ===")
	h1.DevCrash()
	chvRuntime1.Close()
	time.Sleep(100 * time.Millisecond)

	// Phase 3: Start NEW hypervisor and recover
	t.Log("=== Phase 3: Starting new hypervisor and recovering state ===")

	h2 := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  callerAddr,
		RuntimeHTTPAddress: runtimeAddr,
		Pool:               pool,
		WakePollerInterval: 500 * time.Millisecond,
	})
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h2.Shutdown(shutdownCtx)
	}()

	chvRuntime2, err := cloudhv.NewRuntime()
	require.NoError(t, err)
	defer chvRuntime2.Close()

	require.NoError(t, h2.RegisterRuntime(chvRuntime2))

	t.Log("Calling RecoverState()...")
	require.NoError(t, h2.RecoverState(ctx))

	// Wait for job to complete (wake poller will restore it after suspend_duration)
	t.Log("Waiting for job to complete...")
	var completed bool
	var finalState hypervisor.JobState
	for i := 0; i < 120; i++ {
		job, err := h2.GetJob(ctx, jobID)
		require.NoError(t, err)

		finalState = job.State
		if job.State == hypervisor.JobStateCompleted || job.State == hypervisor.JobStateFailed {
			completed = true
			t.Logf("Job finished with state: %s", job.State)
			if job.Result != nil && job.Result.Output != nil {
				t.Logf("Job output: %s", string(job.Result.Output))
			}
			if job.Error != "" {
				t.Logf("Job error: %s", job.Error)
			}
			break
		}
		if i%10 == 0 {
			t.Logf("Job state: %s, waiting...", job.State)
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, completed, "Job did not complete")
	assert.Equal(t, hypervisor.JobStateCompleted, finalState, "Job should have completed successfully")

	// Verify result: (7 + 1) * 2 = 16
	job, err := h2.GetJob(ctx, jobID)
	require.NoError(t, err)

	require.NotNil(t, job.Result, "Job result should not be nil")
	assert.Equal(t, 0, job.Result.ExitCode, "Exit code should be 0")

	var output struct {
		Result struct {
			Value int `json:"value"`
		} `json:"result"`
		PreCheckpointRuns int `json:"pre_checkpoint_runs"`
	}
	require.NoError(t, json.Unmarshal(job.Result.Output, &output))
	assert.Equal(t, 16, output.Result.Value, "Expected (7 + 1) * 2 = 16")
	assert.Equal(t, 1, output.PreCheckpointRuns, "Pre-checkpoint code should only run once")

	t.Log("=== Cloud Hypervisor crash recovery test PASSED ===")

	// Cleanup
	_, _ = pool.Exec(ctx, "DELETE FROM jobs WHERE id = $1", jobID)
	_, _ = pool.Exec(ctx, "DELETE FROM job_definitions WHERE name = $1", "chv-crash-recovery-test")
}

// TestCloudHypervisorProcessCrashRestoreFromCheckpoint verifies that when a running Cloud Hypervisor VM crashes
// after creating a checkpoint, the hypervisor automatically restores from checkpoint on retry.
//
// This test:
// 1. Starts a hypervisor and spawns a job with retry policy
// 2. Job checkpoints with keep_running=true (VM continues running after checkpoint)
// 3. Job sleeps (giving us time to kill it)
// 4. Kill the Cloud Hypervisor process while job is running
// 5. Hypervisor detects failure and retries from checkpoint
// 6. Verifies the job completes successfully with correct output
func TestCloudHypervisorProcessCrashRestoreFromCheckpoint(t *testing.T) {
	firmwarePath, bridgeName := getCloudHypervisorTestConfig(t)

	if utils.PG_DSN == "" {
		t.Skip("PG_DSN environment variable not set")
	}

	// Run migrations
	_, err := migrations.RunMigrations(utils.PG_DSN)
	require.NoError(t, err)

	pool, err := pgxpool.New(context.Background(), utils.PG_DSN)
	require.NoError(t, err)
	defer pool.Close()

	// Build rootfs (IPv6 networking is configured at runtime)
	rootfsPath := buildCloudHypervisorRootfs(t)

	port := portCounter.Add(2)
	callerAddr := fmt.Sprintf("[%s]:%d", cloudhv.IPv6Gateway, port)
	runtimeAddr := fmt.Sprintf("[%s]:%d", cloudhv.IPv6Gateway, port+1)

	ctx := context.Background()

	t.Log("=== Phase 1: Starting hypervisor and spawning job ===")

	h := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  callerAddr,
		RuntimeHTTPAddress: runtimeAddr,
		Pool:               pool,
		WakePollerInterval: 500 * time.Millisecond,
	})
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h.Shutdown(shutdownCtx)
	}()

	chvRuntime, err := cloudhv.NewRuntime()
	require.NoError(t, err)
	defer chvRuntime.Close()

	require.NoError(t, h.RegisterRuntime(chvRuntime))

	// Job definition with retry policy - this is key!
	jd := &hypervisor.JobDefinition{
		Name:        "chv-process-crash-checkpoint-test",
		Version:     "1.0.0",
		RuntimeType: runtime.RuntimeTypeCloudHypervisor,
		Config: &cloudhv.Config{
			FirmwarePath: firmwarePath,
			RootfsPath:   rootfsPath,
			VcpuCount:    1,
			MemSizeMib:   512,
			Network: &cloudhv.NetworkConfig{
				BridgeName: bridgeName,
			},
		},
		RetryPolicy: &hypervisor.RetryPolicy{
			MaxRetries: 3,
			RetryDelay: "100ms",
		},
	}
	require.NoError(t, h.RegisterJobDefinition(ctx, jd))

	// Spawn job that will:
	// 1. Do step 1 (add 1)
	// 2. Checkpoint with keep_running=true
	// 3. Sleep (we'll kill it during this sleep)
	// 4. Do step 2 (double the value) - this runs after restore
	jobID, err := h.Spawn(ctx, hypervisor.SpawnOptions{
		DefinitionName:    "chv-process-crash-checkpoint-test",
		DefinitionVersion: "1.0.0",
		Params: json.RawMessage(`{
			"number": 5,
			"checkpoint_keep_running": true,
			"sleep_after_checkpoint_ms": 2000
		}`),
		Stdout: &testLogWriter{t: t, prefix: "[chv-stdout]"},
		Stderr: &testLogWriter{t: t, prefix: "[chv-stderr]"},
	})
	require.NoError(t, err)
	t.Logf("Spawned job: %s", jobID)

	// Wait for job to checkpoint (it will still be running after checkpoint)
	t.Log("Waiting for job to checkpoint...")
	var checkpointed bool
	for i := 0; i < 120; i++ {
		job, err := h.GetJob(ctx, jobID)
		require.NoError(t, err)

		if job.CheckpointCount > 0 {
			checkpointed = true
			t.Logf("Job has checkpointed (checkpoint_count=%d, state=%s)", job.CheckpointCount, job.State)
			break
		}
		if job.State == hypervisor.JobStateFailed || job.State == hypervisor.JobStateCompleted {
			t.Fatalf("Job reached terminal state before checkpointing: %s", job.State)
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, checkpointed, "Job did not checkpoint")

	// Give the job a moment to start sleeping after checkpoint
	time.Sleep(500 * time.Millisecond)

	// Phase 2: Kill the Cloud Hypervisor process while it's running (after checkpoint)
	t.Log("=== Phase 2: Killing Cloud Hypervisor process while running (after checkpoint) ===")

	// Find and kill the cloud-hypervisor process for this job by its socket path
	// The socket is at /tmp/checker/cloudhv-work/{jobID}/chv.sock
	socketPattern := fmt.Sprintf("cloudhv-work/%s", jobID)
	killCmd := exec.Command("pkill", "-9", "-f", socketPattern)
	killOutput, err := killCmd.CombinedOutput()
	t.Logf("Killed cloud-hypervisor process matching %s: %s (err: %v)", socketPattern, string(killOutput), err)

	// Phase 3: Wait for hypervisor to detect failure, retry from checkpoint, and complete
	t.Log("=== Phase 3: Waiting for retry from checkpoint and completion ===")

	const maxRetries = 3 // Must match the RetryPolicy.MaxRetries in the job definition
	var completed bool
	var finalState hypervisor.JobState
	for i := 0; i < 180; i++ { // Wait up to 90 seconds (Cloud Hypervisor restore can take time)
		job, err := h.GetJob(ctx, jobID)
		require.NoError(t, err)

		finalState = job.State
		// Only consider "done" if completed, or failed with retries exhausted
		if job.State == hypervisor.JobStateCompleted {
			completed = true
			t.Logf("Job finished with state: %s (retry_count=%d)", job.State, job.RetryCount)
			if job.Result != nil && job.Result.Output != nil {
				t.Logf("Job output: %s", string(job.Result.Output))
			}
			if job.Error != "" {
				t.Logf("Job error: %s", job.Error)
			}
			break
		}
		if job.State == hypervisor.JobStateFailed && job.RetryCount >= maxRetries {
			completed = true
			t.Logf("Job failed after exhausting retries: state=%s, retry_count=%d", job.State, job.RetryCount)
			if job.Error != "" {
				t.Logf("Job error: %s", job.Error)
			}
			break
		}
		if i%10 == 0 {
			t.Logf("Job state: %s, retry_count: %d, waiting...", job.State, job.RetryCount)
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, completed, "Job did not complete")
	assert.Equal(t, hypervisor.JobStateCompleted, finalState, "Job should have completed successfully after retry from checkpoint")

	// Verify the result: (5 + 1) * 2 = 12
	job, err := h.GetJob(ctx, jobID)
	require.NoError(t, err)

	require.NotNil(t, job.Result, "Job result should not be nil")
	assert.Equal(t, 0, job.Result.ExitCode, "Exit code should be 0")
	assert.GreaterOrEqual(t, job.RetryCount, 1, "Job should have retried at least once")

	var output struct {
		Result struct {
			Value int `json:"value"`
		} `json:"result"`
		PreCheckpointRuns int `json:"pre_checkpoint_runs"`
	}
	require.NoError(t, json.Unmarshal(job.Result.Output, &output))
	assert.Equal(t, 12, output.Result.Value, "Expected (5 + 1) * 2 = 12")
	// With checkpoint restore, pre-checkpoint code should only run once (not re-run on retry)
	assert.Equal(t, 1, output.PreCheckpointRuns, "Pre-checkpoint code should only run once with checkpoint restore")

	t.Log("=== Cloud Hypervisor process crash restore from checkpoint test PASSED ===")

	// Cleanup
	_, _ = pool.Exec(ctx, "DELETE FROM jobs WHERE id = $1", jobID)
	_, _ = pool.Exec(ctx, "DELETE FROM job_definitions WHERE name = $1", "chv-process-crash-checkpoint-test")
}

// TestCloudHypervisorFullSystemCrashWhileRunning tests recovery when BOTH the hypervisor AND
// the Cloud Hypervisor VM crash simultaneously while the job is running (after checkpoint).
//
// This simulates a catastrophic failure like power loss or OOM kill affecting the whole system.
//
// This test:
// 1. Starts a hypervisor and spawns a job
// 2. Job checkpoints with keep_running=true (VM continues running)
// 3. Kills the Cloud Hypervisor process AND crashes the hypervisor
// 4. Starts a NEW hypervisor and recovers from checkpoint
// 5. Verifies the job completes successfully
func TestCloudHypervisorFullSystemCrashWhileRunning(t *testing.T) {
	firmwarePath, bridgeName := getCloudHypervisorTestConfig(t)

	if utils.PG_DSN == "" {
		t.Skip("PG_DSN environment variable not set")
	}

	// Run migrations
	_, err := migrations.RunMigrations(utils.PG_DSN)
	require.NoError(t, err)

	pool, err := pgxpool.New(context.Background(), utils.PG_DSN)
	require.NoError(t, err)
	defer pool.Close()

	// Build rootfs (IPv6 networking is configured at runtime)
	rootfsPath := buildCloudHypervisorRootfs(t)

	port := portCounter.Add(2)
	callerAddr := fmt.Sprintf("[%s]:%d", cloudhv.IPv6Gateway, port)
	runtimeAddr := fmt.Sprintf("[%s]:%d", cloudhv.IPv6Gateway, port+1)

	ctx := context.Background()

	// Phase 1: Start hypervisor, spawn job, wait for checkpoint
	t.Log("=== Phase 1: Starting hypervisor and spawning job ===")

	h1 := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  callerAddr,
		RuntimeHTTPAddress: runtimeAddr,
		Pool:               pool,
		WakePollerInterval: 500 * time.Millisecond,
	})

	chvRuntime1, err := cloudhv.NewRuntime()
	require.NoError(t, err)
	require.NoError(t, h1.RegisterRuntime(chvRuntime1))

	jd := &hypervisor.JobDefinition{
		Name:        "chv-full-crash-running-test",
		Version:     "1.0.0",
		RuntimeType: runtime.RuntimeTypeCloudHypervisor,
		Config: &cloudhv.Config{
			FirmwarePath: firmwarePath,
			RootfsPath:   rootfsPath,
			VcpuCount:    1,
			MemSizeMib:   512,
			Network: &cloudhv.NetworkConfig{
				BridgeName: bridgeName,
			},
		},
	}
	require.NoError(t, h1.RegisterJobDefinition(ctx, jd))

	// Spawn job with keep_running=true so VM stays running after checkpoint
	jobID, err := h1.Spawn(ctx, hypervisor.SpawnOptions{
		DefinitionName:    "chv-full-crash-running-test",
		DefinitionVersion: "1.0.0",
		Params: json.RawMessage(`{
			"number": 4,
			"checkpoint_keep_running": true,
			"sleep_after_checkpoint_ms": 2000
		}`),
		Stdout: &testLogWriter{t: t, prefix: "[chv-stdout]"},
		Stderr: &testLogWriter{t: t, prefix: "[chv-stderr]"},
	})
	require.NoError(t, err)
	t.Logf("Spawned job: %s", jobID)

	// Wait for job to checkpoint (it will still be running after checkpoint)
	t.Log("Waiting for job to checkpoint...")
	var checkpointed bool
	for i := 0; i < 120; i++ {
		job, err := h1.GetJob(ctx, jobID)
		require.NoError(t, err)

		if job.CheckpointCount > 0 {
			checkpointed = true
			t.Logf("Job has checkpointed (checkpoint_count=%d, state=%s)", job.CheckpointCount, job.State)
			break
		}
		if job.State == hypervisor.JobStateFailed || job.State == hypervisor.JobStateCompleted {
			t.Fatalf("Job reached terminal state before checkpointing: %s", job.State)
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, checkpointed, "Job did not checkpoint")

	// Give the job a moment to start sleeping after checkpoint
	time.Sleep(500 * time.Millisecond)

	// Phase 2: Simulate full system crash - kill BOTH VM and hypervisor
	t.Log("=== Phase 2: Simulating full system crash (VM + hypervisor) ===")

	// Kill the Cloud Hypervisor process
	socketPattern := fmt.Sprintf("cloudhv-work/%s", jobID)
	killCmd := exec.Command("pkill", "-9", "-f", socketPattern)
	killOutput, _ := killCmd.CombinedOutput()
	t.Logf("Killed cloud-hypervisor process: %s", string(killOutput))

	// Crash the hypervisor
	h1.DevCrash()
	chvRuntime1.Close()
	t.Log("Hypervisor crashed (DevCrash called)")

	time.Sleep(100 * time.Millisecond)

	// Phase 3: Start NEW hypervisor and recover
	t.Log("=== Phase 3: Starting new hypervisor and recovering ===")

	h2 := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  callerAddr,
		RuntimeHTTPAddress: runtimeAddr,
		Pool:               pool,
		WakePollerInterval: 500 * time.Millisecond,
	})
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h2.Shutdown(shutdownCtx)
	}()

	chvRuntime2, err := cloudhv.NewRuntime()
	require.NoError(t, err)
	defer chvRuntime2.Close()

	require.NoError(t, h2.RegisterRuntime(chvRuntime2))

	t.Log("Calling RecoverState()...")
	require.NoError(t, h2.RecoverState(ctx))

	// The job was running (not suspended), so RecoverState should detect it needs to be
	// restored from checkpoint. We may need to trigger this by updating the state.
	// Check current state and trigger restore if needed.
	job, err := h2.GetJob(ctx, jobID)
	require.NoError(t, err)
	t.Logf("Job state after recovery: %s", job.State)

	// Wait for job to complete
	t.Log("Waiting for job to complete...")
	var completed bool
	var finalState hypervisor.JobState
	for i := 0; i < 180; i++ {
		job, err := h2.GetJob(ctx, jobID)
		require.NoError(t, err)

		finalState = job.State
		if job.State == hypervisor.JobStateCompleted || job.State == hypervisor.JobStateFailed {
			completed = true
			t.Logf("Job finished with state: %s", job.State)
			if job.Result != nil && job.Result.Output != nil {
				t.Logf("Job output: %s", string(job.Result.Output))
			}
			if job.Error != "" {
				t.Logf("Job error: %s", job.Error)
			}
			break
		}
		if i%10 == 0 {
			t.Logf("Job state: %s, waiting...", job.State)
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, completed, "Job did not complete")
	assert.Equal(t, hypervisor.JobStateCompleted, finalState, "Job should have completed successfully")

	// Verify the result: (4 + 1) * 2 = 10
	job, err = h2.GetJob(ctx, jobID)
	require.NoError(t, err)

	require.NotNil(t, job.Result, "Job result should not be nil")
	assert.Equal(t, 0, job.Result.ExitCode, "Exit code should be 0")

	var output struct {
		Result struct {
			Value int `json:"value"`
		} `json:"result"`
		PreCheckpointRuns int `json:"pre_checkpoint_runs"`
	}
	require.NoError(t, json.Unmarshal(job.Result.Output, &output))
	assert.Equal(t, 10, output.Result.Value, "Expected (4 + 1) * 2 = 10")
	assert.Equal(t, 1, output.PreCheckpointRuns, "Pre-checkpoint code should only run once with checkpoint restore")

	t.Log("=== Cloud Hypervisor full system crash while running test PASSED ===")

	// Cleanup
	_, _ = pool.Exec(ctx, "DELETE FROM jobs WHERE id = $1", jobID)
	_, _ = pool.Exec(ctx, "DELETE FROM job_definitions WHERE name = $1", "chv-full-crash-running-test")
}
