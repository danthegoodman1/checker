//go:build darwin

package nodejs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/danthegoodman1/checker/runtime"
)

// Runtime implements the runtime.Runtime interface for NodeJS on macOS.
// Since macOS doesn't support CRIU, checkpoint/restore operations are no-ops.
// This allows local development while the actual checkpointing works on Linux.
type Runtime struct{}

// checkpoint holds the data needed to restore a process on macOS.
// Since CRIU isn't available, this just stores enough to fake a restore.
type checkpoint struct {
	executionID    string
	env            map[string]string
	config         *Config
	apiHostAddress string
}

func (c *checkpoint) String() string {
	return fmt.Sprintf("nodejs-darwin-checkpoint[exec=%s]", c.executionID)
}

// processHandle holds information about a running NodeJS process on macOS.
type processHandle struct {
	executionID    string
	env            map[string]string
	cmd            *exec.Cmd
	config         *Config
	apiHostAddress string
}

func (h *processHandle) String() string {
	if h.cmd != nil && h.cmd.Process != nil {
		return fmt.Sprintf("nodejs-darwin[exec=%s,pid=%d]", h.executionID, h.cmd.Process.Pid)
	}
	return fmt.Sprintf("nodejs-darwin[exec=%s,pid=none]", h.executionID)
}

// On macOS, this is a no-op since CRIU is not available.
func (h *processHandle) Checkpoint(ctx context.Context, keepRunning bool) (runtime.Checkpoint, error) {
	// Log that we're skipping checkpoint on macOS
	fmt.Printf("[darwin] checkpoint requested for %s (keepRunning=%v) - no-op (CRIU not available on macOS)\n", h.executionID, keepRunning)

	return &checkpoint{
		executionID:    h.executionID,
		env:            h.env,
		config:         h.config,
		apiHostAddress: h.apiHostAddress,
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

// NewRuntime creates a new NodeJS runtime for macOS.
func NewRuntime() *Runtime {
	return &Runtime{}
}

func (r *Runtime) Type() runtime.RuntimeType {
	return runtime.RuntimeTypeNodeJS
}

func (r *Runtime) ParseConfig(raw []byte) (any, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse nodejs config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid nodejs config: %w", err)
	}
	return &cfg, nil
}

func (r *Runtime) Start(ctx context.Context, opts runtime.StartOptions) (runtime.Process, error) {
	cfg, ok := opts.Config.(*Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type: expected *nodejs.Config, got %T", opts.Config)
	}

	return r.startProcess(ctx, opts.ExecutionID, opts.Env, cfg, opts.APIHostAddress)
}

// On macOS, this just restarts the process since CRIU is not available.
func (r *Runtime) Restore(ctx context.Context, chk runtime.Checkpoint) (runtime.Process, error) {
	c, ok := chk.(*checkpoint)
	if !ok {
		return nil, fmt.Errorf("invalid checkpoint type: expected *checkpoint, got %T", chk)
	}

	fmt.Printf("[darwin] restore requested for %s - no-op (restarting process, CRIU not available on macOS)\n", c.executionID)

	return r.startProcess(ctx, c.executionID, c.env, c.config, c.apiHostAddress)
}

func (r *Runtime) startProcess(ctx context.Context, executionID string, env map[string]string, cfg *Config, apiHostAddress string) (runtime.Process, error) {
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

	// Set up environment
	// For nodejs running directly on host, the API URL is the same as the host address
	cmd.Env = append(cmd.Env, fmt.Sprintf("CHECKER_API_URL=%s", apiHostAddress))
	for k, v := range env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start node process: %w", err)
	}

	handle := &processHandle{
		executionID:    executionID,
		env:            env,
		cmd:            cmd,
		config:         cfg,
		apiHostAddress: apiHostAddress,
	}

	return handle, nil
}

// Ensure Runtime implements runtime.Runtime
var _ runtime.Runtime = (*Runtime)(nil)

// Ensure processHandle implements runtime.Process
var _ runtime.Process = (*processHandle)(nil)

// Ensure checkpoint implements runtime.Checkpoint
var _ runtime.Checkpoint = (*checkpoint)(nil)
