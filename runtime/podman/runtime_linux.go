//go:build linux

package podman

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/specgen"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/rs/zerolog"

	"github.com/danthegoodman1/checker/gologger"
	"github.com/danthegoodman1/checker/runtime"
)

// Runtime implements the runtime.Runtime interface for Podman on Linux.
// Uses Podman's checkpoint/restore with exported tar files for portable checkpoints.
type Runtime struct {
	conn   context.Context
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
	conn           context.Context
	apiHostAddress string
	logger         zerolog.Logger
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

	// Create checkpoint with export using the builder pattern
	opts := new(containers.CheckpointOptions).
		WithExport(exportPath).
		WithTCPEstablished(true).
		WithLeaveRunning(keepRunning)

	// Create checkpoint with export
	_, err := containers.Checkpoint(h.conn, h.containerID, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to checkpoint container: %w", err)
	}

	h.logger.Debug().
		Str("export_path", exportPath).
		Msg("checkpoint created and exported")

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

	// Stop the container with timeout
	opts := new(containers.StopOptions).WithTimeout(5)
	err := containers.Stop(h.conn, h.containerID, opts)
	if err != nil {
		// If stop fails, force kill
		h.logger.Debug().Err(err).Msg("graceful stop failed, forcing kill")
		if killErr := containers.Kill(h.conn, h.containerID, nil); killErr != nil {
			return fmt.Errorf("failed to kill container: %w", killErr)
		}
	}
	h.logger.Debug().Msg("container killed")
	return nil
}

func (h *processHandle) Wait(ctx context.Context) (int, error) {
	exitCode, err := containers.Wait(h.conn, h.containerID, nil)
	if err != nil {
		return -1, fmt.Errorf("failed to wait for container: %w", err)
	}
	return int(exitCode), nil
}

