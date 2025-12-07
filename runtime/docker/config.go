package docker

import "github.com/go-playground/validator/v10"

var validate = validator.New()

// Config holds the configuration for a Docker runtime execution.
type Config struct {
	// Image is the Docker image to run.
	// Required. Can be a full image reference (e.g., "node:20-alpine") or local image name.
	Image string `json:"image" validate:"required"`

	// Command is the command to run inside the container.
	// If empty, uses the image's default CMD.
	Command []string `json:"command"`

	// WorkDir is the working directory inside the container.
	// If empty, uses the image's default WORKDIR.
	WorkDir string `json:"work_dir"`

	// Env are additional environment variables to set inside the container.
	// These are merged with the base environment variables set by the hypervisor.
	Env map[string]string `json:"env"`

	// Volumes are volume mounts in the format "host_path:container_path[:ro]".
	// Example: ["/host/data:/container/data", "/host/config:/etc/config:ro"]
	Volumes []string `json:"volumes"`

	// Ports are port mappings in the format "host_port:container_port" or "container_port".
	// Example: ["8080:80", "443"]
	Ports []string `json:"ports"`

	// Network is the Docker network to connect the container to.
	// If empty, uses the default bridge network.
	Network string `json:"network"`

	// Memory is the memory limit for the container (e.g., "512m", "1g").
	// If empty, no memory limit is set.
	Memory string `json:"memory"`

	// CPUs is the number of CPUs to allocate (e.g., "0.5", "2").
	// If empty, no CPU limit is set.
	CPUs string `json:"cpus"`

	// User is the user to run the container as (e.g., "1000:1000", "node").
	// If empty, uses the image's default user.
	User string `json:"user"`

	// CheckpointDir is the directory where checkpoint data will be stored.
	// If empty, a default location based on execution ID will be used.
	// Note: Docker checkpoint requires experimental features enabled.
	CheckpointDir string `json:"checkpoint_dir"`

	// Labels are additional labels to add to the container.
	Labels map[string]string `json:"labels"`

	// Remove determines whether to automatically remove the container when it exits.
	// Default is false to allow checkpoint/restore.
	Remove bool `json:"remove"`
}

func (c *Config) Validate() error {
	return validate.Struct(c)
}
