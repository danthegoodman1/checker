//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/danthegoodman1/checker/gologger"
	"github.com/danthegoodman1/checker/runtime"
)

// Runtime implements the runtime.Runtime interface for Firecracker on Linux.
type Runtime struct {
	logger zerolog.Logger
}

// firecrackerCheckpoint holds the data needed to restore a VM.
type firecrackerCheckpoint struct {
	executionID    string
	snapshotPath   string // Path to snapshot metadata file
	memFilePath    string // Path to memory dump file
	config         *Config
	apiHostAddress string
	tapDeviceName  string // TAP device name (for restore)
	guestMAC       string // MAC address used (for restore)
}

func (c *firecrackerCheckpoint) String() string {
	return fmt.Sprintf("firecracker-checkpoint[exec=%s,snapshot=%s]", c.executionID, c.snapshotPath)
}

func (c *firecrackerCheckpoint) Path() string {
	// Return the snapshot path as the primary checkpoint path
	// The memory file is derived from this (same directory, different extension)
	return c.snapshotPath
}

func (c *firecrackerCheckpoint) GracePeriodMs() int64 {
	// Firecracker snapshots are instant - no grace period needed
	return 0
}

// processHandle holds information about a running Firecracker VM.
type processHandle struct {
	executionID    string
	fcCmd          *exec.Cmd
	socketPath     string
	snapshotDir    string
	config         *Config
	apiHostAddress string
	logger         zerolog.Logger
	httpClient     *http.Client
	stdout         io.Writer
	stderr         io.Writer
	mu             sync.Mutex
	started        bool
	tapDeviceName  string
	guestMAC       string
}

func (h *processHandle) String() string {
	return fmt.Sprintf("firecracker[exec=%s,socket=%s]", h.executionID, h.socketPath)
}

// api sends a PUT request to the Firecracker API.
func (h *processHandle) api(ctx context.Context, endpoint string, body any) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal request body: %w", err)
	}

	url := fmt.Sprintf("http://localhost/%s", endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// apiPatch sends a PATCH request to the Firecracker API.
func (h *processHandle) apiPatch(ctx context.Context, endpoint string, body any) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal request body: %w", err)
	}

	url := fmt.Sprintf("http://localhost/%s", endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	return nil
}

func (h *processHandle) waitForSocket(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if _, err := os.Stat(h.socketPath); err == nil {
			conn, err := net.Dial("unix", h.socketPath)
			if err == nil {
				conn.Close()
				return nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for Firecracker socket")
}

func (h *processHandle) Checkpoint(ctx context.Context, keepRunning bool) (runtime.Checkpoint, error) {
	snapshotFile := filepath.Join(h.snapshotDir, fmt.Sprintf("snapshot-%s", h.executionID))
	memFile := filepath.Join(h.snapshotDir, fmt.Sprintf("mem-%s", h.executionID))

	h.logger.Debug().
		Str("snapshot_file", snapshotFile).
		Str("mem_file", memFile).
		Bool("keep_running", keepRunning).
		Msg("creating checkpoint")

	if err := os.MkdirAll(h.snapshotDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create snapshot directory: %w", err)
	}

	if err := h.apiPatch(ctx, "vm", map[string]string{"state": "Paused"}); err != nil {
		return nil, fmt.Errorf("failed to pause VM: %w", err)
	}

	snapshotReq := map[string]any{
		"snapshot_type": "Full",
		"snapshot_path": snapshotFile,
		"mem_file_path": memFile,
	}
	if err := h.api(ctx, "snapshot/create", snapshotReq); err != nil {
		// Try to resume if snapshot failed
		_ = h.apiPatch(ctx, "vm", map[string]string{"state": "Resumed"})
		return nil, fmt.Errorf("failed to create snapshot: %w", err)
	}

	h.logger.Debug().
		Str("snapshot_file", snapshotFile).
		Str("mem_file", memFile).
		Msg("checkpoint created")

	if keepRunning {
		// Resume the VM
		if err := h.apiPatch(ctx, "vm", map[string]string{"state": "Resumed"}); err != nil {
			return nil, fmt.Errorf("failed to resume VM after checkpoint: %w", err)
		}
	} else {
		// Kill the VM since we're not keeping it running
		if err := h.Kill(ctx); err != nil {
			h.logger.Warn().Err(err).Msg("failed to kill VM after checkpoint")
		}
	}

	return &firecrackerCheckpoint{
		executionID:    h.executionID,
		snapshotPath:   snapshotFile,
		memFilePath:    memFile,
		config:         h.config,
		apiHostAddress: h.apiHostAddress,
		tapDeviceName:  h.tapDeviceName,
		guestMAC:       h.guestMAC,
	}, nil
}

func (h *processHandle) Kill(ctx context.Context) error {
	h.logger.Debug().Msg("killing Firecracker VM")

	if h.fcCmd != nil && h.fcCmd.Process != nil {
		// Send SIGTERM first for graceful shutdown
		if err := h.fcCmd.Process.Signal(syscall.SIGTERM); err != nil {
			h.logger.Debug().Err(err).Msg("SIGTERM failed, sending SIGKILL")
			if err := h.fcCmd.Process.Kill(); err != nil {
				return fmt.Errorf("failed to kill Firecracker process: %w", err)
			}
		}
	}

	h.logger.Debug().Msg("Firecracker VM killed")
	return nil
}

func (h *processHandle) Wait(ctx context.Context) (int, error) {
	h.logger.Debug().Msg("waiting for Firecracker VM to exit")

	if h.fcCmd == nil {
		return -1, fmt.Errorf("no Firecracker process to wait on")
	}

	// Wait for the process in a goroutine so we can respect context cancellation
	done := make(chan error, 1)
	go func() {
		done <- h.fcCmd.Wait()
	}()

	select {
	case <-ctx.Done():
		return -1, ctx.Err()
	case err := <-done:
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode := exitErr.ExitCode()
				h.logger.Debug().Int("exit_code", exitCode).Msg("Firecracker VM exited with error")
				return exitCode, nil
			}
			return -1, fmt.Errorf("failed to wait for Firecracker: %w", err)
		}
		h.logger.Debug().Int("exit_code", 0).Msg("Firecracker VM exited")
		return 0, nil
	}
}

