//go:build linux

package cloudhv

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

// Runtime implements the runtime.Runtime interface for Cloud Hypervisor on Linux.
type Runtime struct {
	logger zerolog.Logger
}

// cloudHypervisorCheckpoint holds the data needed to restore a VM.
type cloudHypervisorCheckpoint struct {
	executionID    string
	snapshotDir    string // Directory containing snapshot files
	config         *Config
	apiHostAddress string
	tapDeviceName  string // TAP device name (for restore)
	guestMAC       string // MAC address used (for restore)
}

func (c *cloudHypervisorCheckpoint) String() string {
	return fmt.Sprintf("cloudhv-checkpoint[exec=%s,snapshot=%s]", c.executionID, c.snapshotDir)
}

func (c *cloudHypervisorCheckpoint) Path() string {
	return c.snapshotDir
}

func (c *cloudHypervisorCheckpoint) GracePeriodMs() int64 {
	// Cloud Hypervisor snapshots are instant - no grace period needed
	return 0
}

// processHandle holds information about a running Cloud Hypervisor VM.
type processHandle struct {
	executionID    string
	chvCmd         *exec.Cmd
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
	return fmt.Sprintf("cloudhv[exec=%s,socket=%s]", h.executionID, h.socketPath)
}

// api sends a PUT request to the Cloud Hypervisor API.
func (h *processHandle) api(ctx context.Context, endpoint string, body any) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(jsonBody)
	}

	url := fmt.Sprintf("http://localhost/api/v1/%s", endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, reqBody)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

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

// apiGet sends a GET request to the Cloud Hypervisor API.
func (h *processHandle) apiGet(ctx context.Context, endpoint string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	url := fmt.Sprintf("http://localhost/api/v1/%s", endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
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
	return fmt.Errorf("timeout waiting for Cloud Hypervisor socket")
}

func (h *processHandle) Checkpoint(ctx context.Context, keepRunning bool) (runtime.Checkpoint, error) {
	snapshotDir := h.snapshotDir

	h.logger.Debug().
		Str("snapshot_dir", snapshotDir).
		Bool("keep_running", keepRunning).
		Msg("creating checkpoint")

	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create snapshot directory: %w", err)
	}

	// Pause the VM first (required before snapshot)
	if err := h.api(ctx, "vm.pause", nil); err != nil {
		return nil, fmt.Errorf("failed to pause VM: %w", err)
	}

	// Create snapshot - Cloud Hypervisor uses destination_url with file:// scheme
	snapshotReq := map[string]string{
		"destination_url": fmt.Sprintf("file://%s", snapshotDir),
	}
	if err := h.api(ctx, "vm.snapshot", snapshotReq); err != nil {
		// Try to resume if snapshot failed
		_ = h.api(ctx, "vm.resume", nil)
		return nil, fmt.Errorf("failed to create snapshot: %w", err)
	}

	h.logger.Debug().
		Str("snapshot_dir", snapshotDir).
		Msg("checkpoint created")

	if keepRunning {
		// Resume the VM
		if err := h.api(ctx, "vm.resume", nil); err != nil {
			return nil, fmt.Errorf("failed to resume VM after checkpoint: %w", err)
		}
	} else {
		// Kill the VM since we're not keeping it running
		if err := h.Kill(ctx); err != nil {
			h.logger.Warn().Err(err).Msg("failed to kill VM after checkpoint")
		}
	}

	return &cloudHypervisorCheckpoint{
		executionID:    h.executionID,
		snapshotDir:    snapshotDir,
		config:         h.config,
		apiHostAddress: h.apiHostAddress,
		tapDeviceName:  h.tapDeviceName,
		guestMAC:       h.guestMAC,
	}, nil
}

func (h *processHandle) Kill(ctx context.Context) error {
	h.logger.Debug().Msg("killing Cloud Hypervisor VM")

	if h.chvCmd != nil && h.chvCmd.Process != nil {
		// Send SIGTERM first for graceful shutdown
		if err := h.chvCmd.Process.Signal(syscall.SIGTERM); err != nil {
			h.logger.Debug().Err(err).Msg("SIGTERM failed, sending SIGKILL")
			if err := h.chvCmd.Process.Kill(); err != nil {
				return fmt.Errorf("failed to kill Cloud Hypervisor process: %w", err)
			}
		}
	}

	h.logger.Debug().Msg("Cloud Hypervisor VM killed")
	return nil
}

