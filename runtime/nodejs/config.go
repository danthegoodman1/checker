package nodejs

import "github.com/go-playground/validator/v10"

var validate = validator.New()

// Config holds the configuration for a NodeJS runtime execution.
type Config struct {
	// EntryPoint is the path to the JavaScript/TypeScript file to execute.
	// This can be a bundled single file or an entry point in a directory.
	EntryPoint string `json:"entry_point" validate:"required"`

	// WorkDir is the working directory for the execution.
	// If empty, defaults to the directory containing EntryPoint.
	WorkDir string `json:"work_dir"`

	// NodePath is the path to the node binary.
	// If empty, uses "node" from PATH.
	NodePath string `json:"node_path"`

	// Args are additional arguments to pass to the script (after the entry point).
	Args []string `json:"args"`

	// Env are additional environment variables to set.
	// These are merged with the base environment variables set by the hypervisor.
	Env map[string]string `json:"env"`

	// CheckpointDir is the directory where checkpoint data will be stored.
	// If empty, a default location based on execution ID will be used.
	CheckpointDir string `json:"checkpoint_dir"`
}

func (c *Config) Validate() error {
	return validate.Struct(c)
}
