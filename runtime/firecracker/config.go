package firecracker

import "github.com/go-playground/validator/v10"

var validate = validator.New()

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
}

// Validate checks that the config has all required fields.
func (c *Config) Validate() error {
	return validate.Struct(c)
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