func (h *processHandle) Wait(ctx context.Context) (int, error) {
	h.logger.Debug().Msg("waiting for Cloud Hypervisor VM to exit")

	if h.chvCmd == nil {
		return -1, fmt.Errorf("no Cloud Hypervisor process to wait on")
	}

	// Wait for the process in a goroutine so we can respect context cancellation
	done := make(chan error, 1)
	go func() {
		done <- h.chvCmd.Wait()
	}()

	select {
	case <-ctx.Done():
		return -1, ctx.Err()
	case err := <-done:
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode := exitErr.ExitCode()
				h.logger.Debug().Int("exit_code", exitCode).Msg("Cloud Hypervisor VM exited with error")
				return exitCode, nil
			}
			return -1, fmt.Errorf("failed to wait for Cloud Hypervisor: %w", err)
		}
		h.logger.Debug().Int("exit_code", 0).Msg("Cloud Hypervisor VM exited")
		return 0, nil
	}
}

func (h *processHandle) Cleanup(ctx context.Context) error {
	h.logger.Debug().Msg("cleaning up Cloud Hypervisor VM")

	if h.chvCmd != nil && h.chvCmd.Process != nil {
		_ = h.chvCmd.Process.Kill()
	}

	if h.socketPath != "" {
		_ = os.Remove(h.socketPath)
	}

	if err := deleteTAPDevice(h.tapDeviceName); err != nil {
		h.logger.Warn().Err(err).Str("tap", h.tapDeviceName).Msg("failed to delete TAP device")
	} else {
		h.logger.Debug().Str("tap", h.tapDeviceName).Msg("deleted TAP device")
	}

	h.logger.Debug().Msg("Cloud Hypervisor VM cleaned up")
	return nil
}

// NewRuntime creates a new Cloud Hypervisor runtime for Linux.
func NewRuntime() (*Runtime, error) {
	logger := gologger.NewLogger().With().
		Str("runtime", string(runtime.RuntimeTypeCloudHypervisor)).
		Str("platform", "linux").
		Logger()

	cmd := exec.Command("cloud-hypervisor", "--version")
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("cloud-hypervisor not found: %w", err)
	}

	logger.Debug().Msg("cloud-hypervisor runtime initialized")

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
	// Take up to 11 characters from the END to stay under the 15-char limit with "chv-" prefix.
	// Snowflake IDs have more variance in trailing characters (sequence/random bits).
	if len(name) > 11 {
		name = name[len(name)-11:]
	}
	return fmt.Sprintf("chv-%s", name)
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
	// "chv-" (4) + base (8) + suffix (up to 3, e.g., "-99") = 15
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
	mountPoint, err := os.MkdirTemp("", "chv-rootfs-mount-")
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
	envContent.WriteString("# Runtime environment variables injected by Cloud Hypervisor runtime\n")
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
// Single quotes are replaced with '\" (end quote, escaped quote, start quote).
func escapeShellValue(s string) string {
	return bytes.NewBuffer(bytes.ReplaceAll([]byte(s), []byte("'"), []byte("'\\''"))).String()
}

func (r *Runtime) Type() runtime.RuntimeType {
	return runtime.RuntimeTypeCloudHypervisor
}

func (r *Runtime) ParseConfig(raw []byte) (any, error) {
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse cloud-hypervisor config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid cloud-hypervisor config: %w", err)
	}
	return cfg.WithDefaults(), nil
}

