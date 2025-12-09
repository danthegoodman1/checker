//go:build linux

package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog"

	"github.com/danthegoodman1/checker/gologger"
	"github.com/danthegoodman1/checker/runtime"
)

// Runtime implements the runtime.Runtime interface for Podman on Linux.
// Uses CLI execution for simplicity and to avoid heavy Go binding dependencies.
type Runtime struct {
	logger zerolog.Logger
}

// podmanCheckpoint holds the data needed to restore a container on Linux.
// The checkpoint is exported to a portable tar file.
type podmanCheckpoint struct {
	executionID    string
	containerID    string
	exportPath     string // Path to exported checkpoint tar file
	env            map[string]string
	config         *Config
	apiHostAddress string
}

func (c *podmanCheckpoint) String() string {
	return fmt.Sprintf("podman-checkpoint[exec=%s,export=%s]", c.executionID, c.exportPath)
}

func (c *podmanCheckpoint) Path() string {
	return c.exportPath
}

func (c *podmanCheckpoint) GracePeriodMs() int64 {
	return 100
}

// processHandle holds information about a running Podman container.
type processHandle struct {
	executionID    string
	containerID    string
	containerName  string
	env            map[string]string
	config         *Config
	checkpointDir  string
	apiHostAddress string
	logger         zerolog.Logger
	logCancel      context.CancelFunc
}

func (h *processHandle) String() string {
	return fmt.Sprintf("podman[exec=%s,container=%s]", h.executionID, h.containerID)
}

func (h *processHandle) Checkpoint(ctx context.Context, keepRunning bool) (runtime.Checkpoint, error) {
	checkpointName := fmt.Sprintf("checkpoint-%s.tar.gz", h.executionID)
	exportPath := filepath.Join(h.checkpointDir, checkpointName)

	h.logger.Debug().
		Str("checkpoint_name", checkpointName).
		Str("export_path", exportPath).
		Bool("keep_running", keepRunning).
		Msg("creating checkpoint")

	// Ensure checkpoint directory exists
	if err := os.MkdirAll(h.checkpointDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create checkpoint directory: %w", err)
	}

	// Build checkpoint command
	args := []string{"container", "checkpoint",
		"--export", exportPath,
		"--tcp-established",
	}
	if keepRunning {
		args = append(args, "--leave-running")
	}
	args = append(args, h.containerID)

	cmd := exec.CommandContext(ctx, "podman", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to checkpoint container: %w, output: %s", err, string(output))
	}

	h.logger.Debug().
		Str("export_path", exportPath).
		Msg("checkpoint created and exported")

	// If not keeping running, remove the old container so restore can reuse the name/ID
	if !keepRunning {
		// Cancel log streaming first
		if h.logCancel != nil {
			h.logCancel()
		}
		rmCmd := exec.CommandContext(ctx, "podman", "rm", "-f", h.containerID)
		if rmErr := rmCmd.Run(); rmErr != nil {
			h.logger.Warn().Err(rmErr).Msg("failed to remove container after checkpoint")
		} else {
			h.logger.Debug().Msg("removed container after checkpoint")
		}
	}

	return &podmanCheckpoint{
		executionID:    h.executionID,
		containerID:    h.containerID,
		exportPath:     exportPath,
		env:            h.env,
		config:         h.config,
		apiHostAddress: h.apiHostAddress,
	}, nil
}

func (h *processHandle) Kill(ctx context.Context) error {
	h.logger.Debug().Msg("killing container")

	// Cancel log streaming
	if h.logCancel != nil {
		h.logCancel()
	}

	// Stop the container with timeout
	cmd := exec.CommandContext(ctx, "podman", "stop", "-t", "5", h.containerID)
	if err := cmd.Run(); err != nil {
		// If stop fails, force kill
		h.logger.Debug().Err(err).Msg("graceful stop failed, forcing kill")
		killCmd := exec.CommandContext(ctx, "podman", "kill", h.containerID)
		if killErr := killCmd.Run(); killErr != nil {
			return fmt.Errorf("failed to kill container: %w", killErr)
		}
	}
	h.logger.Debug().Msg("container killed")
	return nil
}

func (h *processHandle) Wait(ctx context.Context) (int, error) {
	cmd := exec.CommandContext(ctx, "podman", "wait", h.containerID)
	output, err := cmd.Output()
	if err != nil {
		return -1, fmt.Errorf("failed to wait for container: %w", err)
	}

	var exitCode int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(output)), "%d", &exitCode); err != nil {
		return -1, fmt.Errorf("failed to parse exit code: %w", err)
	}
	return exitCode, nil
}