func (h *processHandle) Cleanup(ctx context.Context) error {
	h.logger.Debug().Msg("removing container")

	opts := new(containers.RemoveOptions).WithForce(true)
	_, err := containers.Remove(h.conn, h.containerID, opts)
	if err != nil {
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

	// Connect to Podman socket
	// Try rootless socket first, then root socket
	socketPath := getSocketPath()

	conn, err := bindings.NewConnection(context.Background(), socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to podman socket at %s: %w", socketPath, err)
	}

	logger.Debug().Str("socket", socketPath).Msg("podman runtime initialized")

	return &Runtime{conn: conn, logger: logger}, nil
}

// getSocketPath returns the appropriate Podman socket path.
func getSocketPath() string {
	// Check for explicit socket path in environment
	if socketPath := os.Getenv("PODMAN_SOCKET"); socketPath != "" {
		return "unix://" + socketPath
	}

	// Try user socket first (rootless)
	uid := os.Getuid()
	if uid != 0 {
		userSocket := fmt.Sprintf("/run/user/%d/podman/podman.sock", uid)
		if _, err := os.Stat(userSocket); err == nil {
			return "unix://" + userSocket
		}
	}

	// Fall back to root socket
	return "unix:///run/podman/podman.sock"
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

	checkpointDir := cfg.CheckpointDir
	if checkpointDir == "" {
		checkpointDir = filepath.Join(os.TempDir(), "checker", "podman-checkpoints", opts.ExecutionID)
	}

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

	// Restore from exported checkpoint file
	// This creates a new container from the checkpoint
	restoreOpts := new(containers.RestoreOptions).
		WithImportArchive(c.exportPath).
		WithTCPEstablished(true)

	// When importing, we use an empty string for the container name
	// since we're creating a new container from the archive
	report, err := containers.Restore(r.conn, "", restoreOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to restore container from checkpoint: %w", err)
	}

	// The restored container has a new ID
	containerID := report.Id

	logger := r.logger.With().
		Str("execution_id", c.executionID).
		Str("container_id", containerID).
		Logger()

	logger.Debug().Msg("container restored from checkpoint")

	// Get checkpoint directory from config
	checkpointDir := c.config.CheckpointDir
	if checkpointDir == "" {
		checkpointDir = filepath.Join(os.TempDir(), "checker", "podman-checkpoints", c.executionID)
	}

	// Start log streaming if writers are provided
	if opts.Stdout != nil || opts.Stderr != nil {
		go r.streamLogs(containerID, opts.Stdout, opts.Stderr, logger)
	}

	return &processHandle{
		executionID:    c.executionID,
		containerID:    containerID,
		containerName:  fmt.Sprintf("checker-%s", c.executionID),
		env:            c.env,
		config:         c.config,
		checkpointDir:  checkpointDir,
		conn:           r.conn,
		apiHostAddress: c.apiHostAddress,
		logger:         logger,
	}, nil
}

func (r *Runtime) startContainer(ctx context.Context, executionID string, env map[string]string, cfg *Config, checkpointDir string, apiHostAddress string, stdout, stderr io.Writer) (runtime.Process, error) {
	containerName := fmt.Sprintf("checker-%s", executionID)

	// Build environment variables
	// With host networking, localhost works directly
	envList := make(map[string]string)
	envList["CHECKER_API_URL"] = apiHostAddress
	for k, v := range env {
		envList[k] = v
	}
	for k, v := range cfg.Env {
		envList[k] = v
	}

	// Build labels
	labels := map[string]string{
		"checker.execution_id": executionID,
	}
	for k, v := range cfg.Labels {
		labels[k] = v
	}

	// Create spec generator
	s := specgen.NewSpecGenerator(cfg.Image, false)
	s.Name = containerName
	s.Command = cfg.Command
	s.WorkDir = cfg.WorkDir
	s.Env = envList
	s.Labels = labels

	// Network mode - default to host for checkpoint/restore compatibility
	if cfg.Network != "" {
		s.NetNS = specgen.Namespace{NSMode: specgen.NamespaceMode(cfg.Network)}
	} else {
		s.NetNS = specgen.Namespace{NSMode: specgen.Host}
	}

	// Volume mounts
	if len(cfg.Volumes) > 0 {
		s.Mounts = make([]spec.Mount, 0, len(cfg.Volumes))
		for _, v := range cfg.Volumes {
			// Parse "host:container[:ro]" format
			mount, err := parseVolumeMount(v)
			if err != nil {
				return nil, fmt.Errorf("invalid volume %q: %w", v, err)
			}
			s.Mounts = append(s.Mounts, mount)
		}
	}

	// Resource limits
	if cfg.Memory != "" || cfg.CPUs != "" {
		s.ResourceLimits = &spec.LinuxResources{}

		if cfg.Memory != "" {
			memBytes, err := parseMemoryString(cfg.Memory)
			if err != nil {
				return nil, fmt.Errorf("invalid memory limit %q: %w", cfg.Memory, err)
			}
			s.ResourceLimits.Memory = &spec.LinuxMemory{
				Limit: &memBytes,
			}
		}

		if cfg.CPUs != "" {
			var cpus float64
			if _, err := fmt.Sscanf(cfg.CPUs, "%f", &cpus); err != nil {
				return nil, fmt.Errorf("invalid CPU limit %q: %w", cfg.CPUs, err)
			}
			period := uint64(100000)
			quota := int64(cpus * float64(period))
			s.ResourceLimits.CPU = &spec.LinuxCPU{
				Period: &period,
				Quota:  &quota,
			}
		}
	}

	// User
	if cfg.User != "" {
		s.User = cfg.User
	}

	// Create container
	createResponse, err := containers.CreateWithSpec(r.conn, s, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create container: %w", err)
	}

	containerID := createResponse.ID

	logger := r.logger.With().
		Str("execution_id", executionID).
		Str("container_id", containerID).
		Str("container_name", containerName).
		Logger()

	logger.Debug().Msg("container created")

	// Start container
	if err := containers.Start(r.conn, containerID, nil); err != nil {
		// Clean up on failure
		opts := new(containers.RemoveOptions).WithForce(true)
		containers.Remove(r.conn, containerID, opts)
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	logger.Debug().Msg("container started")

	// Start log streaming if writers are provided
	if stdout != nil || stderr != nil {
		go r.streamLogs(containerID, stdout, stderr, logger)
	}

	return &processHandle{
		executionID:    executionID,
		containerID:    containerID,
		containerName:  containerName,
		env:            env,
		config:         cfg,
		checkpointDir:  checkpointDir,
		conn:           r.conn,
		apiHostAddress: apiHostAddress,
		logger:         logger,
	}, nil
}

// streamLogs fetches container logs and writes them to the provided writers.
func (r *Runtime) streamLogs(containerID string, stdout, stderr io.Writer, logger zerolog.Logger) {
	// Podman log streaming
	follow := true
	stdoutFlag := stdout != nil
	stderrFlag := stderr != nil

	opts := new(containers.LogOptions).
		WithFollow(follow).
		WithStdout(stdoutFlag).
		WithStderr(stderrFlag)

	// Use io.Discard for nil writers
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}

	stdoutChan := make(chan string, 100)
	stderrChan := make(chan string, 100)

	go func() {
		for line := range stdoutChan {
			io.WriteString(stdout, line+"\n")
		}
	}()

	go func() {
		for line := range stderrChan {
			io.WriteString(stderr, line+"\n")
		}
	}()

	err := containers.Logs(r.conn, containerID, opts, stdoutChan, stderrChan)
	if err != nil {
		logger.Debug().Err(err).Msg("log streaming ended")
	}
}

// parseVolumeMount parses a volume string in "host:container[:ro]" format.
func parseVolumeMount(volume string) (spec.Mount, error) {
	// Simple colon split
	colonParts := splitVolume(volume)
	if len(colonParts) < 2 {
		return spec.Mount{}, fmt.Errorf("invalid format, expected host:container[:ro]")
	}

	host := colonParts[0]
	container := colonParts[1]
	var readOnly bool
	if len(colonParts) > 2 && colonParts[2] == "ro" {
		readOnly = true
	}

	opts := []string{"rbind"}
	if readOnly {
		opts = append(opts, "ro")
	}

	return spec.Mount{
		Type:        "bind",
		Source:      host,
		Destination: container,
		Options:     opts,
	}, nil
}

// splitVolume splits a volume string by colon, handling Windows paths.
func splitVolume(s string) []string {
	var result []string
	var current string

	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			result = append(result, current)
			current = ""
		} else {
			current += string(s[i])
		}
	}
	if current != "" {
		result = append(result, current)
	}

	return result
}