func (r *Runtime) Start(ctx context.Context, opts runtime.StartOptions) (runtime.Process, error) {
	cfg, ok := opts.Config.(*Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type: expected *cloudhv.Config, got %T", opts.Config)
	}

	// Apply defaults to ensure cmdline, vcpu_count, etc. are set
	cfg = cfg.WithDefaults()

	r.logger.Debug().
		Str("execution_id", opts.ExecutionID).
		Str("firmware", cfg.FirmwarePath).
		Str("rootfs", cfg.RootfsPath).
		Msg("starting Cloud Hypervisor VM")

	workDir := filepath.Join(os.TempDir(), "checker", "cloudhv-work", opts.ExecutionID)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create work directory: %w", err)
	}

	// Copy rootfs to work directory so we can inject environment variables
	workRootfs := filepath.Join(workDir, "rootfs.raw")
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
	env["CHECKER_GUEST_IP"] = ExecutionIDToIPv6WithCIDR(opts.ExecutionID)
	env["CHECKER_GATEWAY"] = IPv6Gateway

	if err := injectEnvVars(workRootfs, env); err != nil {
		return nil, fmt.Errorf("failed to inject environment variables: %w", err)
	}

	workCfg := *cfg
	workCfg.RootfsPath = workRootfs

	snapshotDir := filepath.Join(os.TempDir(), "checker", "cloudhv-snapshots", opts.ExecutionID)

	return r.startVM(ctx, opts.ExecutionID, &workCfg, snapshotDir, opts.APIHostAddress, opts.Stdout, opts.Stderr)
}

func (r *Runtime) startVM(ctx context.Context, executionID string, cfg *Config, snapshotDir string, apiHostAddress string, stdout, stderr io.Writer) (runtime.Process, error) {
	workDir := filepath.Join(os.TempDir(), "checker", "cloudhv-work", executionID)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create work directory: %w", err)
	}

	socketPath := filepath.Join(workDir, "chv.sock")

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

	// Start cloud-hypervisor with API socket
	chvCmd := exec.CommandContext(ctx, "cloud-hypervisor", "--api-socket", fmt.Sprintf("path=%s", socketPath))
	if stdout != nil {
		chvCmd.Stdout = stdout
	}
	if stderr != nil {
		chvCmd.Stderr = stderr
	}

	if err := chvCmd.Start(); err != nil {
		_ = deleteTAPDevice(tapDeviceName)
		return nil, fmt.Errorf("failed to start cloud-hypervisor: %w", err)
	}

	handle := &processHandle{
		executionID:    executionID,
		chvCmd:         chvCmd,
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

	if err := handle.waitForSocket(ctx, 10*time.Second); err != nil {
		_ = chvCmd.Process.Kill()
		_ = deleteTAPDevice(tapDeviceName)
		return nil, fmt.Errorf("cloud-hypervisor socket not ready: %w", err)
	}

	// Create VM config for Cloud Hypervisor
	// See: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/api.md
	// Using firmware (hypervisor-fw) which includes a built-in kernel with virtio drivers
	vmConfig := map[string]any{
		"payload": map[string]string{
			"firmware": cfg.FirmwarePath,
		},
		"disks": []map[string]any{
			{
				"path":       cfg.RootfsPath,
				"readonly":   false,
				"direct":     false,
				"vhost_user": false,
			},
		},
		"cpus": map[string]int{
			"boot_vcpus": cfg.VcpuCount,
			"max_vcpus":  cfg.VcpuCount,
		},
		"memory": map[string]any{
			"size": int64(cfg.MemSizeMib) * 1024 * 1024, // Cloud Hypervisor expects bytes
		},
		"net": []map[string]any{
			{
				"tap":        tapDeviceName,
				"mac":        guestMAC,
				"num_queues": 2,
				"queue_size": 256,
			},
		},
		"serial": map[string]string{
			"mode": "Tty",
		},
		"console": map[string]string{
			"mode": "Off",
		},
	}

	if err := handle.api(ctx, "vm.create", vmConfig); err != nil {
		_ = chvCmd.Process.Kill()
		_ = deleteTAPDevice(tapDeviceName)
		return nil, fmt.Errorf("failed to create VM: %w", err)
	}

	if err := handle.api(ctx, "vm.boot", nil); err != nil {
		_ = chvCmd.Process.Kill()
		_ = deleteTAPDevice(tapDeviceName)
		return nil, fmt.Errorf("failed to boot VM: %w", err)
	}

	handle.started = true
	logger.Debug().Msg("Cloud Hypervisor VM started")

	return handle, nil
}