func (h *processHandle) Cleanup(ctx context.Context) error {
	h.logger.Debug().Msg("removing container")

	// Cancel log streaming
	if h.logCancel != nil {
		h.logCancel()
	}

	cmd := exec.CommandContext(ctx, "podman", "rm", "-f", h.containerID)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}

	h.logger.Debug().Msg("container removed")
	return nil
}

// NewRuntime creates a new Podman runtime for Linux.
func NewRuntime() (*Runtime, error) {
	logger := gologger.NewLogger().With().
		Str("runtime", string(runtime.RuntimeTypePodman)).
		Str("platform", "linux").
		Logger()

	// Verify podman is available
	cmd := exec.Command("podman", "--version")
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("podman not found: %w", err)
	}

	logger.Debug().Msg("podman runtime initialized")

	return &Runtime{logger: logger}, nil
}

func (r *Runtime) Type() runtime.RuntimeType {
	return runtime.RuntimeTypePodman
}

func (r *Runtime) CheckpointGracePeriodMs() int64 {
	return 100
}

func (r *Runtime) ParseConfig(raw []byte) (any, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse podman config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid podman config: %w", err)
	}
	return &cfg, nil
}

func (r *Runtime) Start(ctx context.Context, opts runtime.StartOptions) (runtime.Process, error) {
	cfg, ok := opts.Config.(*Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type: expected *podman.Config, got %T", opts.Config)
	}

	r.logger.Debug().
		Str("execution_id", opts.ExecutionID).
		Str("image", cfg.Image).
		Msg("starting container")

	// Checkpoint directory is always determined by the runtime
	checkpointDir := filepath.Join(os.TempDir(), "checker", "podman-checkpoints", opts.ExecutionID)

	return r.startContainer(ctx, opts.ExecutionID, opts.Env, cfg, checkpointDir, opts.APIHostAddress, opts.Stdout, opts.Stderr)
}

func (r *Runtime) Restore(ctx context.Context, opts runtime.RestoreOptions) (runtime.Process, error) {
	c, ok := opts.Checkpoint.(*podmanCheckpoint)
	if !ok {
		return nil, fmt.Errorf("invalid checkpoint type: expected *podmanCheckpoint, got %T", opts.Checkpoint)
	}

	r.logger.Debug().
		Str("execution_id", c.executionID).
		Str("export_path", c.exportPath).
		Msg("restoring container from checkpoint")

	// Clean up any existing container with the same name before restoring.
	// This handles crash recovery where the old container (stopped after checkpoint) still exists,
	// or normal wake where we want a clean slate.
	containerName := fmt.Sprintf("checker-%s", c.executionID)
	cleanupCmd := exec.CommandContext(ctx, "podman", "rm", "-f", "--ignore", containerName)
	if cleanupOutput, cleanupErr := cleanupCmd.CombinedOutput(); cleanupErr != nil {
		// Log but don't fail - container might not exist
		r.logger.Debug().
			Err(cleanupErr).
			Str("output", string(cleanupOutput)).
			Str("container_name", containerName).
			Msg("cleanup of existing container failed (may not exist)")
	}

	// Restore from exported checkpoint file
	cmd := exec.CommandContext(ctx, "podman", "container", "restore",
		"--import", c.exportPath,
		"--tcp-established",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to restore container from checkpoint: %w, output: %s", err, string(output))
	}

	// The output contains the new container ID
	containerID := strings.TrimSpace(string(output))

	logger := r.logger.With().
		Str("execution_id", c.executionID).
		Str("container_id", containerID).
		Logger()

	logger.Debug().Msg("container restored from checkpoint")

	// Checkpoint directory is always determined by the runtime
	checkpointDir := filepath.Join(os.TempDir(), "checker", "podman-checkpoints", c.executionID)

	// Start log streaming if writers are provided
	var logCancel context.CancelFunc
	if opts.Stdout != nil || opts.Stderr != nil {
		logCancel = r.streamLogs(containerID, opts.Stdout, opts.Stderr, logger)
	}

	return &processHandle{
		executionID:    c.executionID,
		containerID:    containerID,
		containerName:  fmt.Sprintf("checker-%s", c.executionID),
		env:            c.env,
		config:         c.config,
		checkpointDir:  checkpointDir,
		apiHostAddress: c.apiHostAddress,
		logger:         logger,
		logCancel:      logCancel,
	}, nil
}

