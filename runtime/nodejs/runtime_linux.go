//go:build linux

package nodejs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/danthegoodman1/checker/runtime"
)

// Runtime implements the runtime.Runtime interface for NodeJS on Linux.
// Uses CRIU for checkpoint/restore functionality.
type Runtime struct {
	// CRIUPath is the path to the criu binary. Defaults to "criu" in PATH.
	CRIUPath string
}

// checkpoint holds the data needed to restore a process on Linux.
type checkpoint struct {
	executionID   string
	env           map[string]string
	checkpointDir string
	config        *Config
}

func (c *checkpoint) String() string {
	return fmt.Sprintf("nodejs-linux-checkpoint[exec=%s,dir=%s]", c.executionID, c.checkpointDir)
}

// processHandle holds information about a running NodeJS process on Linux.
type processHandle struct {
	executionID   string
	env           map[string]string
	pid           int
	cmd           *exec.Cmd
	config        *Config
	checkpointDir string
}

func (h *processHandle) String() string {
	return fmt.Sprintf("nodejs-linux[exec=%s,pid=%d]", h.executionID, h.pid)
}

// Uses CRIU to checkpoint the NodeJS process.
func (h *processHandle) Checkpoint(ctx context.Context, keepRunning bool) (runtime.Checkpoint, error) {
	// TODO: Implement actual CRIU checkpoint
	// For now, this is a stub that logs the operation

	/*
		Example CRIU checkpoint command:
		criu dump -t <pid> -D <checkpoint_dir> --shell-job --leave-running

		Options:
		- -t <pid>: Target process ID
		- -D <dir>: Directory to save checkpoint images
		- --shell-job: Allow checkpointing of processes that are session leaders
		- --leave-running: Don't kill the process after checkpointing (only if keepRunning=true)

		For a full suspend (keepRunning=false):
		criu dump -t <pid> -D <checkpoint_dir> --shell-job

		For checkpoint without suspend (keepRunning=true):
		criu dump -t <pid> -D <checkpoint_dir> --shell-job --leave-running
	*/

	fmt.Printf("[linux] checkpoint requested for %s (pid=%d, keepRunning=%v) - STUB: would use CRIU to dump to %s\n",
		h.executionID, h.pid, keepRunning, h.checkpointDir)

	return &checkpoint{
		executionID:   h.executionID,
		env:           h.env,
		checkpointDir: h.checkpointDir,
		config:        h.config,
	}, nil
}

func (h *processHandle) Kill(ctx context.Context) error {
	if h.cmd == nil || h.cmd.Process == nil {
		return fmt.Errorf("process not running")
	}

	// Send SIGTERM first for graceful shutdown
	if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		// If SIGTERM fails, try SIGKILL
		if killErr := h.cmd.Process.Kill(); killErr != nil {
			return fmt.Errorf("failed to kill process: %w", killErr)
		}
	}

	return nil
}

func (h *processHandle) Wait(ctx context.Context) (int, error) {
	if h.cmd == nil {
		return -1, fmt.Errorf("no command to wait on")
	}

	// Wait for the process to exit
	err := h.cmd.Wait()

	if err != nil {
		// Try to get the exit code from the error
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return -1, fmt.Errorf("failed to wait for process: %w", err)
	}

	return 0, nil
}

// NewRuntime creates a new NodeJS runtime for Linux.
func NewRuntime() *Runtime {
	return &Runtime{
		CRIUPath: "criu",
	}
}

func (r *Runtime) Type() runtime.RuntimeType {
	return runtime.RuntimeTypeNodeJS
}

func (r *Runtime) Start(ctx context.Context, executionID string, env map[string]string, config any) (runtime.Process, error) {
	cfg, ok := config.(*Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type: expected *nodejs.Config, got %T", config)
	}

	checkpointDir := cfg.CheckpointDir
	if checkpointDir == "" {
		checkpointDir = filepath.Join(os.TempDir(), "checker", "checkpoints", executionID)
	}

	// Ensure checkpoint directory exists
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create checkpoint directory: %w", err)
	}

	return r.startProcess(ctx, executionID, env, cfg, checkpointDir)
}

// Uses CRIU to restore the NodeJS process from a checkpoint.
func (r *Runtime) Restore(ctx context.Context, chk runtime.Checkpoint) (runtime.Process, error) {
	c, ok := chk.(*checkpoint)
	if !ok {
		return nil, fmt.Errorf("invalid checkpoint type: expected *checkpoint, got %T", chk)
	}

	// TODO: Implement actual CRIU restore
	// For now, this is a stub that logs the operation

	/*
		Example CRIU restore command:
		criu restore -D <checkpoint_dir> --shell-job

		Options:
		- -D <dir>: Directory containing checkpoint images
		- --shell-job: Restore process as session leader

		The restored process will have the same PID if possible,
		otherwise CRIU will fail. May need --pidns or other options.
	*/

	fmt.Printf("[linux] restore requested for %s from %s - STUB: would use CRIU to restore\n",
		c.executionID, c.checkpointDir)

	// TODO: Actually restore with CRIU and get the new PID
	// For now, just restart the process as a stub
	return r.startProcess(ctx, c.executionID, c.env, c.config, c.checkpointDir)
}

func (r *Runtime) startProcess(ctx context.Context, executionID string, env map[string]string, cfg *Config, checkpointDir string) (runtime.Process, error) {
	nodePath := cfg.NodePath
	if nodePath == "" {
		nodePath = "node"
	}

	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = filepath.Dir(cfg.EntryPoint)
	}

	// Build command arguments
	args := []string{cfg.EntryPoint}
	args = append(args, cfg.Args...)

	cmd := exec.CommandContext(ctx, nodePath, args...)
	cmd.Dir = workDir
	cmd.Stdout = os.Stdout // TODO: capture output properly
	cmd.Stderr = os.Stderr

	// Set up environment from hypervisor
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	// Start in a new process group for CRIU compatibility
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start node process: %w", err)
	}

	handle := &processHandle{
		executionID:   executionID,
		env:           env,
		pid:           cmd.Process.Pid,
		cmd:           cmd,
		config:        cfg,
		checkpointDir: checkpointDir,
	}

	return handle, nil
}

// Ensure Runtime implements runtime.Runtime
var _ runtime.Runtime = (*Runtime)(nil)

// Ensure processHandle implements runtime.Process
var _ runtime.Process = (*processHandle)(nil)

// Ensure checkpoint implements runtime.Checkpoint
var _ runtime.Checkpoint = (*checkpoint)(nil)