func (r *Runtime) Restore(ctx context.Context, opts runtime.RestoreOptions) (runtime.Process, error) {
	c, ok := opts.Checkpoint.(*cloudHypervisorCheckpoint)
	if !ok {
		return nil, fmt.Errorf("invalid checkpoint type: expected *cloudHypervisorCheckpoint, got %T", opts.Checkpoint)
	}

	r.logger.Debug().
		Str("execution_id", c.executionID).
		Str("snapshot_dir", c.snapshotDir).
		Msg("restoring Cloud Hypervisor VM from snapshot")

	// Verify snapshot directory exists
	if _, err := os.Stat(c.snapshotDir); err != nil {
		return nil, fmt.Errorf("snapshot directory not found: %w", err)
	}

	workDir := filepath.Join(os.TempDir(), "checker", "cloudhv-work", c.executionID)
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create work directory: %w", err)
	}

	socketPath := filepath.Join(workDir, "chv.sock")

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

	// Start cloud-hypervisor with API socket
	chvCmd := exec.CommandContext(ctx, "cloud-hypervisor", "--api-socket", fmt.Sprintf("path=%s", socketPath))
	if opts.Stdout != nil {
		chvCmd.Stdout = opts.Stdout
	}
	if opts.Stderr != nil {
		chvCmd.Stderr = opts.Stderr
	}

	if err := chvCmd.Start(); err != nil {
		_ = deleteTAPDevice(tapDeviceName)
		return nil, fmt.Errorf("failed to start cloud-hypervisor: %w", err)
	}

	handle := &processHandle{
		executionID:    c.executionID,
		chvCmd:         chvCmd,
		socketPath:     socketPath,
		snapshotDir:    c.snapshotDir,
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

	if err := handle.waitForSocket(ctx, 10*time.Second); err != nil {
		_ = chvCmd.Process.Kill()
		_ = deleteTAPDevice(tapDeviceName)
		return nil, fmt.Errorf("cloud-hypervisor socket not ready: %w", err)
	}

	// Restore from snapshot - Cloud Hypervisor uses source_url with file:// scheme
	restoreReq := map[string]any{
		"source_url": fmt.Sprintf("file://%s", c.snapshotDir),
		"prefault":   false,
		"net_fds":    []any{},
	}

	if err := handle.api(ctx, "vm.restore", restoreReq); err != nil {
		_ = chvCmd.Process.Kill()
		_ = deleteTAPDevice(tapDeviceName)
		return nil, fmt.Errorf("failed to restore snapshot: %w", err)
	}

	// Resume the VM after restore
	if err := handle.api(ctx, "vm.resume", nil); err != nil {
		_ = chvCmd.Process.Kill()
		_ = deleteTAPDevice(tapDeviceName)
		return nil, fmt.Errorf("failed to resume VM after restore: %w", err)
	}

	handle.started = true
	logger.Debug().Msg("Cloud Hypervisor VM restored from snapshot")

	return handle, nil
}

// ReconstructCheckpoint rebuilds a Checkpoint from persisted data.
func (r *Runtime) ReconstructCheckpoint(checkpointPath string, executionID string, env map[string]string, config any, apiHostAddress string) (runtime.Checkpoint, error) {
	cfg, ok := config.(*Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type: expected *cloudhv.Config, got %T", config)
	}

	// Network config is required
	if cfg.Network == nil {
		return nil, fmt.Errorf("network config is required")
	}

	tapDeviceName, err := findUniqueTAPName(executionID)
	if err != nil {
		return nil, fmt.Errorf("failed to generate TAP name: %w", err)
	}
	guestMAC, err := GenerateMAC()
	if err != nil {
		return nil, fmt.Errorf("failed to generate MAC address: %w", err)
	}

	return &cloudHypervisorCheckpoint{
		executionID:    executionID,
		snapshotDir:    checkpointPath,
		config:         cfg,
		apiHostAddress: apiHostAddress,
		tapDeviceName:  tapDeviceName,
		guestMAC:       guestMAC,
	}, nil
}

// Close is a no-op for the Cloud Hypervisor runtime.
func (r *Runtime) Close() error {
	return nil
}

// Ensure Runtime implements runtime.Runtime
var _ runtime.Runtime = (*Runtime)(nil)

// Ensure processHandle implements runtime.Process
var _ runtime.Process = (*processHandle)(nil)

// Ensure cloudHypervisorCheckpoint implements runtime.Checkpoint
var _ runtime.Checkpoint = (*cloudHypervisorCheckpoint)(nil)
