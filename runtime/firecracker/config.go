package firecracker

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"

	"github.com/go-playground/validator/v10"
)

var validate = validator.New()

// IPv6 network constants for Firecracker VMs.
// Uses ULA (Unique Local Address) prefix fdfc::/16.
// Guest IPs are derived from execution IDs, giving virtually unlimited unique addresses.
const (
	IPv6Prefix    = "fdfc::/16"
	IPv6Gateway   = "fdfc::1"
	IPv6PrefixLen = 16
)

// ExecutionIDToIPv6 derives a unique IPv6 address from an execution ID.
// The address format is: fdfc:XXXX:XXXX:XXXX:XXXX:XXXX:XXXX:XXXX
// where XXXX segments are derived from a SHA-256 hash of the execution ID.
// This provides a deterministic mapping from any execution ID string to an IP.
func ExecutionIDToIPv6(executionID string) string {
	// Hash the execution ID to get consistent bytes regardless of format
	hash := sha256.Sum256([]byte(executionID))

	// fdfc:XXXX:XXXX:XXXX:XXXX:XXXX:XXXX:XXXX
	// Use first 14 bytes of hash (112 bits) after the fdfc prefix (16 bits)
	return fmt.Sprintf("fdfc:%02x%02x:%02x%02x:%02x%02x:%02x%02x:%02x%02x:%02x%02x:%02x%02x",
		hash[0], hash[1], hash[2], hash[3], hash[4], hash[5], hash[6], hash[7],
		hash[8], hash[9], hash[10], hash[11], hash[12], hash[13])
}

// ExecutionIDToIPv6WithCIDR returns the IPv6 address with CIDR notation.
func ExecutionIDToIPv6WithCIDR(executionID string) string {
	return fmt.Sprintf("%s/%d", ExecutionIDToIPv6(executionID), IPv6PrefixLen)
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
