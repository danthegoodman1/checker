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
	"github.com/danthegoodman1/checker/runtime/firecracker"
	"github.com/danthegoodman1/checker/utils"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Environment variables for Firecracker tests:
// - FC_KERNEL_PATH: Path to the kernel image (required)
//   Download with: curl -fLo vmlinux-5.10.bin "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/x86_64/vmlinux-5.10.223"
// - FC_BRIDGE_NAME: Name of the bridge for TAP networking (default: fcbr0)
// - PG_DSN: PostgreSQL connection string (required for hypervisor tests)
//
// Before running tests, set up the network bridge:
//   sudo ip link add fcbr0 type bridge
//   sudo ip addr add 172.16.0.1/24 dev fcbr0
//   sudo ip link set fcbr0 up
//   echo 1 | sudo tee /proc/sys/net/ipv4/ip_forward
//   sudo iptables -t nat -A POSTROUTING -s 172.16.0.0/24 ! -o fcbr0 -j MASQUERADE
//
// Run with: FC_KERNEL_PATH=/path/to/vmlinux PG_DSN=postgres://... go test -v -run TestFirecracker

func getFirecrackerTestConfig(t *testing.T) (kernelPath, bridgeName string) {
	t.Helper()

	if goruntime.GOOS != "linux" {
		t.Skip("Firecracker requires Linux")
	}

	kernelPath = os.Getenv("FC_KERNEL_PATH")
	if kernelPath == "" {
		t.Skip("FC_KERNEL_PATH not set. Download kernel with: curl -fLo vmlinux-5.10.bin 'https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/x86_64/vmlinux-5.10.223'")
	}

	if _, err := exec.LookPath("firecracker"); err != nil {
		t.Skip("firecracker not found in PATH")
	}

	bridgeName = os.Getenv("FC_BRIDGE_NAME")
	if bridgeName == "" {
		bridgeName = "fcbr0"
	}

	// Verify bridge exists
	cmd := exec.Command("ip", "link", "show", bridgeName)
	if err := cmd.Run(); err != nil {
		t.Skipf("Bridge %s not found. Set up with: sudo ip link add %s type bridge && sudo ip addr add 172.16.0.1/24 dev %s && sudo ip link set %s up",
			bridgeName, bridgeName, bridgeName, bridgeName)
	}

	return kernelPath, bridgeName
}

// buildFirecrackerRootfs builds a rootfs ext4 image from the checkpoint_restore Dockerfile.
// Returns the path to the rootfs file.
func buildFirecrackerRootfs(t *testing.T, guestIP, gateway string) string {
	t.Helper()

	// Check dependencies
	for _, cmd := range []string{"buildah", "skopeo", "umoci", "mkfs.ext4", "jq"} {
		if _, err := exec.LookPath(cmd); err != nil {
			t.Skipf("%s not found, skipping test", cmd)
		}
	}

	cwd, err := os.Getwd()
	require.NoError(t, err)

	rootfsPath := filepath.Join(t.TempDir(), "rootfs.ext4")

	// Use the build-fc-rootfs.sh script
	scriptPath := filepath.Join(cwd, "scripts", "build-fc-rootfs.sh")
	dockerfilePath := filepath.Join(cwd, "demo", "Dockerfile.checkpoint_restore")
	networkConfig := fmt.Sprintf("%s,%s", guestIP, gateway)

	cmd := exec.Command(scriptPath, dockerfilePath, rootfsPath, networkConfig)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to build rootfs: %v\n%s", err, output)
	}
	t.Logf("Built rootfs: %s", rootfsPath)

	return rootfsPath
}

