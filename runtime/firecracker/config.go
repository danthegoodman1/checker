package firecracker

import (
	"crypto/rand"
	"fmt"

	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
)

var validate = validator.New()

// IPv6 network constants for Firecracker VMs.
// Uses ULA (Unique Local Address) prefix fdfc::/16.
// Guest IPs are derived from execution IDs, giving virtually unlimited unique addresses.
const (
	// IPv6Prefix is the network prefix for Firecracker VMs.
	// Using fdfc::/16 gives us 112 bits for host addressing.
	IPv6Prefix = "fdfc::/16"

	// IPv6Gateway is the gateway address for Firecracker VMs.
	// This is the bridge IP that the hypervisor listens on.
	IPv6Gateway = "fdfc::1"

	// IPv6PrefixLen is the prefix length for guest addresses.
	IPv6PrefixLen = 16
)

// ExecutionIDToIPv6 derives a unique IPv6 address from an execution ID (UUID).
// The address format is: fdfc:XXXX:XXXX:XXXX:XXXX:XXXX:XXXX:XXXX
// where XXXX segments are derived from the first 14 bytes of the UUID.
// This provides a deterministic, collision-free mapping from execution ID to IP.
func ExecutionIDToIPv6(executionID string) (string, error) {
	id, err := uuid.Parse(executionID)
	if err != nil {
		return "", fmt.Errorf("invalid execution ID (expected UUID): %w", err)
	}

	b := id[:]
	// fdfc:XXXX:XXXX:XXXX:XXXX:XXXX:XXXX:XXXX
	// Use first 14 bytes of UUID (112 bits) after the fdfc prefix (16 bits)
	return fmt.Sprintf("fdfc:%02x%02x:%02x%02x:%02x%02x:%02x%02x:%02x%02x:%02x%02x:%02x%02x",
		b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7],
		b[8], b[9], b[10], b[11], b[12], b[13]), nil
}

// ExecutionIDToIPv6WithCIDR returns the IPv6 address with CIDR notation.
func ExecutionIDToIPv6WithCIDR(executionID string) (string, error) {
	ip, err := ExecutionIDToIPv6(executionID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%d", ip, IPv6PrefixLen), nil
}

// NetworkConfig configures TAP networking for the Firecracker VM.
// The bridge must be pre-configured on the host (see LINUX_SETUP.md).
//
// Uses IPv6-only networking with addresses auto-derived from execution IDs.
// This provides virtually unlimited unique addresses with no IP allocation needed.
// Gateway is always fdfc::1, guest IPs are fdfc::<execution_id>.
type NetworkConfig struct {
	// BridgeName is the name of the host bridge to attach the TAP device to.
	// Required. Example: "fcbr0"
	BridgeName string `json:"bridge_name" validate:"required"`

	// GuestMAC is the MAC address for the guest VM's eth0 interface.
	// Optional. If empty, a random MAC is generated.
	GuestMAC string `json:"guest_mac"`
}

// Validate checks that the network config has all required fields.
func (n *NetworkConfig) Validate() error {
	if n.BridgeName == "" {
		return fmt.Errorf("bridge_name is required")
	}
	return nil
}

// GuestIPv6 returns the guest IPv6 address derived from the execution ID.
func (n *NetworkConfig) GuestIPv6(executionID string) (string, error) {
	return ExecutionIDToIPv6WithCIDR(executionID)
}

// GatewayIPv6 returns the gateway IPv6 address.
func (n *NetworkConfig) GatewayIPv6() string {
	return IPv6Gateway
}

// GenerateMAC generates a random MAC address for the guest VM.
// Uses locally administered unicast address format (02:xx:xx:xx:xx:xx).
func GenerateMAC() (string, error) {
	mac := make([]byte, 6)
	if _, err := rand.Read(mac); err != nil {
		return "", fmt.Errorf("failed to generate random MAC: %w", err)
	}
	// Set locally administered bit, clear multicast bit
	mac[0] = (mac[0] | 0x02) & 0xFE
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		mac[0], mac[1], mac[2], mac[3], mac[4], mac[5]), nil
}

// Config holds the configuration for a Firecracker runtime execution.
type Config struct {
	// KernelPath is the path to the kernel image (e.g., vmlinux-5.10.bin).
	// Required. The kernel must be uncompressed.
	KernelPath string `json:"kernel_path" validate:"required"`

	// RootfsPath is the path to the root filesystem ext4 image.
	// Required. The rootfs should have an /init script that runs the workload.
	RootfsPath string `json:"rootfs_path" validate:"required"`

	// VcpuCount is the number of virtual CPUs for the microVM.
	// Defaults to 1 if not specified.
	VcpuCount int `json:"vcpu_count"`

	// MemSizeMib is the memory size in MiB for the microVM.
	// Defaults to 512 if not specified.
	MemSizeMib int `json:"mem_size_mib"`

	// BootArgs are additional kernel boot arguments.
	// If empty, sensible defaults are used for fast boot.
	BootArgs string `json:"boot_args"`

	// Network configures TAP networking for the VM.
	// Required - the VM must be able to communicate with the hypervisor API over HTTP.
	// The rootfs /init script must configure networking in the guest.
	Network *NetworkConfig `json:"network" validate:"required"`
}

// Validate checks that the config has all required fields.
func (c *Config) Validate() error {
	if err := validate.Struct(c); err != nil {
		return err
	}
	if c.Network != nil {
		if err := c.Network.Validate(); err != nil {
			return fmt.Errorf("invalid network config: %w", err)
		}
	}
	return nil
}

// WithDefaults returns a copy of the config with default values applied.
func (c *Config) WithDefaults() *Config {
	cfg := *c
	if cfg.VcpuCount <= 0 {
		cfg.VcpuCount = 1
	}
	if cfg.MemSizeMib <= 0 {
		cfg.MemSizeMib = 512
	}
	if cfg.BootArgs == "" {
		cfg.BootArgs = "console=ttyS0 reboot=k panic=1 pci=off init=/init nomodule audit=0 tsc=reliable no_timer_check noreplace-smp 8250.nr_uarts=1 random.trust_cpu=on"
	}
	return &cfg
}