func (r *Runtime) startContainer(ctx context.Context, executionID string, env map[string]string, cfg *Config, checkpointDir string, apiHostAddress string, stdout, stderr io.Writer) (runtime.Process, error) {
	containerName := fmt.Sprintf("checker-%s", executionID)

	// Build podman run command
	args := []string{"run", "-d", "--name", containerName}

	// Environment variables
	args = append(args, "-e", fmt.Sprintf("CHECKER_API_URL=%s", apiHostAddress))
	for k, v := range env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}
	for k, v := range cfg.Env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}

	// Labels
	args = append(args, "--label", fmt.Sprintf("checker.execution_id=%s", executionID))
	for k, v := range cfg.Labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", k, v))
	}

	// Network mode - default to host for checkpoint/restore compatibility
	if cfg.Network != "" {
		args = append(args, "--network", cfg.Network)
	} else {
		args = append(args, "--network", "host")
	}

	// Volume mounts
	for _, v := range cfg.Volumes {
		args = append(args, "-v", v)
	}

	// Port mappings (only relevant for non-host networking)
	for _, p := range cfg.Ports {
		args = append(args, "-p", p)
	}

	// Resource limits
	if cfg.Memory != "" {
		args = append(args, "--memory", cfg.Memory)
	}
	if cfg.CPUs != "" {
		args = append(args, "--cpus", cfg.CPUs)
	}

	// User
	if cfg.User != "" {
		args = append(args, "--user", cfg.User)
	}

	// Working directory
	if cfg.WorkDir != "" {
		args = append(args, "--workdir", cfg.WorkDir)
	}

	// Image and command
	args = append(args, cfg.Image)
	args = append(args, cfg.Command...)

	// Create and start container
	cmd := exec.CommandContext(ctx, "podman", args...)
	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to start container: %w, output: %s", err, outBuf.String())
	}

	containerID := strings.TrimSpace(outBuf.String())

	logger := r.logger.With().
		Str("execution_id", executionID).
		Str("container_id", containerID).
		Str("container_name", containerName).
		Logger()

	logger.Debug().Msg("container started")

	// Start log streaming if writers are provided
	var logCancel context.CancelFunc
	if stdout != nil || stderr != nil {
		logCancel = r.streamLogs(containerID, stdout, stderr, logger)
	}

	return &processHandle{
		executionID:    executionID,
		containerID:    containerID,
		containerName:  containerName,
		env:            env,
		config:         cfg,
		checkpointDir:  checkpointDir,
		apiHostAddress: apiHostAddress,
		logger:         logger,
		logCancel:      logCancel,
	}, nil
}

// streamLogs fetches container logs and writes them to the provided writers.
// Returns a cancel function to stop log streaming.
func (r *Runtime) streamLogs(containerID string, stdout, stderr io.Writer, logger zerolog.Logger) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		cmd := exec.CommandContext(ctx, "podman", "logs", "-f", containerID)

		// Podman logs outputs everything to stdout by default
		if stdout != nil {
			cmd.Stdout = stdout
		}
		if stderr != nil {
			cmd.Stderr = stderr
		}

		if err := cmd.Run(); err != nil && ctx.Err() == nil {
			logger.Debug().Err(err).Msg("log streaming ended")
		}
	}()

	return cancel
}

// Close is a no-op for CLI-based runtime.
func (r *Runtime) Close() error {
	return nil
}

// ReconstructCheckpoint rebuilds a Checkpoint from persisted data.
func (r *Runtime) ReconstructCheckpoint(checkpointPath string, executionID string, env map[string]string, config any, apiHostAddress string) (runtime.Checkpoint, error) {
	cfg, ok := config.(*Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type: expected *podman.Config, got %T", config)
	}

	return &podmanCheckpoint{
		executionID:    executionID,
		exportPath:     checkpointPath,
		env:            env,
		config:         cfg,
		apiHostAddress: apiHostAddress,
	}, nil
}

// Ensure Runtime implements runtime.Runtime
var _ runtime.Runtime = (*Runtime)(nil)

// Ensure processHandle implements runtime.Process
var _ runtime.Process = (*processHandle)(nil)

// Ensure podmanCheckpoint implements runtime.Checkpoint
var _ runtime.Checkpoint = (*podmanCheckpoint)(nil)