func (h *processHandle) Cleanup(ctx context.Context) error {
	h.logger.Debug().Msg("cleaning up Firecracker VM")

	if h.fcCmd != nil && h.fcCmd.Process != nil {
		_ = h.fcCmd.Process.Kill()
	}

	if h.socketPath != "" {
		_ = os.Remove(h.socketPath)
	}

	if err := deleteTAPDevice(h.tapDeviceName); err != nil {
		h.logger.Warn().Err(err).Str("tap", h.tapDeviceName).Msg("failed to delete TAP device")
	} else {
		h.logger.Debug().Str("tap", h.tapDeviceName).Msg("deleted TAP device")
	}

	h.logger.Debug().Msg("Firecracker VM cleaned up")
	return nil
}

// NewRuntime creates a new Firecracker runtime for Linux.
func NewRuntime() (*Runtime, error) {
	logger := gologger.NewLogger().With().
		Str("runtime", string(runtime.RuntimeTypeFirecracker)).
		Str("platform", "linux").
		Logger()

	cmd := exec.Command("firecracker", "--version")
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("firecracker not found: %w", err)
	}

	logger.Debug().Msg("firecracker runtime initialized")

	return &Runtime{logger: logger}, nil
}

// generateTAPName creates a TAP device name from an execution ID.
// Uses the unique portion of the execution ID (snowflake ID part) while staying under
// the 15-character limit for network interface names (IFNAMSIZ=16 including null).
// For execution IDs like "job-1765430333173062443-1", we skip "job-" and take
// up to 11 characters from the END of the unique snowflake portion.
func generateTAPName(executionID string) string {
	// Skip common prefixes to get to the unique part
	name := executionID
	if len(name) > 4 && name[:4] == "job-" {
		name = name[4:] // Skip "job-" prefix
	}
	// Take up to 11 characters from the END to stay under the 15-char limit with "tap-" prefix.
	// Snowflake IDs have more variance in trailing characters (sequence/random bits).
	if len(name) > 11 {
		name = name[len(name)-11:]
	}
	return fmt.Sprintf("tap-%s", name)
}

// tapDeviceExists checks if a TAP device with the given name already exists.
func tapDeviceExists(tapName string) bool {
	_, err := net.InterfaceByName(tapName)
	return err == nil
}

