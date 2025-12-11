package firecracker

import (
	"crypto/rand"
	"fmt"

	"github.com/go-playground/validator/v10"
)

var validate = validator.New()

// NetworkConfig configures TAP networking for the Firecracker VM.
// The bridge must be pre-configured on the host (see LINUX_SETUP.md).
type NetworkConfig struct {
	// BridgeName is the name of the host bridge to attach the TAP device to.
	// Required for networking. Example: "fcbr0"
	BridgeName string `json:"bridge_name" validate:"required"`

	// GuestIP is the IP address for the guest VM's eth0 interface.
	// Required. Must include CIDR notation. Example: "172.16.0.2/24"
	GuestIP string `json:"guest_ip" validate:"required"`

	// GuestGateway is the default gateway for the guest VM.
	// Required. Example: "172.16.0.1"
	GuestGateway string `json:"guest_gateway" validate:"required"`

	// GuestMAC is the MAC address for the guest VM's eth0 interface.
	// Optional. If empty, a random MAC is generated.
	GuestMAC string `json:"guest_mac"`
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

	// VsockCID is the context ID for the vsock device.
	// Used for VM-to-host communication. Guest CID must be >= 3.
	// If 0, vsock is disabled.
	VsockCID uint32 `json:"vsock_cid"`

	// BootArgs are additional kernel boot arguments.
	// If empty, sensible defaults are used for fast boot.
	BootArgs string `json:"boot_args"`

	// Network configures TAP networking for the VM.
	// Required for hypervisor integration (VM must communicate with hypervisor API).
	// The rootfs /init script must configure networking in the guest.
	Network *NetworkConfig `json:"network" validate:"required"`
}

// Validate checks that the config has all required fields.
func (c *Config) Validate() error {
	if err := validate.Struct(c); err != nil {
		return err
	}
	if c.Network != nil {
		if err := validate.Struct(c.Network); err != nil {
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
