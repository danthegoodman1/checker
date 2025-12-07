package nodejs

// Config holds the configuration for a NodeJS runtime execution.
type Config struct {
	// EntryPoint is the path to the JavaScript/TypeScript file to execute.
	// This can be a bundled single file or an entry point in a directory.
	EntryPoint string

	// WorkDir is the working directory for the execution.
	// If empty, defaults to the directory containing EntryPoint.
	WorkDir string

	// NodePath is the path to the node binary.
	// If empty, uses "node" from PATH.
	NodePath string

	// Args are additional arguments to pass to the script (after the entry point).
	Args []string

	// Env are additional environment variables to set.
	// These are merged with the base environment variables set by the hypervisor.
	Env map[string]string

	// CheckpointDir is the directory where checkpoint data will be stored.
	// If empty, a default location based on execution ID will be used.
	CheckpointDir string
}