// findUniqueTAPName generates a TAP name and appends a numeric suffix if needed.
// Returns the unique name or error if all attempts exhausted.
func findUniqueTAPName(executionID string) (string, error) {
	baseName := generateTAPName(executionID)

	// Try without suffix first
	if !tapDeviceExists(baseName) {
		return baseName, nil
	}

	// Try with numeric suffixes (shorten base to fit suffix within 15 char limit)
	// "tap-" (4) + base (8) + suffix (up to 3, e.g., "-99") = 15
	shortBase := baseName
	if len(shortBase) > 12 {
		shortBase = shortBase[:12] // Leave room for suffix
	}

	for i := 1; i <= 99; i++ {
		candidate := fmt.Sprintf("%s-%d", shortBase, i)
		if len(candidate) > 15 {
			candidate = candidate[:15]
		}
		if !tapDeviceExists(candidate) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("could not find unique TAP name for %s after 99 attempts", executionID)
}

// createTAPDevice creates a TAP device and attaches it to the specified bridge.
// Returns the TAP device name.
func createTAPDevice(tapName, bridgeName string) error {
	cmd := exec.Command("ip", "tuntap", "add", tapName, "mode", "tap")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create TAP device %s: %w, output: %s", tapName, err, string(output))
	}

	cmd = exec.Command("ip", "link", "set", tapName, "up")
	if output, err := cmd.CombinedOutput(); err != nil {
		// Try to clean up the TAP device we created
		_ = exec.Command("ip", "link", "delete", tapName).Run()
		return fmt.Errorf("failed to bring up TAP device %s: %w, output: %s", tapName, err, string(output))
	}

	cmd = exec.Command("ip", "link", "set", tapName, "master", bridgeName)
	if output, err := cmd.CombinedOutput(); err != nil {
		// Try to clean up the TAP device we created
		_ = exec.Command("ip", "link", "delete", tapName).Run()
		return fmt.Errorf("failed to attach TAP %s to bridge %s: %w, output: %s", tapName, bridgeName, err, string(output))
	}

	return nil
}

// deleteTAPDevice removes a TAP device.
func deleteTAPDevice(tapName string) error {
	cmd := exec.Command("ip", "link", "delete", tapName)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to delete TAP device %s: %w, output: %s", tapName, err, string(output))
	}
	return nil
}

// copyFile copies a file from src to dst.
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return fmt.Errorf("failed to copy file: %w", err)
	}

	return dstFile.Sync()
}

// injectEnvVars writes environment variables to /etc/checker.env in the ext4 rootfs.
// The init script sources this file to set up the runtime environment.
func injectEnvVars(rootfsPath string, env map[string]string) error {
	mountPoint, err := os.MkdirTemp("", "fc-rootfs-mount-")
	if err != nil {
		return fmt.Errorf("failed to create mount point: %w", err)
	}
	defer os.RemoveAll(mountPoint)

	cmd := exec.Command("mount", "-o", "loop", rootfsPath, mountPoint)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to mount rootfs: %w, output: %s", err, string(output))
	}

	defer func() {
		exec.Command("umount", mountPoint).Run()
	}()

	etcPath := filepath.Join(mountPoint, "etc")
	if _, err := os.Stat(etcPath); os.IsNotExist(err) {
		if err := os.MkdirAll(etcPath, 0755); err != nil {
			return fmt.Errorf("failed to create /etc: %w", err)
		}
	}

	var envContent bytes.Buffer
	envContent.WriteString("# Runtime environment variables injected by Firecracker runtime\n")
	for k, v := range env {
		// Shell-escape the value by using single quotes and escaping any single quotes in the value
		escapedValue := "'" + escapeShellValue(v) + "'"
		envContent.WriteString(fmt.Sprintf("export %s=%s\n", k, escapedValue))
	}

	envFilePath := filepath.Join(mountPoint, "etc", "checker.env")
	if err := os.WriteFile(envFilePath, envContent.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write environment file: %w", err)
	}

	// Sync to ensure writes are flushed before unmount
	exec.Command("sync").Run()

	return nil
}

// escapeShellValue escapes a string for use inside single quotes in a shell script.
// Single quotes are replaced with '\” (end quote, escaped quote, start quote).
func escapeShellValue(s string) string {
	return bytes.NewBuffer(bytes.ReplaceAll([]byte(s), []byte("'"), []byte("'\\''"))).String()
}

func (r *Runtime) Type() runtime.RuntimeType {
	return runtime.RuntimeTypeFirecracker
}