// TestFirecrackerHypervisorIntegration tests the full hypervisor flow with Firecracker:
// spawn job -> checkpoint -> restore -> complete
func TestFirecrackerHypervisorIntegration(t *testing.T) {
	kernelPath, bridgeName := getFirecrackerTestConfig(t)

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

	// Build rootfs with networking configured
	guestIP := "172.16.0.2/24"
	gateway := "172.16.0.1"
	rootfsPath := buildFirecrackerRootfs(t, guestIP, gateway)

	// Get unique ports for this test
	port := portCounter.Add(2)
	callerAddr := fmt.Sprintf("172.16.0.1:%d", port)    // Use bridge IP so VM can reach it
	runtimeAddr := fmt.Sprintf("172.16.0.1:%d", port+1) // Use bridge IP

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

	fcRuntime, err := firecracker.NewRuntime()
	require.NoError(t, err)
	defer fcRuntime.Close()

	require.NoError(t, h.RegisterRuntime(fcRuntime))

	// Register job definition
	jd := &hypervisor.JobDefinition{
		Name:        "fc-integration-test",
		Version:     "1.0.0",
		RuntimeType: runtime.RuntimeTypeFirecracker,
		Config: &firecracker.Config{
			KernelPath: kernelPath,
			RootfsPath: rootfsPath,
			VcpuCount:  1,
			MemSizeMib: 512,
			Network: &firecracker.NetworkConfig{
				BridgeName:   bridgeName,
				GuestIP:      guestIP,
				GuestGateway: gateway,
			},
		},
	}
	require.NoError(t, h.RegisterJobDefinition(ctx, jd))

	// Spawn job
	t.Log("=== Spawning job ===")
	jobID, err := h.Spawn(ctx, hypervisor.SpawnOptions{
		DefinitionName:    "fc-integration-test",
		DefinitionVersion: "1.0.0",
		Params:            json.RawMessage(`{"number": 5, "suspend_duration": "1s"}`),
		Stdout:            &testLogWriter{t: t, prefix: "[fc-stdout]"},
		Stderr:            &testLogWriter{t: t, prefix: "[fc-stderr]"},
	})
	require.NoError(t, err)
	t.Logf("Spawned job: %s", jobID)

	// Wait for job to be suspended
	t.Log("Waiting for job to be suspended...")
	var suspended bool
	for i := 0; i < 120; i++ { // Wait up to 60 seconds (Firecracker boot can be slower)
		var dbJob query.Job
		err := query.ReliableExec(ctx, pool, pg.StandardContextTimeout, func(ctx context.Context, q *query.Queries) error {
			var err error
			dbJob, err = q.GetJob(ctx, jobID)
			return err
		})
		require.NoError(t, err)

		if dbJob.State == query.JobStateSuspended {
			suspended = true
			t.Logf("Job is now suspended (checkpoint_count=%d)", dbJob.CheckpointCount)
			break
		}
		if dbJob.State == query.JobStateFailed {
			t.Fatalf("Job failed with error: %s", dbJob.Error.String)
		}
		if i%10 == 0 {
			t.Logf("Job state: %s, waiting...", dbJob.State)
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, suspended, "Job did not reach suspended state")

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
		if i%10 == 0 {
			t.Logf("Job state: %s, waiting...", dbJob.State)
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, completed, "Job did not complete")
	assert.Equal(t, query.JobStateCompleted, finalState, "Job should have completed successfully")

	// Verify result
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
	assert.Equal(t, 1, output.PreCheckpointRuns, "Pre-checkpoint code should only run once")

	t.Log("=== Firecracker hypervisor integration test PASSED ===")

	// Cleanup
	_, _ = pool.Exec(ctx, "DELETE FROM jobs WHERE id = $1", jobID)
	_, _ = pool.Exec(ctx, "DELETE FROM job_definitions WHERE name = $1", "fc-integration-test")
}

// TestFirecrackerCrashRecovery tests that Firecracker jobs can be recovered after hypervisor crash.
func TestFirecrackerCrashRecovery(t *testing.T) {
	kernelPath, bridgeName := getFirecrackerTestConfig(t)

	if utils.PG_DSN == "" {
		t.Skip("PG_DSN environment variable not set")
	}

	// Run migrations
	_, err := migrations.RunMigrations(utils.PG_DSN)
	require.NoError(t, err)

	pool, err := pgxpool.New(context.Background(), utils.PG_DSN)
	require.NoError(t, err)
	defer pool.Close()

	// Build rootfs
	guestIP := "172.16.0.3/24" // Different IP to avoid conflicts
	gateway := "172.16.0.1"
	rootfsPath := buildFirecrackerRootfs(t, guestIP, gateway)

	port := portCounter.Add(2)
	callerAddr := fmt.Sprintf("172.16.0.1:%d", port)
	runtimeAddr := fmt.Sprintf("172.16.0.1:%d", port+1)

	ctx := context.Background()

	// Phase 1: Start hypervisor, spawn job, wait for suspend, then "crash"
	t.Log("=== Phase 1: Starting hypervisor and spawning job ===")

	h1 := hypervisor.New(hypervisor.Config{
		CallerHTTPAddress:  callerAddr,
		RuntimeHTTPAddress: runtimeAddr,
		Pool:               pool,
		WakePollerInterval: 500 * time.Millisecond,
	})

	fcRuntime1, err := firecracker.NewRuntime()
	require.NoError(t, err)
	require.NoError(t, h1.RegisterRuntime(fcRuntime1))

	jd := &hypervisor.JobDefinition{
		Name:        "fc-crash-recovery-test",
		Version:     "1.0.0",
		RuntimeType: runtime.RuntimeTypeFirecracker,
		Config: &firecracker.Config{
			KernelPath: kernelPath,
			RootfsPath: rootfsPath,
			VcpuCount:  1,
			MemSizeMib: 512,
			Network: &firecracker.NetworkConfig{
				BridgeName:   bridgeName,
				GuestIP:      guestIP,
				GuestGateway: gateway,
			},
		},
	}
	require.NoError(t, h1.RegisterJobDefinition(ctx, jd))

	jobID, err := h1.Spawn(ctx, hypervisor.SpawnOptions{
		DefinitionName:    "fc-crash-recovery-test",
		DefinitionVersion: "1.0.0",
		Params:            json.RawMessage(`{"number": 7, "suspend_duration": "5s"}`),
		Stdout:            &testLogWriter{t: t, prefix: "[fc-stdout]"},
		Stderr:            &testLogWriter{t: t, prefix: "[fc-stderr]"},
	})
	require.NoError(t, err)
	t.Logf("Spawned job: %s", jobID)

	// Wait for job to be suspended
	t.Log("Waiting for job to be suspended...")
	var suspended bool
	for i := 0; i < 120; i++ {
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
		if dbJob.State == query.JobStateFailed {
			t.Fatalf("Job failed with error: %s", dbJob.Error.String)
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, suspended, "Job did not reach suspended state")

	// Simulate crash
	t.Log("=== Phase 2: Simulating crash (DevCrash) ===")
	h1.DevCrash()
	fcRuntime1.Close()
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

	fcRuntime2, err := firecracker.NewRuntime()
	require.NoError(t, err)
	defer fcRuntime2.Close()

	require.NoError(t, h2.RegisterRuntime(fcRuntime2))

	t.Log("Calling RecoverState()...")
	require.NoError(t, h2.RecoverState(ctx))

	// Update suspend_until to trigger immediate wake
	t.Log("Updating suspend_until to trigger immediate wake...")
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
		if i%10 == 0 {
			t.Logf("Job state: %s, waiting...", dbJob.State)
		}
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

	assert.Equal(t, int32(0), dbJob.ResultExitCode.Int32, "Exit code should be 0")

	var output struct {
		Result struct {
			Value int `json:"value"`
		} `json:"result"`
		PreCheckpointRuns int `json:"pre_checkpoint_runs"`
	}
	require.NoError(t, json.Unmarshal(dbJob.ResultOutput, &output))
	assert.Equal(t, 16, output.Result.Value, "Expected (7 + 1) * 2 = 16")
	assert.Equal(t, 1, output.PreCheckpointRuns, "Pre-checkpoint code should only run once")

	t.Log("=== Firecracker crash recovery test PASSED ===")

	// Cleanup
	_, _ = pool.Exec(ctx, "DELETE FROM jobs WHERE id = $1", jobID)
	_, _ = pool.Exec(ctx, "DELETE FROM job_definitions WHERE name = $1", "fc-crash-recovery-test")
}

// // TestFirecrackerConfigParsing tests that config parsing and validation works correctly.
// func TestFirecrackerConfigParsing(t *testing.T) {
// 	if goruntime.GOOS != "linux" {
// 		t.Skip("Firecracker requires Linux")
// 	}

// 	rt := &firecracker.Runtime{}

// 	// Test valid config with network
// 	validJSON := `{
// 		"kernel_path": "/path/to/kernel",
// 		"rootfs_path": "/path/to/rootfs.ext4",
// 		"vcpu_count": 2,
// 		"mem_size_mib": 1024,
// 		"network": {
// 			"bridge_name": "fcbr0",
// 			"guest_ip": "172.16.0.2/24",
// 			"guest_gateway": "172.16.0.1"
// 		}
// 	}`

// 	cfg, err := rt.ParseConfig([]byte(validJSON))
// 	require.NoError(t, err)

// 	fcCfg, ok := cfg.(*firecracker.Config)
// 	require.True(t, ok)
// 	assert.Equal(t, "/path/to/kernel", fcCfg.KernelPath)
// 	assert.Equal(t, "/path/to/rootfs.ext4", fcCfg.RootfsPath)
// 	assert.Equal(t, 2, fcCfg.VcpuCount)
// 	assert.Equal(t, 1024, fcCfg.MemSizeMib)
// 	require.NotNil(t, fcCfg.Network)
// 	assert.Equal(t, "fcbr0", fcCfg.Network.BridgeName)
// 	assert.Equal(t, "172.16.0.2/24", fcCfg.Network.GuestIP)
// 	assert.Equal(t, "172.16.0.1", fcCfg.Network.GuestGateway)

// 	// Test config without network (should fail validation)
// 	noNetworkJSON := `{
// 		"kernel_path": "/path/to/kernel",
// 		"rootfs_path": "/path/to/rootfs.ext4"
// 	}`
// 	_, err = rt.ParseConfig([]byte(noNetworkJSON))
// 	assert.Error(t, err, "Should fail validation without network config")

// 	// Test config with incomplete network (should fail validation)
// 	incompleteNetworkJSON := `{
// 		"kernel_path": "/path/to/kernel",
// 		"rootfs_path": "/path/to/rootfs.ext4",
// 		"network": {
// 			"bridge_name": "fcbr0"
// 		}
// 	}`
// 	_, err = rt.ParseConfig([]byte(incompleteNetworkJSON))
// 	assert.Error(t, err, "Should fail validation with incomplete network config")

// 	t.Log("=== Firecracker config parsing test PASSED ===")
// }

// // TestFirecrackerRuntimeType tests that the runtime type is correct.
// func TestFirecrackerRuntimeType(t *testing.T) {
// 	if goruntime.GOOS != "linux" {
// 		t.Skip("Firecracker requires Linux")
// 	}

// 	// Skip if firecracker not available
// 	if _, err := exec.LookPath("firecracker"); err != nil {
// 		t.Skip("firecracker not found in PATH")
// 	}

// 	rt, err := firecracker.NewRuntime()
// 	require.NoError(t, err)
// 	defer rt.Close()

// 	assert.Equal(t, "firecracker", string(rt.Type()))
// 	assert.Equal(t, int64(0), rt.CheckpointGracePeriodMs())

// 	t.Log("=== Firecracker runtime type test PASSED ===")
// }