// parseMemoryString parses memory strings like "512m", "1g" into bytes.
func parseMemoryString(s string) (int64, error) {
	var value float64
	var unit string
	if _, err := fmt.Sscanf(s, "%f%s", &value, &unit); err != nil {
		// Try parsing as just a number (bytes)
		if _, err := fmt.Sscanf(s, "%f", &value); err != nil {
			return 0, fmt.Errorf("invalid format")
		}
		return int64(value), nil
	}

	switch unit {
	case "b", "B":
		return int64(value), nil
	case "k", "K", "kb", "KB":
		return int64(value * 1024), nil
	case "m", "M", "mb", "MB":
		return int64(value * 1024 * 1024), nil
	case "g", "G", "gb", "GB":
		return int64(value * 1024 * 1024 * 1024), nil
	default:
		return 0, fmt.Errorf("unknown unit %q", unit)
	}
}

// Close closes the Podman connection.
func (r *Runtime) Close() error {
	// The bindings connection doesn't have a close method,
	// it's based on context which is managed elsewhere
	return nil
}

// Ensure Runtime implements runtime.Runtime
var _ runtime.Runtime = (*Runtime)(nil)

// Ensure processHandle implements runtime.Process
var _ runtime.Process = (*processHandle)(nil)

// Ensure podmanCheckpoint implements runtime.Checkpoint
var _ runtime.Checkpoint = (*podmanCheckpoint)(nil)