func (r *Runtime) ParseConfig(raw []byte) (any, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse firecracker config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid firecracker config: %w", err)
	}
	return cfg.WithDefaults(), nil
}

func (r *Runtime) Start(ctx context.Context, opts runtime.StartOptions) (runtime.Process, error) {
	cfg, ok := opts.Config.(*Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type: expected *firecracker.Config, got %T", opts.Config)
	}

	// Apply defaults to ensure boot_args, vcpu_count, etc. are set
	cfg = cfg.WithDefaults()

	r.logger.Debug().
		Str("execution_id", opts.ExecutionID).
		Str("kernel", cfg.KernelPath).
		Str("rootfs", cfg.RootfsPath).
		Msg("starting Firecracker VM")

	workDir := filepath.Join(os.TempDir(), "checker", "firecracker-work", opts.ExecutionID)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create work directory: %w", err)
	}

	// Copy rootfs to work directory so we can inject environment variables
	workRootfs := filepath.Join(workDir, "rootfs.ext4")
	if err := copyFile(cfg.RootfsPath, workRootfs); err != nil {
		return nil, fmt.Errorf("failed to copy rootfs: %w", err)
	}

	env := make(map[string]string)
	for k, v := range opts.Env {
		env[k] = v
	}
	// Add runtime-required env vars (these override user vars if there's a conflict)
	env["CHECKER_API_URL"] = opts.APIHostAddress
	env["CHECKER_JOB_ID"] = opts.ExecutionID

	// Add IPv6 network configuration derived from execution ID
	guestIP, err := ExecutionIDToIPv6WithCIDR(opts.ExecutionID)
	if err != nil {
		return nil, fmt.Errorf("failed to generate guest IPv6: %w", err)
	}
	env["CHECKER_GUEST_IP"] = guestIP
	env["CHECKER_GATEWAY"] = IPv6Gateway

	if err := injectEnvVars(workRootfs, env); err != nil {
		return nil, fmt.Errorf("failed to inject environment variables: %w", err)
	}

	workCfg := *cfg
	workCfg.RootfsPath = workRootfs

	snapshotDir := filepath.Join(os.TempDir(), "checker", "firecracker-snapshots", opts.ExecutionID)

	return r.startVM(ctx, opts.ExecutionID, &workCfg, snapshotDir, opts.APIHostAddress, opts.Stdout, opts.Stderr)
}

