//go:build linux

package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/checkpoint"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/rs/zerolog"

	"github.com/danthegoodman1/checker/gologger"
	"github.com/danthegoodman1/checker/runtime"
)

// Runtime implements the runtime.Runtime interface for Docker on Linux.
// Uses Docker's checkpoint/restore functionality (requires experimental features + CRIU).
type Runtime struct {
	client *client.Client
	logger zerolog.Logger
}

// dockerCheckpoint holds the data needed to restore a container on Linux.
type dockerCheckpoint struct {
	executionID    string
	containerID    string
	checkpointName string
	checkpointDir  string
	env            map[string]string
	config         *Config
	apiHostAddress string
}

func (c *dockerCheckpoint) String() string {
	return fmt.Sprintf("docker-linux-checkpoint[exec=%s,container=%s,checkpoint=%s]",
		c.executionID, c.containerID, c.checkpointName)
}

// processHandle holds information about a running Docker container on Linux.
type processHandle struct {
	executionID    string
	containerID    string
	env            map[string]string
	config         *Config
	checkpointDir  string
	client         *client.Client
	apiHostAddress string
	logger         zerolog.Logger
}

func (h *processHandle) String() string {
	return fmt.Sprintf("docker-linux[exec=%s,container=%s]", h.executionID, h.containerID)
}

func (h *processHandle) Checkpoint(ctx context.Context, keepRunning bool) (runtime.Checkpoint, error) {
	checkpointName := fmt.Sprintf("checkpoint-%s", h.executionID)

	h.logger.Debug().
		Str("checkpoint_name", checkpointName).
		Bool("keep_running", keepRunning).
		Msg("creating checkpoint")

	// Ensure checkpoint directory exists
	if err := os.MkdirAll(h.checkpointDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create checkpoint directory: %w", err)
	}

	// Create checkpoint using Docker SDK
	err := h.client.CheckpointCreate(ctx, h.containerID, checkpoint.CreateOptions{
		CheckpointID:  checkpointName,
		CheckpointDir: h.checkpointDir,
		Exit:          !keepRunning, // Exit=true means stop container after checkpoint
	})
	if err != nil {
		return nil, fmt.Errorf("failed to checkpoint container: %w", err)
	}

	h.logger.Debug().
		Str("checkpoint_name", checkpointName).
		Msg("checkpoint created")

	return &dockerCheckpoint{
		executionID:    h.executionID,
		containerID:    h.containerID,
		checkpointName: checkpointName,
		checkpointDir:  h.checkpointDir,
		env:            h.env,
		config:         h.config,
		apiHostAddress: h.apiHostAddress,
	}, nil
}

func (h *processHandle) Kill(ctx context.Context) error {
	h.logger.Debug().Msg("killing container")

	// Try graceful stop first (5 second timeout)
	timeout := 5
	err := h.client.ContainerStop(ctx, h.containerID, container.StopOptions{
		Timeout: &timeout,
	})
	if err != nil {
		// If stop fails, force kill
		h.logger.Debug().Err(err).Msg("graceful stop failed, forcing kill")
		if killErr := h.client.ContainerKill(ctx, h.containerID, "SIGKILL"); killErr != nil {
			return fmt.Errorf("failed to kill container: %w", killErr)
		}
	}
	h.logger.Debug().Msg("container killed")
	return nil
}

func (h *processHandle) Wait(ctx context.Context) (int, error) {
	statusCh, errCh := h.client.ContainerWait(ctx, h.containerID, container.WaitConditionNotRunning)

	select {
	case err := <-errCh:
		if err != nil {
			return -1, fmt.Errorf("failed to wait for container: %w", err)
		}
	case status := <-statusCh:
		return int(status.StatusCode), nil
	case <-ctx.Done():
		return -1, ctx.Err()
	}

	return -1, fmt.Errorf("unexpected wait state")
}

// NewRuntime creates a new Docker runtime for Linux.
func NewRuntime() (*Runtime, error) {
	logger := gologger.NewLogger().With().
		Str("runtime", string(runtime.RuntimeTypeDocker)).
		Str("platform", "linux").
		Logger()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}

	// Ping to verify connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx); err != nil {
		return nil, fmt.Errorf("failed to connect to docker daemon: %w", err)
	}

	logger.Debug().Msg("docker runtime initialized")

	return &Runtime{client: cli, logger: logger}, nil
}

func (r *Runtime) Type() runtime.RuntimeType {
	return runtime.RuntimeTypeDocker
}

func (r *Runtime) ParseConfig(raw []byte) (any, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse docker config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid docker config: %w", err)
	}
	return &cfg, nil
}

func (r *Runtime) Start(ctx context.Context, opts runtime.StartOptions) (runtime.Process, error) {
	cfg, ok := opts.Config.(*Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type: expected *docker.Config, got %T", opts.Config)
	}

	r.logger.Debug().
		Str("execution_id", opts.ExecutionID).
		Str("image", cfg.Image).
		Msg("starting container")

	checkpointDir := cfg.CheckpointDir
	if checkpointDir == "" {
		checkpointDir = filepath.Join(os.TempDir(), "checker", "docker-checkpoints", opts.ExecutionID)
	}

	// Ensure checkpoint directory exists
	if err := os.MkdirAll(checkpointDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create checkpoint directory: %w", err)
	}

	return r.startContainer(ctx, opts.ExecutionID, opts.Env, cfg, checkpointDir, opts.APIHostAddress)
}

