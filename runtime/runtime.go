package runtime

import "context"

// RuntimeType identifies the type of runtime (nodejs, docker, firecracker, etc.)
type RuntimeType string

const (
	RuntimeTypeNodeJS RuntimeType = "nodejs"
	// Future runtime types:
	// RuntimeTypeDocker     RuntimeType = "docker"
	// RuntimeTypeFirecracker RuntimeType = "firecracker"
	// RuntimeTypeBubblewrap RuntimeType = "bubblewrap"
)

// Checkpoint holds the data needed to restore a process.
// Each runtime implementation defines what this contains (file paths, serialized state, etc.)
type Checkpoint interface {
	// String returns a human-readable identifier for logging/debugging
	String() string
}

// Process represents a running execution with its own state.
// Each runtime implementation defines what this contains (PID, container ID, etc.)
type Process interface {
	// String returns a human-readable identifier for logging/debugging
	String() string

	// Checkpoint captures the current state of the execution.
	// If keepRunning is true, the process continues after checkpointing.
	// If keepRunning is false, the process is suspended after checkpointing.
	// Returns the checkpoint data needed to restore this process.
	Checkpoint(ctx context.Context, keepRunning bool) (Checkpoint, error)

	// Kill terminates the execution immediately.
	Kill(ctx context.Context) error

	// Wait blocks until the process exits and returns the exit code.
	// Returns an error if the process cannot be waited on.
	Wait(ctx context.Context) (exitCode int, err error)
}

// Runtime is a factory that creates Process instances.
// Each runtime type (NodeJS, Docker, etc.) implements this interface, with
// platform-specific implementations (Linux with CRIU, macOS with no-ops, etc.)
type Runtime interface {
	// Type returns the runtime type this implementation handles
	Type() RuntimeType

	// ParseConfig parses runtime-specific config from JSON.
	ParseConfig(raw []byte) (any, error)

	// Start launches a new execution with the given config.
	// Returns a Process that can be used for subsequent operations.
	// The env parameter contains environment variables set by the hypervisor.
	// The config parameter is runtime-specific (e.g., NodeJSConfig for nodejs).
	Start(ctx context.Context, executionID string, env map[string]string, config any) (Process, error)

	// Restore resumes an execution from a checkpoint.
	// Returns a new Process representing the restored execution.
	Restore(ctx context.Context, checkpoint Checkpoint) (Process, error)
}