func (r *Runtime) startVM(ctx context.Context, executionID string, cfg *Config, snapshotDir string, apiHostAddress string, stdout, stderr io.Writer) (runtime.Process, error) {
	workDir := filepath.Join(os.TempDir(), "checker", "firecracker-work", executionID)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create work directory: %w", err)
	}

	socketPath := filepath.Join(workDir, "fc.sock")

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
		Timeout: 30 * time.Second,
	}

	logger := r.logger.With().
		Str("execution_id", executionID).
		Str("socket", socketPath).
		Logger()

	// Network config is required for hypervisor communication
	if cfg.Network == nil {
		return nil, fmt.Errorf("network config is required")
	}

	tapDeviceName, err := findUniqueTAPName(executionID)
	if err != nil {
		return nil, fmt.Errorf("failed to generate TAP name: %w", err)
	}
	logger.Debug().
		Str("tap", tapDeviceName).
		Str("bridge", cfg.Network.BridgeName).
		Msg("creating TAP device")

	_ = deleteTAPDevice(tapDeviceName)

	if err := createTAPDevice(tapDeviceName, cfg.Network.BridgeName); err != nil {
		return nil, fmt.Errorf("failed to create TAP device: %w", err)
	}

	guestMAC := cfg.Network.GuestMAC
	if guestMAC == "" {
		var err error
		guestMAC, err = GenerateMAC()
		if err != nil {
			_ = deleteTAPDevice(tapDeviceName)
			return nil, fmt.Errorf("failed to generate MAC address: %w", err)
		}
	}
	logger.Debug().Str("mac", guestMAC).Msg("using guest MAC address")

	fcCmd := exec.CommandContext(ctx, "firecracker", "--api-sock", socketPath, "--level", "Error")
	if stdout != nil {
		fcCmd.Stdout = stdout
	}
	if stderr != nil {
		fcCmd.Stderr = stderr
	}

	if err := fcCmd.Start(); err != nil {
		_ = deleteTAPDevice(tapDeviceName)
		return nil, fmt.Errorf("failed to start firecracker: %w", err)
	}

	handle := &processHandle{
		executionID:    executionID,
		fcCmd:          fcCmd,
		socketPath:     socketPath,
		snapshotDir:    snapshotDir,
		config:         cfg,
		apiHostAddress: apiHostAddress,
		logger:         logger,
		httpClient:     httpClient,
		stdout:         stdout,
		stderr:         stderr,
		started:        false,
		tapDeviceName:  tapDeviceName,
		guestMAC:       guestMAC,
	}

	if err := handle.waitForSocket(ctx, 5*time.Second); err != nil {
		_ = fcCmd.Process.Kill()
		_ = deleteTAPDevice(tapDeviceName)
		return nil, fmt.Errorf("firecracker socket not ready: %w", err)
	}

	bootSource := map[string]string{
		"kernel_image_path": cfg.KernelPath,
		"boot_args":         cfg.BootArgs,
	}
	if err := handle.api(ctx, "boot-source", bootSource); err != nil {
		_ = fcCmd.Process.Kill()
		_ = deleteTAPDevice(tapDeviceName)
		return nil, fmt.Errorf("failed to configure boot source: %w", err)
	}

	rootDrive := map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   cfg.RootfsPath,
		"is_root_device": true,
		"is_read_only":   false,
	}
	if err := handle.api(ctx, "drives/rootfs", rootDrive); err != nil {
		_ = fcCmd.Process.Kill()
		_ = deleteTAPDevice(tapDeviceName)
		return nil, fmt.Errorf("failed to configure root drive: %w", err)
	}

	machineConfig := map[string]int{
		"vcpu_count":   cfg.VcpuCount,
		"mem_size_mib": cfg.MemSizeMib,
	}
	if err := handle.api(ctx, "machine-config", machineConfig); err != nil {
		_ = fcCmd.Process.Kill()
		_ = deleteTAPDevice(tapDeviceName)
		return nil, fmt.Errorf("failed to configure machine: %w", err)
	}

	networkConfig := map[string]string{
		"iface_id":      "eth0",
		"guest_mac":     guestMAC,
		"host_dev_name": tapDeviceName,
	}
	if err := handle.api(ctx, "network-interfaces/eth0", networkConfig); err != nil {
		_ = fcCmd.Process.Kill()
		_ = deleteTAPDevice(tapDeviceName)
		return nil, fmt.Errorf("failed to configure network interface: %w", err)
	}
	logger.Debug().
		Str("tap", tapDeviceName).
		Str("mac", guestMAC).
		Msg("configured network interface")

	if err := handle.api(ctx, "actions", map[string]string{"action_type": "InstanceStart"}); err != nil {
		_ = fcCmd.Process.Kill()
		_ = deleteTAPDevice(tapDeviceName)
		return nil, fmt.Errorf("failed to start VM: %w", err)
	}

	handle.started = true
	logger.Debug().Msg("Firecracker VM started")

	return handle, nil
}