func (r *Runtime) Restore(ctx context.Context, chk runtime.Checkpoint) (runtime.Process, error) {
	c, ok := chk.(*dockerCheckpoint)
	if !ok {
		return nil, fmt.Errorf("invalid checkpoint type: expected *dockerCheckpoint, got %T", chk)
	}

	r.logger.Debug().
		Str("execution_id", c.executionID).
		Str("container_id", c.containerID).
		Str("checkpoint_name", c.checkpointName).
		Msg("restoring container from checkpoint")

	// Restore container from checkpoint using Docker SDK
	err := r.client.ContainerStart(ctx, c.containerID, container.StartOptions{
		CheckpointID:  c.checkpointName,
		CheckpointDir: c.checkpointDir,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to restore container from checkpoint: %w", err)
	}

	logger := r.logger.With().
		Str("execution_id", c.executionID).
		Str("container_id", c.containerID).
		Logger()

	logger.Debug().Msg("container restored")

	return &processHandle{
		executionID:    c.executionID,
		containerID:    c.containerID,
		env:            c.env,
		config:         c.config,
		checkpointDir:  c.checkpointDir,
		client:         r.client,
		apiHostAddress: c.apiHostAddress,
		logger:         logger,
	}, nil
}

func (r *Runtime) startContainer(ctx context.Context, executionID string, env map[string]string, cfg *Config, checkpointDir string, apiHostAddress string) (runtime.Process, error) {
	containerName := fmt.Sprintf("checker-%s", executionID)

	// Extract port from apiHostAddress (e.g., "127.0.0.1:18083" -> "18083")
	// and use host.docker.internal with ExtraHosts mapping on Linux
	_, port, _ := strings.Cut(apiHostAddress, ":")
	dockerAPIURL := fmt.Sprintf("host.docker.internal:%s", port)

	// Build environment variables
	envList := make([]string, 0, len(env)+len(cfg.Env)+1)
	envList = append(envList, fmt.Sprintf("CHECKER_API_URL=%s", dockerAPIURL))
	for k, v := range env {
		envList = append(envList, fmt.Sprintf("%s=%s", k, v))
	}
	for k, v := range cfg.Env {
		envList = append(envList, fmt.Sprintf("%s=%s", k, v))
	}

	// Build labels
	labels := map[string]string{
		"checker.execution_id": executionID,
	}
	for k, v := range cfg.Labels {
		labels[k] = v
	}

	// Container config
	containerConfig := &container.Config{
		Image:      cfg.Image,
		Cmd:        cfg.Command,
		WorkingDir: cfg.WorkDir,
		Env:        envList,
		Labels:     labels,
	}

	// Parse port bindings
	portBindings := nat.PortMap{}
	exposedPorts := nat.PortSet{}
	for _, p := range cfg.Ports {
		portMapping, err := nat.ParsePortSpec(p)
		if err != nil {
			return nil, fmt.Errorf("invalid port mapping %q: %w", p, err)
		}
		for _, pm := range portMapping {
			exposedPorts[pm.Port] = struct{}{}
			portBindings[pm.Port] = append(portBindings[pm.Port], pm.Binding)
		}
	}
	containerConfig.ExposedPorts = exposedPorts

	// Host config
	hostConfig := &container.HostConfig{
		PortBindings: portBindings,
		AutoRemove:   cfg.Remove,
		// On Linux, we need to explicitly map host.docker.internal to the host
		ExtraHosts: []string{"host.docker.internal:host-gateway"},
	}

	// Volume binds
	if len(cfg.Volumes) > 0 {
		hostConfig.Binds = cfg.Volumes
	}

	// Network mode
	if cfg.Network != "" {
		hostConfig.NetworkMode = container.NetworkMode(cfg.Network)
	}

	// Resource limits
	if cfg.Memory != "" {
		// Parse memory limit (e.g., "512m", "1g")
		memBytes, err := parseMemoryString(cfg.Memory)
		if err != nil {
			return nil, fmt.Errorf("invalid memory limit %q: %w", cfg.Memory, err)
		}
		hostConfig.Memory = memBytes
	}

	if cfg.CPUs != "" {
		// Parse CPU limit (e.g., "0.5", "2")
		var cpus float64
		if _, err := fmt.Sscanf(cfg.CPUs, "%f", &cpus); err != nil {
			return nil, fmt.Errorf("invalid CPU limit %q: %w", cfg.CPUs, err)
		}
		// Docker uses NanoCPUs (1 CPU = 1e9 NanoCPUs)
		hostConfig.NanoCPUs = int64(cpus * 1e9)
	}

	// User
	if cfg.User != "" {
		containerConfig.User = cfg.User
	}

	// Create container
	resp, err := r.client.ContainerCreate(ctx, containerConfig, hostConfig, &network.NetworkingConfig{}, nil, containerName)
	if err != nil {
		return nil, fmt.Errorf("failed to create container: %w", err)
	}

	logger := r.logger.With().
		Str("execution_id", executionID).
		Str("container_id", resp.ID).
		Str("container_name", containerName).
		Logger()

	logger.Debug().Msg("container created")

	// Start container
	if err := r.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Clean up the created container if start fails
		_ = r.client.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	logger.Debug().Msg("container started")

	return &processHandle{
		executionID:    executionID,
		containerID:    resp.ID,
		env:            env,
		config:         cfg,
		checkpointDir:  checkpointDir,
		client:         r.client,
		apiHostAddress: apiHostAddress,
		logger:         logger,
	}, nil
}

// parseMemoryString parses memory strings like "512m", "1g" into bytes
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

// Close closes the Docker client connection
func (r *Runtime) Close() error {
	if r.client != nil {
		return r.client.Close()
	}
	return nil
}

// Ensure Runtime implements runtime.Runtime
var _ runtime.Runtime = (*Runtime)(nil)

// Ensure processHandle implements runtime.Process
var _ runtime.Process = (*processHandle)(nil)

// Ensure dockerCheckpoint implements runtime.Checkpoint
var _ runtime.Checkpoint = (*dockerCheckpoint)(nil)