func (r *Runtime) Restore(ctx context.Context, opts runtime.RestoreOptions) (runtime.Process, error) {
	c, ok := opts.Checkpoint.(*firecrackerCheckpoint)
	if !ok {
		return nil, fmt.Errorf("invalid checkpoint type: expected *firecrackerCheckpoint, got %T", opts.Checkpoint)
	}

	r.logger.Debug().
		Str("execution_id", c.executionID).
		Str("snapshot_path", c.snapshotPath).
		Str("mem_file", c.memFilePath).
		Msg("restoring Firecracker VM from snapshot")

	// Verify snapshot files exist
	if _, err := os.Stat(c.snapshotPath); err != nil {
		return nil, fmt.Errorf("snapshot file not found: %w", err)
	}
	if _, err := os.Stat(c.memFilePath); err != nil {
		return nil, fmt.Errorf("memory file not found: %w", err)
	}

	workDir := filepath.Join(os.TempDir(), "checker", "firecracker-work", c.executionID)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create work directory: %w", err)
	}

	socketPath := filepath.Join(workDir, "fc.sock")

	_ = os.Remove(socketPath)

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
		Timeout: 30 * time.Second,
	}

	logger := r.logger.With().
		Str("execution_id", c.executionID).
		Str("socket", socketPath).
		Logger()

	if c.config.Network == nil {
		return nil, fmt.Errorf("network config is required")
	}

	tapDeviceName := c.tapDeviceName
	if tapDeviceName == "" {
		var err error
		tapDeviceName, err = findUniqueTAPName(c.executionID)
		if err != nil {
			return nil, fmt.Errorf("failed to generate TAP name: %w", err)
		}
	}
	logger.Debug().
		Str("tap", tapDeviceName).
		Str("bridge", c.config.Network.BridgeName).
		Msg("recreating TAP device for restore")

	_ = deleteTAPDevice(tapDeviceName)

	if err := createTAPDevice(tapDeviceName, c.config.Network.BridgeName); err != nil {
		return nil, fmt.Errorf("failed to recreate TAP device: %w", err)
	}

	fcCmd := exec.CommandContext(ctx, "firecracker", "--api-sock", socketPath, "--level", "Error")
	if opts.Stdout != nil {
		fcCmd.Stdout = opts.Stdout
	}
	if opts.Stderr != nil {
		fcCmd.Stderr = opts.Stderr
	}

	if err := fcCmd.Start(); err != nil {
		_ = deleteTAPDevice(tapDeviceName)
		return nil, fmt.Errorf("failed to start firecracker: %w", err)
	}

	snapshotDir := filepath.Dir(c.snapshotPath)

	handle := &processHandle{
		executionID:    c.executionID,
		fcCmd:          fcCmd,
		socketPath:     socketPath,
		snapshotDir:    snapshotDir,
		config:         c.config,
		apiHostAddress: c.apiHostAddress,
		logger:         logger,
		httpClient:     httpClient,
		stdout:         opts.Stdout,
		stderr:         opts.Stderr,
		started:        false,
		tapDeviceName:  tapDeviceName,
		guestMAC:       c.guestMAC,
	}

	if err := handle.waitForSocket(ctx, 5*time.Second); err != nil {
		_ = fcCmd.Process.Kill()
		_ = deleteTAPDevice(tapDeviceName)
		return nil, fmt.Errorf("firecracker socket not ready: %w", err)
	}

	loadReq := map[string]any{
		"snapshot_path": c.snapshotPath,
		"mem_backend": map[string]string{
			"backend_type": "File",
			"backend_path": c.memFilePath,
		},
		"track_dirty_pages": false,
		"resume_vm":         true, // Resume immediately after loading
	}

	if err := handle.api(ctx, "snapshot/load", loadReq); err != nil {
		_ = fcCmd.Process.Kill()
		_ = deleteTAPDevice(tapDeviceName)
		return nil, fmt.Errorf("failed to load snapshot: %w", err)
	}

	handle.started = true
	logger.Debug().Msg("Firecracker VM restored from snapshot")

	return handle, nil
}

// ReconstructCheckpoint rebuilds a Checkpoint from persisted data.
func (r *Runtime) ReconstructCheckpoint(checkpointPath string, executionID string, env map[string]string, config any, apiHostAddress string) (runtime.Checkpoint, error) {
	cfg, ok := config.(*Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type: expected *firecracker.Config, got %T", config)
	}

	// Network config is required
	if cfg.Network == nil {
		return nil, fmt.Errorf("network config is required")
	}

	dir := filepath.Dir(checkpointPath)
	memFilePath := filepath.Join(dir, fmt.Sprintf("mem-%s", executionID))

	tapDeviceName, err := findUniqueTAPName(executionID)
	if err != nil {
		return nil, fmt.Errorf("failed to generate TAP name: %w", err)
	}
	guestMAC, err := GenerateMAC()
	if err != nil {
		return nil, fmt.Errorf("failed to generate MAC address: %w", err)
	}

	return &firecrackerCheckpoint{
		executionID:    executionID,
		snapshotPath:   checkpointPath,
		memFilePath:    memFilePath,
		config:         cfg,
		apiHostAddress: apiHostAddress,
		tapDeviceName:  tapDeviceName,
		guestMAC:       guestMAC,
	}, nil
}

// Close is a no-op for the Firecracker runtime.
func (r *Runtime) Close() error {
	return nil
}

// Ensure Runtime implements runtime.Runtime
var _ runtime.Runtime = (*Runtime)(nil)

// Ensure processHandle implements runtime.Process
var _ runtime.Process = (*processHandle)(nil)

// Ensure firecrackerCheckpoint implements runtime.Checkpoint
var _ runtime.Checkpoint = (*firecrackerCheckpoint)(nil)
